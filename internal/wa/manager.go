package wa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// maxReconnectErrors caps the reconnect backoff, which grows by a step per
// failed attempt with no ceiling in whatsmeow (client.go:634): after a night
// offline a laptop would wait minutes once the network is back; capped, it
// tries at least once a minute.
const maxReconnectErrors = 30

// closeWait bounds how long Close waits for the clients to stop. A connect
// in the noise handshake ignores its context for up to 20 s
// (handshake.go:24, 46-50) and holds the socket lock Disconnect needs, while
// a daemon taking over waits only 5 s for the lock (daemon.lockWait).
const closeWait = 3 * time.Second

// noKeysReason is what an account says that has no device at start: store.db has
// no keys for its accounts.jid.
const noKeysReason = "this computer has no keys for the device; link the account again"

// Config is what the daemon hands the Manager.
type Config struct {
	Log       *slog.Logger
	StorePath string // store.db
	Archive   *archive.DB
	BaseURL   string // the daemon's http://127.0.0.1:<port>, for the login pages
}

// network is the part of the Manager that reaches WhatsApp's servers; tests
// replace it.
type network struct {
	globals    *waGlobals
	connect    func(*whatsmeow.Client) error
	disconnect func(*whatsmeow.Client)
	logout     func(*whatsmeow.Client, context.Context) error
	qrChannel  func(*whatsmeow.Client, context.Context) (<-chan whatsmeow.QRChannelItem, error)
	pairPhone  func(cli *whatsmeow.Client, ctx context.Context, phone string, push bool, typ whatsmeow.PairClientType, name string) (string, error)
	retryStep  time.Duration // the backoff added per failed connect
}

var liveNetwork = network{
	globals:    globals,
	connect:    (*whatsmeow.Client).Connect,
	disconnect: (*whatsmeow.Client).Disconnect,
	logout:     (*whatsmeow.Client).Logout,
	qrChannel:  (*whatsmeow.Client).GetQRChannel,
	pairPhone:  (*whatsmeow.Client).PairPhone,
	retryStep:  2 * time.Second, // whatsmeow's own (client.go:634)
}

// Manager owns the WhatsApp client of every account and keeps each one's
// status in memory (plan 6.6).
type Manager struct {
	log        *slog.Logger
	db         *archive.DB
	store      *deviceStore
	baseURL    string
	nonces     *loginNonces
	net        network
	closeWait  time.Duration
	pairedWait time.Duration
	qrSilence  time.Duration
	codeWait   time.Duration // add with a phone number or qr_image: the first QR code, from the connect
	phoneWait  time.Duration // and then WhatsApp's answer to PairPhone
	abortWait  time.Duration // and the wait of any call for what it has told to stop, a pairing or a device being unlinked, to end (see awaitEnd)
	logoutWait time.Duration // the Logout of a device that is taken away: a removed account's, a relinked account's old one

	// ctx is every client's BackgroundEventCtx: keepalive and whatsmeow's
	// reconnects run on it (client.go:500-502, 583-584). It is the daemon's
	// and ends on Close, as /admin/stop does not cancel the daemon's own.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // the connects and pairings; Add only under mu while !closed

	// mu is never held while calling into whatsmeow: dispatchEvent holds the
	// client's handler lock while our handler waits for mu (client.go:980-990).
	mu       sync.Mutex
	accounts map[string]*account
	removed  uint64 // how many accounts dropAccount has dropped
	closed   bool

	closeOnce sync.Once
	closeErr  error
}

