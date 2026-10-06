package wa

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
)

// refuseSQL makes inserts into archive.db fail while test_refuse has a row: of
// a chat (1), or of the message M3 alone (2), so that a test can have a write
// fail after others of the same transaction have gone through. It is made
// before the Manager opens the file, and lifted by a delete, not by a change of
// the schema, which a connection that has it loaded does not survive.
const refuseSQL = `
CREATE TABLE test_refuse(x INTEGER);
CREATE TRIGGER test_refuse_chat BEFORE INSERT ON chats
  WHEN EXISTS (SELECT 1 FROM test_refuse WHERE x = 1)
  BEGIN SELECT RAISE(ABORT, 'blocked by the test'); END;
CREATE TRIGGER test_refuse_message BEFORE INSERT ON messages
  WHEN EXISTS (SELECT 1 FROM test_refuse WHERE x = 2) AND new.msg_id = 'M3'
  BEGIN SELECT RAISE(ABORT, 'blocked by the test'); END`

const (
	refuseChats    = 1
	refuseMessageM = 2
)

func (r *rx) archiveExec(t *testing.T, query string, args ...any) {
	t.Helper()
	w, err := sqlitedb.Open(filepath.Join(r.dir, "archive.db"), sqlitedb.Pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (r *rx) refuse(t *testing.T, what int) {
	t.Helper()
	r.archiveExec(t, `INSERT INTO test_refuse VALUES (?)`, what)
}
func (r *rx) allow(t *testing.T) { t.Helper(); r.archiveExec(t, `DELETE FROM test_refuse`) }

// TestReceiveArchiveFailureIsNotAcknowledged: a message the archive cannot take
// is not acknowledged, for WhatsApp to send it again, and leaves nothing of itself:
// not the row that was written before the failing one, not the chat. Everything
// else the handler is given is acknowledged as before, and the same message,
// sent again once the archive works, goes in whole.
func TestReceiveArchiveFailureIsNotAcknowledged(t *testing.T) {
	r := newRx(t, refuseSQL)
	bob := pnJID(bobPN)
	info := at(incoming("M1", bob), time.Second)
	info.PushName = "Zorblax"
	msg := text("SECRET-TEXT")

	r.refuse(t, refuseChats) // the chat is the last thing a message writes, after its row
	if r.deliver("personal", info, msg) {
		t.Fatal("a message the archive refused was acknowledged")
	}
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v chats and messages of a message that was not stored", n)
	}
	// Nothing else is held against: not what is not for the archive, not other events.
	if !r.deliver("personal", incoming("R1", bob), &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{}}) {
		t.Error("a message that is not for the archive was not acknowledged")
	}
	if r.cli("personal").DangerousInternals().DispatchEvent(&events.Connected{}) {
		t.Error("another event was failed")
	}

	log := r.logs.String()
	if !strings.Contains(log, "archive write failed") || !strings.Contains(log, "level=WARN") {
		t.Errorf("the failure is not in the log:\n%s", log)
	}
	for _, secret := range []string{"SECRET", "Zorblax", bobPN, ownPN} {
		if strings.Contains(log, secret) {
			t.Errorf("%q is in the log:\n%s", secret, log)
		}
	}

	r.allow(t)
	r.mustDeliver(t, "personal", info, msg)
	if got := r.msgs(t, "personal", bobPNChat); len(got) != 1 || got[0].Text != "SECRET-TEXT" {
		t.Errorf("after the retry: %+v", got)
	}
	if cs := r.chats(t, "personal"); len(cs) != 1 || cs[0].Name != "Zorblax" {
		t.Errorf("after the retry: chats %+v", cs)
	}
}

// TestReceiveFailureAfterTheMergeLeavesTheMergeUndone: the merge of the chat's
// old rows and the new message are one transaction; the message failing takes
// the merge with it, and the rows are where they were.
func TestReceiveFailureAfterTheMergeLeavesTheMergeUndone(t *testing.T) {
	r := newRx(t, refuseSQL)
	r.seedBobByNumber(t, "personal")

	r.refuse(t, refuseMessageM)
	if r.deliver("personal", byLID("M3", 4*time.Second), text("three")) {
		t.Fatal("a message the archive refused was acknowledged")
	}
	cs := r.chats(t, "personal")
	if len(cs) != 1 || cs[0].JID != bobPNChat {
		t.Errorf("chats %+v, want Bob's, still under the number", cs)
	}
	if got := ids(r.msgs(t, "personal", bobPNChat)); !equalIDs(got, []string{"M1", "M2"}) {
		t.Errorf("under the number: %v, want M1 and M2 where they were", got)
	}
	if got := r.msgs(t, "personal", bobLIDChat); len(got) != 0 {
		t.Errorf("under the LID: %+v, want nothing", got)
	}

	r.allow(t)
	r.mustDeliver(t, "personal", byLID("M3", 4*time.Second), text("three"))
	r.requireOneChatUnderTheLID(t, "personal", "M1", "M2", "M3")
}

