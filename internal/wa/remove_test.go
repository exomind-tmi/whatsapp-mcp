package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

// The numbers of the accounts the remove tests start with.
const (
	personalPhone = "70000000001"
	workPhone     = "70000000003"
)

// seedArchive gives nick's account a chat with two messages, straight into
// archive.db.
func seedArchive(t *testing.T, f *fixture, nick string) {
	t.Helper()
	w, err := sqlitedb.Open(filepath.Join(f.dir, "archive.db"), sqlitedb.Pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec(fmt.Sprintf(`
INSERT INTO chats(account, jid) VALUES ('%[1]s', 'c1@s.whatsapp.net');
INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text) VALUES
  ('%[1]s', 'c1@s.whatsapp.net', 'm1', 'c1@s.whatsapp.net', 0, 1, 'a'),
  ('%[1]s', 'c1@s.whatsapp.net', 'm2', 'c1@s.whatsapp.net', 0, 2, 'b')`, nick)); err != nil {
		t.Fatal(err)
	}
}

// archiveOf is what archive.db holds of each account, its chats and its
// messages; an account it does not have has no entry.
func archiveOf(t *testing.T, f *fixture) map[string][2]int {
	t.Helper()
	accs, err := f.db.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]int{}
	for _, a := range accs {
		out[a.Nick] = [2]int{a.Chats, a.Messages}
	}
	return out
}

// removeFixture is a Manager with four accounts: "personal" and "work", each with
// a device and an archive, "fresh", which was never linked, and "gone", whose
// device this computer has lost. fn tells how the devices connect.
func removeFixture(t *testing.T, fn *fakeNet) (*Manager, *fixture) {
	t.Helper()
	m, f, _ := removeFixtureRefusing(t, fn, "", "")
	return m, f
}

// removeFixtureRefusing is removeFixture with deletes from table refused in the
// database file, "archive.db" or "store.db", as a failing disk would, until lift
// is called. The refusal is made before the Manager opens the files, and it is a
// trigger that looks for a row, so that lifting it is a delete and not a change of
// the schema: a connection that has the schema loaded fails its next statements
// ("no such table") after another one has changed it, as the archive's writer does.
func removeFixtureRefusing(t *testing.T, fn *fakeNet, file, table string) (m *Manager, f *fixture, lift func()) {
	t.Helper()
	return removeFixtureTriggered(t, fn, file, table, "EXISTS (SELECT 1 FROM test_refuse)")
}

// removeFixtureTriggered is removeFixtureRefusing for the deletes that satisfy the
// condition when, an SQL expression on the row, old; lift cancels a condition of
// the form "EXISTS (SELECT 1 FROM test_refuse) AND ...". The statements prep are
// run on archive.db first, as it is when the Manager opens it.
func removeFixtureTriggered(t *testing.T, fn *fakeNet, file, table, when string, prep ...string) (m *Manager, f *fixture, lift func()) {
	t.Helper()
	dir := testutil.TempDir(t)
	archivePath := filepath.Join(dir, "archive.db")
	db, err := archive.Open(archivePath) // the schema, for the refusal to be made in
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, stmt := range prep {
		w, err := sqlitedb.Open(archivePath, sqlitedb.Pragmas)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		w.Close()
	}
	f = &fixture{dir: dir, log: quiet}
	f.devices(t, map[string]string{personalPhone: "Anton", workPhone: ""})
	lift = func() {}
	if file != "" {
		w, err := sqlitedb.Open(filepath.Join(dir, file), sqlitedb.Pragmas)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Exec(`
CREATE TABLE test_refuse(x);
INSERT INTO test_refuse VALUES (1);
CREATE TRIGGER test_refuse_delete BEFORE DELETE ON ` + table + ` WHEN ` + when + `
  BEGIN SELECT RAISE(ABORT, 'blocked by the test'); END`); err != nil {
			t.Fatal(err)
		}
		lift = sync.OnceFunc(func() {
			if _, err := w.Exec(`DELETE FROM test_refuse`); err != nil {
				t.Error(err)
			}
			w.Close()
		})
		t.Cleanup(lift)
	}
	if f.db, err = archive.Open(archivePath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.account(t, "fresh", "")
	f.account(t, "gone", "70000000002")
	f.account(t, "personal", personalPhone)
	f.account(t, "work", workPhone)
	seedArchive(t, f, "personal")
	seedArchive(t, f, "work")
	m = f.start(t, fn.network(readyGlobals()))
	m.wg.Wait()
	return m, f, lift
}

type removeResult struct {
	res RemoveResult
	err error
}

func removeAsync(ctx context.Context, m *Manager, nick string) <-chan removeResult {
	out := make(chan removeResult, 1)
	go func() {
		r, err := m.Remove(ctx, nick)
		out <- removeResult{r, err}
	}()
	return out
}

// removed waits up to 5 s for a call started by removeAsync.
func removed(t *testing.T, res <-chan removeResult) removeResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Remove did not return")
		return removeResult{}
	}
}

