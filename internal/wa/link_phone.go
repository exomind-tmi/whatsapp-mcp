package wa

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow"
)

// codeWait bounds add with a phone number's wait for the first QR code, from
// the connect, which waits for the client globals before (readQR): the code
// comes as soon as the connection is up, and without it, behind a captive
// portal or a connection that takes no answers, the caller would wait for
// nothing. Offline fails faster, with the channel's own end.
const codeWait = 10 * time.Second

// phoneWait bounds WhatsApp's answer to PairPhone, a single round trip; the
// IQ's own timeout is 75 s (request.go:150-154), too long for a tool call.
const phoneWait = 15 * time.Second

// abortWait bounds the wait of a call that gave up its pairing for the pairing
// to end. A connect in the noise handshake ignores the session's context
// (client.go:500-502, handshake.go:24, 46-50), so a pairing that hangs there
// ends only with the connect, after the 20 s of the handshake or a dial's own
// timeout. The call answers before that, and the session leaves the account,
// still linking until then, its reason when it does end.
const abortWait = 3 * time.Second

// The reasons a pairing by phone number ends without a code or a device. Each
// is the caller's error and the account's reason, and holds neither the
// server's own words, which stay in the log, nor the number.
const (
	// couldNotReach: no code came in time, or the answer to the request did
	// not: no connection, or a captive portal in its place.
	couldNotReach = "could not reach WhatsApp; check the connection and call add again"
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
	window, err := m.awaitFirstCode(ctx, nick, s)
	if err != nil {
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
	return LinkTicket{PairCode: code, ExpiresAt: window}, nil
}

// awaitFirstCode waits until s has shown its first QR code and returns the end
// of its window. The wait for the client globals, which may take as long as the
// version check (versionTimeout), is not counted in codeWait: what is bounded
// is WhatsApp's answer, not our own start. Both are bound by ctx, and by the
// end of s, which Close brings.
func (m *Manager) awaitFirstCode(ctx context.Context, nick string, s *pairSession) (time.Time, error) {
	select {
	case <-s.connecting:
	case <-s.done:
		return time.Time{}, m.endedErr(s)
	case <-ctx.Done():
		m.abort(ctx, s, "")
		return time.Time{}, ctx.Err()
	}
	timer := time.NewTimer(m.codeWait)
	defer timer.Stop()
	select {
	case <-s.coded:
	case <-s.done:
		return time.Time{}, m.endedErr(s)
	case <-timer.C:
		m.log.Warn("pairing by phone number: no QR code from WhatsApp", "account", nick, "after", m.codeWait)
		return time.Time{}, m.abort(ctx, s, couldNotReach)
	case <-ctx.Done():
		m.abort(ctx, s, "")
		return time.Time{}, ctx.Err()
	}
	// select picks among what is ready: the code may have come together with
	// the end of s, or with Close, and then no request goes out.
	if s.over() {
		return time.Time{}, m.abort(ctx, s, "")
	}
	return s.window, nil
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

// abort ends the pairing s, which has no use any more, leaving the account
// with reason ("" for the plain cancellation), and waits for it to end, up to
// ctx and abortWait: its client is then disconnected, as for any end of a
// pairing. It returns what the caller is told, which is the account's own
// reason once the session has ended, so that the two never differ; a session
// that has not ended yet leaves the account with reason when it does. A pairing
// that has taken a device is left to run.
func (m *Manager) abort(ctx context.Context, s *pairSession, reason string) error {
	if m.ctx.Err() != nil { // Close ends it, and waits for it
		return errClosing
	}
	if s.stopFor(reason) {
		timer := time.NewTimer(m.abortWait)
		defer timer.Stop()
		select {
		case <-s.done:
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-s.done:
		return m.endedErr(s)
	default:
		if reason == "" {
			reason = cancelledReason
		}
		return errors.New(reason)
	}
}

// endedErr is the error of a pairing that has ended before it gave a code: the
// daemon is closing, or the session failed and its reason is the account's.
func (m *Manager) endedErr(s *pairSession) error {
	if m.ctx.Err() != nil {
		return errClosing
	}
	if reason := s.status().Reason; reason != "" {
		return errors.New(reason)
	}
	return errors.New(cancelledReason)
}
