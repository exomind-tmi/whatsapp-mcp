package wa

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// bobBlob puts at path a blob of Bob's chat with the messages ids.
func (x *hx) bobBlob(path string, ids ...string) {
	blob := hblob()
	conv := hconv(bobChat, "Bob")
	for i, id := range ids {
		conv.Messages = append(conv.Messages, hin(bobChat, id, "text of "+id, time.Duration(i+1)*time.Second))
	}
	blob.Conversations = append(blob.Conversations, conv)
	x.h.put(path, blob)
}

// listed is the account as list shows it.
func (x *hx) listed(nick string) AccountInfo {
	for _, a := range x.m.Accounts(context.Background()) {
		if a.Nick == nick {
			return a
		}
	}
	return AccountInfo{}
}

// poke tells the account's worker to look again, as a connection coming or going would.
func (x *hx) poke(nick string) {
	x.m.mu.Lock()
	defer x.m.mu.Unlock()
	if w := x.m.accounts[nick].history; w != nil {
		w.signal()
	}
}

// TestHistoryFailureIsRetriedWithBackoffAndGivenUpOn: a notification that cannot be
// downloaded is tried again after pauses that double, up to the limit, is given up
// on after MaxAttempts, and is skipped from then on, without holding up the
// notification behind it. What list says of the account is the number of parts that
// are missing and the fixed words of the cause, and not what the error was.
func TestHistoryFailureIsRetriedWithBackoffAndGivenUpOn(t *testing.T) {
	x := newHx(t, 100)
	const base = 60 * time.Millisecond
	x.m.historyRetry, x.m.historyRetryMax = base, 4*base
	secret := &url.Error{Op: "Get", URL: "https://mmg.example/v/t62/blob-path?oh=SECRET", Err: errors.New("connection refused")}
	x.bobBlob("/h/2", "G1", "G2")
	x.h.failNext("/h/1", secret, secret, secret, secret, secret, secret) // one more than it will be given
	x.announce(t, "personal", "H1", "/h/1")
	x.announce(t, "personal", "H2", "/h/2")

	x.connect("personal")
	x.imported2(t, "personal", "H1 is given up on and H2 imported")

	calls := x.h.downloads()
	var first []downloadCall
	for _, c := range calls {
		if c.path == "/h/1" {
			first = append(first, c)
		}
	}
	if len(first) != archive.MaxAttempts {
		t.Fatalf("%d downloads of the failing notification, want %d", len(first), archive.MaxAttempts)
	}
	for i, want := range []time.Duration{base, 2 * base, 4 * base, 4 * base} { // doubling, to the limit
		if gap := first[i+1].at.Sub(first[i].at); gap < want-want/10 {
			t.Errorf("the attempt %d came %v after the one before, want at least %v", i+2, gap, want)
		}
	}
	if last := calls[len(calls)-1]; last.path != "/h/2" || last.at.Before(first[len(first)-1].at) {
		t.Errorf("the notification behind the failing one: %+v, want it imported after", last)
	}
	if gap := calls[len(calls)-1].at.Sub(first[len(first)-1].at); gap > 3*base {
		t.Errorf("the notification behind waited %v after the one that was given up on: no pause is due for a row that is skipped", gap)
	}
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1", "G2"}) {
		t.Errorf("Bob's messages %v", got)
	}
	for _, d := range x.h.deletes() {
		if d.GetDirectPath() == "/h/1" {
			t.Error("a blob that was never read is deleted from the server")
		}
	}

	// Given up on: left in the queue, never offered, reported.
	attempts, last, ok := x.queueRow(t, "personal")
	if !ok || attempts != archive.MaxAttempts || last != causeDownload {
		t.Errorf("the queue row: %d attempts, %q, %v; want the one given up on, with fixed words", attempts, last, ok)
	}
	if _, found, err := x.db.QueueNext(context.Background(), "personal"); found || err != nil {
		t.Errorf("QueueNext offers a notification that was given up on: %v, %v", found, err)
	}
	acc := x.listed("personal")
	if acc.HistoryStuck != 1 || acc.Status != StatusConnected ||
		!strings.Contains(acc.Reason, "1 part of the history sync could not be imported: some older messages are missing") ||
		!strings.Contains(acc.Reason, causeDownload) {
		t.Errorf("list shows %+v", acc)
	}
	for _, text := range []string{acc.Reason, last, x.logs.String()} {
		if strings.Contains(text, "SECRET") || strings.Contains(text, "blob-path") {
			t.Errorf("the address of the blob is in %q", text)
		}
	}
	log := x.logs.String()
	if !strings.Contains(log, "history import failed; trying again later") || !strings.Contains(log, "given up on") || !strings.Contains(log, "connection refused") {
		t.Errorf("the failures are not in the log, with their reason:\n%s", log)
	}
}

