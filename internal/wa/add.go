package wa

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// addAction is what add does with an account.
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

// refusal is a call that is refused: its text is for the user, unlike that of
// the errors that come from the database or the network.
type refusal string

func (r refusal) Error() string { return string(r) }

// errStillFinishing is the answer of add to a call that has waited abortWait for the
// pairing of an earlier call, which it has told to stop, and it is still not over:
// the call has started nothing, and a later one finds it over. See awaitEnd; remove
// answers errStillCancelling and errStillRemoving.
const errStillFinishing = refusal("the previous attempt is still finishing; repeat the call in a minute")

// addOutcome is what admit did for an account that was not refused.
type addOutcome int

const (
	outcomePair      addOutcome = iota + 1 // the account needs a new pairing (not 0: an error return is no outcome)
	outcomeReconnect                       // its device is connecting again, with the existing keys
)

// admitted is what admit did for an account that was not refused.
type admitted struct {
	outcome addOutcome
	sess    *pairSession // the pairing, started: for outcomePair with a kind other than kindNone
	link    *loginNonce  // the login link, issued: for outcomePair with kindNone
}

// add starts the pairing of nick, as the login page does when it first polls:
// the session, or nil if the decision turned out to be a reconnect. See admit.
func (m *Manager) add(ctx context.Context, nick string) (*pairSession, error) {
	adm, err := m.admit(ctx, nick, kindPage, "")
	return adm.sess, err
}

// awaitEnd waits for done, the end of a pairing or of the unlinking of a device that
// a call has told to stop or has begun, for at most abortWait. A connect in the
// noise handshake ignores the stop (client.go:500-502, handshake.go:24, 46-50) and
// holds the socket lock that a Disconnect or a Logout needs, so what hangs there
// ends only with it, ~20 s later, or ~60 s in a network that takes no answers: the
// call is not to wait for that, and it must not go on with the thing still owning
// the account's client, so it answers errStillFinishing instead. Every call that
// waits for such an end waits here: add of each kind and remove. The answer to a
// wait that runs out is still, the call's own words for it. The call's ctx and
// Close end the wait with their errors.
func (m *Manager) awaitEnd(ctx context.Context, done <-chan struct{}, still error) error {
	timer := time.NewTimer(m.abortWait)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return still
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return errClosing
	}
}

// cancelPairing cancels prev, the pairing of nick's account, and waits for it to
// end (awaitEnd), as add and remove do before they decide again. prev that has taken
// a device is not cancelled, and the caller's next decision refuses.
func (m *Manager) cancelPairing(ctx context.Context, nick string, prev *pairSession, still error) error {
	if !prev.stop() { // the phone has just scanned it
		return nil
	}
	m.log.Info("cancelling the previous linking", "account", nick)
	return m.awaitEnd(ctx, prev.done, still)
}

// errNotPageSession is what the login page's start of a pairing gets when the
// account's pairing is not one of a login page: it waits for a code typed on the
// phone, or its QR code is an image in the chat. Such a pairing is not the
// page's to restart. The add that began it ended the link of the page, so this
// is for a poll that was already under way then, or whose nonce an add without
// a phone number issued while the one that began the pairing was starting it.
var errNotPageSession = errors.New("the account is being linked by a pairing code or in the chat, not by a login page")

// ensureRow makes nick's row in archive.db. A failure is told to the caller, an
// agent, only in general: the details, the driver's words, are for the log. A
// caller that has given up gets its own error.
func (m *Manager) ensureRow(ctx context.Context, nick string) error {
	err := m.db.AddAccount(ctx, nick)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.log.Warn("create the account in archive.db", "account", nick, "err", err)
	return errors.New("could not create the account; call add again")
}

