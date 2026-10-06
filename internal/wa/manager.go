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
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
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

	// Offline makes a Manager that never reaches WhatsApp (see offlineNetwork), for
	// the tests of the packages above that run the daemon in-process.
	Offline bool
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

	// joinedGroups is the account's list of groups, for their names (see groups.go);
	// the one query the Manager makes of WhatsApp for the archive. A message's
	// handler never makes it.
	joinedGroups func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error)

	// deleteDevice is the one write of our own to the client's store.Device, whose
	// ID and Deleted whatsmeow reads and writes without a lock. It is a seam, as the
	// connects that read them are, so that a test's network takes the lock of its
	// own around the write and the reads.
	deleteDevice func(*whatsmeow.Client, context.Context) error

	// What the history worker asks of the client (see history.go): its files call no
	// method of the client but through these. The first three reach WhatsApp.
	// parseWebMessage does not, it only reads the client's own ids, but it is a
	// method too, and a seam keeps the rule simple.
	//
	// downloadHistory has whatsmeow's own signature, the flag included: true makes
	// the call store the LID pairs and the push names of the blob before it returns,
	// where false leaves that to a goroutine of its own (message.go:819-823), and
	// the worker files each chat by what the store knows.
	downloadHistory    func(cli *whatsmeow.Client, ctx context.Context, n *waE2E.HistorySyncNotification, synchronousStorage bool) (*waHistorySync.HistorySync, error)
	deleteHistoryMedia func(cli *whatsmeow.Client, ctx context.Context, n *waE2E.HistorySyncNotification) error
	historyReceipt     func(cli *whatsmeow.Client, ctx context.Context, id types.MessageID) error
	parseWebMessage    func(cli *whatsmeow.Client, chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error)
}

var liveNetwork = network{
	globals:      globals,
	connect:      (*whatsmeow.Client).Connect,
	disconnect:   (*whatsmeow.Client).Disconnect,
	logout:       (*whatsmeow.Client).Logout,
	qrChannel:    (*whatsmeow.Client).GetQRChannel,
	pairPhone:    (*whatsmeow.Client).PairPhone,
	retryStep:    2 * time.Second, // whatsmeow's own (client.go:634)
	deleteDevice: func(cli *whatsmeow.Client, ctx context.Context) error { return cli.Store.Delete(ctx) },
	joinedGroups: (*whatsmeow.Client).GetJoinedGroups,

	downloadHistory: (*whatsmeow.Client).DownloadHistorySync,
	// The blob is ours once it is stored, and whatsmeow's own loop deletes it from the
	// server with these very arguments (message.go:736); with the manual download
	// nothing else does.
	deleteHistoryMedia: func(cli *whatsmeow.Client, ctx context.Context, n *waE2E.HistorySyncNotification) error {
		return cli.DeleteMedia(ctx, whatsmeow.MediaHistory, n.GetDirectPath(), n.GetFileEncSHA256(), n.GetEncHandle())
	},
	// What whatsmeow sends for each announcement unless DisableManualHistorySyncReceipt
	// is set (message.go:899-905), which newClient does.
	historyReceipt: func(cli *whatsmeow.Client, ctx context.Context, id types.MessageID) error {
		return cli.SendProtocolMessageReceipt(ctx, id, types.ReceiptTypeHistorySync)
	},
	parseWebMessage: (*whatsmeow.Client).ParseWebMessage,
}