// imported2 waits for the worker to end and the queue to hold nothing that is not
// given up on.
func (x *hx) imported2(t *testing.T, nick, what string) {
	t.Helper()
	x.within(t, 30*time.Second, what, func() bool {
		x.m.mu.Lock()
		a := x.m.accounts[nick]
		ended := a == nil || a.history == nil
		x.m.mu.Unlock()
		if !ended {
			return false
		}
		_, found, err := x.db.QueueNext(context.Background(), nick)
		return err == nil && !found
	})
}

// TestHistoryStuckReasonOfAnAccountThatHasAWorseOne: the missing history is the
// reason of a connected account only, and never replaces another: an account that
// has to be linked again is told that first. The count is there whichever it is.
func TestHistoryStuckReasonOfAnAccountThatHasAWorseOne(t *testing.T) {
	stuck := archive.Stuck{Count: 2, LastError: causeWrite}
	for status, wantReason := range map[Status]bool{
		StatusConnected: true, StatusReconnecting: true,
		StatusLinking: false, StatusNeedsLink: false, StatusError: false, StatusReplaced: false, StatusClientOutdated: false,
	} {
		got := withStuckHistory(AccountInfo{Status: status}, stuck)
		if got.HistoryStuck != 2 || (got.Reason != "") != wantReason {
			t.Errorf("%s: %+v", status, got)
		}
	}
	if got := withStuckHistory(AccountInfo{Status: StatusConnected, Reason: "something else"}, stuck); got.Reason != "something else" || got.HistoryStuck != 2 {
		t.Errorf("a reason that is there is replaced: %+v", got)
	}
	if got := stuckReason(stuck); got != "2 parts of the history sync could not be imported: some older messages are missing (archive write failed)" {
		t.Errorf("reason %q", got)
	}
	if got := stuckReason(archive.Stuck{Count: 1}); strings.Contains(got, "(") || !strings.HasPrefix(got, "1 part of") {
		t.Errorf("reason %q", got)
	}
}