// remove is Remove, bounded: a call that waits for something that never comes
// fails the test instead of hanging it.
func remove(t *testing.T, m *Manager, nick string) removeResult {
	t.Helper()
	return removed(t, removeAsync(context.Background(), m, nick))
}

func hasAccount(m *Manager, nick string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.accounts[nick]
	return ok
}

// unlinkingOf is the goroutine that is taking nick's device away, if any.
func unlinkingOf(m *Manager, nick string) *unlink {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accounts[nick].unlinking
}

// requireEnded fails unless every goroutine the Manager has started has ended.
func requireEnded(t *testing.T, m *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("a goroutine of the Manager is still running")
	}
}

// TestRemove takes each state an account can be in through Remove: the device
// goes the way Remove documents, the hint comes under the conditions it names,
// and whatever the state, the archive, the account and its device are gone, and
// the others' are not.
func TestRemove(t *testing.T) {
	const phone = personalPhone
	banned := func(t *testing.T, m *Manager) {
		m.accounts["personal"].cli.DangerousInternals().DispatchEvent(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour})
	}
	unlinkedOnThePhone := func(t *testing.T, m *Manager) {
		old := m.accounts["personal"].cli
		old.DangerousInternals().DispatchEvent(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
		if err := deleteStored(context.Background(), old); err != nil { // as whatsmeow does on LoggedOut
			t.Fatal(err)
		}
	}
	// Logout could not: Disconnect, Delete, and a Disconnect for a connect that came up between.
	fallback := []string{"logout " + phone, "disconnect " + phone + " before cancel", "disconnect pairing before cancel"}
	for _, tc := range []struct {
		name  string
		nick  string
		fn    *fakeNet
		prep  func(*testing.T, *Manager)
		calls []string // what Remove made WhatsApp do
		hint  string
	}{
		{"connected, logout works", "personal", &fakeNet{connected: map[string]bool{phone: true}}, nil,
			[]string{"logout " + phone}, ""},
		{"connected, logout fails", "personal", &fakeNet{connected: map[string]bool{phone: true}, logoutErr: errors.New("timed out")}, nil,
			fallback, removeDeviceHint},
		{"not connected", "personal", &fakeNet{}, nil, fallback, removeDeviceHint},
		{"banned", "personal", &fakeNet{}, banned, fallback, removeDeviceHint},
		{"unlinked on the phone", "personal", &fakeNet{}, unlinkedOnThePhone,
			[]string{"logout pairing", "disconnect pairing before cancel"}, ""}, // whatsmeow deleted the device: nothing to tell
		{"never linked", "fresh", &fakeNet{}, nil, nil, ""},
		{"keys lost", "gone", &fakeNet{}, nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := removeFixture(t, tc.fn)
			if tc.prep != nil {
				tc.prep(t, m)
			}
			before, devices := tc.fn.called(true), storedDevices(t, m)

			r := remove(t, m, tc.nick)
			if r.err != nil || r.res.Hint != tc.hint {
				t.Fatalf("Remove = %+v, %v; want no error and the hint %q", r.res, r.err, tc.hint)
			}
			if got := tc.fn.called(true)[len(before):]; !slices.Equal(got, tc.calls) {
				t.Errorf("Remove made WhatsApp do %v, want %v", got, tc.calls)
			}
			if hasAccount(m, tc.nick) {
				t.Error("the account is still in the manager")
			}
			for _, a := range m.Accounts(context.Background()) {
				if a.Nick == tc.nick {
					t.Errorf("list still shows %+v", a)
				}
			}
			want := map[string][2]int{"fresh": {}, "gone": {}, "personal": {1, 2}, "work": {1, 2}}
			delete(want, tc.nick)
			if got := archiveOf(t, f); !maps.Equal(got, want) {
				t.Errorf("archive.db holds %v, want %v", got, want)
			}
			wantDevices := slices.DeleteFunc(slices.Clone(devices), func(d string) bool { return tc.nick == "personal" && d == adJID(personalPhone).String() })
			if got := storedDevices(t, m); !slices.Equal(got, wantDevices) {
				t.Errorf("devices %v, want %v", got, wantDevices)
			}
			requireEnded(t, m)
		})
	}
}