// account is under Manager.mu, nick aside. Its status follows the events
// of one client only: the pairing one while a pairing owns it, the
// account's own otherwise. A dropped client's late events, such as its
// LoggedOut, would otherwise move the status of the device now in use.
type account struct {
	nick  string // also in info, which is written whole
	info  AccountInfo
	phone string            // accounts.jid's user part, the number the account is bound to; "" until first linked
	cli   *whatsmeow.Client // nil while the account has no device
	pcli  *whatsmeow.Client // the pairing client while it owns the status, else nil
	sess  *pairSession      // the latest pairing, kept after it ends for the login page

	// What remove sets (see remove.go). add refuses an account that is removing or
	// unlinking, and remove one that is removing; a remove finding the account
	// unlinking waits for that.
	removing   bool    // a remove call has the account
	unlinking  *unlink // the goroutine taking its device away, which may outlive the call that began it
	removeHint string  // what remove tells once the device is gone: kept for a remove that has to be repeated
}

// newAccount is an account with no device.
func newAccount(nick string) *account {
	return &account{nick: nick, info: AccountInfo{Nick: nick, Status: StatusNeedsLink, Reason: "not linked yet"}}
}

// owner is the client whose events move the account's status.
func (a *account) owner() *whatsmeow.Client {
	if a.pcli != nil {
		return a.pcli
	}
	return a.cli
}

// NewManager opens store.db, loads the accounts and connects those with a
// device in the background. The daemon calls it before it serves (see load).
func NewManager(ctx context.Context, cfg Config) (*Manager, error) {
	return newManager(ctx, cfg, liveNetwork)
}

func newManager(ctx context.Context, cfg Config, net network) (*Manager, error) {
	st, err := openStore(ctx, cfg.StorePath, newWALog(cfg.Log, "Database"))
	if err != nil {
		return nil, err
	}
	m := &Manager{
		log:        cfg.Log,
		db:         cfg.Archive,
		store:      st,
		baseURL:    cfg.BaseURL,
		nonces:     newLoginNonces(),
		net:        net,
		closeWait:  closeWait,
		pairedWait: pairedWait,
		qrSilence:  qrSilence,
		codeWait:   codeWait,
		phoneWait:  phoneWait,
		abortWait:  abortWait,
		logoutWait: logoutWait,
		accounts:   map[string]*account{},
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	if err := m.load(ctx); err != nil {
		m.cancel()
		st.Close()
		return nil, err
	}
	net.globals.start(ctx, m.log) // the daemon's ctx, not m.ctx: see start
	for _, a := range m.accounts {
		if a.cli != nil {
			m.connect(a, a.cli)
		}
	}
	return m, nil
}

// load builds the accounts from archive.db and their devices in store.db.
//
// A device no account points to is an orphan and is deleted. Pairing
// records accounts.jid in PrePairCallback, before whatsmeow saves the new
// device (pair.go:204 vs 226), so a fresh pairing never leaves one; orphans
// are the old devices of a relink cut short by a crash before dropOld. The
// sweep runs before the daemon serves, so it never sees a pairing in
// progress. It ends with the clean-up of store.db that a remove makes
// (deviceStore.forget).
func (m *Manager) load(ctx context.Context) error {
	accs, err := m.db.Accounts(ctx)
	if err != nil {
		return err
	}
	devs, err := m.store.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices in store.db: %w", err)
	}
	// Keyed by the AD-JID as GetDevice matches it (container.go:177-188).
	byJID := make(map[string]*store.Device, len(devs))
	for _, d := range devs {
		byJID[d.ID.String()] = d
	}

	for _, acc := range accs {
		a := newAccount(acc.Nick)
		m.accounts[acc.Nick] = a
		if acc.JID == "" {
			continue
		}
		jid, err := types.ParseJID(acc.JID)
		if err != nil {
			m.log.Warn("account has an invalid jid", "account", acc.Nick, "err", err)
			a.info.Reason = "the account's device id is invalid; link the account again"
			continue
		}
		a.phone = jid.User
		dev := byJID[jid.String()]
		if dev == nil {
			a.info.Reason = noKeysReason
			continue
		}
		delete(byJID, jid.String())
		// Read before the client starts: whatsmeow writes PushName on its own
		// goroutines (appstate.go:368), so later it comes from the events.
		a.info.Status, a.info.Reason = StatusReconnecting, ""
		a.info.Phone, a.info.PushName = "+"+jid.User, dev.PushName
		a.cli = m.newClient(a, dev)
	}

	for jid, d := range byJID {
		m.log.Info("deleting an orphan device from store.db", "jid", jid)
		if err := m.store.DeleteDevice(ctx, d); err != nil {
			m.log.Warn("delete orphan device", "jid", jid, "err", err)
		}
	}
	// What the devices deleted before leave behind: the orphans just now, devices
	// whatsmeow deleted on LoggedOut, a remove cut short by a crash.
	if err := m.store.forget(ctx); err != nil {
		m.log.Warn("clean up store.db", "err", err)
	}
	return nil
}