// TestHistoryDroppedConnectionIsNotAnAttempt: a download that fails because the
// connection has gone costs the notification nothing: it is not counted, no pause is
// put before the next, the worker ends, and nothing is tried before the connection is
// back. When it is, the notification is imported.
func TestHistoryDroppedConnectionIsNotAnAttempt(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	var mu sync.Mutex
	var seen []Status
	drop := true
	x.h.onDownload = func(ctx context.Context, cli *whatsmeow.Client, n *waE2E.HistorySyncNotification) error {
		mu.Lock()
		seen = append(seen, infoOf(x.m, "personal").Status)
		first := drop
		drop = false
		mu.Unlock()
		if first { // the network goes in the middle of the download
			cli.DangerousInternals().DispatchEvent(&events.Disconnected{})
			return errors.New("connection reset")
		}
		return nil
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.idle(t, "personal")

	if attempts, _, ok := x.queueRow(t, "personal"); !ok || attempts != 0 {
		t.Errorf("the queue row: %d attempts, %v; want the notification untouched", attempts, ok)
	}
	time.Sleep(30 * time.Millisecond) // a worker that went on would have tried again by now
	if n := len(x.h.downloads()); n != 1 {
		t.Fatalf("%d downloads while the connection was down", n)
	}
	if got := infoOf(x.m, "personal").Status; got != StatusReconnecting {
		t.Errorf("status %s: the worker must not touch it", got)
	}

	x.connect("personal")
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
		t.Errorf("Bob's messages %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, s := range seen {
		if s != StatusConnected {
			t.Errorf("download %d began with the account %s: nothing is tried before the connection is back", i+1, s)
		}
	}
}

// TestHistoryPauseSurvivesAReconnect: the pause after a failed import is the account's,
// not the worker's, so a connection that drops and comes back, ending the worker and
// starting another, is not a way round it: a notification that fails is not tried
// again at once, and so given up on in a moment.
func TestHistoryPauseSurvivesAReconnect(t *testing.T) {
	x := newHx(t, 100)
	const pause = 400 * time.Millisecond
	x.m.historyRetry, x.m.historyRetryMax = pause, pause
	x.bobBlob("/h/1", "G1")
	x.h.failNext("/h/1", errors.New("the media server is down"))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	// The download is recorded when it begins, and the failure is counted, and the pause
	// set, after it returns: a connection that drops in between is the failure of a
	// dropped connection, which is not counted, and there is no pause to keep.
	x.within(t, 5*time.Second, "the first attempt to be counted", func() bool {
		return strings.Contains(x.logs.String(), "history import failed; trying again later")
	})
	cli := x.cli("personal").DangerousInternals()
	cli.DispatchEvent(&events.Disconnected{})
	x.poke("personal") // what a worker waiting out its pause is told when the connection goes
	x.idle(t, "personal")
	if n := len(x.h.downloads()); n != 1 {
		t.Fatalf("%d downloads: the pause was not waited out by the worker that ended", n)
	}
	cli.DispatchEvent(&events.Connected{}) // at once
	x.imported(t, "personal")
	calls := x.h.downloads()
	if len(calls) != 2 {
		t.Fatalf("%d downloads", len(calls))
	}
	if gap := calls[1].at.Sub(calls[0].at); gap < pause-pause/10 {
		t.Errorf("the second attempt came %v after the first, want the pause of %v kept", gap, pause)
	}
}

// TestHistoryChunkFailureKeepsTheBlobAndTheQueue: a chunk that cannot be written fails
// the attempt: the queue row stays, counted, with fixed words; the chunks written
// before it stay (the writes are idempotent); the blob is not deleted from the server,
// as the data is not all stored; and the next attempt, with the archive working, writes
// all of it once and deletes the blob.
func TestHistoryChunkFailureKeepsTheBlobAndTheQueue(t *testing.T) {
	x := newHx(t, 2, refuseSQL)
	x.m.historyRetry, x.m.historyRetryMax = 300*time.Millisecond, 300*time.Millisecond
	x.bobBlob("/h/1", "M1", "M2", "M3", "M4", "M5")
	x.refuse(t, refuseMessageM) // M3
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")

	x.within(t, 5*time.Second, "the failed attempt", func() bool {
		a, _, ok := x.queueRow(t, "personal")
		return ok && a == 1
	})
	if _, last, _ := x.queueRow(t, "personal"); last != causeWrite {
		t.Errorf("last_error %q, want %q", last, causeWrite)
	}
	if n := len(x.h.deletes()); n != 0 {
		t.Errorf("the blob is deleted from the server after a chunk failed (%d times)", n)
	}
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"M1"}) {
		t.Errorf("after the failed attempt the archive has %v, want the chunk that was written", got)
	}
	if n := x.size(t, "personal"); n != [2]int{1, 1} {
		t.Errorf("the archive has %v chats and messages", n)
	}

	x.allow(t)
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"M1", "M2", "M3", "M4", "M5"}) {
		t.Errorf("Bob's messages %v", got)
	}
	if n := len(x.h.deletes()); n != 1 {
		t.Errorf("the blob is deleted %d times, want once, after the last chunk", n)
	}
}

