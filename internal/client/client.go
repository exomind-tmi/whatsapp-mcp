// Package client talks to a running daemon over its loopback HTTP API. The
// shim and the status/stop subcommands share it.
package client

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/proc"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
)

// CallTimeout outlives the slowest tool (download 120 s + media retry 45 s).
const CallTimeout = 200 * time.Second

// ErrNotRunning means there is no daemon answering at the daemon.json port.
var ErrNotRunning = errors.New("daemon is not running")

func baseURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// Health is the /healthz answer. Proof is home.TokenProof for the nonce the
// request carried.
type Health struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	Proof   string `json:"proof,omitempty"`
}

// Probe reads daemon.json and checks /healthz. The returned Version is the
// one the live daemon reports. The port counts as ours only when the answer
// proves knowledge of the token: daemon.json outlives a killed daemon, and
// its port may since belong to any process, which must never get the token.
func Probe(ctx context.Context, h home.Home) (home.DaemonInfo, error) {
	info, err := h.ReadDaemonInfo()
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	token, err := h.ReadToken()
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	nonce := rand.Text()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(info.Port)+"/healthz?nonce="+nonce, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	var hz Health
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&hz) != nil || hz.Version == "" {
		return info, fmt.Errorf("%w: unexpected /healthz answer on port %d (HTTP %d)", ErrNotRunning, info.Port, resp.StatusCode)
	}
	if hz.PID != info.PID || !hmac.Equal([]byte(hz.Proof), []byte(home.TokenProof(token, nonce))) {
		return info, fmt.Errorf("%w: port %d is not served by the daemon in daemon.json", ErrNotRunning, info.Port)
	}
	info.Version = hz.Version
	return info, nil
}

// HTTPClient returns a client that authenticates to the daemon.
func HTTPClient(h home.Home) *http.Client {
	return &http.Client{Timeout: CallTimeout, Transport: &bearer{h: h, base: http.DefaultTransport}}
}

// Connect opens an MCP session to the daemon at port.
func Connect(ctx context.Context, h home.Home, port int, version string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "whatsapp-mcp-shim", Version: version}, nil)
	return c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             baseURL(port) + "/mcp",
		HTTPClient:           HTTPClient(h),
		DisableStandaloneSSE: true, // the daemon is stateless: no server-initiated messages
	}, &mcp.ClientSessionOptions{ProtocolVersion: tools.ProtocolVersion})
}

// Stop asks the running daemon to shut down and waits until it is gone.
func Stop(ctx context.Context, h home.Home) error {
	info, err := Probe(ctx, h)
	if err != nil {
		return err
	}
	return StopDaemon(ctx, h, info)
}

// StopDaemon stops the daemon described by info (not whichever daemon runs
// by the time the request is sent) and waits until it is gone.
func StopDaemon(ctx context.Context, h home.Home, info home.DaemonInfo) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL(info.Port)+"/admin/stop", nil)
	resp, err := HTTPClient(h).Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("daemon refused to stop: HTTP %d", resp.StatusCode)
	}
	return waitGone(ctx, h, info.PID, 40*time.Second)
}

// waitGone polls until the daemon with pid is gone: daemon.json vanished or
// belongs to a successor, or the lock is free. The lock alone settles it when
// daemon.json could not be deleted (a concurrent reader on Windows).
func waitGone(ctx context.Context, h home.Home, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := h.ReadDaemonInfo()
		if errors.Is(err, os.ErrNotExist) || (err == nil && info.PID != pid) || lockFree(h) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not stop within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// lockFree reports whether no daemon holds the lock. Holding it for a moment
// is harmless: a starting daemon retries for 5 s.
func lockFree(h home.Home) bool {
	l, err := proc.TryLock(h.LockFile())
	if err != nil {
		return false
	}
	l.Unlock()
	return true
}
