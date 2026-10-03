package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// toolsDaemon answers /healthz like the daemon and serves the real tools
// over accs on /mcp. It returns the home and the daemon.json it wrote.
func toolsDaemon(t *testing.T, accs []wa.AccountInfo) (home.Home, home.DaemonInfo) {
	t.Helper()
	h := home.Home{Dir: testutil.TempDir(t)}
	const token = "test-token"
	os.WriteFile(h.TokenFile(), []byte(token), 0o600)

	server := tools.NewServer("v0.1.0", tools.Deps{WA: &toolstest.WA{Accs: accs}})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Health{Version: "v0.1.0", PID: os.Getpid(),
			Proof: home.TokenProof(token, r.URL.Query().Get("nonce"))})
	})
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	info := home.DaemonInfo{PID: os.Getpid(), Port: srv.Listener.Addr().(*net.TCPAddr).Port, Version: "v0.1.0",
		StartedAt: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)}
	if err := h.WriteDaemonInfo(info); err != nil {
		t.Fatal(err)
	}
	return h, info
}

// TestStatusOutput pins the CLI table, which decodes the manage-accounts
// output: a field renamed there must not silently blank a column here.
func TestStatusOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		accs []wa.AccountInfo
		want string
	}{
		{"accounts", []wa.AccountInfo{{Nick: "personal", Status: wa.StatusError, Reason: "temporarily banned", Phone: "+70000000000"}},
			"ACCOUNT   STATUS  PHONE         CHATS  MESSAGES  REASON\n" +
				"personal  error   +70000000000  0      0         temporarily banned\n"},
		{"none", nil, "no accounts linked\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, info := toolsDaemon(t, tc.accs)
			var out bytes.Buffer
			if err := Status(context.Background(), h, "v0.1.0", &out); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("daemon v0.1.0  pid %d  port %d  up since %s\nhome   %s\n\n",
				info.PID, info.Port, info.StartedAt.Local().Format(time.DateTime), h.Dir) + tc.want
			if out.String() != want {
				t.Fatalf("status =\n%s\nwant\n%s", out.String(), want)
			}
		})
	}
}