// TestHistoryDeleteFailureOnlyLogs: the blob that cannot be deleted from the server is
// left there, with its data safe in the archive: the notification is imported, off the
// queue, and not counted against, and the log says so.
func TestHistoryDeleteFailureOnlyLogs(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	x.h.deleteErr = errors.New("the media server refused")
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
		t.Errorf("Bob's messages %v", got)
	}
	if n := len(x.h.downloads()); n != 1 {
		t.Errorf("%d downloads: a blob that could not be deleted is not imported again", n)
	}
	log := x.logs.String()
	if !strings.Contains(log, "its data is stored") || !strings.Contains(log, "the media server refused") || strings.Contains(log, "history import failed") {
		t.Errorf("the failed delete is not logged as only that:\n%s", log)
	}
	if acc := x.listed("personal"); acc.HistoryStuck != 0 || acc.Reason != "" {
		t.Errorf("list shows %+v", acc)
	}
}

// TestHistoryWorkerBookkeeping: the two decisions that keep the worker from being lost
// or doubled. A signal that is waiting when the worker would end keeps it going, and
// without one it ends and is forgotten, so that the next start makes a new one; and no
// worker is started for an account that is not the Manager's.
func TestHistoryWorkerBookkeeping(t *testing.T) {
	x := newHx(t, 100)
	x.m.mu.Lock()
	a := x.m.accounts["personal"]
	x.m.mu.Unlock()

	w := &historyWorker{wake: make(chan struct{}, 1)}
	x.m.mu.Lock()
	a.history = w
	x.m.mu.Unlock()
	w.signal()
	w.signal() // one pending is as good as many
	if !x.m.historyMore(a, w) {
		t.Error("a worker with a signal waiting is told to end")
	}
	if x.m.historyMore(a, w) {
		t.Error("a worker with no signal waiting is told to go on")
	}
	x.m.mu.Lock()
	gone := a.history == nil
	x.m.mu.Unlock()
	if !gone {
		t.Error("a worker that ends is still the account's")
	}

	x.m.mu.Lock()
	x.m.dropAccount("personal")
	x.m.mu.Unlock()
	x.m.startHistory(a)
	x.m.mu.Lock()
	started := a.history != nil
	x.m.mu.Unlock()
	if started {
		t.Error("a worker is started for an account that has been dropped")
	}
}

// TestHistoryPanicIsAFailedAttempt: a panic in the import (here in the parse, and in
// the download) is recovered and logged with its stack and no content, counted as a
// failed attempt, and the worker goes on: the retry succeeds, and a notification that
// panics every time would be given up on like any other.
func TestHistoryPanicIsAFailedAttempt(t *testing.T) {
	for name, arm := range map[string]func(x *hx, once *sync.Once){
		"in the parse": func(x *hx, once *sync.Once) {
			x.h.onParse = func(n int) { once.Do(func() { panic("SECRET-PANIC") }) }
		},
		"in the download": func(x *hx, once *sync.Once) {
			x.h.onDownload = func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) error {
				once.Do(func() { panic("SECRET-PANIC") })
				return nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newHx(t, 100)
			x.bobBlob("/h/1", "G1", "G2")
			arm(x, new(sync.Once))
			x.connect("personal")
			x.announce(t, "personal", "H1", "/h/1")
			x.imported(t, "personal")
			if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1", "G2"}) {
				t.Errorf("Bob's messages %v", got)
			}
			log := x.logs.String()
			if !strings.Contains(log, "history import panicked") || !strings.Contains(log, "goroutine") ||
				!strings.Contains(log, "history import failed; trying again later") || !strings.Contains(log, "cause=\"internal error\"") {
				t.Errorf("the panic is not in the log as a failed attempt:\n%s", log)
			}
			if n := len(x.h.downloads()); n != 2 {
				t.Errorf("%d downloads, want the one that panicked and the retry", n)
			}
		})
	}
}

// panicOnce is a log handler that panics at the first record that says msg.
type panicOnce struct {
	slog.Handler
	msg  string
	once *sync.Once
}

