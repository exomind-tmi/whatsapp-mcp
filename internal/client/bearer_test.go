package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

func TestBearerRereadsTokenOn401(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	os.WriteFile(h.TokenFile(), []byte("old"), 0o600)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(body) // echo proves the body survived the retry
	}))
	defer srv.Close()

	c := HTTPClient(h)
	post := func() (int, string) {
		resp, err := c.Post(srv.URL, "text/plain", strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Token unchanged on disk: no retry, the 401 is returned.
	if code, _ := post(); code != http.StatusUnauthorized || calls.Load() != 1 {
		t.Fatalf("code=%d calls=%d", code, calls.Load())
	}
	// The daemon was recreated with a new token: one re-read, one retry.
	os.WriteFile(h.TokenFile(), []byte("new"), 0o600)
	calls.Store(0)
	if code, body := post(); code != 200 || body != "payload" || calls.Load() != 2 {
		t.Fatalf("code=%d body=%q calls=%d", code, body, calls.Load())
	}
}
