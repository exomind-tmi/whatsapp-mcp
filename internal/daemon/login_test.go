package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

const goodNonce = "Zm9vYmFyYmF6cXV4MTIzNA" // 22 characters, as a real one

// fakeLogin is the login backend: one live nonce for one nick, as the
// Manager's, and the state it shows. The nonce rules themselves are tested
// in wa; here is how the HTTP side maps them.
type fakeLogin struct {
	mu      sync.Mutex
	state   wa.LoginState
	err     error // returned for a valid nonce
	expired bool
	calls   []string
}

func (f *fakeLogin) valid(nick, nonce string) bool {
	return nick == "personal" && nonce == goodNonce && !f.expired
}

func (f *fakeLogin) ValidLogin(nick, nonce string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "valid "+nick+"?"+nonce)
	return f.valid(nick, nonce)
}

func (f *fakeLogin) Login(_ context.Context, nick, nonce string) (wa.LoginState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "login "+nick+"?"+nonce)
	if !f.valid(nick, nonce) {
		return wa.LoginState{}, wa.ErrNoLogin
	}
	return f.state, f.err
}

func (f *fakeLogin) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeLogin) set(s wa.LoginState, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.err = s, err
}

// syncBuffer is a log destination the server's goroutines may write while
// the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type loginServer struct {
	*httptest.Server
	port int
	be   *fakeLogin
	logs *syncBuffer
}

// newLoginServer serves the daemon's mux with the fake backend and logs
// everything, the server's own errors included, to a buffer.
func newLoginServer(t *testing.T) *loginServer {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	be := &fakeLogin{state: wa.LoginState{State: "starting"}}
	srv := httptest.NewUnstartedServer(nil)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	srv.Config.Handler = newMux(muxDeps{
		version: "v1.2.3",
		token:   "secret",
		server:  tools.NewServer("v1.2.3", tools.Deps{WA: &toolstest.WA{}}),
		stop:    make(chan struct{}),
		log:     log,
		login:   be,
		port:    port,
	})
	srv.Config.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelError)
	srv.Start()
	t.Cleanup(srv.Close)
	return &loginServer{Server: srv, port: port, be: be, logs: logs}
}

type reply struct {
	code   int
	header http.Header
	body   string
}

// get sends method to path with the given Host header, "" for the server's
// own address, and no credentials.
func (s *loginServer) do(t *testing.T, method, path, host string) reply {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, string(b)}
}

func (s *loginServer) get(t *testing.T, path string) reply { return s.do(t, "GET", path, "") }

const pagePath = "/login/personal?t=" + goodNonce
const statePath = "/login/personal/state?t=" + goodNonce

// wantCSP is the policy, written out: a policy compared with the code's own
// variable would pass when weakened. Only the hashes of the two inline blocks
// are left to the page (TestLoginPage checks them against it).
const wantCSP = "default-src 'none'; img-src data:; connect-src 'self'; style-src HASH; script-src HASH; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

var cspHashRe = regexp.MustCompile(`'sha256-[A-Za-z0-9+/]{43}='`)

// requireLoginHeaders checks what every answer under /login/ carries, and
// what none does.
func requireLoginHeaders(t *testing.T, what string, r reply) {
	t.Helper()
	if got := cspHashRe.ReplaceAllString(r.header.Get("Content-Security-Policy"), "HASH"); got != wantCSP {
		t.Errorf("%s: Content-Security-Policy = %q, want %q", what, got, wantCSP)
	}
	for k, want := range map[string]string{
		"Cache-Control":                "no-store",
		"Referrer-Policy":              "no-referrer",
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", what, k, got, want)
		}
	}
	for k := range r.header {
		if k == "Set-Cookie" || strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("%s: header %s must not be set", what, k)
		}
	}
}

