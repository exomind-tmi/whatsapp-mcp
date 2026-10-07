package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
	"github.com/exomind-tmi/whatsapp-mcp/internal/daemon"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/qr"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// startDaemon runs the real daemon in-process on a temp home, with no WhatsApp
// to reach.
func startDaemon(t *testing.T, version string) home.Home {
	t.Helper()
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, h, version, daemon.WithoutWhatsApp()) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := h.ReadDaemonInfo(); err == nil {
			testutil.WaitForLog(t, h.Log("daemon.log"), "never reaches WhatsApp") // it reaches no WhatsApp
			return h
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start")
		}
	}
}

// shimSession connects a client to a shim server. Its own WA fails the test
// if the shim ever runs a tool locally instead of forwarding it.
func shimSession(t *testing.T, h home.Home, version string) *mcp.ClientSession {
	t.Helper()
	f := newForwarder(h, version, slog.New(slog.DiscardHandler))
	t.Cleanup(f.close)
	local, localArchive := &toolstest.WA{}, &toolstest.Archive{}
	t.Cleanup(func() {
		if calls := local.Calls(); len(calls) != 0 {
			t.Errorf("shim ran tools locally: %v", calls)
		}
		if localArchive.Asked() {
			t.Errorf("shim read an archive locally: %+v", localArchive.Queries())
		}
	})
	s := tools.NewServer(version, tools.Deps{WA: local, Archive: localArchive})
	s.AddReceivingMiddleware(f.intercept)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res, tools.ResultText(res)
}

func TestShimForwardsToDaemon(t *testing.T) {
	h := startDaemon(t, "v0.1.0")
	cs := shimSession(t, h, "v0.1.0")

	// tools/list is answered by the shim itself.
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(list.Tools) != 6 {
		t.Fatalf("tools/list: %v %+v", err, list)
	}

	res, text := callText(t, cs, "manage-accounts", map[string]any{"action": "list"})
	if res.IsError || !strings.Contains(text, `"accounts":[]`) {
		t.Fatalf("list via daemon: isError=%v %s", res.IsError, text)
	}
	if res.StructuredContent == nil {
		t.Error("structuredContent lost in forwarding")
	}

	// add answers with a link; the page is not opened.
	res, text = callText(t, cs, "manage-accounts", map[string]any{"action": "add", "account_id": "personal"})
	if res.IsError || !strings.Contains(text, `"login_url":"http://127.0.0.1:`) {
		t.Fatalf("add via daemon: isError=%v %s", res.IsError, text)
	}
	// remove-account asks first, then forgets the account the add made, and a
	// second removal is refused: which is also how an isError result is seen to
	// survive the forwarding.
	res, text = callText(t, cs, "remove-account", map[string]any{"account_id": "personal"})
	if res.IsError || !strings.Contains(text, `"status":"needs_confirmation"`) {
		t.Fatalf("remove-account without confirm via daemon: isError=%v %s", res.IsError, text)
	}
	res, text = callText(t, cs, "remove-account", map[string]any{"account_id": "personal", "confirm": "personal"})
	if res.IsError || !strings.Contains(text, `"status":"removed"`) {
		t.Fatalf("remove-account via daemon: isError=%v %s", res.IsError, text)
	}
	res, text = callText(t, cs, "remove-account", map[string]any{"account_id": "personal", "confirm": "personal"})
	if !res.IsError || !strings.Contains(text, `no account "personal"`) {
		t.Fatalf("remove-account: isError lost in forwarding: %v %s", res.IsError, text)
	}
}

// readCalls are a valid call of each read tool.
var readCalls = map[string]map[string]any{
	"list-chats":          {},
	"get-messages":        {"chat": "+7 999 123-45-67"},
	"get-message-context": {"chat": "+7 999 123-45-67", "message_id": "M1"},
	"search-messages":     {"query": "hello"},
}