// TestRemoveWhileWhatsmeowDeletesTheDevice: whatsmeow's Delete drops the device's
// ID before it sets Deleted (store.go:286-298), and a remove that looks in
// between finds a device that is gone and has nothing to delete, hint or not.
func TestRemoveWhileWhatsmeowDeletesTheDevice(t *testing.T) {
	m, f := removeFixture(t, &fakeNet{})
	dev := m.accounts["personal"].cli.Store
	if err := m.store.DeleteDevice(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
	deviceMu.Lock()
	dev.ID = nil // Deleted is still false
	deviceMu.Unlock()
	if r := remove(t, m, "personal"); r.err != nil || r.res.Hint != "" {
		t.Fatalf("Remove = %+v, %v", r.res, r.err)
	}
	if _, there := archiveOf(t, f)["personal"]; there || hasAccount(m, "personal") {
		t.Error("the account is still there")
	}
}

// TestRemoveUnknown: an account that is not there is told so, and nothing moves.
func TestRemoveUnknown(t *testing.T) {
	fn := &fakeNet{}
	m, f := removeFixture(t, fn)
	before, archive := fn.called(true), archiveOf(t, f)
	r := remove(t, m, "nobody")
	var rf refusal
	if !errors.As(r.err, &rf) || !strings.Contains(r.err.Error(), `no account "nobody"`) || r.res != (RemoveResult{}) {
		t.Errorf("Remove = %+v, %v; want a refusal that names the account", r.res, r.err)
	}
	if !slices.Equal(fn.called(true), before) || !maps.Equal(archiveOf(t, f), archive) || len(m.Accounts(context.Background())) != 4 {
		t.Error("a remove of an unknown account changed something")
	}
}

// TestRemoveEndsThePairing: a pairing that the phone has not scanned, of any
// kind, is cancelled and awaited, and its login link ends, before the account
// goes; with a device, of a relink, it goes too.
func TestRemoveEndsThePairing(t *testing.T) {
	for _, kind := range []string{"page", "phone", "chat"} {
		for _, relink := range []bool{false, true} {
			name := kind
			if relink {
				name += ", relink of an account with a device"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				fn := &fakeNet{firstCode: "2@one"}
				m, f := removeFixture(t, fn)
				nick, number, hint := "fresh", typedPhone, ""
				if relink {
					nick, number, hint = "personal", "+7 000 000 00 01", removeDeviceHint
					forceStatus(m, nick, StatusNeedsLink)
				}
				var nonce string
				switch kind {
				case "page":
					nonce = linkNonce(t, m, nick)
					if _, err := m.Login(ctx, nick, nonce); err != nil {
						t.Fatal(err)
					}
					eventually(t, "the code", stateIs(sessOf(m, nick), pairCode))
				case "phone":
					if r := link(t, m, nick, number); r.err != nil {
						t.Fatal(r.err)
					}
				case "chat":
					if r := linkQR(t, m, nick); r.err != nil {
						t.Fatal(r.err)
					}
				}
				s, q := sessOf(m, nick), fn.qr(t, 0)

				r := remove(t, m, nick)
				if r.err != nil || r.res.Hint != hint {
					t.Fatalf("Remove = %+v, %v; want the hint %q", r.res, r.err, hint)
				}
				if !isDone(s) || q.ctx.Err() == nil || !slices.Contains(fn.called(false), "disconnect pairing before cancel") {
					t.Errorf("the pairing was not cancelled and awaited: %v", fn.called(true))
				}
				if nonce != "" {
					if m.ValidLogin(nick, nonce) {
						t.Error("the login link survived the remove")
					}
					if _, err := m.Login(ctx, nick, nonce); !errors.Is(err, ErrNoLogin) {
						t.Errorf("Login by the link of a removed account = %v, want ErrNoLogin", err)
					}
				}
				if _, there := archiveOf(t, f)[nick]; there || hasAccount(m, nick) {
					t.Error("the account is still there")
				}
				// The relinked account's device is gone, the other accounts' are not.
				if got := storedDevices(t, m); slices.Contains(got, adJID(personalPhone).String()) == relink || !slices.Contains(got, adJID(workPhone).String()) {
					t.Errorf("devices %v", got)
				}
				requireEnded(t, m)
			})
		}
	}
}

