package wa

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// addAction is what add does with an account (plan 5.3, Anton's decision
// of 2026-10-01).
type addAction int

const (
	addRefuse    addAction = iota
	addReconnect           // connect again with the device's keys, no QR
	addPair                // link a new device
	addRestart             // cancel the pairing in progress, then decide again
)

// pairingPhase is where an account's pairing stands, as add sees it.
type pairingPhase int

const (
	noPairing    pairingPhase = iota // none, or it has ended
	pairingOpen                      // showing codes; the next add cancels it
	pairingBound                     // PrePairCallback took a device: it must run to its end
)

type addDecision struct {
	action addAction
	reason string // why add refuses
}

// addPolicy decides what add does with an account whose status is info; a
// nick with no account yet is needs_link. device tells whether the account
// has a device, pairing where its pairing stands, now whether a ban is over.
//
// A pairing in progress starts over, unless the phone has scanned it:
// accounts.jid then names the new device. A linked account is never paired
// again: a laptop offline would lose a working device. replaced and error
// keep valid keys, as whatsmeow deletes them only on LoggedOut
// (connectionevents.go:40-51, 108-111), so a plain connect brings them
// back. A ban whose end WhatsApp did not give cannot be waited out, so add
// tries to reconnect and learns whether it is still on, as a restart of
// the daemon would.
func addPolicy(info AccountInfo, device bool, pairing pairingPhase, now time.Time) addDecision {
	refuse := func(format string, args ...any) addDecision {
		return addDecision{action: addRefuse, reason: fmt.Sprintf(format, args...)}
	}
	switch pairing {
	case pairingBound:
		return refuse("account %q was just linked and is connecting; check its status with list", info.Nick)
	case pairingOpen:
		return addDecision{action: addRestart}
	}
	reconnect := addDecision{action: addReconnect}
	if !device { // nothing to reconnect
		reconnect.action = addPair
	}
	switch info.Status {
	case StatusConnected:
		return refuse("account %q is already linked and connected", info.Nick)
	case StatusReconnecting:
		return refuse("account %q is linked and reconnects by itself; there is no need to add it again", info.Nick)
	case StatusClientOutdated:
		return refuse("WhatsApp rejected this version of whatsapp-mcp; update whatsapp-mcp, and account %q reconnects by itself", info.Nick)
	case StatusReplaced:
		return reconnect
	case StatusError:
		if !info.ExpiresAt.IsZero() && now.Before(info.ExpiresAt) {
			return refuse("account %q is banned by WhatsApp until %s (%s); call add again after that",
				info.Nick, info.ExpiresAt.Local().Format(time.RFC3339), info.Reason)
		}
		return reconnect
	}
	return addDecision{action: addPair} // needs_link, or linking whose pairing has ended
}

var errClosing = errors.New("whatsapp-mcp is shutting down")

// refusal is an add that the policy refuses: its text is for the user, unlike
// that of the errors that come from the database or the network.
type refusal string

func (r refusal) Error() string { return string(r) }

// addOutcome is what admit did for an account that was not refused.
type addOutcome int

const (
	outcomePair      addOutcome = iota + 1 // the account needs a new pairing (not 0: an error return is no outcome)
	outcomeReconnect                       // its device is connecting again, with the existing keys
)

// add starts the pairing of nick, as the login page does when it first polls
// (plan 6.3): the session, or nil if the decision turned out to be a
// reconnect. See admit.
func (m *Manager) add(ctx context.Context, nick string) (*pairSession, error) {
	_, s, err := m.admit(ctx, nick, kindPage, "")
	return s, err
}

// errNotPageSession is what the login page's start of a pairing gets when the
// account's pairing is not one of a login page: it waits for a code typed on the
// phone, or its QR code is an image in the chat. Such a pairing is not the
// page's to restart. The add that began it ended the link of the page, so this
// is for a poll that was already under way then, or whose nonce an add without
// a phone number issued while the one that began the pairing was starting it.
var errNotPageSession = errors.New("the account is being linked by a pairing code or in the chat, not by a login page")