// TestReceiveClosedArchive: a handler whose archive is gone fails its message
// and does not panic.
func TestReceiveClosedArchive(t *testing.T) {
	r := newRx(t)
	r.db.Close()
	if r.deliver("personal", incoming("M1", pnJID(bobPN)), text("hello")) {
		t.Error("a message was acknowledged with no archive to hold it")
	}
	if !strings.Contains(r.logs.String(), "archive write failed") {
		t.Errorf("no failure in the log:\n%s", r.logs)
	}
}

// panicLIDs is a LID store that fails as a store never should.
type panicLIDs struct{ store.LIDStore }

func (panicLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) { panic("the store broke") }
func (panicLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) { panic("the store broke") }
func (panicLIDs) PutLIDMapping(context.Context, types.JID, types.JID) error { panic("the store broke") }

// TestReceivePanicIsNotAcknowledged: whatsmeow recovers the panic of a handler and
// takes the event for handled, which would acknowledge a message nobody stored.
func TestReceivePanicIsNotAcknowledged(t *testing.T) {
	r := newRx(t)
	r.cli("personal").Store.LIDs = panicLIDs{}
	if r.deliver("personal", incoming("M1", pnJID(bobPN)), text("SECRET-TEXT")) {
		t.Error("a message whose handler panicked was acknowledged")
	}
	if log := r.logs.String(); !strings.Contains(log, "panicked") || strings.Contains(log, "SECRET") {
		t.Errorf("want the panic in the log, and not the text:\n%s", log)
	}
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v", n)
	}
}

// TestReceiveWithNoLIDStore: a client whose device was never saved has none; its
// message is filed under the number it came from.
func TestReceiveWithNoLIDStore(t *testing.T) {
	r := newRx(t)
	r.cli("personal").Store.LIDs = nil
	r.mustDeliver(t, "personal", byLID("M1", time.Second), text("hello"))
	if cs := r.chats(t, "personal"); len(cs) != 1 || cs[0].JID != bobLIDChat {
		t.Errorf("chats %+v", cs)
	}
}