// Manager owns the WhatsApp client of every account and keeps each one's
// status in memory (see next).
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
	writeWait  time.Duration // how long the handler of a message waits for archive.db
	groupsWait time.Duration // the fetch of an account's groups and the write of their names
	now        func() time.Time

	// The history worker's limits (see history.go).
	historyChunk    int           // messages in a transaction
	historyWait     time.Duration // the download of one notification
	historyRetry    time.Duration // the pause after a failed import, doubled by each failure up to historyRetryMax
	historyRetryMax time.Duration

	// ctx is every client's BackgroundEventCtx: keepalive and whatsmeow's
	// reconnects run on it (client.go:500-502, 583-584). It ends on Close alone,
	// not with the ctx that NewManager got, which a signal cancels: a call in
	// flight, a remove that has taken the device away and must delete the
	// archive, is not to be cut by it. The daemon stops its server first, which
	// waits for the calls, and closes the Manager after.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // the connects and pairings, the group fetches and the handlers of events at work; Add only under mu while !closed

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

	syncingGroups bool // a goroutine is fetching the names of the account's groups (see startGroupSync)

	// What the history worker keeps (see history.go): the worker that is running, and
	// when the next import may start after a failed one. The pause is the account's,
	// not the worker's: a worker that ends with the connection and starts again with
	// it must not find it over.
	history     *historyWorker
	historyNext time.Time
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
	net := liveNetwork
	if cfg.Offline {
		net = offlineNetwork()
	}
	return newManager(ctx, cfg, net)
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
		writeWait:  writeWait,
		groupsWait: groupsWait,
		now:        time.Now,
		accounts:   map[string]*account{},

		historyChunk:    historyChunk,
		historyWait:     historyWait,
		historyRetry:    historyRetry,
		historyRetryMax: historyRetryMax,
	}
	m.ctx, m.cancel = context.WithCancel(context.WithoutCancel(ctx))
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

	// Only the count is logged: no JID, no number.
	var deleted int
	var errs []error
	for _, d := range byJID {
		if err := m.store.DeleteDevice(ctx, d); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	if len(byJID) > 0 {
		// A warning, as the keys are gone for good: most often the orphan of a
		// relink cut short, but also what is left when archive.db has been deleted
		// or restored from a backup, which unlinks no phone.
		attrs := []any{"deleted", deleted, "failed", len(errs)}
		if len(errs) > 0 {
			attrs = append(attrs, "err", errors.Join(errs...))
		}
		m.log.Warn("deleted devices from store.db that no account points to; the phone may still list them", attrs...)
	}
	// What the devices deleted before leave behind: the orphans just now, devices
	// whatsmeow deleted on LoggedOut, a remove cut short by a crash.
	if err := m.store.forget(ctx); err != nil {
		m.log.Warn("clean up store.db", "err", err)
	}
	return nil
}

