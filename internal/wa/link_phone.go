package wa

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow"
)

// phoneWait bounds WhatsApp's answer to PairPhone, a single round trip; the
// IQ's own timeout is 75 s (request.go:150-154), too long for a tool call.
const phoneWait = 15 * time.Second

// The reasons a pairing by phone number ends without a code or a device, besides
// couldNotReach (link_wait.go). Each is the caller's error and the account's
// reason, and holds neither the server's own words, which stay in the log, nor
// the number.
const (
	// rejectedReason: WhatsApp answered with an error for the request, as for a
	// number it does not know or a request it did not understand.
	rejectedReason = "WhatsApp did not accept the request for a pairing code; check that the number is in international format and has WhatsApp, and call add again"
	// rateLimitedReason: WhatsApp's 429, for too many codes asked in a while.
	rateLimitedReason = "WhatsApp does not give more pairing codes for now; wait a few minutes and call add again"
	// codeExpired is for a pairing by code whose channel ended after the first
	// code: the codes ran out, the connection dropped, or the phone's code never
	// came to a link, which whatsmeow only logs (pair-code.go:145-150).
	codeExpired = "the pairing code expired, was not accepted, or the connection dropped; call add again"
)

// issueCode is the rest of add with a phone number, for the pairing s that
// admit has started: it waits for the first QR code, which tells that the
// connection is up (pair-code.go:78-81), asks WhatsApp for a pairing code and
// hands it out. The pairing then goes on as one by QR does, over the same
// channel (pair-code.go:145-247, pair.go:132-160), and its outcome is the
// account's status: linking now, connected once the new device does, else
// needs_link with the reason.
//
// The ticket's ExpiresAt is the end of the QR window, which the first code
// starts: the connection closes with the last QR code, and nobody knows when
// the code itself expires (pair-code.go:83-85), so it is a limit and not a
// promise, and the phone may refuse the code earlier.
//
// Every failure ends the pairing, so that nothing is left: the session is over,
// its client disconnected, accounts.jid and the account's number untouched.
// Only a connect that hangs outlives the call (see abortWait).
func (m *Manager) issueCode(ctx context.Context, nick string, s *pairSession, digits string) (LinkTicket, error) {
	if err := m.awaitFirstCode(ctx, nick, s); err != nil {
		return LinkTicket{}, err
	}
	pctx, cancel := context.WithTimeout(ctx, m.phoneWait)
	defer cancel()
	code, err := m.net.pairPhone(s.cli, pctx, digits, true, whatsmeow.PairClientChrome, pairCodeName())
	if err != nil {
		return LinkTicket{}, m.pairPhoneFailed(ctx, nick, s, err)
	}
	if s.over() { // it ended while the request was in flight: a code for nothing
		return LinkTicket{}, m.abort(ctx, s, "")
	}
	m.log.Info("pairing code issued", "account", nick) // never the code or the number (plan 10)
	return LinkTicket{PairCode: code, ExpiresAt: s.window}, nil
}

// pairPhoneFailed ends s, for which PairPhone returned err, and returns what
// the caller is told. The session may have ended while the request was in
// flight, on its own or by a newer add, and the request then failed with it:
// the reason it ended with is the account's, which abort returns once it has
// ended, and not the reason that err would give.
func (m *Manager) pairPhoneFailed(ctx context.Context, nick string, s *pairSession, err error) error {
	switch {
	case m.ctx.Err() != nil: // Close: the session ends with it
		return errClosing
	case ctx.Err() != nil: // the caller gave up
		m.abort(ctx, s, "")
		return ctx.Err()
	}
	m.log.Warn("pairing by phone number: no pairing code", "account", nick, "err", err)
	return m.abort(ctx, s, phoneFailure(err))
}

// phoneFailure is the reason for which the pairing ends when PairPhone returned
// err. Only an error that WhatsApp answered the request with says anything
// about the number; no answer, a socket that dropped or a timeout say that the
// connection is to blame, and so does a server that failed itself.
func phoneFailure(err error) string {
	var missing *whatsmeow.ElementMissingError
	if errors.As(err, &missing) {
		return rejectedReason // an answer that is not what the request gets
	}
	var iqe *whatsmeow.IQError
	if !errors.As(err, &iqe) {
		return couldNotReach
	}
	switch {
	case iqe.Code == 429:
		return rateLimitedReason
	case iqe.Code >= 500:
		return couldNotReach
	}
	return rejectedReason
}