// admit is the core of the add tool for nick: it refuses with the reason as
// the error, reconnects the account's device, or finds that the account needs
// a pairing and, with a kind other than kindNone, starts it and returns the
// session.
//
// The kind says who asks. The add tool without a phone number passes kindNone:
// its pairing starts only when the login page polls (kindPage), so an unopened
// link costs no session. With qr_image it passes kindChat, which starts the
// pairing at once, as its first QR code is the answer. With a phone number it
// passes kindPhone and the number's digits, user: before it cancels or starts
// anything, and only for a decision to pair (a reconnect ignores the number,
// and a refusal of the policy comes first), the number must be one the account
// may take (phoneRefusal). A nick that is refused for it does not get an
// account, which would show up in list; otherwise the account exists from the
// first call on. The tool and the page each decide for themselves, and the
// page's answer is the one the user sees: a status that moved in between, say
// the account got linked, shows there as a failed or a reconnecting login,
// never as a second pairing.
//
// Each decision is taken, and acted on, under one hold of mu, so a status that
// moves while a pairing is cancelled or built is decided again. Waiting for a
// cancelled pairing to end, which may take a noise handshake (up to 20 s,
// handshake.go:24) or a dial, stops with ctx and not before: the next add of
// the nick waits for a pairing that hangs in its connect.
func (m *Manager) admit(ctx context.Context, nick string, kind pairKind, user string) (addOutcome, *pairSession, error) {
	m.mu.Lock()
	_, known := m.accounts[nick]
	if !known && kind == kindPhone {
		if reason := m.phoneRefusal(newAccount(nick), user); reason != "" { // not created: it would show up in list
			m.mu.Unlock()
			return 0, nil, refusal(reason)
		}
	}
	m.mu.Unlock()
	if !known {
		// The account exists from now on: a pairing that fails leaves it
		// needs_link, "not linked yet" after a restart.
		if err := m.db.AddAccount(ctx, nick); err != nil {
			return 0, nil, err
		}
	}
	var s *pairSession // built on the first pair decision, unless kindNone
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			s.drop()
			return 0, nil, errClosing
		}
		a := m.accounts[nick]
		if a == nil {
			a = newAccount(nick)
			m.accounts[nick] = a
		}
		phase := noPairing
		if a.sess != nil {
			phase = a.sess.phase()
		}
		d := addPolicy(a.info, a.cli != nil, phase, time.Now())
		if kind == kindPhone && (d.action == addPair || d.action == addRestart) { // a restart pairs again
			if reason := m.phoneRefusal(a, user); reason != "" {
				d = addDecision{action: addRefuse, reason: reason}
			}
		}
		switch d.action {
		case addPair:
			if kind == kindNone {
				m.mu.Unlock()
				return outcomePair, nil, nil
			}
			if s != nil {
				m.wg.Add(1) // under mu and not closed, as track does
				a.sess, a.pcli = s, s.cli
				a.info = with(a.info, StatusLinking, "", time.Time{})
				m.mu.Unlock()
				m.log.Info("linking started", "account", nick)
				go m.runPairing(a, s)
				return outcomePair, s, nil
			}
			m.mu.Unlock()
			s.drop()
			var err error
			if s, err = m.newPairing(a, kind); err != nil {
				return 0, nil, err
			}
		case addRestart:
			prev := a.sess
			m.mu.Unlock()
			if kind == kindPage && prev.kind != kindPage { // a page restarts only its own kind
				s.drop()
				return 0, nil, errNotPageSession
			}
			// false: the phone has just scanned it, and the next round refuses.
			if prev.stop() {
				m.log.Info("cancelling the previous linking", "account", nick)
				select {
				case <-prev.done:
				case <-ctx.Done():
					s.drop()
					return 0, nil, ctx.Err()
				}
			}
		case addReconnect:
			// Before the connect: Connected does not move replaced (see next).
			a.info = with(a.info, StatusReconnecting, "", time.Time{})
			cli := a.cli
			m.mu.Unlock()
			s.drop()
			m.log.Info("reconnecting the account with its existing device", "account", nick)
			m.connect(a, cli)
			return outcomeReconnect, nil, nil
		default:
			m.mu.Unlock()
			s.drop()
			return 0, nil, refusal(d.reason)
		}
	}
}