// newClient is the one place that builds a client, with the settings of
// plan 6.2; pairing builds its client here too, since that client stays on
// once linked, and adds its PrePairCallback.
func (m *Manager) newClient(a *account, dev *store.Device) *whatsmeow.Client {
	cli := whatsmeow.NewClient(dev, newWALog(m.log.With("account", a.nick), "Client"))
	cli.BackgroundEventCtx = m.ctx
	cli.InitialAutoReconnect = true // a laptop offline at start
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	cli.AutomaticMessageRerequestFromPhone = true
	// History is not stored until M2 (Anton's decision): no history blob is
	// downloaded and no hist_sync receipt is sent (message.go:892-906); the
	// notification itself is still acked like any message (message.go:495).
	cli.ManualHistorySyncDownload = true
	cli.DisableManualHistorySyncReceipt = true
	// Called on autoReconnect's goroutine after a failed attempt
	// (client.go:654), between its own reads and writes of the counter. A
	// connect that succeeds and drops before login does not call it, so a
	// run of those may pass the cap until the next failure.
	cli.AutoReconnectHook = func(err error) bool {
		cli.AutoReconnectErrors = min(cli.AutoReconnectErrors, maxReconnectErrors)
		return !errors.Is(err, store.ErrDeviceDeleted) // fails for good (client.go:539-541)
	}
	cli.AddEventHandlerWithSuccessStatus(m.handler(a, cli))
	return cli
}

// handler returns the event handler of the account's client cli. It
// returns true for every event: false stops the dispatch to the handlers
// after it, such as a pairing's QR channel (client.go:989-993), and makes
// whatsmeow withhold the ack (message.go:459-462), which M1, storing no
// messages, has no use for. It is never removed: RemoveEventHandler would
// deadlock from inside a handler (client.go:812-820), and the events of a
// client the account no longer follows are dropped in handle.
func (m *Manager) handler(a *account, cli *whatsmeow.Client) func(any) bool {
	return func(evt any) bool {
		m.handle(a, cli, evt)
		return true
	}
}

func (m *Manager) handle(a *account, cli *whatsmeow.Client, evt any) {
	// Close cancels ctx before Disconnect, so each socket drops as if the
	// server closed it and whatsmeow sends Disconnected (client.go:599-603):
	// a false reconnecting per account.
	if m.ctx.Err() != nil {
		return
	}
	if e, ok := evt.(*events.Message); ok {
		if n := e.Message.GetProtocolMessage().GetHistorySyncNotification(); n != nil {
			m.log.Debug("history sync ignored until M2", "account", a.nick, "type", n.GetSyncType().String(), "chunk", n.GetChunkOrder())
		}
		return
	}
	m.mu.Lock()
	if cli != a.owner() {
		m.mu.Unlock()
		return
	}
	if e, ok := evt.(*events.PushNameSetting); ok {
		a.info.PushName = e.Action.GetName()
		m.mu.Unlock()
		return
	}
	old := a.info
	a.info = next(a.info, evt, time.Now())
	cur := a.info
	if cli == a.pcli && old.Status == StatusLinking && cur.Status != StatusLinking {
		a.sess.leave()
	}
	m.mu.Unlock()
	if cur.Status != old.Status || cur.Reason != old.Reason {
		m.log.Info("account status", "account", a.nick, "status", cur.Status, "reason", cur.Reason)
	}
}