func (p panicOnce) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == p.msg {
		p.once.Do(func() { panic("a bug in the code that logs") })
	}
	return p.Handler.Handle(ctx, r)
}

// TestHistoryWorkerPanicEndsTheWorkerAndNothingElse: a panic outside the import itself,
// here in the log of its outcome, ends the worker, which is logged with its stack and no
// content to be read in it; the process lives, the work that was done stays done, and the next
// notification gets a worker again.
func TestHistoryWorkerPanicEndsTheWorkerAndNothingElse(t *testing.T) {
	x := newHx(t, 100)
	x.m.log = slog.New(panicOnce{Handler: x.m.log.Handler(), msg: "history sync imported", once: new(sync.Once)})
	x.bobBlob("/h/1", "G1")
	x.bobBlob("/h/2", "G2")
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal") // the worker is gone, and the queue is empty: the import was committed
	requireEnded(t, x.m)
	if log := x.logs.String(); !strings.Contains(log, "history worker panicked") || !strings.Contains(log, "stack=") {
		t.Errorf("the panic is not in the log as one of the worker, with its stack:\n%s", log)
	}
	x.announce(t, "personal", "H2", "/h/2")
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1", "G2"}) {
		t.Errorf("Bob's messages %v", got)
	}
}

// TestHistoryPanicEveryTimeIsGivenUpOn: a notification that makes the code panic at
// each try ends as stuck, as one that fails.
func TestHistoryPanicEveryTimeIsGivenUpOn(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	x.h.onParse = func(int) { panic("always") }
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported2(t, "personal", "the notification to be given up on")
	if attempts, last, _ := x.queueRow(t, "personal"); attempts != archive.MaxAttempts || last != causeInternal {
		t.Errorf("the queue row: %d attempts, %q", attempts, last)
	}
	// And the account still works for what comes live.
	x.mustDeliver(t, "personal", at(incoming("L1", pnJID(bobPN)), time.Second), text("still alive"))
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"L1"}) {
		t.Errorf("Bob's messages %v", got)
	}
}

