package wa

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// pairedWait bounds the wait for the first connect after a scan: the server
// then ends the stream with 515 and whatsmeow reconnects once, without a
// retry if that fails (connectionevents.go:26-39).
const pairedWait = 30 * time.Second

// qrSilence is the longest the QR channel may stay silent past what it
// promised: after the connect, for the first code, and past each code's
// timeout, for the next item. A keepalive failure disconnects the client
// without the Disconnected that ends the channel (keepalive.go:46-50,
// client.go:600), and the emitter then stops without a final item
// (qrchan.go:114-116), so nothing else would end the pairing.
const qrSilence = 30 * time.Second

// oldDeviceHint is for a relink whose old device could not be unlinked.
const oldDeviceHint = "the old device of this account may still be listed on the phone: remove it in WhatsApp → Linked devices"

const cancelledReason = "linking was cancelled; call add again"

// differentNumberReason is why a relink with another number is refused. It says
// what removing costs, as the way out of it is a remove, which deletes the archive,
// and the refusal can be false: the number a JID holds is not always the one a
// user types (Mexico, Argentina, Brazil), so the user decides, not the agent.
const differentNumberReason = "this is a different phone number; to use it, remove the account and add it again " +
	"(removing the account deletes its message archive: ask the user first)"

// The timeouts of the QR codes whatsmeow shows: the first one only when the
// server sent six refs, else every code gets the shorter one
// (qrchan.go:92-96).
const (
	qrFirst = 60 * time.Second
	qrNext  = 20 * time.Second
)

// qrWindow is how long a pairing's connection lives from its first code when
// the server sent six refs: the first code is shown for qrFirst, each of the
// other five for qrNext, and then the channel closes and the client
// disconnects (qrchan.go:92-96, 77-83). A pairing code, whose own expiry
// nobody knows (pair-code.go:83-85), lives inside that window; it is a limit,
// not a promise, as the code may stop working earlier. whatsmeow can lengthen
// the window by restarting a code on an ADV secret rotation (qrchan.go:117-124),
// which has not been seen on the code path.
const qrWindow = qrFirst + 5*qrNext

// windowFor is how long the connection of a pairing lives from its first QR
// code, whose timeout is first: qrWindow for the six refs that make it qrFirst,
// and for fewer refs (pair.go:67-79) only the first code's own timeout is known,
// so that is what the window is taken to be.
func windowFor(first time.Duration) time.Duration {
	if first == qrFirst {
		return qrWindow
	}
	return first
}

// pairKind is how a pairing is meant to end, which add must know: only a
// pairing that a login page started is for a login page to restart; one that
// waits for a code typed on the phone, or that has shown its QR code in the
// chat, was started by an add that ended the link of the page. kindNone is not
// a kind of session, it is admit's request to decide and start nothing, so no
// session has it.
type pairKind int

const (
	kindNone  pairKind = iota // admit only: decide, start no pairing
	kindPage                  // a QR page shows the codes; the page's first poll starts it
	kindPhone                 // the user types a pairing code on the phone; add with phone starts it
	kindChat                  // the first QR code is an image in the chat; add with qr_image starts it
)

type pairState string

const (
	pairStarting pairState = "starting" // connecting to WhatsApp, no QR code yet
	pairCode     pairState = "code"     // Code is the QR code to scan
	pairPaired   pairState = "paired"   // scanned; the new device is connecting
	pairDone     pairState = "done"     // the new device is the account's; the account's status says how it runs
	pairFailed   pairState = "failed"   // Reason says why
)

// pairStatus is a pairing as the login page shows it.
type pairStatus struct {
	State   pairState
	Code    string    // the QR code's content; only the latest one scans (qrchan.go:117-124)
	Expires time.Time // when the next code replaces Code
	Reason  string    // why it failed
	Hint    string    // for the user, even on success
}

// pairSession links a new device to an account by QR code (plan 6.3), shown
// on a login page or, with kindChat, as an image in the chat, or, with
// kindPhone, replaced by a pairing code that the session's client asks for once
// the first QR code is out (plan 5.3). It runs on the Manager's ctx, not the
// request's: a page reload or the next add finds it alive.
type pairSession struct {
	kind   pairKind
	cli    *whatsmeow.Client
	ch     <-chan whatsmeow.QRChannelItem
	ctx    context.Context // the QR channel's; ends on stop, Close or the session's end
	cancel context.CancelFunc
	done   chan struct{} // closed when the session's goroutine has ended

	// left is closed, by leave under Manager.mu, once the client's events
	// move the account out of linking: after a scan, Connected tells the
	// new device works.
	left  chan struct{}
	leave func()

	// connecting is closed by readQR once the client globals are final, just
	// before the connect: what add with a phone number or qr_image times from is
	// the connection, not our own start.
	connecting chan struct{}

	// coded is closed by markCoded when the first QR code is out: the
	// connection is up, and PairPhone may ask for a code (pair-code.go:78-81).
	// window is when the QR codes run out, windowFor from the first one: written
	// before coded is closed, read after, and never again.
	coded     chan struct{}
	markCoded func()
	window    time.Time

	mu        sync.Mutex
	st        pairStatus
	refusal   string // why PrePairCallback refused the device
	bound     bool   // PrePairCallback took a device: from then on only Close cancels
	cancelled bool   // by stop, before any device was taken
	why       string // the reason the account is left with when stop gave one
}

