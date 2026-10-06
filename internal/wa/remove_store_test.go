package wa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"modernc.org/sqlite"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

// What the tests of a remove read in the files, and break in store.db and archive.db.

// filesHave tells whether the bytes s are in the database file at p or in its WAL.
func filesHave(t *testing.T, p, s string) bool {
	t.Helper()
	for _, name := range []string{p, p + "-wal"} {
		b, err := os.ReadFile(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}

// fillDevice gives the device of nick a row, with a marker beginning with tag, in
// each table of store.db that holds something of its own, as a device that has
// been in use would have; the markers come back. The device row itself is
// written through the client, so that it is in the WAL that the Manager is writing.
func fillDevice(t *testing.T, m *Manager, nick, tag string) []string {
	t.Helper()
	ctx := context.Background()
	m.mu.Lock()
	cli := m.accounts[nick].cli
	m.mu.Unlock()
	jid := deviceID(cli).String()
	marker := func(kind string) string { return tag + "-" + kind }
	hash := sha256.Sum256([]byte(tag))
	identity := []byte((marker("identity") + strings.Repeat("-", 32))[:32])
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := m.store.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO whatsmeow_sessions(our_jid, their_id, session) VALUES (?, 'p1.0', ?)`, jid, []byte(marker("session")))
	exec(`INSERT INTO whatsmeow_identity_keys(our_jid, their_id, identity) VALUES (?, 'p1.0', ?)`, jid, identity)
	exec(`INSERT INTO whatsmeow_sender_keys(our_jid, chat_id, sender_id, sender_key) VALUES (?, 'c', 's', ?)`, jid, []byte(marker("senderkey")))
	exec(`INSERT INTO whatsmeow_contacts(our_jid, their_jid, full_name) VALUES (?, 'p1@s.whatsapp.net', ?)`, jid, marker("contact"))
	exec(`INSERT INTO whatsmeow_message_secrets(our_jid, chat_jid, sender_jid, message_id, key) VALUES (?, 'c', 's', 'm1', ?)`, jid, []byte(marker("secret")))
	exec(`INSERT INTO whatsmeow_event_buffer(our_jid, ciphertext_hash, plaintext, server_timestamp, insert_timestamp) VALUES (?, ?, ?, 1, 1)`,
		jid, hash[:], []byte(marker("event")))
	exec(`INSERT INTO whatsmeow_retry_buffer(our_jid, chat_jid, message_id, format, plaintext, timestamp) VALUES (?, 'c', 'm1', 'f', ?, 1)`, jid, []byte(marker("retry")))
	// No foreign key ties these to the device (see deviceStore.forget).
	exec(`INSERT INTO whatsmeow_privacy_tokens(our_jid, their_jid, token, timestamp) VALUES (?, ?, ?, 1)`,
		jid, marker("partner")+"@s.whatsapp.net", []byte(marker("token")))
	cli.Store.PushName = marker("pushname")
	if err := cli.Store.Save(ctx); err != nil {
		t.Fatal(err)
	}
	return []string{marker("session"), string(identity), marker("senderkey"), marker("contact"), marker("secret"),
		marker("event"), marker("retry"), marker("partner"), marker("token"), marker("pushname")}
}

// TestRemoveLeavesNothingOfTheDeviceInStoreDB reads the files: the keys of a device
// deleted stay readable in a freed page of store.db or in its WAL unless
// secure_delete and the checkpoint remove them, and the tables that nothing
// cascades to (the privacy tokens: the JID and token of each chat partner) are
// cleaned by hand. The other device's rows stay where they are. The device is
// written through the Manager's own connection, so its rows are in the WAL, as
// those of a device paired and used in the same run of the daemon.
func TestRemoveLeavesNothingOfTheDeviceInStoreDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   *fakeNet
	}{
		{"logout works", &fakeNet{connected: map[string]bool{personalPhone: true}}},
		{"logout not possible", &fakeNet{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := removeFixture(t, tc.fn)
			gone := fillDevice(t, m, "personal", "zqxjvper")
			kept := fillDevice(t, m, "work", "zqxjvwrk")
			for _, marker := range append(slices.Clone(gone), kept...) {
				if !filesHave(t, f.storePath(), marker) {
					t.Fatalf("%q is not in the files before the remove: the test checks nothing", marker)
				}
			}

			if r := remove(t, m, "personal"); r.err != nil {
				t.Fatal(r.err)
			}
			for _, marker := range gone {
				if filesHave(t, f.storePath(), marker) {
					t.Errorf("%q of the removed device is still in store.db or its WAL", marker)
				}
			}
			for _, marker := range kept {
				if !filesHave(t, f.storePath(), marker) {
					t.Errorf("%q of the other device is gone", marker)
				}
			}
			var tokens int
			if err := m.store.db.QueryRow(`SELECT count(*) FROM whatsmeow_privacy_tokens`).Scan(&tokens); err != nil || tokens != 1 {
				t.Errorf("%d privacy tokens left, %v; want the other device's alone", tokens, err)
			}
		})
	}
}

// storeExec runs a statement on store.db of the fixture, before any Manager is
// started on it.
func (f *fixture) storeExec(t *testing.T, query string, args ...any) {
	t.Helper()
	c, err := openStore(context.Background(), f.storePath(), newWALog(quiet, "Database"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// TestStartCleansUpAfterDeletedDevices: what a device leaves in the tables that
// nothing cascades to, when whatsmeow deletes it on LoggedOut or a remove is cut
// short by a crash, is gone after the next start, and the live device's is not.
func TestStartCleansUpAfterDeletedDevices(t *testing.T) {
	f := newFixture(t)
	f.account(t, "work", workPhone)
	f.devices(t, map[string]string{workPhone: ""})
	const insert = `INSERT INTO whatsmeow_privacy_tokens(our_jid, their_jid, token, timestamp) VALUES (?, ?, ?, 1)`
	f.storeExec(t, insert, adJID(personalPhone).String(), "zqxjvghost@s.whatsapp.net", []byte("zqxjvghost-token"))
	f.storeExec(t, insert, adJID(workPhone).String(), "zqxjvlive@s.whatsapp.net", []byte("zqxjvlive-token"))
	if !filesHave(t, f.storePath(), "zqxjvghost") {
		t.Fatal("the marker is not in the file before the start: the test checks nothing")
	}

	m := f.start(t, (&fakeNet{}).network(readyGlobals()))
	if filesHave(t, f.storePath(), "zqxjvghost") {
		t.Error("the tokens of a device that is gone are still in store.db")
	}
	var live int
	if err := m.store.db.QueryRow(`SELECT count(*) FROM whatsmeow_privacy_tokens`).Scan(&live); err != nil || live != 1 || !filesHave(t, f.storePath(), "zqxjvlive") {
		t.Errorf("%d tokens left, %v; want the live device's alone", live, err)
	}
}

// TestForgetSaysWhenTheCheckpointIsHeldOff: a reader that holds off the checkpoint
// makes the driver wait out its busy timeout and answer as if all were well;
// forget tells, and has deleted the tokens all the same.
func TestForgetSaysWhenTheCheckpointIsHeldOff(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempDir(t), "store.db")
	st, err := openStore(ctx, path, newWALog(quiet, "Database"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.db.SetMaxOpenConns(1)
	if _, err := st.db.Exec(`PRAGMA busy_timeout = 200`); err != nil { // the one connection
		t.Fatal(err)
	}
	other, err := sqlitedb.Open(path, sqlitedb.Pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	reader, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var n int
	if _, err := reader.ExecContext(ctx, `BEGIN DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM whatsmeow_device`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO whatsmeow_privacy_tokens(our_jid, their_jid, token, timestamp) VALUES ('ghost', 'p', x'00', 1)`); err != nil {
		t.Fatal(err)
	}

	if err := st.forget(ctx); !errors.Is(err, sqlitedb.ErrCheckpointBusy) {
		t.Errorf("forget with a reader = %v, want the busy checkpoint", err)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM whatsmeow_privacy_tokens`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d tokens left, %v", n, err)
	}
	if _, err := reader.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if err := st.forget(ctx); err != nil {
		t.Errorf("forget without a reader = %v", err)
	}
}

// TestRemoveGoesOnWhenStoreDBCannotBeCleaned: the device is deleted when the clean-up
// of store.db is tried, so a failure of it is reported in the log and is not the
// remove's: the archive goes, and so does the account.
func TestRemoveGoesOnWhenStoreDBCannotBeCleaned(t *testing.T) {
	var buf bytes.Buffer
	m, f := removeFixture(t, &fakeNet{})
	m.log = debugLog(&buf)
	old := m.accounts["personal"].cli
	if err := deleteStored(context.Background(), old); err != nil { // as whatsmeow does on LoggedOut: no write is left for the unlinking
		t.Fatal(err)
	}
	m.store.db.Close()

	if r := remove(t, m, "personal"); r.err != nil {
		t.Fatalf("Remove = %+v, %v", r.res, r.err)
	}
	if !strings.Contains(buf.String(), "clean up store.db") {
		t.Errorf("the failed clean-up is not in the log:\n%s", buf.String())
	}
	if _, there := archiveOf(t, f)["personal"]; there || hasAccount(m, "personal") {
		t.Error("the account is still there")
	}
}

// TestRemoveGoesOnWhenTheArchiveCannotBeScrubbed: the archive's rows are deleted
// when its full-text index is merged, vacuumed and checkpointed, so a failure of
// these is reported in the log and is not the remove's: the account goes, as it is
// gone from the archive. Here the index has been dropped from the file (with the
// triggers that feed it, which would fail the delete itself), so the merge fails.
func TestRemoveGoesOnWhenTheArchiveCannotBeScrubbed(t *testing.T) {
	m, f, _ := removeFixtureTriggered(t, &fakeNet{}, "", "", "",
		"DROP TRIGGER messages_ai; DROP TRIGGER messages_au; DROP TRIGGER messages_ad; DROP TABLE messages_fts")
	var buf bytes.Buffer
	m.log = debugLog(&buf)
	if r := remove(t, m, "personal"); r.err != nil {
		t.Fatalf("Remove = %+v, %v", r.res, r.err)
	}
	if !strings.Contains(buf.String(), "may have left copies") {
		t.Errorf("the failed scrub is not in the log:\n%s", buf.String())
	}
	if _, there := archiveOf(t, f)["personal"]; there || hasAccount(m, "personal") {
		t.Error("the account is still there")
	}
}

// TestRemoveBoundsTheLogout: a Logout that gets no answer does not hold the remove for
// as long as the request takes to give up: it is cut at logoutWait, and the device
// goes the way of a Logout that was not possible, with the hint. The wait for the
// unlinking is made longer than the test would sit through, so that an unbound
// Logout shows as a remove that did not finish.
func TestRemoveBoundsTheLogout(t *testing.T) {
	fn := &fakeNet{connected: map[string]bool{personalPhone: true}, logoutHangs: true}
	m, f := removeFixture(t, fn)
	m.logoutWait, m.abortWait = 50*time.Millisecond, time.Minute
	start := time.Now()
	r := remove(t, m, "personal")
	if r.err != nil || r.res.Hint != removeDeviceHint || time.Since(start) > 2*time.Second {
		t.Fatalf("Remove = %+v, %v after %v; want the hint of a Logout that gave out, soon", r.res, r.err, time.Since(start))
	}
	want := []string{"logout " + personalPhone, "disconnect " + personalPhone + " before cancel", "disconnect pairing before cancel"}
	if got := fn.called(true); !slices.Equal(got[len(got)-3:], want) {
		t.Errorf("calls %v, want them to end with %v", got, want)
	}
	if _, there := archiveOf(t, f)["personal"]; there {
		t.Error("the archive is still there")
	}
}

// deleteHook is what the SQL function test_hook calls, with the JID of the device
// that a DELETE on whatsmeow_device is about to remove; a true answer makes the
// trigger of removeFixtureTriggered abort that DELETE.
var deleteHook atomic.Pointer[func(jid string) bool]

func init() {
	sqlite.MustRegisterScalarFunction("test_hook", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if hook := deleteHook.Load(); hook != nil {
			if jid, ok := args[0].(string); ok && (*hook)(jid) {
				return int64(1), nil
			}
		}
		return int64(0), nil
	})
}

// TestRemoveWhenWhatsmeowDeletesTheDeviceBesideUs: a stream error that says the
// device is removed makes whatsmeow delete it from its own goroutine
// (connectionevents.go:40-47), and the Delete of the remove can meet it on the ID it
// has just dropped (store.go:286-298) and fail. The device is gone, then, and so
// is the question: no error, and no hint, as the server removed it itself.
func TestRemoveWhenWhatsmeowDeletesTheDeviceBesideUs(t *testing.T) {
	m, f, _ := removeFixtureTriggered(t, &fakeNet{}, "store.db", "whatsmeow_device", "test_hook(old.jid) = 1")
	cli := m.accounts["personal"].cli
	hook := func(jid string) bool { // runs inside the Delete, which holds deviceMu (the network's deleteDevice)
		if jid != cli.Store.ID.String() {
			return false
		}
		cli.Store.ID = nil // the other Delete has got as far as this, and our DELETE is the one to fail
		return true
	}
	deleteHook.Store(&hook)
	t.Cleanup(func() { deleteHook.Store(nil) })

	r := remove(t, m, "personal")
	if r.err != nil || r.res.Hint != "" {
		t.Fatalf("Remove = %+v, %v; want no error and no hint", r.res, r.err)
	}
	if _, there := archiveOf(t, f)["personal"]; there || hasAccount(m, "personal") {
		t.Error("the account is still there")
	}
}

// TestRemoveWhenAStrayDeviceStays: an account has the device of a pairing that
// failed beside its own. The own one goes, and the hint of it is kept; the stray one
// cannot be deleted, and the call fails, with the account needs_link and its
// archive, its own client let be (not connected again: its device is gone). The
// repeat takes the stray one and gives the hint of the first.
func TestRemoveWhenAStrayDeviceStays(t *testing.T) {
	fn := &fakeNet{connected: map[string]bool{personalPhone: true}, logoutErr: errors.New("timed out")}
	m, f, lift := removeFixtureTriggered(t, fn, "store.db", "whatsmeow_device", "EXISTS (SELECT 1 FROM test_refuse) AND old.jid LIKE '%:13@%'")
	var buf bytes.Buffer
	m.log = debugLog(&buf)
	forceStatus(m, "personal", StatusNeedsLink)
	s := pair(t, m, "personal")
	q := fn.qr(t, 0)
	q.ch <- code("2@a", time.Minute)
	eventually(t, "the code", stateIs(s, pairCode))
	stray := types.NewADJID(personalPhone, 0, 13)
	if !q.cli.PrePairCallback(stray, "android", "") {
		t.Fatal("PrePairCallback refused")
	}
	saveDevice(t, q.cli.Store, stray)
	q.ch <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("boom")}
	close(q.ch)
	<-s.done
	connects := countCalls(fn, "connect "+personalPhone)

	r := remove(t, m, "personal")
	if r.err == nil || !strings.Contains(r.err.Error(), "could not delete the keys of the device") || strings.Contains(r.err.Error(), "blocked by the test") {
		t.Fatalf("Remove = %+v, %v; want the error of the stray device's delete, without the database's words", r.res, r.err)
	}
	if !strings.Contains(buf.String(), "blocked by the test") {
		t.Errorf("the database's words are not in the log:\n%s", buf.String())
	}
	m.wg.Wait()
	m.mu.Lock()
	a := m.accounts["personal"]
	cli, hint, removing := a.cli, a.removeHint, a.removing
	m.mu.Unlock()
	if got := infoOf(m, "personal"); got.Status != StatusNeedsLink || got.Reason != removalInterruptedReason || cli != nil || hint != removeDeviceHint || removing {
		t.Errorf("account %+v, client %v, hint %q, removing %v; want needs_link, removal interrupted, the hint of the device that is gone",
			got, cli, hint, removing)
	}
	if n := countCalls(fn, "connect "+personalPhone); n != connects {
		t.Errorf("%d connects after, %d before: the client of a device that is gone was connected again", n, connects)
	}
	if got := archiveOf(t, f)["personal"]; got != [2]int{1, 2} {
		t.Errorf("the archive %v was touched, though a device is still there", got)
	}
	if got := storedDevices(t, m); !slices.Contains(got, stray.String()) || slices.Contains(got, adJID(personalPhone).String()) {
		t.Errorf("devices %v, want the stray one left and the account's own gone", got)
	}

	lift()
	if r := remove(t, m, "personal"); r.err != nil || r.res.Hint != removeDeviceHint {
		t.Fatalf("the repeated Remove = %+v, %v; want the hint", r.res, r.err)
	}
	if got := storedDevices(t, m); !slices.Equal(got, []string{adJID(workPhone).String()}) {
		t.Errorf("devices %v, want work's alone", got)
	}
}

// TestRemoveInterruptedSaysSo: a remove that took the device away and was then
// given up on, or that the archive failed, leaves the account needs_link; list sends
// that to add, which keeps the archive, so the reason tells how to go on with the
// remove.
func TestRemoveInterruptedSaysSo(t *testing.T) {
	m, f, _ := removeFixtureRefusing(t, &fakeNet{}, "archive.db", "accounts")
	if r := remove(t, m, "personal"); r.err == nil {
		t.Fatal("the delete of the archive was not refused")
	}
	got := infoOf(m, "personal")
	if got.Status != StatusNeedsLink || !strings.Contains(got.Reason, "call remove-account again") || !strings.Contains(got.Reason, "add") {
		t.Errorf("account %+v", got)
	}
	if got := archiveOf(t, f)["personal"]; got != [2]int{1, 2} {
		t.Errorf("the archive %v", got)
	}
}

// TestRemovalInterruptedReason pins the wording of the reason an interrupted
// remove leaves: finishing it deletes the archive for good, and the agent may meet
// the reason long after the call, so it is told to ask the user first, as the
// refusal of a relink with another number does; and that add is the other way out,
// which keeps the archive.
func TestRemovalInterruptedReason(t *testing.T) {
	for _, want := range []string{"the removal was interrupted", "call remove-account again", "deletes the message archive",
		"ask the user first", "call add to link the account again", "the archive is kept"} {
		if !strings.Contains(removalInterruptedReason, want) {
			t.Errorf("the reason %q lacks %q", removalInterruptedReason, want)
		}
	}
}

// TestAddRefusedWhileTheArchiveIsDeleted: between the device going and the archive
// going the account is needs_link with no device, which add would pair; it is
// refused all the same, as the account is being removed. The archive's writer is
// held by another connection to keep the remove at its delete.
func TestAddRefusedWhileTheArchiveIsDeleted(t *testing.T) {
	ctx := context.Background()
	m, f := removeFixture(t, &fakeNet{})
	w, err := sqlitedb.Open(filepath.Join(f.dir, "archive.db"), sqlitedb.Pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	conn, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	res := removeAsync(ctx, m, "personal")
	eventually(t, "the device gone, and the delete of the archive waiting", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		a := m.accounts["personal"]
		return a.cli == nil && a.unlinking == nil && a.removing
	})
	for _, req := range []LinkRequest{{Nick: "personal"}, {Nick: "personal", Phone: "+7 000 000 00 01"}, {Nick: "personal", QRImage: true}} {
		if _, err := m.Link(ctx, req); err == nil || !strings.Contains(err.Error(), "is being removed") {
			t.Errorf("Link %+v while the archive is being deleted: %v", req, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if r := removed(t, res); r.err != nil {
		t.Fatal(r.err)
	}
}

// TestLoginLinksChangeInTheHoldOfMu: an add that hands out a link, and one that ends
// links, change the nonces in the same hold of mu as the decision they follow; an
// issue or a revoke after it would let a later add's link be ended by an earlier
// one, or the other way round. Every way to a decision is covered: the link, the
// pairing for the chat and by code, with the cancel of the one before, a reconnect,
// and a remove.
func TestLoginLinksChangeInTheHoldOfMu(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.account(t, "personal", personalPhone)
	f.devices(t, map[string]string{personalPhone: ""})
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	m.wg.Wait()
	var changes atomic.Int32
	m.nonces.onChange = func() {
		changes.Add(1)
		if m.mu.TryLock() {
			m.mu.Unlock()
			t.Error("a login link was issued or revoked outside the hold of mu that decided")
		}
	}

	for _, req := range []LinkRequest{{Nick: "fresh"}, {Nick: "fresh", QRImage: true}, {Nick: "fresh"}, {Nick: "fresh", Phone: typedPhone}} {
		if _, err := m.Link(ctx, req); err != nil {
			t.Fatalf("Link %+v: %v", req, err)
		}
	}
	forceStatus(m, "personal", StatusReplaced)
	if _, err := m.Link(ctx, LinkRequest{Nick: "personal"}); err != nil {
		t.Fatal(err)
	}
	if r := remove(t, m, "fresh"); r.err != nil {
		t.Fatal(r.err)
	}
	if n := changes.Load(); n < 6 {
		t.Errorf("only %d changes of links seen", n)
	}
}

// TestRemovedAccountIsNotBroughtBackByAPoll: a poll of the login page that found the
// nonce before a remove revoked it, and reaches admit after the remove, finds no
// account, and makes none: no row in archive.db, no entry in the manager, no
// pairing.
func TestRemovedAccountIsNotBroughtBackByAPoll(t *testing.T) {
	ctx := context.Background()
	fn := &fakeNet{firstCode: "2@one"}
	m, f := removeFixture(t, fn)
	nonce := linkNonce(t, m, "fresh")
	if r := remove(t, m, "fresh"); r.err != nil {
		t.Fatal(r.err)
	}
	if _, err := m.Login(ctx, "fresh", nonce); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Login by the link of a removed account = %v, want ErrNoLogin", err)
	}
	// What the poll that passed the nonce check earlier meets: a nonce that works, of
	// an account that is gone.
	n := m.nonces.issue("fresh")
	if _, err := m.Login(ctx, "fresh", n.value); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Login of a poll that overtook the remove = %v, want ErrNoLogin", err)
	}
	if s, err := m.add(ctx, "fresh"); s != nil || !errors.Is(err, ErrNoLogin) {
		t.Errorf("add by the page = %v, %v; want ErrNoLogin", s, err)
	}
	if _, there := archiveOf(t, f)["fresh"]; there || hasAccount(m, "fresh") || qrCount(fn) != 0 {
		t.Error("the poll brought the removed account back")
	}
	// The add tool still can: the nick is free.
	if _, err := m.Link(ctx, LinkRequest{Nick: "fresh"}); err != nil {
		t.Errorf("Link of the removed nick = %v", err)
	}
}

// TestDeviceGone: a device is gone when whatsmeow has deleted it, or is deleting it,
// which drops the ID before it sets Deleted.
func TestDeviceGone(t *testing.T) {
	m, _ := removeFixture(t, &fakeNet{})
	cli := m.accounts["personal"].cli
	if deviceGone(cli) {
		t.Fatal("a client with its device is gone")
	}
	deviceMu.Lock()
	defer deviceMu.Unlock()
	id := cli.Store.ID
	cli.Store.ID = nil
	if !deviceGone(cli) {
		t.Error("a client with no ID is not on its way")
	}
	cli.Store.ID, cli.Store.Deleted = id, true
	if !deviceGone(cli) {
		t.Error("a deleted device is not gone")
	}
}
