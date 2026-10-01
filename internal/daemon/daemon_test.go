package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
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

// TestRunIssuesLoginLink: the add tool of the real daemon hands out a link
// to its own port, and the mux answers the link's path with no token. The
// page opens, but its poll is not made: that would start a pairing on the
// real network.
func TestRunIssuesLoginLink(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h, "v0.1.0") }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	var info home.DaemonInfo
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var err error
		if info, err = h.ReadDaemonInfo(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon.json not written")
		}
	}

	cs, err := client.Connect(ctx, h, info.Port, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(args map[string]any) tools.ManageOut {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "manage-accounts", Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("manage-accounts %v: %v %v", args, err, res)
		}
		var out tools.ManageOut
		if err := json.Unmarshal([]byte(tools.ResultText(res)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := call(map[string]any{"action": "add", "account_id": "fresh"})
	u, err := url.Parse(out.LoginURL)
	if err != nil || out.Status != "linking" {
		t.Fatalf("add = %+v, %v", out, err)
	}
	nonce := u.Query().Get("t")
	if u.Scheme != "http" || u.Host != fmt.Sprintf("127.0.0.1:%d", info.Port) || u.Path != "/login/fresh" || len(nonce) != 22 {
		t.Errorf("login URL %q: want the daemon's own port and a 128-bit nonce", out.LoginURL)
	}
	if acc := call(map[string]any{"action": "list"}).Accounts; len(acc) != 1 || acc[0].AccountID != "fresh" || acc[0].Status != "needs_link" {
		t.Errorf("accounts after add: %+v", acc)
	}

	// Under /login/ the mux asks for no token but a valid nonce on our own
	// Host, with which the page opens (and starts no pairing: only its poll
	// does, which this test leaves out as it would reach the real network);
	// neither a wrong nonce nor another Host gets in.
	for _, host := range []string{"", fmt.Sprintf("localhost:%d", info.Port)} {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/login/fresh?t=%s", info.Port, nonce), nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("the page of the issued link, Host %q: %d, want 200", host, resp.StatusCode)
		}
	}
	for _, tc := range []struct{ path, host string }{
		{"/login/fresh?t=wrong", ""},
		{"/login/fresh?t=" + nonce, "evil.example"},
		{"/login/fresh/state?t=" + nonce, "evil.example"},
	} {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", info.Port, tc.path), nil)
		if tc.host != "" {
			req.Host = tc.host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with Host %q: %d, want 404", tc.path, tc.host, resp.StatusCode)
		}
	}
	if acc := call(map[string]any{"action": "list"}).Accounts; acc[0].Status != "needs_link" {
		t.Errorf("a request started the pairing: %+v", acc)
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