// admit is the core of the add tool for nick: it refuses with the reason as
// the error, reconnects the account's device, or finds that the account needs
// a pairing and, with a kind other than kindNone, starts it and returns the
// session.
//
// The kind says who asks. The add tool without a phone number passes kindNone:
// its pairing starts only when the login page polls (kindPage), so an unopened
// link costs no session, and what comes back is the link, issued. With qr_image
// it passes kindChat, which starts the pairing at once, as its first QR code is
// the answer. With a phone number it passes kindPhone and the number's digits,
// user: before it cancels or starts anything, and only for a decision to pair (a
// reconnect ignores the number, and a refusal of the policy comes first), the
// number must be one the account may take (phoneRefusal). A nick that is refused
// for it does not get an account, which would show up in list; otherwise the
// account exists from the first call on. The tool and the page each decide for
// themselves, and the page's answer is the one the user sees: a status that
// moved in between, say the account got linked, shows there as a failed or a
// reconnecting login, never as a second pairing.
//
// Each decision is taken, and acted on, under one hold of mu, so a status that
// moves while a pairing is cancelled or built is decided again. That includes
// the login link: an add that hands out no link of its own, a reconnect or a
// pairing for the chat or by code, revokes the nick's, and an add that does hand
// one out issues it, in the same hold as the decision, so that the link left is
// always the latest decision's, whichever call gets on first. The login page
// itself, kindPage, leaves the links alone: the one it holds is its own, and it
// makes no account: a poll that finds none, because a remove has taken the account
// since its nonce was found, is answered ErrNoLogin and does not bring it back.
// Waiting for a cancelled pairing to end, which may take a noise handshake (up to
// 20 s, handshake.go:24) or a dial, is bound by abortWait and ctx (awaitEnd): the
// next add of the nick, finding it still not over, refuses to start another over
// it. An account that a remove has is refused.
//
// The account's row in archive.db and its entry in the manager go together: a
// remove deletes the row and then drops the entry, counting it (m.removed), and
// admit that has ensured the row, and then finds no entry, makes the row again if
// a remove has dropped any account since (rowAt), as it may have been this one.
func (m *Manager) admit(ctx context.Context, nick string, kind pairKind, user string) (admitted, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return admitted{}, errClosing // before it makes a row
	}
	_, known := m.accounts[nick]
	if !known && kind == kindPage {
		m.mu.Unlock()
		return admitted{}, ErrNoLogin
	}
	if !known && kind == kindPhone {
		if reason := m.phoneRefusal(newAccount(nick), user); reason != "" { // not created: it would show up in list
			m.mu.Unlock()
			return admitted{}, refusal(reason)
		}
	}
	rowAt := m.removed // an account that is known has its row as of now, and one that is not gets it next
	m.mu.Unlock()
	if !known {
		// The account exists from now on: a pairing that fails leaves it
		// needs_link, "not linked yet" after a restart.
		if err := m.ensureRow(ctx, nick); err != nil {
			return admitted{}, err
		}
	}
	var s *pairSession // built on the first pair decision, unless kindNone
	var built *account // the account s was built for
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			s.drop()
			return admitted{}, errClosing
		}
		a := m.accounts[nick]
		if a == nil {
			if kind == kindPage { // removed since the nonce was found
				m.mu.Unlock()
				s.drop()
				return admitted{}, ErrNoLogin
			}
			if rowAt != m.removed { // a remove has dropped an account since the row was ensured
				rowAt = m.removed
				m.mu.Unlock()
				s.drop()
				s, built = nil, nil
				if err := m.ensureRow(ctx, nick); err != nil {
					return admitted{}, err
				}
				continue
			}
			a = newAccount(nick)
			m.accounts[nick] = a
		}
		if a.removing || a.unlinking != nil {
			m.mu.Unlock()
			s.drop()
			return admitted{}, refusal(fmt.Sprintf("account %q is being removed; repeat the call in a minute", nick))
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
				link := m.nonces.issue(nick)
				m.mu.Unlock()
				return admitted{outcome: outcomePair, link: link}, nil
			}
			if s != nil && built == a {
				m.wg.Add(1) // under mu and not closed, as track does
				a.sess, a.pcli = s, s.cli
				a.info = with(a.info, StatusLinking, "", time.Time{})
				m.stopHistoryLocked(a) // the client an import in flight reads with is not the account's now
				if kind != kindPage {
					m.nonces.revoke(nick)
				}
				m.mu.Unlock()
				m.log.Info("linking started", "account", nick)
				go m.runPairing(a, s)
				return admitted{outcome: outcomePair, sess: s}, nil
			}
			m.mu.Unlock()
			s.drop() // built for an account that a remove has taken since, or not at all
			var err error
			if s, err = m.newPairing(a, kind); err != nil {
				return admitted{}, err
			}
			built = a
		case addRestart:
			prev := a.sess
			m.mu.Unlock()
			if kind == kindPage && prev.kind != kindPage { // a page restarts only its own kind
				s.drop()
				return admitted{}, errNotPageSession
			}
			if err := m.cancelPairing(ctx, nick, prev, errStillFinishing); err != nil {
				s.drop()
				return admitted{}, err
			}
		case addReconnect:
			// Before the connect: Connected does not move replaced (see next).
			a.info = with(a.info, StatusReconnecting, "", time.Time{})
			if kind != kindPage {
				m.nonces.revoke(nick)
			}
			cli := a.cli
			m.mu.Unlock()
			s.drop()
			m.log.Info("reconnecting the account with its existing device", "account", nick)
			m.connect(a, cli)
			return admitted{outcome: outcomeReconnect}, nil
		default:
			m.mu.Unlock()
			s.drop()
			return admitted{}, refusal(d.reason)
		}
	}
}
