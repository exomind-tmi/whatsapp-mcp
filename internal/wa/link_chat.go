package wa

import (
	"context"

	"github.com/exomind-tmi/whatsapp-mcp/internal/qr"
)

// drawFailed is the reason for a QR code that cannot be drawn: the pairing ends
// with it, as a code nobody sees cannot be scanned.
const drawFailed = "could not draw the QR code; call add again"

// issueQR is the rest of add with qr_image, for the pairing s that admit has
// started: it waits for the first QR code (awaitFirstCode, as add with a phone
// number does) and hands it out drawn as a PNG, for the chat to show. The
// pairing then goes on as one by QR does, over the same channel, and its
// outcome is the account's status: linking now, connected once the new device
// is, else needs_link with the reason.
//
// The image is that of the code at the moment of the answer, and it is not
// replaced: the codes that follow it rotate unseen, so nothing but a new add
// gives a fresh one. The ticket's ExpiresAt is therefore the end of that code's
// life, which for the first code is its start plus its timeout, 60 s when the
// server sent six refs (qrchan.go:92-96); it is not the end of the QR window,
// as for a pairing code, whose own expiry is unknown.
//
// Every failure ends the pairing, as for issueCode: the session is over, its
// client disconnected, accounts.jid and the account's number untouched.
func (m *Manager) issueQR(ctx context.Context, nick string, s *pairSession) (LinkTicket, error) {
	if err := m.awaitFirstCode(ctx, nick, s); err != nil {
		return LinkTicket{}, err
	}
	return m.qrTicket(ctx, nick, s)
}

// qrTicket is the ticket for the code that s shows now, drawn.
//
// The code and its end are read together, so that they are one code's. The
// first one was recorded before awaitFirstCode saw it (readQR), and a later one
// replaces it only if the call stalls for longer than a code lasts, or if
// WhatsApp rotates the ADV secret, which brings the current code again with the
// new secret (qrchan.go:117-124, notification.go:511-515): the image would then
// not scan. That has not been seen, and is not handled. A pairing that has
// ended since has no code left to show, and one that ends while the code is
// drawn gets no image: the check comes last, so that the window in which an
// image of a dead pairing can still go out, which no check can close, is as
// short as it can be.
func (m *Manager) qrTicket(ctx context.Context, nick string, s *pairSession) (LinkTicket, error) {
	st := s.status()
	if st.State != pairCode {
		return LinkTicket{}, m.abort(ctx, s, "")
	}
	png, err := qr.PNG(st.Code)
	if err != nil {
		m.log.Warn("draw the QR code", "account", nick, "err", err) // not the code (plan 10)
		return LinkTicket{}, m.abort(ctx, s, drawFailed)
	}
	if s.over() { // it ended while the code was drawn: an image for nothing
		return LinkTicket{}, m.abort(ctx, s, "")
	}
	m.log.Info("QR code issued", "account", nick) // never the code (plan 10)
	return LinkTicket{QRPNG: png, ExpiresAt: st.Expires}, nil
}
