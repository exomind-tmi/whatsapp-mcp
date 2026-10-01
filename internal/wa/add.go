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

// add is the core of the add tool for nick: it refuses with the reason as
// the error, reconnects the account's device and returns no pairing, or
// starts a pairing and returns it. Each decision is taken, and acted on,
// under one hold of mu, so a status that moves while a pairing is cancelled
// or built is decided again. Waiting for a cancelled pairing to end, which
// may take a noise handshake (up to 20 s, handshake.go:24), stops with ctx.
func (m *Manager) add(ctx context.Context, nick string) (*pairSession, error) {
	m.mu.Lock()
	_, known := m.accounts[nick]
	m.mu.Unlock()
	if !known {
		// The account exists from now on: a pairing that fails leaves it
		// needs_link, "not linked yet" after a restart.
		if err := m.db.AddAccount(ctx, nick); err != nil {
			return nil, err
		}
	}
	var s *pairSession // built on the first pair decision
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			s.drop()
			return nil, errClosing
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
		switch d.action {
		case addPair:
			if s != nil {
				m.wg.Add(1) // under mu and not closed, as track does
				a.sess, a.pcli = s, s.cli
				a.info = with(a.info, StatusLinking, "", time.Time{})
				m.mu.Unlock()
				m.log.Info("linking started", "account", nick)
				go m.runPairing(a, s)
				return s, nil
			}
			m.mu.Unlock()
			s.drop()
			var err error
			if s, err = m.newPairing(a); err != nil {
				return nil, err
			}
		case addRestart:
			prev := a.sess
			m.mu.Unlock()
			// false: the phone has just scanned it, and the next round refuses.
			if prev.stop() {
				m.log.Info("cancelling the previous linking", "account", nick)
				select {
				case <-prev.done:
				case <-ctx.Done():
					s.drop()
					return nil, ctx.Err()
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
			return nil, nil
		default:
			m.mu.Unlock()
			s.drop()
			return nil, errors.New(d.reason)
		}
	}
}
