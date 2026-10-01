package qr

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

// waCode has the length and the alphabet of what WhatsApp's QR channel
// sends: ref, noise key, identity key and advertising secret, in base64.
const waCode = "2@AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjKlMnOpQrSt," +
	"AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhI=," +
	"ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210ZyXwVuTsR=,AbCdEfGhIjKlMnOpQrStUv=="

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	return img
}

func TestQRPNG(t *testing.T) {
	for _, content := range []string{waCode, "2@x", strings.Repeat("a", 20)} {
		b, err := PNG(content)
		if err != nil {
			t.Fatal(err)
		}
		img := decodePNG(t, b)
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		q, _ := qrcode.New(content, qrcode.Medium)
		bitmap := q.Bitmap()
		// The smallest whole pixels per module that make 256 px: a square of
		// 256 up to one module more.
		if w != h || w < 256 || w >= 256+len(bitmap) {
			t.Errorf("%d-character code: image %dx%d, want a square of 256 px and a module", len(content), w, h)
		}

		// Every module sits where the library's own bitmap, quiet zone
		// included, says: the image is that code, crisp and in whole pixels.
		px := w / len(bitmap)
		if px*len(bitmap) != w {
			t.Fatalf("image width %d is not a whole number of %d-module cells", w, len(bitmap))
		}
		dark := 0
		for y, row := range bitmap {
			for x, want := range row {
				r, g, bl, _ := img.At(x*px+px/2, y*px+px/2).RGBA()
				got := r < 0x8000 && g < 0x8000 && bl < 0x8000
				if got != want {
					t.Fatalf("module (%d,%d) is dark=%v, want %v", x, y, got, want)
				}
				if want {
					dark++
				}
			}
		}
		if dark == 0 || dark == len(bitmap)*len(bitmap) {
			t.Errorf("%d dark modules: an empty or a full image", dark)
		}
		// The quiet zone: four white modules on every side.
		mid, last := len(bitmap)/2, len(bitmap)-1
		for i := range 4 {
			if bitmap[i][mid] || bitmap[mid][i] || bitmap[last-i][mid] || bitmap[mid][last-i] {
				t.Errorf("the quiet zone has a dark module at distance %d", i)
			}
		}
	}
}

func TestQRPNGDiffers(t *testing.T) {
	a, _ := PNG(waCode)
	b, _ := PNG(strings.Replace(waCode, "2@", "2@x", 1))
	if bytes.Equal(a, b) {
		t.Error("two codes drew the same image")
	}
}

func TestQRPNGTooLong(t *testing.T) {
	if _, err := PNG(strings.Repeat("a", 8000)); err == nil {
		t.Error("a code that does not fit a QR symbol was drawn")
	}
}