func TestLoginPage(t *testing.T) {
	s := newLoginServer(t)
	r := s.get(t, pagePath)
	if r.code != 200 || r.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("page = %d %q", r.code, r.header.Get("Content-Type"))
	}
	requireLoginHeaders(t, "page", r)
	// Fetching the page starts nothing, however often: a link preview or a
	// prefetch must not use the link up. Only the poll starts the pairing.
	s.get(t, pagePath)
	s.get(t, pagePath)
	if got := s.be.called(); !slices.Equal(got, slices.Repeat([]string{"valid personal?" + goodNonce}, 3)) {
		t.Errorf("fetching the page called %v: it may only check the nonce", got)
	}

	// Self-contained: nothing is fetched from anywhere, and nothing runs but
	// the two inline blocks the CSP admits by their hash.
	for _, bad := range []string{"http://", "https://", "//cdn", "<link", "@import", "url(", "<iframe", "<form", "document.cookie", "localStorage", "eval(", "innerHTML"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("the page contains %q", bad)
		}
	}
	if m := regexp.MustCompile(`(?i)\s(on[a-z]+|style|href)=`).FindString(r.body); m != "" {
		t.Errorf("the page has an inline handler, style or link: %q", m)
	}
	for tag, directive := range map[string]string{"style": "style-src", "script": "script-src"} {
		blocks := regexp.MustCompile(`(?s)<`+tag+`>(.*?)</`+tag+`>`).FindAllStringSubmatch(r.body, -1)
		if len(blocks) != 1 {
			t.Fatalf("%d <%s> blocks, want 1", len(blocks), tag)
		}
		sum := sha256.Sum256([]byte(blocks[0][1]))
		want := directive + " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "';"
		if !strings.Contains(r.header.Get("Content-Security-Policy"), want) {
			t.Errorf("the CSP lacks %q for the inline %s", want, tag)
		}
	}
	// It polls the state of its own page, carrying the query, and says what to
	// do when that fails, with the pairing's own hint kept.
	for _, want := range []string{`location.pathname + "/state" + location.search`, "add again", "data:image/png;base64,",
		`s.hint ? s.hint + " " + again : again`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

func TestLoginState(t *testing.T) {
	s := newLoginServer(t)
	defer func() {
		if got := s.be.called(); len(got) == 0 || !strings.HasPrefix(got[0], "login personal?") {
			t.Errorf("the poll did not call Login: %v", got)
		}
	}()
	for _, tc := range []struct {
		name  string
		state wa.LoginState
		want  string
	}{
		{"starting", wa.LoginState{State: "starting"}, `{"state":"starting"}`},
		{"paired", wa.LoginState{State: "paired", Hint: "wait"}, `{"state":"paired","hint":"wait"}`},
		{"done", wa.LoginState{State: "done"}, `{"state":"done"}`},
		{"failed", wa.LoginState{State: "failed", Reason: "QR expired, call add again"}, `{"state":"failed","reason":"QR expired, call add again"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.be.set(tc.state, nil)
			r := s.get(t, statePath)
			if r.code != 200 || r.header.Get("Content-Type") != "application/json" || strings.TrimSpace(r.body) != tc.want {
				t.Errorf("state = %d %q %q, want %s", r.code, r.header.Get("Content-Type"), r.body, tc.want)
			}
			requireLoginHeaders(t, "state", r)
		})
	}

	// A code comes as a base64 PNG, not a URL: the image of that code.
	s.be.set(wa.LoginState{State: "code", Code: waCode, Hint: "update WhatsApp"}, nil)
	r := s.get(t, statePath)
	var got struct {
		State  string `json:"state"`
		QRPNG  string `json:"qr_png"`
		Reason string `json:"reason"`
		Hint   string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(r.body), &got); err != nil || r.code != 200 {
		t.Fatalf("state = %d %q: %v", r.code, r.body, err)
	}
	if got.State != "code" || got.Reason != "" || got.Hint != "update WhatsApp" {
		t.Errorf("state %+v", got)
	}
	if strings.Contains(r.body, "data:") || strings.Contains(r.body, "http") || strings.Contains(r.body, waCode) {
		t.Error("the state carries a URL or the code's text, not just the image")
	}
	raw, err := base64.StdEncoding.DecodeString(got.QRPNG)
	if err != nil || len(raw) == 0 {
		t.Fatalf("qr_png is not base64: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) || http.DetectContentType(raw) != "image/png" {
		t.Error("qr_png is not a PNG")
	}
	img := decodePNG(t, raw)
	if img.Bounds().Empty() {
		t.Error("the image is empty")
	}
	want, _ := qrPNG(waCode)
	if !bytes.Equal(raw, want) {
		t.Error("the image is not the code's")
	}
}

// TestLoginRefusals: whatever is wrong, the answer is one and the same 404:
// no oracle tells a wrong nonce from an unknown nick or an expired link.
func TestLoginRefusals(t *testing.T) {
	s := newLoginServer(t)
	port := strconv.Itoa(s.port)
	s.be.set(wa.LoginState{State: "code", Code: waCode}, nil)

	want := s.get(t, "/login/personal?t=wrong")
	if want.code != 404 || want.body != "404 page not found\n" {
		t.Fatalf("a wrong nonce: %d %q", want.code, want.body)
	}
	requireLoginHeaders(t, "404", want)

	type request struct{ name, path, host string }
	var reqs []request
	for _, p := range []string{pagePath, statePath} {
		for _, h := range []string{"evil.example", "evil.example:" + port, "127.0.0.1", "localhost", "127.0.0.1:1", "localhost:" + port + "0",
			"127.0.0.1:" + port + ".evil.example", "[::1]:" + port, "0.0.0.0:" + port, "127.0.0.2:" + port, "localhost.evil.example:" + port} {
			reqs = append(reqs, request{"host " + h + " " + p, p, h})
		}
	}
	for _, p := range []string{
		"/login/personal", "/login/personal?t=", "/login/personal?t=" + goodNonce + "x", "/login/personal?t=" + goodNonce[1:],
		"/login/personal?t=" + strings.ToUpper(goodNonce), "/login/personal?T=" + goodNonce, "/login/personal?u=" + goodNonce,
		"/login/business?t=" + goodNonce, "/login/Personal?t=" + goodNonce, "/login/pers%20onal?t=" + goodNonce,
		"/login/personal/state", "/login/personal/state?t=wrong", "/login/business/state?t=" + goodNonce,
		"/login/personal/stat?t=" + goodNonce, "/login/personal/state/?t=" + goodNonce, "/login/personal/state/x?t=" + goodNonce,
		"/login/personal/?t=" + goodNonce, "/login/?t=" + goodNonce, "/login//state?t=" + goodNonce,
		// The same path, escaped: a nick has no character that needs it.
		"/login/%70ersonal?t=" + goodNonce, "/login/personal%2fstate?t=" + goodNonce, "/login/personal/%73tate?t=" + goodNonce,
	} {
		reqs = append(reqs, request{p, p, ""})
	}
	for _, rq := range reqs {
		r := s.do(t, "GET", rq.path, rq.host)
		// Go's mux may redirect an unclean path; that tells nothing either, as
		// the redirect target is refused the same way.
		if r.code != 404 || r.body != want.body {
			t.Errorf("%s: %d %q, want the one 404", rq.name, r.code, r.body)
		}
		if r.code == 404 {
			requireLoginHeaders(t, rq.name, r)
		}
	}
	// The valid nonce, now expired.
	s.be.expired = true
	for _, p := range []string{pagePath, statePath} {
		if r := s.get(t, p); r.code != 404 || r.body != want.body {
			t.Errorf("expired %s: %d %q", p, r.code, r.body)
		}
	}
}

// TestLoginAbsoluteForm: a request line that names the host makes Go take it
// for the Host, whatever the header says; a rebinding page cannot send one,
// but it is refused all the same.
func TestLoginAbsoluteForm(t *testing.T) {
	s := newLoginServer(t)
	for _, p := range []string{pagePath, statePath} {
		conn, err := net.Dial("tcp", s.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET http://127.0.0.1:%d%s HTTP/1.1\r\nHost: evil.example\r\nConnection: close\r\n\r\n", s.port, p)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != 404 {
			t.Errorf("absolute-form %s: %d, want 404", p, resp.StatusCode)
		}
	}
	if got := s.be.called(); len(got) != 0 {
		t.Errorf("the backend was asked: %v", got)
	}
}

func TestLoginWrongHostNeverReachesTheBackend(t *testing.T) {
	s := newLoginServer(t)
	for _, h := range []string{"evil.example", "evil.example:" + strconv.Itoa(s.port), "127.0.0.1"} {
		s.do(t, "GET", pagePath, h)
		s.do(t, "GET", statePath, h)
	}
	if got := s.be.called(); len(got) != 0 {
		t.Errorf("the backend was asked: %v", got)
	}
	// Both of the daemon's own names work, in any case.
	for _, h := range []string{"127.0.0.1:" + strconv.Itoa(s.port), "localhost:" + strconv.Itoa(s.port), "LocalHost:" + strconv.Itoa(s.port)} {
		if r := s.do(t, "GET", statePath, h); r.code != 200 {
			t.Errorf("Host %q: %d", h, r.code)
		}
	}
}

func TestLoginOnlyGet(t *testing.T) {
	s := newLoginServer(t)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"} {
		for _, p := range []string{pagePath, statePath, "/login/personal?t=wrong", "/login/personal/state?t=wrong"} {
			r := s.do(t, method, p, "")
			if r.code != http.StatusMethodNotAllowed || r.header.Get("Allow") != "GET" {
				t.Errorf("%s %s = %d, Allow %q; want 405 and GET", method, p, r.code, r.header.Get("Allow"))
			}
			requireLoginHeaders(t, method+" "+p, r)
		}
	}
	if got := s.be.called(); len(got) != 0 {
		t.Errorf("a method other than GET reached the backend: %v", got)
	}
	// A cross-origin preflight is not answered with CORS headers.
	if r := s.do(t, "OPTIONS", statePath, ""); r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("a CORS header on a preflight")
	}
}

func TestLoginBackendError(t *testing.T) {
	s := newLoginServer(t)
	s.be.set(wa.LoginState{}, errors.New("boom"))
	r := s.get(t, statePath)
	if r.code != 500 || r.body != "internal error\n" {
		t.Errorf("%d %q", r.code, r.body)
	}
	requireLoginHeaders(t, statePath, r)
	if r := s.get(t, pagePath); r.code != 200 { // the page does not call Login
		t.Errorf("the page with a failing backend: %d", r.code)
	}
	if !strings.Contains(s.logs.String(), "boom") {
		t.Error("a failure of the backend is not logged")
	}
	s.be.set(wa.LoginState{State: "code", Code: strings.Repeat("a", 8000)}, nil) // cannot be drawn
	if r := s.get(t, statePath); r.code != 500 || strings.Contains(r.body, "aaaa") {
		t.Errorf("an undrawable code: %d %q", r.code, r.body)
	}
	if !strings.Contains(s.logs.String(), "draw the QR code") || strings.Contains(s.logs.String(), "aaaa") {
		t.Errorf("the failure is not logged, or the code is:\n%s", s.logs.String())
	}
}

// TestLoginKeepsTheNonceOutOfTheLog: no request, good or bad, leaves the
// nonce or the query in the log (plan 10).
func TestLoginKeepsTheNonceOutOfTheLog(t *testing.T) {
	s := newLoginServer(t)
	const other = "T3RoZXJOb25jZUFiQ2RFZg"
	s.be.set(wa.LoginState{State: "code", Code: waCode}, nil)
	for _, p := range []string{pagePath, statePath, "/login/personal?t=" + other, "/login/nobody/state?t=" + other} {
		s.get(t, p)
	}
	s.do(t, "POST", pagePath, "")
	s.do(t, "GET", pagePath, "evil.example")
	s.be.set(wa.LoginState{}, errors.New("boom"))
	s.get(t, statePath)
	s.Close()

	log := s.logs.String()
	for _, secret := range []string{goodNonce, other, "?t=", "&t=", waCode, "/login/"} {
		if strings.Contains(log, secret) {
			t.Errorf("the log holds %q:\n%s", secret, log)
		}
	}
}

// TestLoginNeedsNoBearer: the browser has no token; the nonce is the
// capability. /mcp and /admin/stop keep requiring it.
func TestLoginNeedsNoBearer(t *testing.T) {
	s := newLoginServer(t)
	s.be.set(wa.LoginState{State: "starting"}, nil)
	for _, p := range []string{pagePath, statePath} {
		if r := s.get(t, p); r.code != 200 { // s.do sends no Authorization
			t.Errorf("%s without a token: %d", p, r.code)
		}
	}
	for _, p := range []string{"/mcp", "/admin/stop"} {
		req, _ := http.NewRequest("POST", s.URL+p, strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d, want 401", p, resp.StatusCode)
		}
	}
}