func (s *pairSession) status() pairStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *pairSession) update(f func(*pairStatus)) {
	s.mu.Lock()
	f(&s.st)
	s.mu.Unlock()
}

func (s *pairSession) refuse(reason string) {
	s.mu.Lock()
	s.refusal = reason
	s.mu.Unlock()
}

func (s *pairSession) refused() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refusal
}

// bind records that PrePairCallback takes a device, unless stop came first:
// the two exclude each other, so a device once taken is never cancelled.
func (s *pairSession) bind() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return false
	}
	s.bound = true
	return true
}

func (s *pairSession) unbind() {
	s.mu.Lock()
	s.bound = false
	s.mu.Unlock()
}

// stop cancels the session unless it has taken a device, and reports
// whether it did. The session's own goroutine then disconnects the client:
// whatsmeow ends the QR channel on a cancel only once a code is out
// (qrchan.go:125-131), and a Disconnect here would wait out a connect in
// its noise handshake, which holds the socket lock (client.go:527-528).
func (s *pairSession) stop() bool { return s.stopFor("") }

// stopFor is stop for a pairing that has no use any more, with the reason it
// leaves the account with; "" for cancelledReason. The first stop's reason
// stands: a pairing that a newer add has cancelled keeps the account's
// cancelledReason when the call that was waiting on it comes to give up too.
func (s *pairSession) stopFor(reason string) bool {
	s.mu.Lock()
	if s.bound {
		s.mu.Unlock()
		return false
	}
	if !s.cancelled {
		s.cancelled, s.why = true, reason
	}
	s.mu.Unlock()
	s.cancel()
	return true
}

// cancelReason is why a cancelled session ended.
func (s *pairSession) cancelReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.why != "" {
		return s.why
	}
	return cancelledReason
}

// drop discards a session built but never started; nil is fine.
func (s *pairSession) drop() {
	if s != nil {
		s.cancel()
	}
}

// over tells whether s has ended or is ending: it was stopped, Close has come,
// or its end has set the failure, which runPairing does before it cancels the
// session's context.
func (s *pairSession) over() bool {
	return s.ctx.Err() != nil || s.status().State == pairFailed
}

