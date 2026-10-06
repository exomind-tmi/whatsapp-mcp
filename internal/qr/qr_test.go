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
	for _, size := range []int{Page, Chat} {
		for _, content := range []string{waCode, "2@x", strings.Repeat("a", 20)} {
			checkPNG(t, content, size)
		}
	}
}

// checkPNG draws content at size and checks the image module by module against
// the library's own bitmap.
func checkPNG(t *testing.T, content string, size int) {
	t.Helper()
	b, err := PNG(content, size)
	if err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, b)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	q, _ := qrcode.New(content, qrcode.Medium)
	bitmap := q.Bitmap()
	// The smallest whole pixels per module that make size px: a square of
	// size up to one module more.
	if w != h || w < size || w >= size+len(bitmap) {
		t.Errorf("%d-character code at %d px: image %dx%d, want a square of that size up to a module more", len(content), size, w, h)
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

func TestQRPNGDiffers(t *testing.T) {
	a, _ := PNG(waCode, Page)
	b, _ := PNG(strings.Replace(waCode, "2@", "2@x", 1), Page)
	if bytes.Equal(a, b) {
		t.Error("two codes drew the same image")
	}
}

func TestQRPNGTooLong(t *testing.T) {
	if _, err := PNG(strings.Repeat("a", 8000), Page); err == nil {
		t.Error("a code that does not fit a QR symbol was drawn")
	}
}

// TestQRPNGChatIsLarger: the chat shows the preview, and a phone reads the
// opened image from a screen, so it is the large one.
func TestQRPNGChatIsLarger(t *testing.T) {
	page, _ := PNG(waCode, Page)
	chat, _ := PNG(waCode, Chat)
	if p, c := decodePNG(t, page).Bounds().Dx(), decodePNG(t, chat).Bounds().Dx(); c < 2*p {
		t.Errorf("chat image is %d px against the page's %d px", c, p)
	}
}