// newClient is the one place that builds a client, with the settings of a
// permanent one; pairing builds its client here too, since that client stays on
// once linked, and adds its PrePairCallback.
func (m *Manager) newClient(a *account, dev *store.Device) *whatsmeow.Client {
	cli := whatsmeow.NewClient(dev, newWALog(m.log.With("account", a.nick), "Client"))
	cli.BackgroundEventCtx = m.ctx
	cli.InitialAutoReconnect = true // a laptop offline at start
	cli.SynchronousAck = true
	cli.AutomaticMessageRerequestFromPhone = true
	// The archive needs EnableDecryptedEventBuffer. Decrypting a message spends its
	// key: the session ratchet moves on, and the same ciphertext cannot be
	// decrypted again. Without the buffer a message that is decrypted but not yet
	// in the archive is lost if the process dies in between, and so is one whose
	// handler returns false because the archive could not be written, though
	// WhatsApp sends it again, as it is not acknowledged (message.go:459-462). With
	// the buffer whatsmeow keeps the plaintext in store.db, written in the transaction that
	// decrypts, until the handler has returned success (message.go:518-566), and
	// answers a redelivery from it. Once the handler has succeeded the plaintext is
	// cleared and only the hash stays, so that a message sent again is dropped
	// before the handler sees it (message.go:463-487, 408); the acknowledgement,
	// with SynchronousAck, goes out after that (message.go:489-497). The cost is
	// that the plaintext of a message passes through store.db and its WAL; remove
	// deletes it with the device.
	//
	// A limit of the buffer that nothing here can close: an entry is found by the
	// ciphertext and by the address the sender encrypts from (message.go:339-355,
	// 530-533), which is the sender's phone JID until the store knows a LID for it,
	// and the LID after. A message of a sender known by number alone that the
	// archive refused, and is sent again once the pair has become known (a fetch of
	// the groups brings pairs, as does another message), is looked up under the
	// other address and not found, is decrypted a second time, which the spent key
	// cannot do (an old counter), and is acknowledged all the same
	// (message.go:408-419, 489-497). It is lost. TestDecryptedEventBufferMissesWhenTheLIDBecomesKnown
	// shows it, and fails if a later whatsmeow closes it.
	cli.EnableDecryptedEventBuffer = true
	// whatsmeow downloads no history blob and sends no hist_sync receipt
	// (message.go:892-906): the notification is queued by the handler (receive), for
	// the worker to download, and is acknowledged like any message once it is.
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

// handler returns the event handler of the account's client cli. It returns
// true for every event but a message that could not be stored (see receive).
// False is not free: it stops the dispatch to the handlers after it, such as a
// pairing's QR channel (client.go:989-993), and makes whatsmeow withhold the ack
// (message.go:459-462). A QR channel takes no interest in messages (qrchan.go:136-200),
// so what that costs is the one message, which is meant. It is never removed:
// RemoveEventHandler would deadlock from inside a handler (client.go:812-820),
// and the events of a client the account no longer follows are dropped in
// handle.
//
// It runs on whatsmeow's goroutines, one stanza at a time per client, and
// whatsmeow holds its handler lock while it does (client.go:980-990): it makes
// no call to WhatsApp, and holds mu for nothing but the account's own state.
func (m *Manager) handler(a *account, cli *whatsmeow.Client) func(any) bool {
	return func(evt any) bool { return m.handle(a, cli, evt) }
}

func (m *Manager) handle(a *account, cli *whatsmeow.Client, evt any) bool {
	switch e := evt.(type) {
	case *events.Message:
		return m.receive(a, cli, e)
	case *events.JoinedGroup:
		m.groupNamed(a, cli, e.JID, e.Name)
		return true
	case *events.GroupInfo:
		if e.Name != nil {
			m.groupNamed(a, cli, e.JID, e.Name.Name)
		}
		return true
	}
	// Close cancels ctx before Disconnect, so each socket drops as if the
	// server closed it and whatsmeow sends Disconnected (client.go:599-603):
	// a false reconnecting per account.
	if m.ctx.Err() != nil {
		return true
	}
	m.mu.Lock()
	if cli != a.owner() {
		m.mu.Unlock()
		return true
	}
	if e, ok := evt.(*events.PushNameSetting); ok {
		a.info.PushName = e.Action.GetName()
		m.mu.Unlock()
		return true
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
	if _, ok := evt.(*events.Connected); ok {
		m.startGroupSync(a, cli)
	}
	// The history that is waiting, from before the connection was lost or from the
	// last run of the daemon, can be imported again.
	switch evt.(type) {
	case *events.Connected, *events.KeepAliveRestored:
		if cur.Status == StatusConnected {
			m.startHistory(a)
		}
	}
	return true
}

// connect connects cli in the background once the client globals are
// final; the account stays reconnecting until Connected. With
// InitialAutoReconnect whatsmeow retries a network error itself and returns
// nil, but not a handshake timeout or an HTTP answer such as a captive
// portal's (client.go:504-534), which its reconnect loop does retry later
// on (client.go:645-656); so they are retried here, with the same backoff.
// Once Close has begun it does nothing. While the account's devices are being
// unlinked no attempt starts either: whatsmeow's Delete writes the ID and Deleted
// of the device, which Connect reads under a lock Delete does not take, so an
// attempt beside it is a data race. The attempt that has passed the check is the
// one left, and the unlinking's second Disconnect closes it (unlinkClient). The
// loop that stops here is not missed: an unlinking that leaves the device
// connects its client again (runUnlink), and one that takes it needs no connect.
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
			m.mu.Lock()
			unlinking := a.unlinking != nil
			m.mu.Unlock()
			if unlinking {
				return
			}
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

// loginPendingReason is what an account says that waits for the link that add has
// handed out: the pairing starts only when the link's page is opened (see Login),
// so until then the account is needs_link to the Manager, but not to the agent,
// which would link it again and end the link that is waiting.
const loginPendingReason = "a login link was issued and not opened yet"

// Accounts lists the accounts by nick with their status and archive size. An
// account with a login link that works and has not been opened is shown linking.
func (m *Manager) Accounts(ctx context.Context) []AccountInfo {
	sizes, err := m.db.Accounts(ctx)
	if err != nil {
		m.log.Warn("archive size", "err", err) // the statuses are still worth showing
	}
	size := make(map[string]archive.Account, len(sizes))
	for _, s := range sizes {
		size[s.Nick] = s
	}
	stuck := m.stuckHistory(ctx, sizes) // before mu: it reads archive.db
	m.mu.Lock()
	out := make([]AccountInfo, 0, len(m.accounts))
	for _, a := range m.accounts {
		info := a.info
		if info.Status == StatusNeedsLink && m.nonces.pending(a.nick) {
			info = with(info, StatusLinking, loginPendingReason, time.Time{})
			info.LoginPending = true
		}
		info.Chats, info.Messages = size[a.nick].Chats, size[a.nick].Messages
		if s, ok := stuck[a.nick]; ok {
			info = withStuckHistory(info, s)
		}
		out = append(out, info)
	}
	m.mu.Unlock()
	slices.SortFunc(out, func(a, b AccountInfo) int { return strings.Compare(a.Nick, b.Nick) })
	return out
}

// Close disconnects every client and closes store.db; later calls return
// the first result. The daemon calls it after the HTTP server has stopped
// and before archive.db closes. A client still busy after
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
