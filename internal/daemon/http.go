package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
)

// muxDeps is what the endpoints need. A struct rather than parameters, so
// that a new endpoint adds a field instead of growing every call.
type muxDeps struct {
	version string
	token   string
	server  *mcp.Server
	stop    chan<- struct{} // closed once by /admin/stop
	log     *slog.Logger
	login   loginBackend
	port    int // the daemon's, which the login pages' Host header must name
}

// newMux wires the daemon endpoints. /healthz is open and reveals the
// version, the pid and, for a nonce, a proof of knowing the token (never the
// token itself); /login/ is open to the browser that holds a link's nonce;
// everything else needs the bearer token. All of it is served only to requests
// addressed to the daemon itself (onlyOwnHost).
func newMux(d muxDeps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		hz := client.Health{Version: d.version, PID: os.Getpid()}
		if nonce := r.URL.Query().Get("nonce"); nonce != "" {
			hz.Proof = home.TokenProof(d.token, nonce)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hz)
	})

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return d.server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: d.log})
	mux.Handle("/mcp", requireBearer(d.token, mcpHandler))
	mux.Handle("/login/", newLoginHandler(d.login, d.log))

	var once sync.Once
	mux.Handle("POST /admin/stop", requireBearer(d.token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		once.Do(func() { close(d.stop) })
	})))
	return onlyOwnHost(d.port, mux)
}

// onlyOwnHost serves next to the requests whose Host is the daemon's own,
// 127.0.0.1 or localhost on its port, and answers every other with the one 404.
// A Host that is not ours is a DNS rebinding attempt: a web page that names the
// daemon by its own domain, which then reads the answer, the pid, the version and
// the proof of /healthz among them. A request line that carries the host itself
// makes Go ignore the header, so it is refused too: a browser never sends one to
// an origin. The headers of the login pages go on the refusal, as on every answer
// under /login/, so that nothing tells one path from another.
func onlyOwnHost(port int, next http.Handler) http.Handler {
	hosts := [2]string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("localhost:%d", port)}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host := strings.ToLower(r.Host); r.URL.IsAbs() || (host != hosts[0] && host != hosts[1]) {
			setLoginHeaders(w.Header())
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireBearer is deliberately not the SDK's auth.RequireBearerToken, which
// is built around OAuth token verification.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