// TestShimForwardsTheReadTools: each of the read tools reaches the daemon, which
// reads its archive, and what it answers (the structured content, the notes, the
// isError of a message that is not there) comes back as it was.
func TestShimForwardsTheReadTools(t *testing.T) {
	h := startDaemon(t, "v0.1.0")
	cs := shimSession(t, h, "v0.1.0") // fails the test if the shim reads anything itself
	if res, text := callText(t, cs, "manage-accounts", map[string]any{"action": "add", "account_id": "personal"}); res.IsError {
		t.Fatal(text)
	}
	for name, args := range readCalls {
		res, text := callText(t, cs, name, args)
		if name == "get-message-context" {
			if !res.IsError || !strings.Contains(text, "no such message") {
				t.Errorf("%s: isError=%v %s, want the daemon's answer for a message that is not there", name, res.IsError, text)
			}
			continue
		}
		if res.IsError || res.StructuredContent == nil || !strings.Contains(text, "account personal is linking") {
			t.Errorf("%s via daemon: isError=%v structured=%v %s", name, res.IsError, res.StructuredContent != nil, text)
		}
	}
	if res, text := callText(t, cs, "search-messages", map[string]any{"query": "ab"}); !res.IsError || !strings.Contains(text, "query too short") {
		t.Errorf("a query that is too short: isError=%v %s", res.IsError, text)
	}
}

func TestShimForwardsEveryListedTool(t *testing.T) {
	h := startDaemon(t, "v0.1.0")
	cs := shimSession(t, h, "v0.1.0")
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		// shimSession fails the test if any call is handled by the shim itself.
		for _, args := range []map[string]any{
			{"action": "list"}, {"action": "add", "account_id": "personal"}, {"action": "remove", "account_id": "personal"}, readCalls[tool.Name],
		} {
			cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name, Arguments: args})
		}
	}
}

func TestShimReportsOutdatedToolList(t *testing.T) {
	h := startDaemon(t, "v0.1.0")
	cs := shimSession(t, h, "v0.1.0")
	f := newForwarder(h, "v0.1.0", slog.New(slog.DiscardHandler))
	defer f.close()
	// A tool this shim knows but the (newer) daemon no longer has.
	f.known = func(string) bool { return true }
	res, err := f.call(context.Background(), &mcp.CallToolParamsRaw{Name: "tool-the-daemon-dropped"})
	if err != nil || !res.IsError || !strings.Contains(tools.ResultText(res), "restart the session") {
		t.Fatalf("dropped tool: %v %+v", err, res)
	}
	// A name nobody knows gets the daemon's usual error.
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "no-such-tool"}); err == nil ||
		!strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unknown tool: %v", err)
	}
}