func (s *pairSession) phase() pairingPhase {
	select {
	case <-s.done:
		return noPairing
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bound {
		return pairingBound
	}
	return pairingOpen
}

// newPairing builds a pairing for a: its client, from newClient as it stays
// the account's once linked, and the QR channel, which must precede the
// connect that emits the codes.
func (m *Manager) newPairing(a *account, kind pairKind) (*pairSession, error) {
	ctx, cancel := context.WithCancel(m.ctx)
	s := &pairSession{
		kind: kind, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		left: make(chan struct{}), connecting: make(chan struct{}), coded: make(chan struct{}),
		st: pairStatus{State: pairStarting},
	}
	s.leave = sync.OnceFunc(func() { close(s.left) })
	s.markCoded = sync.OnceFunc(func() { close(s.coded) })
	s.cli = m.newClient(a, m.store.NewDevice())
	s.cli.PrePairCallback = m.prePair(a, s)
	ch, err := m.net.qrChannel(s.cli, ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start linking: %w", err)
	}
	s.ch = ch
	return s, nil
}

// prePair returns the PrePairCallback of a's pairing, which whatsmeow calls
// before it saves the new device (pair.go:204 vs 226): the account is bound
// to its device here, so a crash leaves no device without one (see load).
// A relink must keep the account's number; the archive is keyed by nick. A
// number belongs to one account (Anton's decision of 2026-10-01).
func (m *Manager) prePair(a *account, s *pairSession) func(types.JID, string, string) bool {
	return func(jid types.JID, _, _ string) bool {
		prev, refusal := m.claimPhone(a, jid.User)
		if refusal != "" {
			s.refuse(refusal)
			return false
		}
		if !s.bind() {
			m.releasePhone(a, prev)
			s.refuse(cancelledReason)
			return false
		}
		if err := m.db.SetAccountJID(s.ctx, a.nick, jid.String()); err != nil {
			s.unbind()
			m.releasePhone(a, prev)
			m.log.Error("record the linked device", "account", a.nick, "err", err)
			s.refuse("could not record the linked device; call add again")
			return false
		}
		m.mu.Lock()
		a.info.Phone = "+" + jid.User
		m.mu.Unlock()
		return true
	}
}

// claimPhone checks that a may take the number user and records it as a's,
// in one hold of mu: pairings of two accounts run side by side, and when
// both scan the same phone the second callback must see the first's claim.
// It returns a's previous number for releasePhone, or the reason it
// refuses. The accounts' numbers are those of load, also of
// accounts that have no keys on this computer.
func (m *Manager) claimPhone(a *account, user string) (prev, refusal string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev = a.phone
	if refusal = m.phoneRefusal(a, user); refusal != "" {
		return prev, refusal
	}
	a.phone = user
	return prev, ""
}

// phoneRefusal is why a may not take the number user, or "" if it may: a
// relinked account keeps its number, and a number belongs to one account.
// add with a phone number asks it too, before it starts anything, but only
// claimPhone, from PrePairCallback, records the number. mu must be held.
func (m *Manager) phoneRefusal(a *account, user string) string {
	if a.phone != "" && a.phone != user {
		m.log.Warn("linking refused: a different phone number", "account", a.nick)
		return differentNumberReason
	}
	for nick, other := range m.accounts {
		if other != a && user != "" && other.phone == user {
			m.log.Warn("linking refused: the number is linked to another account", "account", a.nick, "other", nick)
			return fmt.Sprintf("this number is already linked as account %q", nick)
		}
	}
	return ""
}

// releasePhone gives back what claimPhone recorded, when the device is not
// taken after all.
func (m *Manager) releasePhone(a *account, prev string) {
	m.mu.Lock()
	a.phone = prev
	m.mu.Unlock()
}

// pairEnd is how reading the QR channel ended.
type pairEnd struct {
	paired bool
	status Status // with reason, when not paired
	reason string
}

func (m *Manager) runPairing(a *account, s *pairSession) {
	defer m.wg.Done()
	defer close(s.done)
	defer s.cancel()
	end := m.readQR(a, s)
	if end.paired {
		m.finishPaired(a, s)
		return
	}
	// whatsmeow disconnects on most endings, not on a passkey or a cancel
	// before the first code.
	m.net.disconnect(s.cli)
	m.mu.Lock()
	a.pcli = nil // the next pairing claims the account only once done is closed
	a.info = with(a.info, end.status, end.reason, time.Time{})
	m.mu.Unlock()
	s.update(func(p *pairStatus) { *p = pairStatus{State: pairFailed, Reason: end.reason, Hint: p.Hint} })
	m.log.Info("linking failed", "account", a.nick, "status", end.status, "reason", end.reason)
}

// readQR connects the pairing client and follows its QR channel to the end.
// The channel is read without pauses: a code that finds its buffer full
// ends the pairing (qrchan.go:98-107), and the final items are sent
// blocking, under the client's dispatcher lock (client.go:981). Once a
// device is bound only Close cancels the session; the next start then
// follows accounts.jid to the new device.
func (m *Manager) readQR(a *account, s *pairSession) pairEnd {
	fail := func(reason string) pairEnd { return pairEnd{status: StatusNeedsLink, reason: reason} }
	ctx := s.ctx
	if m.net.globals.wait(ctx) != nil {
		return fail(s.cancelReason())
	}
	close(s.connecting)
	if err := m.net.connect(s.cli); err != nil {
		m.log.Warn("linking: connect failed", "account", a.nick, "err", err)
		if ctx.Err() != nil { // given up on while it hung (the noise handshake ignores ctx): the account has that reason
			return fail(s.cancelReason())
		}
		return fail("could not connect to WhatsApp; check the network and call add again")
	}
	noCode, expired := "could not get a QR code from WhatsApp; check the network and call add again", "QR expired, call add again"
	switch s.kind {
	case kindPhone: // nobody sees a QR code on this path
		noCode, expired = couldNotReach, codeExpired
	case kindChat: // the call reports a missing first code as the phone path does, so that its error and the account's reason agree
		noCode = couldNotReach
	}
	silence := time.NewTimer(m.qrSilence)
	defer silence.Stop()
	shown := false
	for {
		var it whatsmeow.QRChannelItem
		var ok bool
		select {
		case <-ctx.Done():
			return fail(s.cancelReason())
		case <-silence.C:
			if s.phase() == pairingBound {
				continue // scanned: PairSuccess or PairError always follows (pair.go:148-159)
			}
			m.log.Warn("linking: the QR channel went silent", "account", a.nick, "after", m.qrSilence)
			if !shown {
				return fail(noCode)
			}
			return fail(expired)
		case it, ok = <-s.ch:
		}
		if !ok { // cancelled or a full buffer: no final item
			if ctx.Err() != nil {
				return fail(s.cancelReason())
			}
			return fail("linking was interrupted; call add again")
		}
		switch it.Event {
		case whatsmeow.QRChannelEventCode:
			now := time.Now()
			if !shown {
				m.log.Info("linking: QR code ready", "account", a.nick) // never the code itself (plan 10)
				s.window = now.Add(windowFor(it.Timeout))               // before markCoded, as the field says
			}
			shown = true
			silence.Reset(it.Timeout + m.qrSilence)
			s.update(func(p *pairStatus) { p.State, p.Code, p.Expires = pairCode, it.Code, now.Add(it.Timeout) })
			s.markCoded()
		case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
			// Not final: the same code can be scanned again (qrchan.go:154-157).
			s.update(func(p *pairStatus) {
				p.Hint = "the phone scanned the code without multi-device support; update WhatsApp on the phone and scan again"
			})
		case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
			// It waits for a WebAuthn answer (pair-passkey.go:79, 211), which
			// a daemon cannot give: the pairing would hang to its timeout.
			return fail("the phone asked to confirm the link with a passkey, which whatsapp-mcp cannot do; call add again")
		case whatsmeow.QRChannelSuccess.Event:
			m.log.Info("linking: code scanned", "account", a.nick)
			return pairEnd{paired: true}
		case whatsmeow.QRChannelTimeout.Event:
			// Also sent when the server drops the socket (qrchan.go:196-197).
			if !shown {
				return fail(noCode)
			}
			return fail(expired)
		case whatsmeow.QRChannelEventError:
			if refusal := s.refused(); refusal != "" {
				return fail(refusal)
			}
			return fail(fmt.Sprintf("linking failed: %v; call add again", it.Error))
		case whatsmeow.QRChannelErrUnexpectedEvent.Event:
			// Connected, ConnectFailure, LoggedOut or TemporaryBan
			// (qrchan.go:198-199); our handler, called first, may have put
			// their reason in the status. Before a scan the client is not
			// logged in, so a ban, whose end would be worth keeping, does
			// not come here.
			m.mu.Lock()
			cur := a.info
			m.mu.Unlock()
			if cur.Status != StatusLinking && cur.Reason != "" {
				return fail("linking failed: " + cur.Reason)
			}
			return fail("WhatsApp answered unexpectedly while linking; call add again")
		case whatsmeow.QRChannelClientOutdated.Event:
			return pairEnd{status: StatusClientOutdated, reason: "WhatsApp rejected this client version; whatsapp-mcp needs an update"}
		default:
			m.log.Warn("linking: unknown QR channel event ignored", "account", a.nick, "event", it.Event)
		}
	}
}

// finishPaired makes the new device the account's once it connects or
// pairedWait passes, then drops the old one: only now, so a pairing that
// fails or brings a different number leaves the account as it was.
func (m *Manager) finishPaired(a *account, s *pairSession) {
	s.update(func(p *pairStatus) { p.State, p.Code, p.Expires = pairPaired, "", time.Time{} })
	timer := time.NewTimer(m.pairedWait)
	defer timer.Stop()
	late := false
	select {
	case <-s.left:
	case <-s.ctx.Done(): // Close: a bound session is not cancelled otherwise
	case <-timer.C:
		late = true
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		// Close disconnects the account's client, the old one, having
		// listed it under mu; the next start connects the new device and
		// deletes the old one as an orphan (see load).
		m.net.disconnect(s.cli)
		s.update(func(p *pairStatus) { p.State = pairDone })
		return
	}
	old := a.cli
	a.cli, a.pcli = s.cli, nil
	if a.info.Status == StatusLinking { // late, or Close has begun and lists the new client
		a.info = with(a.info, StatusReconnecting, "", time.Time{})
	}
	m.mu.Unlock()
	if late {
		m.log.Warn("linked, but the new device has not connected yet; retrying", "account", a.nick, "after", m.pairedWait)
		m.connect(a, s.cli)
	}
	hint := m.dropOld(a, old)
	s.update(func(p *pairStatus) { p.State, p.Hint = pairDone, hint })
	m.log.Info("linked", "account", a.nick)
}

// dropOld unlinks a relinked account's old device and deletes its keys; it
// returns a hint when the phone may still list it. If the process dies first,
// load deletes the device as an orphan, accounts.jid having moved on.
//
// The policy pairs only accounts in needs_link, whose old device is gone
// already: whatsmeow deletes it on LoggedOut (connectionevents.go:40-46).
// The rest is for a device still alive.
func (m *Manager) dropOld(a *account, old *whatsmeow.Client) string {
	if old == nil {
		return ""
	}
	listed, err := m.unlinkClient(a.nick, old)
	if err != nil {
		m.log.Warn("delete the old device; the next start deletes it", "account", a.nick, "err", err)
	}
	if err == nil && !listed {
		return ""
	}
	return oldDeviceHint
}