// TestReceiveWaitsForTheArchiveOnlyAWhile: a writer held for good makes the
// handler give up its message after writeWait, and while it waits the Manager is
// not held up: it answers as it always does. (The handler runs under whatsmeow's
// handler lock; the Manager's mu is for the account's state alone.)
func TestReceiveWaitsForTheArchiveOnlyAWhile(t *testing.T) {
	r := newRx(t)
	r.m.writeWait = 150 * time.Millisecond
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.db.Tx(context.Background(), func(*archive.Tx) error { close(held); <-release; return nil })
	}()
	<-held

	result := make(chan bool, 1)
	start := time.Now()
	go func() { result <- r.deliver("personal", incoming("M1", pnJID(bobPN)), text("hello")) }()
	accounts := make(chan []AccountInfo, 1)
	go func() { accounts <- r.m.Accounts(context.Background()) }()
	select {
	case a := <-accounts:
		if len(a) != 2 {
			t.Errorf("accounts %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Error("the Manager is held up by a handler that waits for the archive")
	}
	select {
	case acked := <-result:
		if acked {
			t.Error("a message that could not be written was acknowledged")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("the handler waited %v for a writer", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler is still waiting for the archive")
	}
	close(release)
	<-done
	r.mustDeliver(t, "personal", incoming("M1", pnJID(bobPN)), text("hello"))
}

// TestReceiveMakesNoCallToWhatsApp: the handler is synchronous, and on a
// whatsmeow goroutine that waits for it. It reads the databases and writes the
// archive, and that is all: not a connect, not a query for groups, and the client
// that has no socket stays without one.
func TestReceiveMakesNoCallToWhatsApp(t *testing.T) {
	r := newRx(t)
	before := r.fn.called(true)
	bob := pnJID(bobPN)
	r.mustDeliver(t, "personal", at(incoming("M1", bob), time.Second), text("hello"))
	r.mustDeliver(t, "personal", byLID("M2", 2*time.Second), text("by LID"))
	r.mustDeliver(t, "personal", at(inGroup("G1", bob), 3*time.Second), text("in a group"))
	r.mustDeliver(t, "personal", at(incoming("E1", bob), 4*time.Second), edit("M1", text("fixed")))
	r.mustDeliver(t, "personal", at(incoming("D1", bob), 5*time.Second), revoke("M1"))
	r.mustDeliver(t, "personal", outgoing("H1", pnJID(ownPN)), historyNotice())
	if got := r.fn.called(true); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("calls %v, were %v", got, before)
	}
	r.fn.mu.Lock()
	groups := r.fn.groupsCalls
	r.fn.mu.Unlock()
	if groups != 0 {
		t.Errorf("%d queries for groups", groups)
	}
	if r.cli("personal").IsConnected() {
		t.Error("the client is connected")
	}
}

// TestReceiveLogsNoContent: the log has what failed and where, never what was
// said, who said it or to whom.
func TestReceiveLogsNoContent(t *testing.T) {
	r := newRx(t, refuseSQL)
	bob := pnJID(bobPN)
	hist := historyNotice()
	hist.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = []byte("SECRET-PAYLOAD")
	secret := func(info types.MessageInfo) types.MessageInfo { info.PushName = "Zorblax"; return info }

	r.mustDeliver(t, "personal", secret(at(incoming("M1", bob), time.Second)), text("SECRET-TEXT"))
	r.mustDeliver(t, "personal", secret(byLID("M2", 2*time.Second)), text("SECRET-TEXT"))
	r.mustDeliver(t, "personal", secret(at(inGroup("G1", bob), 3*time.Second)), text("SECRET-TEXT"))
	r.mustDeliver(t, "personal", outgoing("H1", pnJID(ownPN)), hist)
	r.refuse(t, refuseChats)
	r.deliver("personal", secret(at(incoming("M9", bob), 9*time.Second)), text("SECRET-TEXT"))
	r.deliver("personal", secret(byLID("M8", 9*time.Second)), text("SECRET-TEXT"))

	log := r.logs.String()
	if !strings.Contains(log, "archive write failed") {
		t.Fatalf("the test makes no failure to look at:\n%s", log)
	}
	for _, secret := range []string{"SECRET", "Zorblax", bobPN, bobLID, ownPN, groupID} {
		if strings.Contains(log, secret) {
			t.Errorf("%q is in the log:\n%s", secret, log)
		}
	}
}

// TestReceiveAfterClose: once the Manager is closing no message is written and
// none is acknowledged, whatever it is: the archive closes next, and what is not
// acknowledged comes again at the next start.
func TestReceiveAfterClose(t *testing.T) {
	r := newRx(t)
	cli := r.cli("personal")
	r.mustDeliver(t, "personal", incoming("M0", pnJID(bobPN)), text("before"))
	if err := r.m.Close(); err != nil {
		t.Fatal(err)
	}
	for name, msg := range map[string]*waE2E.Message{
		"a text":       text("after"),
		"a reaction":   {ReactionMessage: &waE2E.ReactionMessage{}},
		"history sync": historyNotice(),
	} {
		info := outgoing("M1", pnJID(bobPN))
		if send(cli, info, msg) {
			t.Errorf("%s was acknowledged after Close", name)
		}
	}
	if got := ids(r.msgs(t, "personal", bobPNChat)); !equalIDs(got, []string{"M0"}) {
		t.Errorf("messages %v, want only the one from before", got)
	}
	// Close drops the sockets, which whatsmeow reports as events: they are not held against.
	if cli.DangerousInternals().DispatchEvent(&events.Disconnected{}) {
		t.Error("an event other than a message was failed")
	}
	requireEnded(t, r.m)
}

// TestReceiveOnceTheContextIsCancelled: Close cancels the Manager's context first
// and takes its lock after, and a message in between must not be written either.
func TestReceiveOnceTheContextIsCancelled(t *testing.T) {
	r := newRx(t)
	r.m.cancel()
	if r.deliver("personal", incoming("M1", pnJID(bobPN)), text("hello")) {
		t.Error("a message was acknowledged after the context ended")
	}
	// Nor one that is not for the archive: the rule is the same for every message,
	// a history notification among them, which WhatsApp does not repeat once it has
	// been acknowledged.
	if r.deliver("personal", incoming("R1", pnJID(bobPN)), &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{}}) {
		t.Error("a message that is not for the archive was acknowledged after the context ended")
	}
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v", n)
	}
}

// TestReceiveRacingClose: messages arrive on many goroutines while the Manager
// closes. Each is acknowledged and in the archive, or neither; and when Close
// has returned no handler is at work, so that the archive may be closed.
func TestReceiveRacingClose(t *testing.T) {
	for round := range 5 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			r := newRx(t)
			cli := r.cli("personal")
			const n = 60
			acked := make([]bool, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range n {
				wg.Go(func() {
					<-start
					acked[i] = send(cli, at(incoming(fmt.Sprintf("M%03d", i), pnJID(bobPN)), time.Duration(i)*time.Second), text("x"))
				})
			}
			close(start)
			if err := r.m.Close(); err != nil {
				t.Error(err)
			}
			requireEnded(t, r.m)
			wg.Wait()

			stored := map[string]bool{}
			for _, m := range r.msgs(t, "personal", bobPNChat) {
				stored[m.ID] = true
			}
			for i := range n {
				if id := fmt.Sprintf("M%03d", i); acked[i] != stored[id] {
					t.Errorf("%s: acknowledged %v, stored %v", id, acked[i], stored[id])
				}
			}
		})
	}
}

