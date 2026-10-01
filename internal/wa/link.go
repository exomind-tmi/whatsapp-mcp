package wa

import (
	"context"
	"errors"
	"fmt"
)

// LoginPath is where the login pages live; the daemon serves what follows it,
// "<nick>" and "<nick>/state".
const LoginPath = "/login/"

// ErrNoLogin is what Login says for a nonce that does not open any login
// page: wrong, expired, replaced by a newer add, or of an unknown nick. The
// reasons are not told apart, so the answer is no oracle.
var ErrNoLogin = errors.New("no such login page")

// ErrQRImageWithPhone is for an add that asks for both ways to link that exclude
// each other, the QR code in the chat and the pairing code for a number.
var ErrQRImageWithPhone = errors.New("qr_image and phone are mutually exclusive")

// Link is the add tool (plan 5.3). It applies the add policy (plan 6.4): an
// account that is linked is refused, one whose device is intact reconnects
// with its keys (and ignores the phone number and qr_image), and the others get
// a login link or, with a phone number, a pairing code or, with qr_image, the
// QR code as a PNG. The link's pairing starts when its page asks for the state
// (see Login), not here; the code's and the image's start here, as they come
// from it. A new add, whatever it hands out, ends the link of the one before.
func (m *Manager) Link(ctx context.Context, req LinkRequest) (LinkTicket, error) {
	nick := req.Nick
	if !ValidNick(nick) {
		return LinkTicket{}, fmt.Errorf("invalid account nickname %q", nick)
	}
	kind, digits := kindNone, ""
	switch {
	case req.QRImage && req.Phone != "": // before anything starts
		return LinkTicket{}, ErrQRImageWithPhone
	case req.QRImage:
		kind = kindChat
	case req.Phone != "":
		if !ValidPhone(req.Phone) { // not echoed: it is the user's number
			return LinkTicket{}, errors.New("invalid phone number: use the international format, for example +7 999 000 00 00")
		}
		kind, digits = kindPhone, phoneDigits(req.Phone)
	}
	outcome, s, err := m.admit(ctx, nick, kind, digits)
	if err != nil {
		return LinkTicket{}, err
	}
	switch {
	case outcome == outcomeReconnect:
		m.nonces.revoke(nick)
		return LinkTicket{Reconnecting: true}, nil
	case s != nil: // a pairing by code or in the chat has no page, so the link of the add before ends with its own pairing
		m.nonces.revoke(nick)
		if kind == kindChat {
			return m.issueQR(ctx, nick, s)
		}
		return m.issueCode(ctx, nick, s, digits)
	}
	n := m.nonces.issue(nick)
	m.log.Info("login link issued", "account", nick) // never the link itself (plan 10)
	return LinkTicket{LoginURL: m.baseURL + LoginPath + nick + "?t=" + n.value, ExpiresAt: n.expires}, nil
}

// LoginState is a pairing as the login page shows it.
type LoginState struct {
	// State is "starting" (connecting to WhatsApp), "code" (Code is the QR
	// code to scan), "paired" (scanned; the new device is connecting), "done"
	// (the account is linked) or "failed" (Reason says why).
	State  string
	Code   string // the QR code's content, with State "code"
	Reason string
	Hint   string // for the user, even on success
}

func loginState(p pairStatus) LoginState {
	return LoginState{State: string(p.State), Code: p.Code, Reason: p.Reason, Hint: p.Hint}
}

const reconnectingHint = "the account has its device and is reconnecting with the existing keys: there is no QR code to scan"

// ValidLogin tells whether nonce opens nick's login page, and starts
// nothing: a page that is only fetched, as a link preview or a prefetch does,
// must not use up the link.
func (m *Manager) ValidLogin(nick, nonce string) bool {
	return m.nonces.find(nick, nonce) != nil
}

// Login is what each poll of a login page calls; the page itself only checks
// the nonce (ValidLogin). The nonce is checked here too, ErrNoLogin when it is
// not valid. The first call with a nonce starts the pairing (plan 6.3, with
// the page's script as the one that opens it); later ones, such as a reload
// of the page, return the state of that pairing, whatever became of it: the
// next add makes a new link. A first call that starts none, because the
// account has been linked since add or reconnects by itself, is answered
// with that, and so is every call after it. A pairing that add began with a
// phone number (by code) or with qr_image (by the QR code in the chat) is not
// the page's: the add ended the page's link, and a poll that had passed the
// nonce check before it is answered ErrNoLogin, the pairing untouched (see
// errNotPageSession).
func (m *Manager) Login(ctx context.Context, nick, nonce string) (LoginState, error) {
	n := m.nonces.find(nick, nonce)
	if n == nil {
		return LoginState{}, ErrNoLogin
	}
	if err := n.acquire(ctx); err != nil { // concurrent polls must not start two pairings
		return LoginState{}, err
	}
	defer n.release()
	if !m.nonces.live(nick, n) { // a newer add replaced it while this waited
		return LoginState{}, ErrNoLogin
	}
	if n.sess == nil && n.settled == nil {
		if err := m.startLogin(ctx, nick, n); err != nil {
			return LoginState{}, err
		}
	}
	if n.sess != nil {
		return loginState(n.sess.status()), nil
	}
	return *n.settled, nil
}

// startLogin makes the first call's decision for n: a pairing, or the answer
// that stands in for it. Only a request that ended first is an error, and
// the next poll decides again.
func (m *Manager) startLogin(ctx context.Context, nick string, n *loginNonce) error {
	s, err := m.add(ctx, nick)
	switch {
	case s != nil:
		// The pairing runs whatever became of the request, so the nonce must
		// know it: a reload would otherwise restart it.
		n.sess = s
		m.nonces.extend(nick, n, m.nonces.now().Add(pairingGrace))
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errNotPageSession): // not this page's: its link ended with the add that began that
		return ErrNoLogin
	case err != nil:
		n.settled = &LoginState{State: string(pairFailed), Reason: m.loginReason(nick, err)}
	default: // the account turned reconnectable since add
		n.settled = &LoginState{State: string(pairDone), Hint: reconnectingHint}
	}
	return nil
}

// loginReason is what the page says of an add that failed: a refusal in its
// own words, anything else only in general, as the details, a database
// path or a library's error, are for the log.
func (m *Manager) loginReason(nick string, err error) string {
	var r refusal
	if errors.As(err, &r) || errors.Is(err, errClosing) {
		return err.Error()
	}
	m.log.Warn("could not start linking", "account", nick, "err", err)
	return "could not start linking; call add again"
}