// fakeDaemon answers /healthz like the daemon and serves the real tools, over
// w, on /mcp, but breaks the connection on the first `breaks` tools/call
// requests. It returns the number of tools/call requests that reached it.
func fakeDaemon(t *testing.T, version string, breaks int32, w tools.WA) (home.Home, *atomic.Int32) {
	t.Helper()
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	const token = "test-token"
	os.WriteFile(h.TokenFile(), []byte(token), 0o600)

	server := tools.NewServer(version, tools.Deps{WA: w, Archive: &toolstest.Archive{}})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(client.Health{Version: version, PID: os.Getpid(),
			Proof: home.TokenProof(token, r.URL.Query().Get("nonce"))})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"tools/call"`)) && calls.Add(1) <= breaks {
			panic(http.ErrAbortHandler) // drops the connection without an answer
		}
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if err := h.WriteDaemonInfo(home.DaemonInfo{PID: os.Getpid(), Port: port, Version: version}); err != nil {
		t.Fatal(err)
	}
	return h, &calls
}

func TestTransportErrorRetry(t *testing.T) {
	list := &mcp.CallToolParamsRaw{Name: "manage-accounts", Arguments: json.RawMessage(`{"action":"list"}`)}
	for _, tc := range []struct {
		name      string
		readOnly  bool
		breaks    int32
		wantCalls int32
		wantOK    bool
		wantMsg   string // what a failed call is told with
	}{
		{"write tool is never repeated", false, 1, 1, false, msgUnconfirmed},
		{"read tool is repeated once", true, 1, 2, true, ""},
		// It reads, so nothing may have taken effect: the agent is not told to check.
		{"read tool is repeated only once", true, 2, 2, false, msgReadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := fakeDaemon(t, "v0.1.0", tc.breaks, &toolstest.WA{})
			f := newForwarder(h, "v0.1.0", slog.New(slog.DiscardHandler))
			defer f.close()
			f.readOnly = func(string) bool { return tc.readOnly }
			res, err := f.call(context.Background(), list)
			if err != nil {
				t.Fatal(err)
			}
			text := tools.ResultText(res)
			if res.IsError == tc.wantOK || (!tc.wantOK && !strings.HasPrefix(text, tc.wantMsg)) {
				t.Errorf("isError=%v %s", res.IsError, text)
			}
			if tc.wantMsg == msgReadFailed && (strings.Contains(text, "for a send") || strings.Contains(text, "took effect")) {
				t.Errorf("a read that failed twice is told in the words of a send: %s", text)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("tools/call reached the daemon %d times, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestShimRepeatsOnlyWhatReads: the shim repeats a call that a broken connection
// cut off once, and only for a tool that reads (tools.ReadOnly, which it uses
// itself): a read tool is repeated, and one that changes anything is not, for it
// may have taken effect.
func TestShimRepeatsOnlyWhatReads(t *testing.T) {
	accounts := &toolstest.WA{Accs: []wa.AccountInfo{{Nick: "personal", Status: wa.StatusConnected}}}
	for _, tc := range []struct {
		tool      string
		args      map[string]any
		wantCalls int32
		wantOK    bool
	}{
		{"list-chats", readCalls["list-chats"], 2, true},
		{"get-messages", readCalls["get-messages"], 2, true},
		{"get-message-context", map[string]any{"chat": "+7 999 123-45-67", "message_id": "M1"}, 2, false}, // the answer is an error, of the daemon
		{"search-messages", readCalls["search-messages"], 2, true},
		{"manage-accounts", map[string]any{"action": "list"}, 1, false},
		{"remove-account", map[string]any{"account_id": "personal", "confirm": "personal"}, 1, false},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			h, calls := fakeDaemon(t, "v0.1.0", 1, accounts)
			f := newForwarder(h, "v0.1.0", slog.New(slog.DiscardHandler)) // with the real ReadOnly
			defer f.close()
			raw, _ := json.Marshal(tc.args)
			res, err := f.call(context.Background(), &mcp.CallToolParamsRaw{Name: tc.tool, Arguments: raw})
			if err != nil {
				t.Fatal(err)
			}
			text := tools.ResultText(res)
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("tools/call reached the daemon %d times, want %d", got, tc.wantCalls)
			}
			if unconfirmed := strings.HasPrefix(text, msgUnconfirmed); unconfirmed != (tc.wantCalls == 1) {
				t.Errorf("unconfirmed=%v: isError=%v %s", unconfirmed, res.IsError, text)
			}
			if tc.wantOK && res.IsError {
				t.Errorf("the repeated call failed: %s", text)
			}
		})
	}
}

// TestAReadThatFailsTwiceIsNotToldAsASend: a read tool that the connection cut off
// twice has changed nothing, so the agent is told to repeat it and not, as for a send,
// to check whether it took effect.
func TestAReadThatFailsTwiceIsNotToldAsASend(t *testing.T) {
	accounts := &toolstest.WA{Accs: []wa.AccountInfo{{Nick: "personal", Status: wa.StatusConnected}}}
	for name, args := range readCalls {
		t.Run(name, func(t *testing.T) {
			h, calls := fakeDaemon(t, "v0.1.0", 2, accounts)
			f := newForwarder(h, "v0.1.0", slog.New(slog.DiscardHandler)) // with the real ReadOnly
			defer f.close()
			raw, _ := json.Marshal(args)
			res, err := f.call(context.Background(), &mcp.CallToolParamsRaw{Name: name, Arguments: raw})
			if err != nil {
				t.Fatal(err)
			}
			text := tools.ResultText(res)
			if calls.Load() != 2 || !res.IsError {
				t.Fatalf("calls %d, isError %v: %s", calls.Load(), res.IsError, text)
			}
			if !strings.HasPrefix(text, msgReadFailed) || strings.Contains(text, "for a send") || strings.Contains(text, "took effect") {
				t.Errorf("a read that failed twice is told in the words of a send: %s", text)
			}
			if !strings.Contains(text, "nothing was changed") {
				t.Errorf("the agent is not told that nothing was changed: %s", text)
			}
		})
	}
}

// TestShimForwardsTheQRImage: the result of add with qr_image, the usual output
// as text and as structured content and the QR as an image beside them, reaches
// the client as the daemon made it, the image byte for byte; and the argument
// reaches the daemon.
func TestShimForwardsTheQRImage(t *testing.T) {
	data, err := qr.PNG("2@aGVsbG8sIHdvcmxk,c2VjcmV0,a2V5,YWR2", qr.Chat)
	if err != nil {
		t.Fatal(err)
	}
	daemonWA := &toolstest.WA{Ticket: wa.LinkTicket{QRPNG: data, ExpiresAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}}
	h, _ := fakeDaemon(t, "v0.1.0", 0, daemonWA)
	cs := shimSession(t, h, "v0.1.0")

	// What the daemon itself answers with, for the shim's answer to be compared to:
	// over a WA of its own, so that daemonWA records only what the shim sent.
	direct := daemonSession(t, &toolstest.WA{Ticket: daemonWA.Ticket})
	args := map[string]any{"action": "add", "account_id": "personal", "qr_image": true}
	want, err := direct.CallTool(context.Background(), &mcp.CallToolParams{Name: "manage-accounts", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	got, text := callText(t, cs, "manage-accounts", args)

	if got.IsError || len(got.Content) != 2 {
		t.Fatalf("isError=%v, %d content blocks, want the text and the image: %s", got.IsError, len(got.Content), text)
	}
	if tc, ok := got.Content[0].(*mcp.TextContent); !ok || !strings.Contains(tc.Text, `"status":"linking"`) || !strings.Contains(tc.Text, "scan the QR code in the image") {
		t.Errorf("content[0] = %#v, want the JSON text", got.Content[0])
	}
	img, ok := got.Content[1].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/png" || !bytes.Equal(img.Data, data) {
		t.Fatalf("content[1] = %#v, want the PNG of the QR code, byte for byte", got.Content[1])
	}
	if _, err := png.Decode(bytes.NewReader(img.Data)); err != nil {
		t.Errorf("the image is not a PNG: %v", err)
	}
	if got.StructuredContent == nil {
		t.Error("structuredContent lost in forwarding")
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("the shim changed the result:\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	if calls, want := daemonWA.Calls(), []toolstest.Call{{Method: "Link", Nick: "personal", QRImage: true}}; !slices.Equal(calls, want) {
		t.Errorf("the daemon's WA calls = %+v, want %+v: qr_image did not reach it", calls, want)
	}
}

// daemonSession is a client of the tools served over w with no shim between.
func daemonSession(t *testing.T, w tools.WA) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := tools.NewServer("v0.1.0", tools.Deps{WA: w, Archive: &toolstest.Archive{}}).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestShimUsesNewerDaemonAsIs(t *testing.T) {
	// An older shim works with a newer daemon without touching bin/.
	h := startDaemon(t, "v0.2.0")
	res, text := callText(t, shimSession(t, h, "v0.1.0"), "manage-accounts", map[string]any{"action": "list"})
	if res.IsError {
		t.Fatal(text)
	}
	if vs := versions(h.BinDir()); len(vs) != 0 {
		t.Fatalf("older shim installed itself: %v", vs)
	}
}
