package wa

import (
	"context"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// blockLIDs is a LID store that holds the handler in its first lookup until release
// is closed, to have a handler at work for a test to look at.
type blockLIDs struct {
	store.LIDStore
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func newBlockLIDs() blockLIDs {
	return blockLIDs{entered: make(chan struct{}), release: make(chan struct{}), once: new(sync.Once)}
}

func (b blockLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return types.EmptyJID, nil
}
func (b blockLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	return types.EmptyJID, nil
}
func (b blockLIDs) PutLIDMapping(context.Context, types.JID, types.JID) error { return nil }

// TestReceiveCloseWaitsForAHandlerAtWork: the archive closes after the Manager, so
// the Manager's Close must not return while a handler is still at work, and a
// message that was cut off by it is not acknowledged.
func TestReceiveCloseWaitsForAHandlerAtWork(t *testing.T) {
	r := newRx(t)
	cli := r.cli("personal")
	b := newBlockLIDs()
	cli.Store.LIDs = b
	acked := make(chan bool, 1)
	go func() { acked <- send(cli, at(incoming("M1", pnJID(bobPN)), time.Second), text("hello")) }()
	<-b.entered
	closed := make(chan struct{})
	go func() { r.m.Close(); close(closed) }()
	select {
	case <-closed:
		t.Error("Close returned while a handler was still at work: the archive may be closed under it")
	case <-time.After(300 * time.Millisecond):
	}
	close(b.release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned")
	}
	if <-acked {
		t.Error("a message that was cut off by Close was acknowledged")
	}
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v of a message that was cut off", n)
	}
}

// TestReceiveCloseCancelsWhatWaitsForTheWriter: a write that waits for archive.db
// is cancelled by Close, and not left to wait for the time a write is given, which
// would keep Close, and the daemon that stops with it, waiting for a writer that
// is held.
func TestReceiveCloseCancelsWhatWaitsForTheWriter(t *testing.T) {
	for name, tc := range map[string]struct {
		work    func(r *rx) (acked bool)
		wantAck bool
	}{
		"message": {func(r *rx) bool {
			return r.deliver("personal", at(incoming("M1", pnJID(bobPN)), time.Second), text("hello"))
		}, false},
		"history notification": {func(r *rx) bool {
			return r.deliver("personal", outgoing("H1", pnJID(ownPN)), historyNotice())
		}, false},
		"name of a group": {func(r *rx) bool {
			return !r.cli("personal").DangerousInternals().DispatchEvent(&events.JoinedGroup{GroupInfo: *group(teamID, "Team")})
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRx(t)
			r.m.writeWait = 2 * time.Second // what a broken cancellation would wait for
			held, release := make(chan struct{}), make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				r.db.Tx(context.Background(), func(*archive.Tx) error { close(held); <-release; return nil })
			}()
			<-held
			acked := make(chan bool, 1)
			go func() { acked <- tc.work(r) }()
			time.Sleep(100 * time.Millisecond) // the handler is waiting for the writer
			start := time.Now()
			r.m.Close()
			if d := time.Since(start); d > time.Second {
				t.Errorf("Close took %v: it waited for the write instead of cancelling it", d)
			}
			if got := <-acked; got != tc.wantAck {
				t.Errorf("acknowledged = %v, want %v", got, tc.wantAck)
			}
			close(release)
			<-done
		})
	}
}

// TestReceiveStaleClientsOtherEventsAreNotFailed: the events of a client that is no
// longer the account's are not its to answer for, and false would stop whatever
// handlers come after ours, so none is given for what is not a message.
func TestReceiveStaleClientsOtherEventsAreNotFailed(t *testing.T) {
	r := newRx(t)
	old := r.cli("personal")
	if res := remove(t, r.m, "personal"); res.err != nil {
		t.Fatal(res.err)
	}
	for _, evt := range []any{&events.Connected{}, &events.Disconnected{}, &events.JoinedGroup{GroupInfo: *group(teamID, "Team")}} {
		if old.DangerousInternals().DispatchEvent(evt) {
			t.Errorf("a stale client's %T was failed", evt)
		}
	}
}

// TestReceiveOfAnOldClientWhoseNickIsTakenAgain: the nick of a removed account is
// given to a new one, and the old client, which is not the new account's, writes
// nothing for it.
func TestReceiveOfAnOldClientWhoseNickIsTakenAgain(t *testing.T) {
	r := newRx(t)
	old := r.cli("personal")
	r.mustDeliver(t, "personal", at(incoming("M1", pnJID(bobPN)), time.Second), text("before"))
	if res := remove(t, r.m, "personal"); res.err != nil {
		t.Fatal(res.err)
	}
	if _, err := r.m.admit(context.Background(), "personal", kindChat, ""); err != nil {
		t.Fatal(err)
	}
	if !send(old, at(incoming("M2", pnJID(bobPN)), 2*time.Second), text("late, from the old device")) {
		t.Error("a message of the old device was failed")
	}
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the new account has %v of the old device's message", n)
	}
}

// handlerFiles are the files whose code runs for an event on whatsmeow's goroutine.
var handlerFiles = []string{"receive.go", "author.go", "classify.go", "chatid.go", "reconcile.go", "groups.go"}

// TestHandlerFilesNeverCallTheClient: the handler makes no call to WhatsApp, which
// the test of a message with a fake network shows for what goes through the
// network's seam. A call made straight on the client would only fail with "not
// connected", and nothing would see it, so the code is looked at as well: no method
// of the client is called in the files of the handler (its fields are read; the
// network's seam takes the client as an argument).
func TestHandlerFilesNeverCallTheClient(t *testing.T) {
	call := regexp.MustCompile(`\bcli\.[A-Z]\w*\(`)
	for _, name := range handlerFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if loc := call.FindIndex(src); loc != nil {
			t.Errorf("%s calls a method of the client: %q", name, src[loc[0]:loc[1]])
		}
	}
}
