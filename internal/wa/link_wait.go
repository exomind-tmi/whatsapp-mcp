package wa

import (
	"context"
	"errors"
	"time"
)

// What add with a phone number (link_phone.go) and add with qr_image
// (link_chat.go) have in common: both start the pairing at once, wait for its
// first QR code, which tells that the connection is up, and end the pairing for
// the same failures.

// codeWait bounds the wait for the first QR code, from the connect, which waits
// for the client globals before (readQR): the code comes as soon as the
// connection is up, and without it, behind a captive portal or a connection
// that takes no answers, the caller would wait for nothing. Offline fails
// faster, with the channel's own end.
const codeWait = 10 * time.Second

// abortWait bounds the wait of a call for what it has told to stop to end, the
// one wait of every path (awaitEnd): a call that gave up its pairing, an add that
// restarts the pairing of the one before, a remove. A connect in the noise
// handshake ignores the session's context (client.go:500-502, handshake.go:24,
// 46-50), so a pairing that hangs there ends only with the connect, after the 20 s
// of the handshake or a dial's own timeout. The call answers before that, and the
// session leaves the account, still linking until then, its reason when it does
// end.
const abortWait = 3 * time.Second

// couldNotReach is the reason a pairing ends with when no first QR code came in
// time, or, for a pairing code, the answer to the request did not: no
// connection, or a captive portal in its place. Like every reason of a pairing
// it is the caller's error and the account's, and holds neither the server's own
// words, which stay in the log, nor the user's number.
const couldNotReach = "could not reach WhatsApp; check the connection and call add again"

// awaitFirstCode waits until s has shown its first QR code, the window of which
// is then s.window. The wait for the client globals, which may take as long as
// the version check (versionTimeout), is not counted in codeWait: what is bound
// is WhatsApp's answer, not our own start. Both are bound by ctx, and by the
// end of s, which Close brings. Every failure ends the pairing (see abort).
func (m *Manager) awaitFirstCode(ctx context.Context, nick string, s *pairSession) error {
	select {
	case <-s.connecting:
	case <-s.done:
		return m.endedErr(s)
	case <-ctx.Done():
		m.abort(ctx, s, "")
		return ctx.Err()
	}
	timer := time.NewTimer(m.codeWait)
	defer timer.Stop()
	select {
	case <-s.coded:
	case <-s.done:
		return m.endedErr(s)
	case <-timer.C:
		m.log.Warn("linking: no QR code from WhatsApp", "account", nick, "after", m.codeWait)
		return m.abort(ctx, s, couldNotReach)
	case <-ctx.Done():
		m.abort(ctx, s, "")
		return ctx.Err()
	}
	// select picks among what is ready: the code may have come together with
	// the end of s, or with Close, and then no request goes out.
	if s.over() {
		return m.abort(ctx, s, "")
	}
	return nil
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
		// Still going on after abortWait is no failure here: the call answers anyway.
		if err := m.awaitEnd(ctx, s.done, errStillFinishing); err != nil && !errors.Is(err, errStillFinishing) {
			return err
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