// rowsOf counts what archive.db holds of the account in a table, by a connection
// of its own: an orphan has no account to be listed under.
func (r *rx) rowsOf(t *testing.T, table, nick string) int {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(r.dir, "archive.db"), sqlitedb.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE account = ?`, nick).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestReceiveOfARemovedAccount: the events of an account that has been removed
// write nothing for it, and the archive does not get its rows back, which the
// account's foreign key would refuse anyway. They are taken (acknowledged), as
// nobody is left to send them again. The accounts that are not removed go on.
func TestReceiveOfARemovedAccount(t *testing.T) {
	r := newRx(t)
	old := r.cli("personal")
	bob := pnJID(bobPN)
	r.mustDeliver(t, "personal", incoming("M1", bob), text("before"))
	r.mustDeliver(t, "work", incoming("M1", bob), text("before"))

	if res := remove(t, r.m, "personal"); res.err != nil {
		t.Fatal(res.err)
	}
	if !send(old, at(incoming("M2", bob), time.Second), text("after")) {
		t.Error("a message of a removed account's client was failed: there is nobody to send it again")
	}
	send(old, incoming("E1", bob), edit("M1", text("fixed")))
	old.DangerousInternals().DispatchEvent(&events.JoinedGroup{GroupInfo: types.GroupInfo{JID: groupJID(), GroupName: types.GroupName{Name: "Team"}}})
	for _, table := range []string{"messages", "chats"} {
		if n := r.rowsOf(t, table, "personal"); n != 0 {
			t.Errorf("%s still has %d rows of the removed account", table, n)
		}
	}
	if _, ok := archiveOf(t, r.fixture)["personal"]; ok {
		t.Error("the account is back in the archive")
	}
	r.mustDeliver(t, "work", at(incoming("M2", bob), time.Second), text("after"))
	if got := ids(r.msgs(t, "work", bobPNChat)); !equalIDs(got, []string{"M1", "M2"}) {
		t.Errorf("work's messages %v", got)
	}
}

// TestReceiveOfADroppedAccountWhoseClientIsStillItsOwn is the guard that an
// account no longer in the Manager is not written for, on its own: a removal
// ends the account's client first (the test above), so no real path has the two
// apart, and the test takes the account out as the removal's last step does.
func TestReceiveOfADroppedAccountWhoseClientIsStillItsOwn(t *testing.T) {
	r := newRx(t)
	cli := r.cli("personal")
	r.m.mu.Lock()
	r.m.dropAccount("personal")
	r.m.mu.Unlock()
	if !send(cli, incoming("M1", pnJID(bobPN)), text("hello")) {
		t.Error("the message of a dropped account was failed")
	}
	if n := r.rowsOf(t, "messages", "personal") + r.rowsOf(t, "chats", "personal"); n != 0 {
		t.Errorf("%d rows were written for a dropped account", n)
	}
}

// TestReceiveRacingRemove: messages arrive while the account is removed. At the
// end none of it is left, in any table, and the others' are.
func TestReceiveRacingRemove(t *testing.T) {
	for round := range 3 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			r := newRx(t)
			old := r.cli("personal")
			r.mustDeliver(t, "work", incoming("W1", pnJID(bobPN)), text("work's"))
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for g := range 4 {
				wg.Go(func() {
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						id := fmt.Sprintf("M%d-%d", g, i)
						send(old, at(incoming(id, pnJID(bobPN)), time.Duration(i)*time.Second), text("x"))
						send(old, at(inGroup(id, pnJID(bobPN)), time.Duration(i)*time.Second), text("x"))
					}
				})
			}
			time.Sleep(20 * time.Millisecond)
			if res := remove(t, r.m, "personal"); res.err != nil {
				t.Fatal(res.err)
			}
			close(stop)
			wg.Wait()
			for _, table := range []string{"messages", "chats"} {
				if n := r.rowsOf(t, table, "personal"); n != 0 {
					t.Errorf("%s has %d rows of the removed account", table, n)
				}
			}
			if n := r.size(t, "work"); n != [2]int{1, 1} {
				t.Errorf("work has %v", n)
			}
		})
	}
}
