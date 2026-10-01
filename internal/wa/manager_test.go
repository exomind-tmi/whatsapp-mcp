package wa

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

var errHandshake = errors.New("noise handshake failed: timed out waiting for handshake response")

// fakeNet stands in for WhatsApp's servers.
type fakeNet struct {
	fails     map[string][]error          // phone: what its Connects return before one succeeds
	hold      func(cli *whatsmeow.Client) // runs inside Connect, e.g. to block it
	connected map[string]bool             // phone: whether its socket is up, for Logout
	logoutErr error                       // what Logout of a connected client returns; on nil it deletes the device, as whatsmeow does

	firstCode string                      // when set, each QR channel starts with this code, as WhatsApp's first once connected
	pairCode  string                      // what PairPhone returns; "" for pairCodeOK
	pairErr   error                       // what it fails with instead
	pairHold  func(context.Context) error // runs inside PairPhone, e.g. to wait for its ctx; its error is the result

	mu     sync.Mutex
	calls  []string   // "connect <phone>" once Connect returns, "disconnect <phone>", "logout <phone>", "qr <phone>", "pairphone <phone>"
	qrs    []fakeQR   // one per GetQRChannel
	phones []pairCall // one per PairPhone
}

// pairCodeOK is what the fake PairPhone returns, in the format of the real
// one (pair-code.go:142).
const pairCodeOK = "7K2M-QX9P"

// pairCall is a PairPhone as the Manager made it.
type pairCall struct {
	cli   *whatsmeow.Client
	phone string
	push  bool
	typ   whatsmeow.PairClientType
	name  string
}

// fakeQR is a QR channel the test feeds.
type fakeQR struct {
	cli *whatsmeow.Client
	ctx context.Context
	ch  chan whatsmeow.QRChannelItem
}

// phoneOf names a client in the calls: its phone, or "pairing" before it
// has a device ID.
func phoneOf(cli *whatsmeow.Client) string {
	if id := cli.Store.ID; id != nil {
		return id.User
	}
	return "pairing"
}

func (f *fakeNet) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeNet) network(g *waGlobals) network {
	return network{
		globals:   g,
		retryStep: time.Millisecond,
		connect: func(cli *whatsmeow.Client) error {
			if f.hold != nil {
				f.hold(cli)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			phone := phoneOf(cli)
			f.calls = append(f.calls, "connect "+phone)
			if errs := f.fails[phone]; len(errs) > 0 {
				f.fails[phone] = errs[1:]
				return errs[0]
			}
			return nil
		},
		disconnect: func(cli *whatsmeow.Client) {
			call := "disconnect " + phoneOf(cli)
			if cli.BackgroundEventCtx.Err() == nil {
				call += " before cancel"
			}
			f.record(call)
		},
		logout: func(cli *whatsmeow.Client, ctx context.Context) error {
			phone := phoneOf(cli)
			f.record("logout " + phone)
			f.mu.Lock()
			connected := f.connected[phone]
			f.mu.Unlock()
			switch {
			case cli.Store.ID == nil:
				return whatsmeow.ErrNotLoggedIn
			case !connected:
				return whatsmeow.ErrNotConnected
			case f.logoutErr != nil:
				return f.logoutErr
			}
			return cli.Store.Delete(ctx)
		},
		qrChannel: func(cli *whatsmeow.Client, ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			ch := make(chan whatsmeow.QRChannelItem, 8)
			if f.firstCode != "" {
				ch <- code(f.firstCode, 60*time.Second)
			}
			f.calls = append(f.calls, "qr "+phoneOf(cli))
			f.qrs = append(f.qrs, fakeQR{cli: cli, ctx: ctx, ch: ch})
			return ch, nil
		},
		pairPhone: func(cli *whatsmeow.Client, ctx context.Context, phone string, push bool, typ whatsmeow.PairClientType, name string) (string, error) {
			f.mu.Lock()
			f.calls = append(f.calls, "pairphone "+phoneOf(cli))
			f.phones = append(f.phones, pairCall{cli, phone, push, typ, name})
			hold, err, code := f.pairHold, f.pairErr, f.pairCode
			f.mu.Unlock()
			if hold != nil {
				err = hold(ctx)
			}
			if err != nil {
				return "", err
			}
			if code == "" {
				code = pairCodeOK
			}
			return code, nil
		},
	}
}

// qr returns the channel of the i-th pairing.
func (f *fakeNet) qr(t *testing.T, i int) fakeQR {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.qrs) {
		t.Fatalf("pairing %d has no QR channel; %d so far", i, len(f.qrs))
	}
	return f.qrs[i]
}