// TestHistoryReceipt: the phone is told that the announcement has arrived, once for
// each delivery that was kept, after it is kept and whatever becomes of the
// download, which here has not even begun to finish. An announcement that was not
// kept, or is not the phone's own, gets none; one that is delivered again is one row
// and is told again; and a receipt that cannot be sent, or panics, costs the message
// nothing.
func TestHistoryReceipt(t *testing.T) {
	t.Run("sent once, after the row, before the download is done", func(t *testing.T) {
		x := newHx(t, 100)
		x.bobBlob("/h/1", "G1")
		gate := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-gate:
			default:
				close(gate)
			}
		})
		var mu sync.Mutex
		var rowAtReceipt bool
		var rowErr error
		hooked := make(chan struct{})
		x.h.onReceipt = func(id string) error {
			_, found, err := x.db.QueueNext(context.Background(), "personal")
			mu.Lock()
			rowAtReceipt, rowErr = found, err
			mu.Unlock()
			close(hooked)
			return nil
		}
		x.h.onDownload = func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) error {
			<-gate
			return nil
		}
		x.connect("personal")
		x.announce(t, "personal", "H1", "/h/1")
		waitFor(t, "the receipt", hooked)
		// The download is held at the gate, and the phone has been told all the same.
		x.within(t, 5*time.Second, "the download", func() bool { return len(x.h.downloads()) == 1 })
		mu.Lock()
		if !rowAtReceipt {
			t.Errorf("the receipt went out before the notification was in the queue (%v)", rowErr)
		}
		mu.Unlock()
		close(gate)
		x.imported(t, "personal")
		time.Sleep(20 * time.Millisecond)
		if got := x.h.receipted(); len(got) != 1 || got[0] != "H1" {
			t.Errorf("receipts %v, want exactly one, for H1", got)
		}
	})

	t.Run("delivered again", func(t *testing.T) {
		x := newHx(t, 100)
		x.bobBlob("/h/1", "G1")
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		x.h.onDownload = func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) error {
			<-gate
			return nil
		}
		x.connect("personal")
		x.announce(t, "personal", "H1", "/h/1")
		x.announce(t, "personal", "H1", "/h/1")
		x.within(t, 5*time.Second, "both receipts", func() bool { return len(x.h.receipted()) == 2 })
		if n := x.queued(t, "personal"); n != 1 {
			t.Errorf("%d rows in the queue, want the one notification once", n)
		}
	})

	t.Run("not kept, not the phone's, no id", func(t *testing.T) {
		x := newHx(t, 100, `DROP TABLE history_queue`)
		x.connect("personal")
		if x.deliver("personal", outgoing("H1", pnJID(ownPN)), notice("/h/1")) {
			t.Error("a notification that could not be kept was acknowledged")
		}
		x.mustDeliver(t, "personal", incoming("H2", pnJID(bobPN)), notice("/h/2")) // a stranger's
		x.mustDeliver(t, "personal", outgoing("", pnJID(ownPN)), notice("/h/3"))   // no id
		time.Sleep(30 * time.Millisecond)
		if got := x.h.receipted(); len(got) != 0 {
			t.Errorf("receipts %v for what was not kept", got)
		}
	})

	t.Run("that fails or panics", func(t *testing.T) {
		for name, fail := range map[string]func(string) error{
			"fails":  func(string) error { return errors.New("not connected") },
			"panics": func(string) error { panic("SECRET-PANIC") },
		} {
			t.Run(name, func(t *testing.T) {
				x := newHx(t, 100)
				x.bobBlob("/h/1", "G1")
				x.h.onReceipt = fail
				x.connect("personal")
				x.announce(t, "personal", "H1", "/h/1")
				x.imported(t, "personal")
				if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
					t.Errorf("Bob's messages %v: the history is imported all the same", got)
				}
				x.within(t, 5*time.Second, "the receipt to be logged", func() bool {
					return strings.Contains(x.logs.String(), "send the receipt of a history sync") || strings.Contains(x.logs.String(), "history receipt panicked")
				})
			})
		}
	})
}

// TestHistoryWorkerNeedsAConnectedAccount: a notification queued while the account is
// not connected is imported when it is, and by one worker; the worker does not run for
// an account that is not connected, linking, or without a device.
func TestHistoryWorkerNeedsAConnectedAccount(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	x.announce(t, "personal", "H1", "/h/1") // the account is reconnecting: the fake network never says Connected
	time.Sleep(30 * time.Millisecond)
	if n := len(x.h.downloads()); n != 0 {
		t.Fatalf("%d downloads for an account that is not connected", n)
	}
	x.m.mu.Lock()
	running := x.m.accounts["personal"].history != nil
	x.m.mu.Unlock()
	if running {
		t.Error("a worker is left running for an account that is not connected")
	}
	x.m.mu.Lock()
	personal := x.m.accounts["personal"]
	x.m.mu.Unlock()
	for _, s := range []Status{StatusNeedsLink, StatusLinking, StatusReplaced, StatusError, StatusClientOutdated} {
		forceStatus(x.m, "personal", s)
		x.m.startHistory(personal)
		x.idle(t, "personal")
	}
	if n := len(x.h.downloads()); n != 0 {
		t.Fatalf("%d downloads for an account that is %s", n, infoOf(x.m, "personal").Status)
	}
	forceStatus(x.m, "personal", StatusReconnecting)
	x.connect("personal")
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
		t.Errorf("Bob's messages %v", got)
	}
}

