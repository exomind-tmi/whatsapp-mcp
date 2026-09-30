package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
)

// newMux wires the daemon endpoints. /healthz is open and reveals the
// version, the pid and, for a nonce, a proof of knowing the token (never the
// token itself); everything else needs the bearer token.
func newMux(version, token string, server *mcp.Server, stop chan<- struct{}, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		hz := client.Health{Version: version, PID: os.Getpid()}
		if nonce := r.URL.Query().Get("nonce"); nonce != "" {
			hz.Proof = home.TokenProof(token, nonce)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hz)
	})

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: log})
	mux.Handle("/mcp", requireBearer(token, mcpHandler))

	var once sync.Once
	mux.Handle("POST /admin/stop", requireBearer(token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		once.Do(func() { close(stop) })
	})))
	return mux
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