// called returns the calls so far, sorted unless in order.
func (f *fakeNet) called(inOrder bool) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inOrder {
		return slices.Clone(f.calls)
	}
	return slices.Sorted(slices.Values(f.calls))
}

// readyGlobals are client globals already final, without touching
// whatsmeow's; heldGlobals never become final.
func readyGlobals() *waGlobals {
	g := heldGlobals()
	close(g.ready)
	return g
}

func heldGlobals() *waGlobals {
	g := newGlobals(nil)
	g.once.Do(func() {})
	return g
}

// fixture is a state directory with archive.db open.
type fixture struct {
	dir string
	db  *archive.DB
	log *slog.Logger // the Manager's
}

func newFixture(t *testing.T) *fixture {
	dir := testutil.TempDir(t)
	db, err := archive.Open(filepath.Join(dir, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &fixture{dir: dir, db: db, log: quiet}
}

func (f *fixture) storePath() string { return filepath.Join(f.dir, "store.db") }

func adJID(phone string) types.JID { return types.NewADJID(phone, 0, 12) }

// account adds an account with jid as accounts.jid: "" for none, or a
// phone for the AD-JID of its device.
func (f *fixture) account(t *testing.T, nick, jid string) {
	t.Helper()
	ctx := context.Background()
	if err := f.db.AddAccount(ctx, nick); err != nil {
		t.Fatal(err)
	}
	if jid != "" && !strings.Contains(jid, "@") {
		jid = adJID(jid).String()
	}
	if jid != "" {
		if err := f.db.SetAccountJID(ctx, nick, jid); err != nil {
			t.Fatal(err)
		}
	}
}

// devices saves a device with the given push name for each phone in
// store.db, as a pairing does.
func (f *fixture) devices(t *testing.T, pushNames map[string]string) {
	t.Helper()
	ctx := context.Background()
	c, err := openStore(ctx, f.storePath(), newWALog(quiet, "Database"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for phone, name := range pushNames {
		d := c.NewDevice()
		d.PushName = name
		saveDevice(t, d, adJID(phone))
	}
}

// saveDevice gives d the ID jid and saves it, as a pairing does.
func saveDevice(t *testing.T, d *store.Device, jid types.JID) {
	t.Helper()
	d.ID = &jid
	d.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{1},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := d.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) start(t *testing.T, net network) *Manager {
	t.Helper()
	m, err := newManager(context.Background(), Config{Log: f.log, StorePath: f.storePath(), Archive: f.db, BaseURL: "http://127.0.0.1:1"}, net)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// TestManagerLoad covers the statuses at start, the orphan sweep, the
// client settings and the retries of a connect whatsmeow does not retry.
func TestManagerLoad(t *testing.T) {
	f := newFixture(t)
	f.account(t, "bad", "7.0.1@s.whatsapp.net") // ParseJID fails on two dots
	f.account(t, "fresh", "")
	f.account(t, "gone", "70000000002")
	f.account(t, "old", "70000000004")
	f.account(t, "personal", "70000000001")
	f.account(t, "work", "70000000003")
	f.devices(t, map[string]string{"70000000001": "Anton", "70000000003": "", "70000000004": "", "70000000009": "orphan"})
	fn := &fakeNet{fails: map[string][]error{
		"70000000003": {errHandshake, errHandshake},
		"70000000004": {store.ErrDeviceDeleted, errHandshake},
	}}
	m := f.start(t, fn.network(readyGlobals()))
	m.wg.Wait() // the connects are done

	want := []AccountInfo{
		{Nick: "bad", Status: StatusNeedsLink, Reason: "the account's device id is invalid; link the account again"},
		{Nick: "fresh", Status: StatusNeedsLink, Reason: "not linked yet"},
		{Nick: "gone", Status: StatusNeedsLink, Reason: "this computer has no keys for the device; link the account again"},
		{Nick: "old", Status: StatusReconnecting, Phone: "+70000000004"},
		{Nick: "personal", Status: StatusReconnecting, Phone: "+70000000001", PushName: "Anton"},
		{Nick: "work", Status: StatusReconnecting, Phone: "+70000000003"},
	}
	if got := m.Accounts(context.Background()); !slices.Equal(got, want) {
		t.Errorf("Accounts =\n%+v, want\n%+v", got, want)
	}
	// work retried until it connected; old gave up on its deleted device.
	wantCalls := []string{"connect 70000000001", "connect 70000000003", "connect 70000000003", "connect 70000000003", "connect 70000000004"}
	if got := fn.called(false); !slices.Equal(got, wantCalls) {
		t.Errorf("calls %v, want %v", got, wantCalls)
	}

	devs, err := m.store.GetAllDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, d := range devs {
		left = append(left, d.ID.User)
	}
	if slices.Sort(left); !slices.Equal(left, []string{"70000000001", "70000000003", "70000000004"}) {
		t.Errorf("devices after the sweep: %v, want the orphan gone", left)
	}

	cli := m.accounts["personal"].cli
	if cli.BackgroundEventCtx != m.ctx || !cli.InitialAutoReconnect || !cli.SynchronousAck ||
		!cli.EnableDecryptedEventBuffer || !cli.AutomaticMessageRerequestFromPhone ||
		!cli.ManualHistorySyncDownload || !cli.DisableManualHistorySyncReceipt || cli.PrePairCallback != nil {
		t.Errorf("client settings differ from plan 6.2: %+v", cli)
	}
}

// TestManagerEvents sends events through the client, so through the
// handler it registered.
func TestManagerEvents(t *testing.T) {
	f := newFixture(t)
	f.account(t, "personal", "70000000001")
	f.devices(t, map[string]string{"70000000001": "Anton"})
	var buf bytes.Buffer
	f.log = debugLog(&buf)
	m := f.start(t, (&fakeNet{}).network(readyGlobals()))
	m.wg.Wait()
	dispatch := m.accounts["personal"].cli.DangerousInternals().DispatchEvent

	for _, tc := range []struct {
		evt  any
		want AccountInfo
	}{
		{&events.Connected{}, AccountInfo{Status: StatusConnected, PushName: "Anton"}},
		{&events.PushNameSetting{Action: &waSyncAction.PushNameSetting{Name: proto.String("Антон")}},
			AccountInfo{Status: StatusConnected, PushName: "Антон"}},
		{&events.Disconnected{}, AccountInfo{Status: StatusReconnecting, PushName: "Антон"}},
		{&events.LoggedOut{Reason: events.ConnectFailureLoggedOut},
			AccountInfo{Status: StatusNeedsLink, Reason: "device unlinked: 401: logged out from another device", PushName: "Антон"}},
	} {
		if dispatch(tc.evt) {
			t.Fatalf("a handler failed %T", tc.evt)
		}
		tc.want.Nick, tc.want.Phone = "personal", "+70000000001"
		if got := m.Accounts(context.Background()); len(got) != 1 || got[0] != tc.want {
			t.Errorf("after %T: %+v, want %+v", tc.evt, got, tc.want)
		}
	}

	// History is neither stored nor shown in the log until M2.
	history := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		HistorySyncNotification: &waE2E.HistorySyncNotification{
			SyncType:                          waE2E.HistorySyncType_INITIAL_BOOTSTRAP.Enum(),
			InitialHistBootstrapInlinePayload: []byte("SECRET"),
		},
	}}}
	if dispatch(history) {
		t.Error("a handler failed a history notification")
	}
	if out := buf.String(); !strings.Contains(out, "history sync ignored") || strings.Contains(out, "SECRET") {
		t.Errorf("want the history notification logged without its content:\n%s", out)
	}

	// Close drops the sockets, which whatsmeow reports as events; they no
	// longer move the status.
	m.Close()
	dispatch(&events.StreamReplaced{})
	if got := m.Accounts(context.Background()); got[0].Status != StatusNeedsLink {
		t.Errorf("an event after Close moved the status to %s", got[0].Status)
	}
}

func TestManagerAccountsArchiveSize(t *testing.T) {
	f := newFixture(t)
	f.account(t, "personal", "")
	f.account(t, "work", "")
	w, err := sqlitedb.Open(filepath.Join(f.dir, "archive.db"), sqlitedb.Pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec(`
INSERT INTO chats(account, jid) VALUES ('work', 'c1@s.whatsapp.net'), ('work', 'c2@g.us');
INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text) VALUES
  ('work', 'c1@s.whatsapp.net', 'm1', 'c1@s.whatsapp.net', 0, 1, 'a'),
  ('work', 'c1@s.whatsapp.net', 'm2', 'c1@s.whatsapp.net', 0, 2, 'b'),
  ('work', 'c2@g.us', 'm3', 'c1@s.whatsapp.net', 0, 3, 'c')`); err != nil {
		t.Fatal(err)
	}
	m := f.start(t, (&fakeNet{}).network(readyGlobals()))

	got := m.Accounts(context.Background())
	if len(got) != 2 || got[0].Chats != 0 || got[0].Messages != 0 || got[1].Chats != 2 || got[1].Messages != 3 {
		t.Errorf("Accounts = %+v, want work with 2 chats and 3 messages", got)
	}
}

// TestManagerClose stops a connect in progress: Close cancels first, waits
// for the connect to return and only then disconnects.
func TestManagerClose(t *testing.T) {
	f := newFixture(t)
	f.account(t, "fresh", "")
	f.account(t, "personal", "70000000001")
	f.account(t, "work", "70000000003")
	f.devices(t, map[string]string{"70000000001": "", "70000000003": ""})
	entered := make(chan struct{}, 2)
	fn := &fakeNet{hold: func(cli *whatsmeow.Client) {
		entered <- struct{}{}
		<-cli.BackgroundEventCtx.Done()
		// Returns late, so a Close that does not wait disconnects first.
		time.Sleep(20 * time.Millisecond)
	}}
	m := f.start(t, fn.network(readyGlobals()))
	<-entered
	<-entered

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, phone := range []string{"70000000001", "70000000003"} {
		calls := fn.called(true)
		c, d := slices.Index(calls, "connect "+phone), slices.Index(calls, "disconnect "+phone)
		if c < 0 || d < c {
			t.Errorf("calls %v: want %s disconnected after its connect returned, on a cancelled context", calls, phone)
		}
	}
	if _, err := m.store.GetAllDevices(context.Background()); err == nil {
		t.Error("store.db still open")
	}
	n := len(fn.called(false))
	if err := m.Close(); err != nil || len(fn.called(false)) != n {
		t.Errorf("second Close = %v, calls %v; want a no-op", err, fn.called(true))
	}
}

// TestManagerCloseHung stops a connect stuck in the noise handshake, which
// ignores its context: Close gives up on it and still closes store.db.
func TestManagerCloseHung(t *testing.T) {
	f := newFixture(t)
	f.account(t, "personal", "70000000001")
	f.devices(t, map[string]string{"70000000001": ""})
	entered, hang := make(chan struct{}), make(chan struct{})
	fn := &fakeNet{hold: func(*whatsmeow.Client) {
		close(entered)
		<-hang
	}}
	m := f.start(t, fn.network(readyGlobals()))
	t.Cleanup(func() { close(hang) })
	<-entered
	m.closeWait = 50 * time.Millisecond

	start := time.Now()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Close took %v", d)
	}
	if _, err := m.store.GetAllDevices(context.Background()); err == nil {
		t.Error("store.db still open")
	}
}

// TestManagerCloseBeforeGlobals stops a daemon still waiting for the
// WhatsApp Web version: no client connects, and Close does not hang.
func TestManagerCloseBeforeGlobals(t *testing.T) {
	f := newFixture(t)
	f.account(t, "personal", "70000000001")
	f.devices(t, map[string]string{"70000000001": ""})
	fn := &fakeNet{}
	m := f.start(t, fn.network(heldGlobals()))
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fn.called(true); !slices.Equal(got, []string{"disconnect 70000000001"}) {
		t.Errorf("calls %v, want no connect before the globals were final", got)
	}
}

func TestNewManagerFails(t *testing.T) {
	f := newFixture(t)
	f.db.Close() // load cannot list the accounts
	_, err := newManager(context.Background(), Config{Log: quiet, StorePath: f.storePath(), Archive: f.db}, (&fakeNet{}).network(readyGlobals()))
	if err == nil {
		t.Fatal("newManager succeeded without archive.db")
	}
	// Only on Windows does TempDir's cleanup then fail if store.db was left
	// open.
}

func TestReconnectBackoffCapped(t *testing.T) {
	f := newFixture(t)
	f.account(t, "personal", "70000000001")
	f.devices(t, map[string]string{"70000000001": ""})
	m := f.start(t, (&fakeNet{}).network(readyGlobals()))
	cli := m.accounts["personal"].cli
	for _, tc := range []struct{ errs, want int }{{5, 5}, {maxReconnectErrors, maxReconnectErrors}, {500, maxReconnectErrors}} {
		cli.AutoReconnectErrors = tc.errs
		if !cli.AutoReconnectHook(errors.New("dial failed")) || cli.AutoReconnectErrors != tc.want {
			t.Errorf("after %d errors: %d, want %d and a retry", tc.errs, cli.AutoReconnectErrors, tc.want)
		}
	}
	if cli.AutoReconnectHook(store.ErrDeviceDeleted) {
		t.Error("whatsmeow would keep reconnecting a deleted device")
	}
}