// TestHistoryFailureThatAnotherTryWouldMeetAgainIsGivenUpAtOnce: a blob the media server
// says is gone is not there at the next try, the same answer would come at each of the
// five and the notifications behind it would wait out the pauses between them: it is
// given up on at the first (with a pause that would hold the next up for good, to see
// that there is none). What an earlier failure of another kind has used of the attempts
// counts, and the rest is used at once.
func TestHistoryFailureThatAnotherTryWouldMeetAgainIsGivenUpAtOnce(t *testing.T) {
	for name, gone := range map[string]error{
		"403": whatsmeow.ErrMediaDownloadFailedWith403,
		"404": whatsmeow.ErrMediaDownloadFailedWith404,
		"410": whatsmeow.ErrMediaDownloadFailedWith410,
	} {
		t.Run(name, func(t *testing.T) {
			x := newHx(t, 100)
			x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
			x.bobBlob("/h/2", "G1")
			x.h.failNext("/h/1", gone, gone, gone, gone, gone, gone)
			x.announce(t, "personal", "H1", "/h/1")
			x.announce(t, "personal", "H2", "/h/2")
			x.connect("personal")
			x.imported2(t, "personal", "H1 given up on and H2 imported")

			if n := len(x.h.downloads()); n != 2 {
				t.Errorf("%d downloads, want one for each notification", n)
			}
			if attempts, last, ok := x.queueRow(t, "personal"); !ok || attempts != archive.MaxAttempts || last != causeGone {
				t.Errorf("the queue row: %d attempts, %q, %v; want the one given up on, for the blob that is gone", attempts, last, ok)
			}
			if acc := x.listed("personal"); acc.HistoryStuck != 1 || !strings.Contains(acc.Reason, causeGone) {
				t.Errorf("list shows %+v", acc)
			}
			if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
				t.Errorf("Bob's messages %v", got)
			}
		})
	}

	t.Run("after a failure that a try may cure", func(t *testing.T) {
		x := newHx(t, 100)
		x.m.historyRetry, x.m.historyRetryMax = 5*time.Millisecond, 5*time.Millisecond
		x.bobBlob("/h/1", "G1")
		x.h.failNext("/h/1", errors.New("connection reset"), whatsmeow.ErrMediaDownloadFailedWith410, whatsmeow.ErrMediaDownloadFailedWith410)
		x.announce(t, "personal", "H1", "/h/1")
		x.connect("personal")
		x.imported2(t, "personal", "H1 given up on")
		if n := len(x.h.downloads()); n != 2 {
			t.Errorf("%d downloads, want the one that failed in passing and the one that found the blob gone", n)
		}
		if attempts, last, _ := x.queueRow(t, "personal"); attempts != archive.MaxAttempts || last != causeGone {
			t.Errorf("the queue row: %d attempts, %q", attempts, last)
		}
	})

	t.Run("a failure of the network is tried again", func(t *testing.T) {
		x := newHx(t, 100)
		x.m.historyRetry, x.m.historyRetryMax = 5*time.Millisecond, 5*time.Millisecond
		x.bobBlob("/h/1", "G1")
		x.h.failNext("/h/1", errors.New("connection reset"), context.DeadlineExceeded)
		x.announce(t, "personal", "H1", "/h/1")
		x.connect("personal")
		x.imported(t, "personal")
		if n := len(x.h.downloads()); n != 3 {
			t.Errorf("%d downloads, want the two that failed and the one that did not", n)
		}
	})
}

// TestHistoryRowThatCannotBeCountedIsNotDownloadedInALoop: the queue that refuses the
// count that would give a row up leaves it offered, with the failures it has, and the
// worker must not take that for "given up on" and go on at once: it waits out the pause
// like after any other failure, or it downloads the blob again as fast as it can.
func TestHistoryRowThatCannotBeCountedIsNotDownloadedInALoop(t *testing.T) {
	x := newHx(t, 100, `CREATE TRIGGER no_last BEFORE UPDATE ON history_queue WHEN NEW.attempts >= 5 BEGIN SELECT RAISE(ABORT, 'read-only'); END`)
	const pause = 50 * time.Millisecond
	x.m.historyRetry, x.m.historyRetryMax = pause, pause
	x.h.onDownload = func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) error {
		return errors.New("the media server is down")
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.within(t, 5*time.Second, "the fifth download", func() bool { return len(x.h.downloads()) >= archive.MaxAttempts })
	time.Sleep(10 * pause)
	// One more every pause, ten pauses on, and some slack: a loop makes thousands.
	if n := len(x.h.downloads()); n > archive.MaxAttempts+12 {
		t.Errorf("%d downloads of a row that could not be counted, within %v of the fifth", n, 10*pause)
	}
	if !strings.Contains(x.logs.String(), "record a failed history import") {
		t.Errorf("the failure to count it is not in the log:\n%s", x.logs)
	}
}