// TestRemoveRefusesAPairingThePhoneScanned: a pairing whose device is taken
// must run to its end, as for add; the remove after that works.
func TestRemoveRefusesAPairingThePhoneScanned(t *testing.T) {
	fn := &fakeNet{connected: map[string]bool{personalPhone: true}}
	m, f := removeFixture(t, fn)
	forceStatus(m, "personal", StatusNeedsLink)
	s := pair(t, m, "personal")
	q := fn.qr(t, 0)
	q.ch <- code("2@a", time.Minute)
	eventually(t, "the code", stateIs(s, pairCode))
	newJID := types.NewADJID(personalPhone, 0, 13)
	if !q.cli.PrePairCallback(newJID, "android", "") {
		t.Fatal("PrePairCallback refused")
	}
	before, archive, devices := fn.called(true), archiveOf(t, f), storedDevices(t, m)

	r := remove(t, m, "personal")
	if r.err == nil || !strings.Contains(r.err.Error(), "just linked and is connecting") || !strings.Contains(r.err.Error(), "then remove it") {
		t.Fatalf("Remove = %+v, %v; want the refusal", r.res, r.err)
	}
	if q.ctx.Err() != nil || isDone(s) || !slices.Equal(fn.called(true), before) || !maps.Equal(archiveOf(t, f), archive) ||
		!slices.Equal(storedDevices(t, m), devices) || !hasAccount(m, "personal") {
		t.Error("a refused remove touched the pairing or the account")
	}
	m.mu.Lock()
	removing := m.accounts["personal"].removing
	m.mu.Unlock()
	if removing {
		t.Error("a refused remove left the account marked")
	}

	saveDevice(t, q.cli.Store, newJID)
	q.ch <- whatsmeow.QRChannelSuccess
	eventually(t, "paired", stateIs(s, pairPaired))
	q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	<-s.done
	if r := remove(t, m, "personal"); r.err != nil || r.res.Hint != "" {
		t.Errorf("Remove after the pairing = %+v, %v", r.res, r.err)
	}
	if got := storedDevices(t, m); !slices.Equal(got, []string{adJID(workPhone).String()}) {
		t.Errorf("devices %v, want work's alone", got)
	}
}

// TestRemoveStrayDevice: a pairing that saved its device and then failed leaves
// it in store.db under accounts.jid and on no client of the account; remove takes
// it away too.
func TestRemoveStrayDevice(t *testing.T) {
	fn := &fakeNet{}
	m, f := removeFixture(t, fn)
	s := pair(t, m, "fresh")
	q := fn.qr(t, 0)
	q.ch <- code("2@a", time.Minute)
	eventually(t, "the code", stateIs(s, pairCode))
	jid := types.NewADJID("70000000009", 0, 13)
	if !q.cli.PrePairCallback(jid, "android", "") {
		t.Fatal("PrePairCallback refused")
	}
	saveDevice(t, q.cli.Store, jid)
	q.ch <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("boom")}
	close(q.ch)
	<-s.done
	if got := storedDevices(t, m); !slices.Contains(got, jid.String()) {
		t.Fatalf("devices %v: the failed pairing left none", got)
	}

	r := remove(t, m, "fresh")
	if r.err != nil || r.res.Hint != removeDeviceHint {
		t.Fatalf("Remove = %+v, %v", r.res, r.err)
	}
	if got := storedDevices(t, m); slices.Contains(got, jid.String()) || len(got) != 2 {
		t.Errorf("devices %v, want the stray one gone and the others left", got)
	}
	if _, there := archiveOf(t, f)["fresh"]; there || hasAccount(m, "fresh") {
		t.Error("the account is still there")
	}
}

