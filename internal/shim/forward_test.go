package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
	"github.com/exomind-tmi/whatsapp-mcp/internal/daemon"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
)

// startDaemon runs the real daemon in-process on a temp home.
func startDaemon(t *testing.T, version string) home.Home {
	t.Helper()
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, h, version) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := h.ReadDaemonInfo(); err == nil {
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
	local := &toolstest.WA{}
	t.Cleanup(func() {
		if calls := local.Calls(); len(calls) != 0 {
			t.Errorf("shim ran tools locally: %v", calls)
		}
	})
	s := tools.NewServer(version, tools.Deps{WA: local})
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
	if err != nil || len(list.Tools) != 1 {
		t.Fatalf("tools/list: %v %+v", err, list)
	}

	res, text := callText(t, cs, "manage-accounts", map[string]any{"action": "list"})
	if res.IsError || !strings.Contains(text, `"accounts":[]`) {
		t.Fatalf("list via daemon: isError=%v %s", res.IsError, text)
	}
	if res.StructuredContent == nil {
		t.Error("structuredContent lost in forwarding")
	}

	// add answers with a link; the page is not opened, as that would start a
	// pairing on the real network.
	res, text = callText(t, cs, "manage-accounts", map[string]any{"action": "add", "account_id": "personal"})
	if res.IsError || !strings.Contains(text, `"login_url":"http://127.0.0.1:`) {
		t.Fatalf("add via daemon: isError=%v %s", res.IsError, text)
	}
	// remove is not implemented yet, which is also how an isError result is
	// seen to survive the forwarding.
	res, text = callText(t, cs, "manage-accounts", map[string]any{"action": "remove", "account_id": "personal"})
	if !res.IsError || !strings.Contains(text, "not implemented yet") {
		t.Fatalf("remove: isError lost in forwarding: %v %s", res.IsError, text)
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
		for _, action := range []string{"list", "add", "remove"} {
			cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name,
				Arguments: map[string]any{"action": action, "account_id": "personal"}})
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

// fakeDaemon answers /healthz like the daemon and serves the real tools on
// /mcp, but breaks the connection on the first `breaks` tools/call requests.
// It returns the number of tools/call requests that reached it.
func fakeDaemon(t *testing.T, version string, breaks int32) (home.Home, *atomic.Int32) {
	t.Helper()
	h := home.Home{Dir: testutil.TempDir(t)}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	const token = "test-token"
	os.WriteFile(h.TokenFile(), []byte(token), 0o600)

	server := tools.NewServer(version, tools.Deps{WA: &toolstest.WA{}})
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
	}{
		{"write tool is never repeated", false, 1, 1, false},
		{"read tool is repeated once", true, 1, 2, true},
		{"read tool is repeated only once", true, 2, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := fakeDaemon(t, "v0.1.0", tc.breaks)
			f := newForwarder(h, "v0.1.0", slog.New(slog.DiscardHandler))
			defer f.close()
			f.readOnly = func(string) bool { return tc.readOnly }
			res, err := f.call(context.Background(), list)
			if err != nil {
				t.Fatal(err)
			}
			text := tools.ResultText(res)
			if res.IsError == tc.wantOK || (!tc.wantOK && !strings.HasPrefix(text, msgUnconfirmed)) {
				t.Errorf("isError=%v %s", res.IsError, text)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("tools/call reached the daemon %d times, want %d", got, tc.wantCalls)
			}
		})
	}
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
