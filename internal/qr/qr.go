// Package qr draws the pairing codes WhatsApp's QR channel hands out, for the
// login page and for the chat alike.
package qr

import (
	qrcode "github.com/skip2/go-qrcode"
)

// The sizes the image is asked for, in pixels, at least. Page is for the login
// page, which has room for little more. Chat is for a chat, which shows the
// image as a small preview and the full one only when it is opened, and the
// phone has to read that one from a screen.
const (
	Page = 256
	Chat = 640
)

// PNG draws content as a QR code of size pixels or a little more: medium error
// correction, a quiet zone of four modules (the library's border), black on
// white. Whole pixels per module keep the edges crisp, so the image is the
// smallest multiple of the module count that reaches size, and it is meant to
// be shown as it is, not scaled. WhatsApp's codes carry about 150 characters,
// which is a 57-module symbol.
func PNG(content string, size int) ([]byte, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	modules := len(q.Bitmap()) // with the quiet zone
	return q.PNG(-((size + modules - 1) / modules))
}
