package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/proc"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

// squatter stands on the port of a stale daemon.json and answers /healthz
// with whatever answer builds. It counts requests that carried a token.
func squatter(t *testing.T, h home.Home, pid int, answer func(nonce string) Health) *atomic.Int32 {
	t.Helper()
	var leaked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		json.NewEncoder(w).Encode(answer(r.URL.Query().Get("nonce")))
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if err := h.WriteDaemonInfo(home.DaemonInfo{PID: pid, Port: port, Version: "v0.1.0"}); err != nil {
		t.Fatal(err)
	}
	return &leaked
}

func TestProbeRequiresProofOfToken(t *testing.T) {
	const token = "secret"
	for _, tc := range []struct {
		name   string
		answer func(nonce string) Health
		ok     bool
	}{
		{"daemon", func(n string) Health { return Health{Version: "v0.2.0", PID: 42, Proof: home.TokenProof(token, n)} }, true},
		{"no proof", func(string) Health { return Health{Version: "v99.0.0", PID: 42} }, false},
		{"wrong proof", func(n string) Health { return Health{Version: "v0.0.1", PID: 42, Proof: home.TokenProof("guess", n)} }, false},
		{"wrong pid", func(n string) Health { return Health{Version: "v0.2.0", PID: 7, Proof: home.TokenProof(token, n)} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := home.Home{Dir: testutil.TempDir(t)}
			os.WriteFile(h.TokenFile(), []byte(token), 0o600)
			leaked := squatter(t, h, 42, tc.answer)

			info, err := Probe(context.Background(), h)
			if tc.ok != (err == nil) {
				t.Fatalf("Probe = %+v, %v", info, err)
			}
			if !tc.ok {
				if !errors.Is(err, ErrNotRunning) {
					t.Fatalf("err = %v, want ErrNotRunning", err)
				}
				// What status and stop do next must not reach the port either.
				Stop(context.Background(), h)
				Status(context.Background(), h, "v0.1.0", nil)
			}
			if leaked.Load() != 0 {
				t.Fatal("the token was sent to the port")
			}
		})
	}
}

func TestWaitGoneOnFreeLock(t *testing.T) {
	// daemon.json could not be deleted, but its daemon released the lock.
	h := home.Home{Dir: testutil.TempDir(t)}
	h.WriteDaemonInfo(home.DaemonInfo{PID: 42, Port: 1, Version: "v0.1.0"})
	if err := waitGone(context.Background(), h, 42, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	l, err := proc.TryLock(h.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if err := waitGone(context.Background(), h, 42, 300*time.Millisecond); err == nil {
		t.Fatal("a daemon still holding the lock counted as gone")
	}
}
