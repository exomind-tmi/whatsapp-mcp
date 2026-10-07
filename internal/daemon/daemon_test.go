package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
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

// newMuxServer serves newMux, which answers only to its own port, so the
// listener is made first.
func newMuxServer(t *testing.T, stop chan<- struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = newMux(muxDeps{
		version: "v1.2.3",
		token:   "secret",
		server:  tools.NewServer("v1.2.3", tools.Deps{WA: &toolstest.WA{}}),
		stop:    stop,
		log:     slog.New(slog.DiscardHandler),
		port:    srv.Listener.Addr().(*net.TCPAddr).Port,
	})
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestMuxAuth(t *testing.T) {
	stop := make(chan struct{})
	srv := newMuxServer(t, stop)

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

// TestMuxOnlyOwnHost: a page that reaches the daemon under a name of its own
// (DNS rebinding) reads nothing, not the pid, the version and the proof of
// /healthz, and stops nothing even with the token; the daemon's own two names,
// which the shim and the client use, work.
func TestMuxOnlyOwnHost(t *testing.T) {
	stop := make(chan struct{})
	srv := newMuxServer(t, stop)
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
	do := func(method, path, host string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	for _, host := range []string{"evil.example", "evil.example:" + port, "127.0.0.1", "localhost", "127.0.0.2:" + port,
		"127.0.0.1:" + port + ".evil.example", "[::1]:" + port, "0.0.0.0:" + port} {
		for _, tc := range []struct{ method, path string }{
			{"GET", "/healthz?nonce=n1"}, {"POST", "/admin/stop"}, {"POST", "/mcp"}, {"GET", "/login/personal?t=x"},
		} {
			code, body := do(tc.method, tc.path, host)
			if code != http.StatusNotFound || strings.Contains(body, "v1.2.3") || strings.Contains(body, "proof") || strings.Contains(body, "pid") {
				t.Errorf("%s %s with Host %q = %d %q, want the 404 and nothing of the daemon", tc.method, tc.path, host, code, body)
			}
		}
	}
	select {
	case <-stop:
		t.Fatal("a request under a foreign Host stopped the daemon")
	default:
	}

	// A request line that names the host makes Go ignore the Host header.
	for _, target := range []string{"/healthz?nonce=n1", "/admin/stop"} {
		method := "GET"
		if target == "/admin/stop" {
			method = "POST"
		}
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "%s http://127.0.0.1:%s%s HTTP/1.1\r\nHost: evil.example\r\nAuthorization: Bearer secret\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", method, port, target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("absolute-form %s %s = %d, want 404", method, target, resp.StatusCode)
		}
	}

	for _, host := range []string{"", "127.0.0.1:" + port, "localhost:" + port, "LocalHost:" + port} {
		if code, body := do("GET", "/healthz?nonce=n1", host); code != http.StatusOK || !strings.Contains(body, home.TokenProof("secret", "n1")) {
			t.Errorf("/healthz with Host %q = %d %q", host, code, body)
		}
	}
	if code, _ := do("POST", "/admin/stop", "localhost:"+port); code != http.StatusAccepted {
		t.Errorf("/admin/stop with the daemon's own Host = %d, want 202", code)
	}
	select {
	case <-stop:
	case <-time.After(time.Second):
		t.Error("stop not signalled by a request under the daemon's own Host")
	}
}

// TestRunLifecycle runs the real daemon in-process, without WhatsApp: lock,
// token, daemon.json, a second daemon exits cleanly, and shutdown removes
// daemon.json.
func TestRunLifecycle(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h, "v0.1.0", WithoutWhatsApp()) }()

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
	// The daemon of a test reaches no WhatsApp: its version check is the offline one.
	testutil.WaitForLog(t, h.Log("daemon.log"), "never reaches WhatsApp")

	// A second daemon loses the lock after its retry window and exits 0,
	// leaving the winner's log alone even when it is due for rotation.
	logPath := h.Log("daemon.log")
	lf, _ := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	lf.Write(make([]byte, 10<<20+1))
	lf.Close()
	if err := Run(context.Background(), h, "v0.1.0", WithoutWhatsApp()); err != nil {
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

// runDaemon runs the real daemon in-process, without WhatsApp, until the test ends,
// and returns what it wrote to daemon.json and a context that the test may use.
func runDaemon(t *testing.T, h home.Home) (home.DaemonInfo, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h, "v0.1.0", WithoutWhatsApp()) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
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
	testutil.WaitForLog(t, h.Log("daemon.log"), "never reaches WhatsApp") // it reaches no WhatsApp
	return info, ctx
}

// TestRunIssuesLoginLink: the add tool of the real daemon, which reaches no
// WhatsApp, hands out a link to its own port, and the mux answers the link's
// path with no token. The page opens without starting a pairing; its poll does.
func TestRunIssuesLoginLink(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	info, ctx := runDaemon(t, h)

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
	// The pairing starts when the page is polled, but list says what the account
	// waits for, and does not send the agent to link it again.
	const waiting = "a login link was issued and not opened yet"
	listed := call(map[string]any{"action": "list"})
	if acc := listed.Accounts; len(acc) != 1 || acc[0].AccountID != "fresh" || acc[0].Status != "linking" || acc[0].Reason != waiting {
		t.Errorf("accounts after add: %+v", acc)
	}
	if !strings.Contains(listed.NextStep, "login_url") || strings.Contains(listed.NextStep, "re-link") {
		t.Errorf("list's next_step after add: %q", listed.NextStep)
	}

	// Under /login/ the mux asks for no token but a valid nonce on our own
	// Host, with which the page opens (and starts no pairing: only its poll
	// does, below); neither a wrong nonce nor another Host gets in.
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
	if acc := call(map[string]any{"action": "list"}).Accounts; acc[0].Reason != waiting {
		t.Errorf("a request started the pairing: %+v", acc)
	}

	// The poll starts it; with no WhatsApp the QR code never comes.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/login/fresh/state?t=%s", info.Port, nonce))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"state":"starting"`) {
		t.Errorf("the poll = %d %s, want the pairing started", resp.StatusCode, body)
	}
	if acc := call(map[string]any{"action": "list"}).Accounts; acc[0].Status != "linking" || acc[0].Reason != "" {
		t.Errorf("after the poll: %+v, want the pairing's linking", acc)
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
	if err := Run(context.Background(), h, "v0.1.0", WithoutWhatsApp()); err == nil || !strings.Contains(err.Error(), "store.db") {
		t.Fatalf("Run = %v, want store.db's error", err)
	}
	if _, err := os.Stat(h.DaemonJSON()); !os.IsNotExist(err) {
		t.Fatalf("daemon.json left behind: %v", err)
	}
}

// seedArchive makes an archive.db in the home with an account that has a chat with
// Bob in it: three messages, the last of which Bob deleted. The daemon opens it as
// it finds it, as it does after a restart.
func seedArchive(t *testing.T, h home.Home) {
	t.Helper()
	ctx := context.Background()
	db, err := archive.Open(h.ArchiveDB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AddAccount(ctx, "personal"); err != nil {
		t.Fatal(err)
	}
	const bob = "70000000100@s.whatsapp.net"
	at := time.Unix(1_700_000_000, 0)
	err = db.Tx(ctx, func(tx *archive.Tx) error {
		for i, text := range []string{"rent for the garage is due", "I will pay the rent tomorrow", "never mind the rent"} {
			sender := bob
			if i == 1 {
				sender = "70000000001@s.whatsapp.net" // ours
			}
			if err := tx.Upsert(archive.Row{Account: "personal", Chat: bob, ID: fmt.Sprintf("M%d", i+1), Sender: sender, FromMe: i == 1,
				TS: at.Add(time.Duration(i) * time.Second), Text: text, Raw: []byte("raw")}); err != nil {
				return err
			}
		}
		if err := tx.Revoke(archive.Revoke{Account: "personal", Chat: bob, ID: "M3", RevokedAt: at.Add(time.Minute)}); err != nil {
			return err
		}
		return tx.TouchChat(archive.ChatUpd{Account: "personal", JID: bob, Name: "Bob", LastMessageTS: at.Add(2 * time.Second)})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunServesTheReadTools: the daemon hands the tools the archive it opened and
// the Manager it built, so that a client of /mcp reads what is in archive.db: the
// chats, the messages of one by a phone number (the Manager says which chat that
// is), the window around one, and a search by a person's name (the Manager says
// which addresses that is). The account has no device, so the answers carry a note
// that it is not connected, and still come.
func TestRunServesTheReadTools(t *testing.T) {
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	seedArchive(t, h)
	info, ctx := runDaemon(t, h)
	cs, err := client.Connect(ctx, h, info.Port, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(tool string, args map[string]any, out any) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s %v: %v %v", tool, args, err, tools.ResultText(res))
		}
		if err := json.Unmarshal([]byte(tools.ResultText(res)), out); err != nil {
			t.Fatal(err)
		}
	}
	const note = "account personal is needs_link: this is the archive up to when it was last connected; " +
		"to receive new messages: manage-accounts action=add account_id=personal — re-link, the message archive is kept"

	var chats tools.ChatsOut
	call("list-chats", nil, &chats)
	if len(chats.Chats) != 1 || chats.Chats[0].Name != "Bob" || chats.Chats[0].Phone != "+70000000100" || chats.Chats[0].Account != "personal" ||
		len(chats.Notes) != 1 || chats.Notes[0] != note {
		t.Errorf("list-chats = %+v", chats)
	}

	var page tools.MessagesOut
	call("get-messages", map[string]any{"chat": "+7 000 000 0100"}, &page)
	if len(page.Messages) != 3 || page.Messages[0].ID != "M1" || !page.Messages[2].Revoked || page.Messages[1].SenderName != "" || len(page.Notes) != 1 {
		t.Errorf("get-messages = %+v", page)
	}

	var window tools.ContextOut
	call("get-message-context", map[string]any{"chat": page.Chat, "message_id": "M2", "before": 1, "after": 1}, &window)
	if len(window.Messages) != 3 || !window.Messages[1].Target || window.Messages[1].ID != "M2" {
		t.Errorf("get-message-context = %+v", window)
	}

	var found tools.SearchOut
	call("search-messages", map[string]any{"query": "rent", "sender": "bob"}, &found)
	if len(found.Results) != 2 || found.Results[0].ID != "M3" || found.Results[1].ID != "M1" || found.Results[1].ChatName != "Bob" {
		t.Errorf("search-messages = %+v, want the messages of Bob that have the word, the newest first", found)
	}

	// What goes wrong is told to the agent, and the daemon goes on.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search-messages", Arguments: map[string]any{"query": "ab"}})
	if err != nil || !res.IsError || !strings.Contains(tools.ResultText(res), "query too short") {
		t.Errorf("a query that is too short: %v %v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get-messages", Arguments: map[string]any{"chat": "Bob"}})
	if err != nil || !res.IsError || !strings.Contains(tools.ResultText(res), "not a chat") {
		t.Errorf("a chat that is not one: %v %v", err, res)
	}
}
