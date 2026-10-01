package daemon

import (
	qrcode "github.com/skip2/go-qrcode"
)

// qrPixels is the QR image's size, at least: whole pixels per module keep the
// edges crisp, so it is the smallest multiple of the module count that reaches
// it, and the page shows the image as it is, not scaled.
const qrPixels = 256

// qrPNG draws content as a QR code: medium error correction, a quiet zone of
// four modules (the library's border), black on white. WhatsApp's codes
// carry about 150 characters, which is a 57-module symbol.
func qrPNG(content string) ([]byte, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	modules := len(q.Bitmap()) // with the quiet zone
	return q.PNG(-((qrPixels + modules - 1) / modules))
}
