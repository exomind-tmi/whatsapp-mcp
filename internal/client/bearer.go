package client

import (
	"net/http"
	"sync"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
)

// bearer adds the daemon token to every request. On 401 it re-reads the
// token file and retries once: the request was rejected before running, so
// the retry is safe for any tool.
type bearer struct {
	h    home.Home
	base http.RoundTripper

	mu    sync.Mutex
	token string
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := b.current(false)
	if err != nil {
		return nil, err
	}
	resp, err := b.send(req, tok)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || (req.Body != nil && req.GetBody == nil) {
		return resp, err
	}
	fresh, err := b.current(true)
	if err != nil || fresh == tok {
		return resp, nil
	}
	resp.Body.Close()
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, err
		}
	}
	return b.send(retry, fresh)
}

func (b *bearer) send(req *http.Request, tok string) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return b.base.RoundTrip(r)
}

func (b *bearer) current(reload bool) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token == "" || reload {
		t, err := b.h.ReadToken()
		if err != nil {
			return "", err
		}
		b.token = t
	}
	return b.token, nil
}