// panicOnceLIDs is a LID store that panics once, when it is armed and asked for the LID of a
// number: the filing of the chats after an import is what asks.
type panicOnceLIDs struct {
	store.LIDStore
	armed atomic.Bool
}

func (p *panicOnceLIDs) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	if p.armed.CompareAndSwap(true, false) {
		panic("a bug in the LID store")
	}
	return p.LIDStore.GetLIDForPN(ctx, pn)
}

// TestHistoryPanicAfterTheLastCommitIsNoFailedImport: the notification is off the queue
// when its blob is deleted from the server and its chats are filed, and a panic in
// either (whatsmeow's delete reads the first of the media hosts without looking whether
// there are any) is logged and no more: it is not an import that failed, no pause is put
// before the next (the one here would be waited out for good), and the other step is
// done all the same.
func TestHistoryPanicAfterTheLastCommitIsNoFailedImport(t *testing.T) {
	for name, tc := range map[string]struct {
		arm func(x *hx, lids *panicOnceLIDs) // makes the step panic
		log string                           // what the log says of it
		// what the other step has done by then
		filed func(t *testing.T, x *hx)
	}{
		"the delete of the blob": {
			arm: func(x *hx, _ *panicOnceLIDs) {
				var once atomic.Bool
				x.h.onDelete = func(*waE2E.HistorySyncNotification) {
					if once.CompareAndSwap(false, true) {
						panic("index out of range [0] with length 0")
					}
				}
			},
			log: "history sync clean-up panicked",
			filed: func(t *testing.T, x *hx) {
				if got := ids(x.msgs(t, "personal", carolLID+"@lid")); !equalIDs(got, []string{"C1"}) {
					t.Errorf("Carol's chat is not filed under her LID after the panic of the delete: %v", got)
				}
			},
		},
		"the filing of the chats": {
			arm: func(x *hx, lids *panicOnceLIDs) {
				x.h.onDelete = func(n *waE2E.HistorySyncNotification) {
					if n.GetDirectPath() == "/h/1" {
						lids.armed.Store(true)
					}
				}
			},
			log: "history sync filing of chats panicked",
			filed: func(t *testing.T, x *hx) {
				if n := len(x.h.deletes()); n != 2 {
					t.Errorf("%d blobs deleted from the server, want both", n)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newHx(t, 100)
			x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
			cli := x.cli("personal")
			lids := &panicOnceLIDs{LIDStore: cli.Store.LIDs}
			cli.Store.LIDs = lids
			x.mustDeliver(t, "personal", at(incoming("C1", pnJID(carolPN)), time.Second), text("sent by number"))
			x.h.put("/h/1", withPairs(hblob(), carolLID, carolPN))
			x.bobBlob("/h/2", "G1")
			tc.arm(x, lids)
			x.connect("personal")
			x.announce(t, "personal", "H1", "/h/1")
			x.announce(t, "personal", "H2", "/h/2")
			x.imported(t, "personal")

			log := x.logs.String()
			if !strings.Contains(log, tc.log) || !strings.Contains(log, "stack=") {
				t.Errorf("the panic is not in the log, with its stack:\n%s", log)
			}
			if strings.Contains(log, "import failed") || strings.Contains(log, "history import panicked") {
				t.Errorf("an import that was committed is said to have failed:\n%s", log)
			}
			if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
				t.Errorf("Bob's messages %v: the notification behind waited", got)
			}
			tc.filed(t, x)
		})
	}
}
