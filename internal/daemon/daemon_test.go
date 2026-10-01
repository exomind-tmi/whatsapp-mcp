package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
)

func TestEnsureTokenCreatesOnce(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	first, err := ensureToken(h)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Fatalf("token %q is not 32 hex bytes", first)
	}
	second, err := ensureToken(h)
	if err != nil || second != first {
		t.Fatalf("second call changed the token: %q, %v", second, err)
	}
}

func TestEnsureTokenKeepsExisting(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	os.WriteFile(h.TokenFile(), []byte("preset"), 0o600)
	if tok, err := ensureToken(h); err != nil || tok != "preset" {
		t.Fatalf("ensureToken = %q, %v", tok, err)
	}
}

func TestMuxAuth(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(newMux(muxDeps{
		version: "v1.2.3",
		token:   "secret",
		server:  tools.NewServer("v1.2.3", tools.Deps{WA: &toolstest.WA{}}),
		stop:    stop,
		log:     slog.New(slog.DiscardHandler),
	}))
	defer srv.Close()

	do := func(method, path, auth string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := do("GET", "/healthz", ""); code != 200 || !strings.Contains(body, `"version":"v1.2.3"`) ||
		strings.Contains(body, "proof") {
		t.Fatalf("healthz = %d %s", code, body)
	}
	if _, body := do("GET", "/healthz?nonce=n1", ""); !strings.Contains(body, home.TokenProof("secret", "n1")) ||
		strings.Contains(body, "secret") {
		t.Fatalf("healthz with nonce = %s", body)
	}
	for _, auth := range []string{"", "Bearer wrong", "secret"} {
		if code, _ := do("POST", "/mcp", auth); code != http.StatusUnauthorized {
			t.Errorf("/mcp with %q = %d, want 401", auth, code)
		}
		if code, _ := do("POST", "/admin/stop", auth); code != http.StatusUnauthorized {
			t.Errorf("/admin/stop with %q = %d, want 401", auth, code)
		}
	}
	if code, _ := do("POST", "/mcp", "Bearer secret"); code == http.StatusUnauthorized {
		t.Error("/mcp rejected a valid token")
	}
	if code, _ := do("POST", "/admin/stop", "Bearer secret"); code != http.StatusAccepted {
		t.Fatalf("/admin/stop = %d, want 202", code)
	}
	select {
	case <-stop:
	case <-time.After(time.Second):
		t.Fatal("stop not signalled")
	}
	do("POST", "/admin/stop", "Bearer secret") // a second stop must not panic
}

// TestRunLifecycle runs the real daemon in-process: lock, token, daemon.json,
// a second daemon exits cleanly, and shutdown removes daemon.json.
func TestRunLifecycle(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h, "v0.1.0") }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := h.ReadDaemonInfo(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon.json not written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := h.ReadToken(); err != nil {
		t.Fatalf("token: %v", err)
	}

	// A second daemon loses the lock after its retry window and exits 0,
	// leaving the winner's log alone even when it is due for rotation.
	logPath := h.Log("daemon.log")
	lf, _ := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	lf.Write(make([]byte, 10<<20+1))
	lf.Close()
	if err := Run(context.Background(), h, "v0.1.0"); err != nil {
		t.Fatalf("second daemon: %v", err)
	}
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("the losing daemon rotated the winner's log: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.DaemonJSON()); !os.IsNotExist(err) {
		t.Fatalf("daemon.json left after shutdown: %v", err)
	}
	for _, db := range []string{"archive.db", "store.db"} {
		if _, err := os.Stat(filepath.Join(h.Dir, db)); err != nil {
			t.Fatalf("%s not created: %v", db, err)
		}
	}
}

// TestRunManagerFails: a store.db the Manager cannot open stops the daemon
// before it serves, with the error and without daemon.json.
func TestRunManagerFails(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.StoreDB(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), h, "v0.1.0"); err == nil || !strings.Contains(err.Error(), "store.db") {
		t.Fatalf("Run = %v, want store.db's error", err)
	}
	if _, err := os.Stat(h.DaemonJSON()); !os.IsNotExist(err) {
		t.Fatalf("daemon.json left behind: %v", err)
	}
}