// connect connects cli in the background once the client globals are
// final; the account stays reconnecting until Connected (plan 6.6). With
// InitialAutoReconnect whatsmeow retries a network error itself and returns
// nil, but not a handshake timeout or an HTTP answer such as a captive
// portal's (client.go:504-534), which its reconnect loop does retry later
// on (client.go:645-656); so they are retried here, with the same backoff.
// Once Close has begun it does nothing.
func (m *Manager) connect(a *account, cli *whatsmeow.Client) {
	m.mu.Lock()
	ok := m.track()
	m.mu.Unlock()
	if !ok {
		return
	}
	go func() {
		defer m.wg.Done()
		if m.net.globals.wait(m.ctx) != nil {
			return // closing
		}
		for n := 1; ; n = min(n+1, maxReconnectErrors) {
			err := m.net.connect(cli)
			if err == nil || m.ctx.Err() != nil || errors.Is(err, whatsmeow.ErrAlreadyConnected) {
				return
			}
			if errors.Is(err, store.ErrDeviceDeleted) {
				// A device that remove or a relink took away is no news; one that
				// the account still has, deleted by whatsmeow, is.
				m.mu.Lock()
				ours := a.cli == cli && a.unlinking == nil
				m.mu.Unlock()
				if ours {
					m.log.Error("connect", "account", a.nick, "err", err)
				}
				return
			}
			delay := time.Duration(n) * m.net.retryStep
			m.log.Warn("connect failed, retrying", "account", a.nick, "in", delay, "err", err)
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()
}

// track counts a goroutine Close must wait for, unless Close has begun;
// under mu, which orders every Add before Close's Wait.
func (m *Manager) track() bool {
	if m.closed {
		return false
	}
	m.wg.Add(1)
	return true
}

// Accounts lists the accounts by nick with their status and archive size.
func (m *Manager) Accounts(ctx context.Context) []AccountInfo {
	sizes, err := m.db.Accounts(ctx)
	if err != nil {
		m.log.Warn("archive size", "err", err) // the statuses are still worth showing
	}
	size := make(map[string]archive.Account, len(sizes))
	for _, s := range sizes {
		size[s.Nick] = s
	}
	m.mu.Lock()
	out := make([]AccountInfo, 0, len(m.accounts))
	for _, a := range m.accounts {
		info := a.info
		info.Chats, info.Messages = size[a.nick].Chats, size[a.nick].Messages
		out = append(out, info)
	}
	m.mu.Unlock()
	slices.SortFunc(out, func(a, b AccountInfo) int { return strings.Compare(a.Nick, b.Nick) })
	return out
}

// Close disconnects every client and closes store.db; later calls return
// the first result. The daemon calls it after the HTTP server has stopped
// and before archive.db closes (plan 4.4). A client still busy after
// closeWait is left behind: the process is exiting.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		// Cancel first: it ends the connects still waiting or retrying,
		// whatsmeow's reconnect loops (client.go:637-643) and the pairings,
		// so no connection comes up after its Disconnect. A pairing
		// disconnects its own client as it ends (runPairing, finishPaired).
		m.cancel()
		m.mu.Lock()
		m.closed = true
		var clients []*whatsmeow.Client
		for _, a := range m.accounts {
			if a.cli != nil {
				clients = append(clients, a.cli)
			}
		}
		m.mu.Unlock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.wg.Wait()
			var wg sync.WaitGroup
			for _, cli := range clients {
				wg.Go(func() { m.net.disconnect(cli) })
			}
			wg.Wait()
		}()
		select {
		case <-done:
		case <-time.After(m.closeWait):
			m.log.Warn("clients still stopping, closing store.db anyway", "after", m.closeWait)
		}
		m.closeErr = m.store.Close()
	})
	return m.closeErr
}