// TestRemoveWhenTheArchiveHasNoRow: a repeat after a delete that was committed
// and not told, or an account that lost its row some other way, is as good as
// done: the account goes.
func TestRemoveWhenTheArchiveHasNoRow(t *testing.T) {
	m, f := removeFixture(t, &fakeNet{})
	if err := f.db.DeleteAccount(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if r := remove(t, m, "gone"); r.err != nil {
		t.Fatalf("Remove = %+v, %v", r.res, r.err)
	}
	if hasAccount(m, "gone") {
		t.Error("the account is still in the manager")
	}
}

// TestRemoveFreesTheNumber: an account's number belongs to it until it is
// removed; then another nick may take it, and the removed nick starts again,
// empty.
func TestRemoveFreesTheNumber(t *testing.T) {
	fn := &fakeNet{firstCode: "2@one"}
	m, f := removeFixture(t, fn)
	const number = "+7 000 000 00 01"
	if r := link(t, m, "other", number); r.err == nil || !strings.Contains(r.err.Error(), `already linked as account "personal"`) {
		t.Fatalf("a number of another account was taken: %+v, %v", r.tk, r.err)
	}
	if r := remove(t, m, "personal"); r.err != nil {
		t.Fatal(r.err)
	}
	if r := link(t, m, "other", number); r.err != nil || r.tk.PairCode != pairCodeOK {
		t.Fatalf("the freed number was refused: %+v, %v", r.tk, r.err)
	}
	if got := numberOf(m, "other"); got != "" { // taken at the scan
		t.Errorf("number %q", got)
	}
	// The nick is free as well, and its archive is not the old one.
	if tk, err := m.Link(context.Background(), LinkRequest{Nick: "personal"}); err != nil || tk.LoginURL == "" {
		t.Fatalf("Link of the removed nick = %+v, %v", tk, err)
	}
	if got := infoOf(m, "personal"); got.Status != StatusNeedsLink || got.Reason != "not linked yet" {
		t.Errorf("account %+v", got)
	}
	if got := archiveOf(t, f)["personal"]; got != [2]int{} {
		t.Errorf("the new account has the archive %v of the old one", got)
	}
}

// TestRemoveOffline: with no network the connect of the account fails and
// retries, Logout fails at once, and the remove takes the fallback, in a moment
// and for good: the retries end with the deleted device.
func TestRemoveOffline(t *testing.T) {
	errs := slices.Repeat([]error{errHandshake}, 1000)
	fn := &fakeNet{fails: map[string][]error{personalPhone: errs}}
	f := newFixture(t)
	var buf bytes.Buffer
	f.log = debugLog(&buf)
	f.account(t, "personal", personalPhone)
	f.devices(t, map[string]string{personalPhone: ""})
	seedArchive(t, f, "personal")
	m := f.start(t, fn.network(readyGlobals()))

	start := time.Now()
	r := remove(t, m, "personal")
	if r.err != nil || r.res.Hint != removeDeviceHint {
		t.Fatalf("Remove = %+v, %v", r.res, r.err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Remove took %v with no network", d)
	}
	requireEnded(t, m) // the connect loop has ended with ErrDeviceDeleted
	if _, there := archiveOf(t, f)["personal"]; there || len(storedDevices(t, m)) != 0 {
		t.Error("something of the account is left")
	}
	// The end of the retries is nothing to report: the device was taken away on purpose.
	if log := buf.String(); strings.Contains(log, "level=ERROR") || strings.Contains(log, "deleted device") {
		t.Errorf("the retries of a removed account's connect were reported:\n%s", log)
	}
}

// TestNoConnectAttemptBesideTheUnlinking: the retries of a connect that fails start
// no attempt while the account's device is being taken away. whatsmeow's Delete
// writes the ID and the Deleted that Connect reads, and gives them no lock.
func TestNoConnectAttemptBesideTheUnlinking(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enter, once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	fn := &fakeNet{
		fails:      map[string][]error{personalPhone: slices.Repeat([]error{errHandshake}, 100000)},
		logoutHold: func(*whatsmeow.Client) { enter.Do(func() { close(entered) }); <-release },
	}
	f := newFixture(t)
	f.account(t, "personal", personalPhone)
	f.devices(t, map[string]string{personalPhone: ""})
	m := f.start(t, fn.network(readyGlobals()))
	t.Cleanup(letGo) // before Close, which waits for the unlinking
	connects := func() int { return countCalls(fn, "connect "+personalPhone) }
	eventually(t, "the retries", func() bool { return connects() >= 3 })

	res := removeAsync(context.Background(), m, "personal")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the unlinking did not start")
	}
	// The retries wait at most maxReconnectErrors steps of a millisecond: one that
	// was waiting has met the unlinking by now, and one on its way has ended.
	time.Sleep(100 * time.Millisecond)
	before := connects()
	time.Sleep(100 * time.Millisecond)
	if after := connects(); after != before {
		t.Errorf("%d connect attempts while the device was being unlinked", after-before)
	}

	letGo()
	if r := removed(t, res); r.err != nil || r.res.Hint != removeDeviceHint {
		t.Fatalf("Remove = %+v, %v", r.res, r.err)
	}
	requireEnded(t, m)
}

// TestRemoveWhenTheArchiveCannotBeDeleted: the device is gone by then, so the
// account stays as needs_link, "no keys", with its archive and number, and a
// repeated call goes on from there, with the hint the first one had.
func TestRemoveWhenTheArchiveCannotBeDeleted(t *testing.T) {
	fn := &fakeNet{}
	m, f, lift := removeFixtureRefusing(t, fn, "archive.db", "accounts")
	var buf bytes.Buffer
	m.log = debugLog(&buf)

	r := remove(t, m, "personal")
	if r.err == nil || !strings.Contains(r.err.Error(), "could not delete the archive") || !strings.Contains(r.err.Error(), repeatOrTell) {
		t.Fatalf("Remove = %+v, %v; want the error of the delete", r.res, r.err)
	}
	// What the database said is for the log, not for the agent.
	if strings.Contains(r.err.Error(), "blocked by the test") || !strings.Contains(buf.String(), "blocked by the test") {
		t.Errorf("the database's words: in the error %q, in the log:\n%s", r.err, buf.String())
	}
	m.mu.Lock()
	a := m.accounts["personal"]
	cli, removing := a.cli, a.removing
	m.mu.Unlock()
	if got := infoOf(m, "personal"); got.Status != StatusNeedsLink || got.Reason != removalInterruptedReason || got.Phone != "" || cli != nil || removing {
		t.Errorf("account %+v, device %v, removing %v; want needs_link with no keys", got, cli, removing)
	}
	if got := archiveOf(t, f)["personal"]; got != [2]int{1, 2} {
		t.Errorf("the archive %v was touched", got)
	}
	if got := storedDevices(t, m); !slices.Equal(got, []string{adJID(workPhone).String()}) {
		t.Errorf("devices %v: the device of the account should be gone", got)
	}
	if got := numberOf(m, "personal"); got != personalPhone {
		t.Errorf("number %q, want it kept", got)
	}

	lift()
	calls := len(fn.called(true))
	r = remove(t, m, "personal")
	if r.err != nil || r.res.Hint != removeDeviceHint {
		t.Fatalf("the repeated Remove = %+v, %v; want the hint of the first", r.res, r.err)
	}
	if len(fn.called(true)) != calls {
		t.Errorf("the repeated Remove went to WhatsApp again: %v", fn.called(true)[calls:])
	}
	if _, there := archiveOf(t, f)["personal"]; there || hasAccount(m, "personal") {
		t.Error("the account is still there")
	}
}

// TestRemoveWhenTheDeviceCannotBeDeleted: the archive stays, as does the device,
// and its client, which was disconnected, connects again; a repeated call finds
// the account as it was.
func TestRemoveWhenTheDeviceCannotBeDeleted(t *testing.T) {
	fn := &fakeNet{}
	m, f, lift := removeFixtureRefusing(t, fn, "store.db", "whatsmeow_device")
	var buf bytes.Buffer
	m.log = debugLog(&buf)

	r := remove(t, m, "personal")
	if r.err == nil || !strings.Contains(r.err.Error(), "could not delete the keys of the device") || !strings.Contains(r.err.Error(), repeatOrTell) {
		t.Fatalf("Remove = %+v, %v; want the error of the delete", r.res, r.err)
	}
	// What the database said is for the log, not for the agent.
	if strings.Contains(r.err.Error(), "blocked by the test") || !strings.Contains(buf.String(), "blocked by the test") {
		t.Errorf("the database's words: in the error %q, in the log:\n%s", r.err, buf.String())
	}
	m.wg.Wait()
	if got := archiveOf(t, f)["personal"]; got != [2]int{1, 2} {
		t.Errorf("the archive %v was touched, though the device is still there", got)
	}
	if got := storedDevices(t, m); len(got) != 2 {
		t.Errorf("devices %v", got)
	}
	m.mu.Lock()
	cli := m.accounts["personal"].cli
	m.mu.Unlock()
	if got := infoOf(m, "personal"); got.Status != StatusReconnecting || cli == nil || deviceDeleted(cli) {
		t.Errorf("account %+v: want it with its device, reconnecting", got)
	}
	if n := countCalls(fn, "connect "+personalPhone); n != 2 {
		t.Errorf("%d connects, want the first and the reconnect: %v", n, fn.called(true))
	}

	lift()
	if r := remove(t, m, "personal"); r.err != nil || r.res.Hint != removeDeviceHint {
		t.Fatalf("the repeated Remove = %+v, %v", r.res, r.err)
	}
	if got := storedDevices(t, m); !slices.Equal(got, []string{adJID(workPhone).String()}) {
		t.Errorf("devices %v", got)
	}
}

// hungUnlink is an account whose connect hangs, as in a network that takes no
// answers, and with it everything that needs the socket lock the connect holds, a
// Logout among it: until unhang.
func hungUnlink(t *testing.T) (m *Manager, f *fixture, fn *fakeNet, unhang func()) {
	t.Helper()
	f = newFixture(t)
	f.account(t, "personal", personalPhone)
	f.devices(t, map[string]string{personalPhone: ""})
	seedArchive(t, f, "personal")
	entered, release := make(chan struct{}), make(chan struct{})
	var enter, once sync.Once
	unhang = func() { once.Do(func() { close(release) }) }
	fn = &fakeNet{
		hold:       func(*whatsmeow.Client) { enter.Do(func() { close(entered) }); <-release },
		logoutHold: func(*whatsmeow.Client) { <-release },
	}
	m = f.start(t, fn.network(readyGlobals()))
	t.Cleanup(unhang) // before Close, which would wait for the connect
	m.abortWait = 50 * time.Millisecond
	<-entered
	return m, f, fn, unhang
}

// TestRemoveStillUnlinking: a Logout held up by a connect that hangs does not
// hold up the remove: it answers, having erased nothing, the account refuses
// whatever else comes, and the repeat finds the unlinking over, with the hint it
// ended with.
func TestRemoveStillUnlinking(t *testing.T) {
	m, f, fn, unhang := hungUnlink(t)
	devices := storedDevices(t, m)

	start := time.Now()
	r := remove(t, m, "personal")
	if !errors.Is(r.err, errStillRemoving) || r.res != (RemoveResult{}) || time.Since(start) > time.Second {
		t.Fatalf("Remove = %+v, %v after %v; want errStillRemoving, soon", r.res, r.err, time.Since(start))
	}
	if got := archiveOf(t, f)["personal"]; got != [2]int{1, 2} || !hasAccount(m, "personal") || !slices.Equal(storedDevices(t, m), devices) {
		t.Error("an unfinished remove changed the account")
	}
	if unlinkingOf(m, "personal") == nil {
		t.Fatal("the unlinking is not going on")
	}
	// Nothing else is let at the account meanwhile, and the repeat does not begin another unlinking.
	for _, req := range []LinkRequest{{Nick: "personal"}, {Nick: "personal", Phone: "+7 000 000 00 01"}, {Nick: "personal", QRImage: true}} {
		if _, err := m.Link(context.Background(), req); err == nil || !strings.Contains(err.Error(), "is being removed") {
			t.Errorf("Link %+v of an account being removed: %v", req, err)
		}
	}
	if r := remove(t, m, "personal"); !errors.Is(r.err, errStillRemoving) {
		t.Errorf("the repeat while it goes on = %+v, %v", r.res, r.err)
	}
	if n := countCalls(fn, "logout "+personalPhone); n != 1 {
		t.Errorf("%d Logouts: the repeat began another unlinking", n)
	}

	unhang()
	eventually(t, "the unlinking to end", func() bool { return unlinkingOf(m, "personal") == nil })
	if r := remove(t, m, "personal"); r.err != nil || r.res.Hint != removeDeviceHint {
		t.Fatalf("the repeat after it = %+v, %v", r.res, r.err)
	}
	if n := countCalls(fn, "logout "+personalPhone); n != 1 {
		t.Errorf("%d Logouts", n)
	}
	if _, there := archiveOf(t, f)["personal"]; there || hasAccount(m, "personal") || len(storedDevices(t, m)) != 0 {
		t.Error("something of the account is left")
	}
}

// TestRemoveOneAtATime: a second remove of an account that one has taken is
// told to repeat, and does not start anything; the first finishes.
func TestRemoveOneAtATime(t *testing.T) {
	m, f, _, unhang := hungUnlink(t)
	m.abortWait = time.Minute // the first waits
	first := removeAsync(context.Background(), m, "personal")
	eventually(t, "the unlinking", func() bool { return unlinkingOf(m, "personal") != nil })

	if r := remove(t, m, "personal"); !errors.Is(r.err, errStillRemoving) {
		t.Errorf("the second Remove = %+v, %v", r.res, r.err)
	}
	unhang()
	if r := removed(t, first); r.err != nil || r.res.Hint != removeDeviceHint {
		t.Errorf("the first Remove = %+v, %v", r.res, r.err)
	}
	if _, there := archiveOf(t, f)["personal"]; there {
		t.Error("the archive is still there")
	}
}

// TestRemoveCtxAndClose: the call ends with its ctx, or with Close, and leaves
// the account for a repeat.
func TestRemoveCtxAndClose(t *testing.T) {
	t.Run("ctx done before", func(t *testing.T) {
		fn := &fakeNet{}
		m, f := removeFixture(t, fn)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		before := fn.called(true)
		if _, err := m.Remove(ctx, "personal"); !errors.Is(err, context.Canceled) {
			t.Errorf("Remove = %v", err)
		}
		requireEnded(t, m) // what it began, if it began anything, has been done by now
		if !slices.Equal(fn.called(true), before) || archiveOf(t, f)["personal"] != [2]int{1, 2} || !hasAccount(m, "personal") {
			t.Error("a call that was over began something")
		}
	})
	t.Run("ctx done while it waits", func(t *testing.T) {
		m, f, _, unhang := hungUnlink(t)
		m.abortWait = time.Minute
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if r := removed(t, removeAsync(ctx, m, "personal")); !errors.Is(r.err, context.DeadlineExceeded) {
			t.Errorf("Remove = %+v, %v", r.res, r.err)
		}
		if archiveOf(t, f)["personal"] != [2]int{1, 2} || !hasAccount(m, "personal") {
			t.Error("a call that gave up changed the account")
		}
		unhang()
		eventually(t, "the unlinking to end", func() bool { return unlinkingOf(m, "personal") == nil })
		if r := remove(t, m, "personal"); r.err != nil {
			t.Errorf("the repeat = %+v, %v", r.res, r.err)
		}
	})
	t.Run("Close while it waits", func(t *testing.T) {
		m, f, _, _ := hungUnlink(t)
		m.abortWait, m.closeWait = time.Minute, 50*time.Millisecond
		res := removeAsync(context.Background(), m, "personal")
		eventually(t, "the unlinking", func() bool { return unlinkingOf(m, "personal") != nil })
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if r := removed(t, res); !errors.Is(r.err, errClosing) {
			t.Errorf("Remove = %+v, %v; want the closing error", r.res, r.err)
		}
		if archiveOf(t, f)["personal"] != [2]int{1, 2} {
			t.Error("a remove that Close ended deleted the archive")
		}
	})
	t.Run("after Close", func(t *testing.T) {
		m, f := removeFixture(t, &fakeNet{})
		m.Close()
		// An account with a device fails on the goroutine the unlinking would need;
		// one without, which would go straight to its archive, on this check.
		for _, nick := range []string{"personal", "fresh", "gone"} {
			if _, err := m.Remove(context.Background(), nick); !errors.Is(err, errClosing) {
				t.Errorf("Remove(%s) after Close = %v", nick, err)
			}
		}
		if got := archiveOf(t, f); got["personal"] != [2]int{1, 2} || len(got) != 4 {
			t.Errorf("a remove after Close deleted from the archive: %v", got)
		}
	})
}

// TestRemoveRaceWithAdd: a remove of a nick at the same time as an add of each kind,
// and the login page's poll, ends with the account either there, with its row in
// archive.db, or neither; and once the remove after it has run, with none of them and
// nothing of the Manager's still running.
func TestRemoveRaceWithAdd(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	ctx := context.Background()
	for round := range 60 {
		nonce := linkNonce(t, m, "x") // the page has a link, and an account, to poll
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, call := range []func(){
			func() { m.Remove(ctx, "x") },
			func() { m.Link(ctx, LinkRequest{Nick: "x"}) },
			func() { m.Link(ctx, LinkRequest{Nick: "x", QRImage: true}) },
			func() { m.Link(ctx, LinkRequest{Nick: "x", Phone: typedPhone}) },
			func() { m.Login(ctx, "x", nonce) },
			func() { m.Remove(ctx, "x") },
		} {
			wg.Go(func() { <-start; call() })
		}
		close(start)
		wg.Wait()
		_, inDB := archiveOf(t, f)["x"]
		if inMap := hasAccount(m, "x"); inMap != inDB {
			t.Fatalf("round %d: the account is in the manager: %v, in archive.db: %v", round, inMap, inDB)
		}
		if hasAccount(m, "x") {
			if r := remove(t, m, "x"); r.err != nil {
				t.Fatalf("round %d: %v", round, r.err)
			}
		}
		if _, inDB := archiveOf(t, f)["x"]; inDB || hasAccount(m, "x") {
			t.Fatalf("round %d: the account is still there after the remove", round)
		}
		requireEnded(t, m)
	}
}

// TestSignalDoesNotCutARemoveInFlight: the daemon's ctx ends with a signal, but a
// remove that is unlinking the device goes on to the end, the archive's deletion
// too: the Manager ends on Close alone, which the daemon calls once its server has
// waited for the calls.
func TestSignalDoesNotCutARemoveInFlight(t *testing.T) {
	ctx, signal := context.WithCancel(context.Background())
	f := newFixture(t)
	f.account(t, "personal", personalPhone)
	f.devices(t, map[string]string{personalPhone: ""})
	seedArchive(t, f, "personal")
	entered, release := make(chan struct{}), make(chan struct{})
	fn := &fakeNet{connected: map[string]bool{personalPhone: true}, logoutHold: func(*whatsmeow.Client) {
		close(entered)
		<-release
	}}
	m := f.startWith(t, ctx, fn.network(readyGlobals()))
	m.wg.Wait()

	res := removeAsync(context.Background(), m, "personal")
	select {
	case <-entered: // the Logout is under way
	case <-time.After(5 * time.Second):
		t.Fatal("the remove did not reach the Logout")
	}
	signal()
	if m.ctx.Err() != nil {
		t.Error("the signal ended the Manager's context")
	}
	close(release)
	if r := removed(t, res); r.err != nil || r.res.Hint != "" {
		t.Fatalf("Remove = %+v, %v; want it done in spite of the signal", r.res, r.err)
	}
	if _, there := archiveOf(t, f)["personal"]; there || hasAccount(m, "personal") {
		t.Error("the account is still there")
	}
	if got := storedDevices(t, m); len(got) != 0 {
		t.Errorf("devices %v, want none", got)
	}
	if err := m.Close(); err != nil || m.ctx.Err() == nil {
		t.Errorf("Close = %v, context ended: %v", err, m.ctx.Err() != nil)
	}
}
