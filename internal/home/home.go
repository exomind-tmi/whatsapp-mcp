// Package home owns the state directory of whatsapp-mcp. Location, layout,
// private modes, logs and atomic writes are the kit's (mcp-plugin-kit/home);
// this package adds the files that only the daemon and its shim know about.
package home

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	kit "github.com/exomind-tmi/mcp-plugin-kit/home"
)

const (
	org    = "exomind-tmi"
	server = "whatsapp-mcp"
)

var (
	// EnvHome overrides the state directory.
	EnvHome = kit.EnvHome(server)
	// EnvLogLevel selects the log level: debug, info (default), warn, error.
	EnvLogLevel = kit.EnvLogLevel(server)
)

// Home is the state directory, ~/.mcp/exomind-tmi/whatsapp-mcp by default.
type Home struct{ Dir string }

// Resolve picks the state directory from WHATSAPP_MCP_HOME or the user profile.
func Resolve() (Home, error) {
	h, err := kit.Resolve(org, server)
	return Home{h.Dir}, err
}

// Ensure creates the state directory and its fixed subdirectories, private to
// the user: the keys of the linked devices live in them.
func (h Home) Ensure() error { return h.kit().Ensure() }

func (h Home) kit() kit.Home { return kit.Home{Dir: h.Dir} }

func (h Home) LockFile() string       { return filepath.Join(h.Dir, "daemon.lock") }
func (h Home) DaemonJSON() string     { return filepath.Join(h.Dir, "daemon.json") }
func (h Home) TokenFile() string      { return filepath.Join(h.Dir, "token") }
func (h Home) ArchiveDB() string      { return filepath.Join(h.Dir, "archive.db") }
func (h Home) StoreDB() string        { return filepath.Join(h.Dir, "store.db") } // whatsmeow's device keys
func (h Home) BinDir() string         { return h.kit().BinDir() }
func (h Home) LogsDir() string        { return h.kit().LogsDir() }
func (h Home) Log(name string) string { return h.kit().Log(name) }

// ReadToken returns the daemon's bearer token.
func (h Home) ReadToken() (string, error) {
	b, err := os.ReadFile(h.TokenFile())
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("token file is empty")
	}
	return t, nil
}

// TokenProof shows that the daemon knows the token without revealing it:
// the shim sends a fresh nonce to /healthz and checks the answer before it
// trusts the port with the Bearer token.
func TokenProof(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// DaemonInfo is the content of daemon.json, written by the lock holder.
type DaemonInfo struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

func (h Home) ReadDaemonInfo() (DaemonInfo, error) {
	var d DaemonInfo
	b, err := os.ReadFile(h.DaemonJSON())
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("parse daemon.json: %w", err)
	}
	if d.Port <= 0 {
		return d, errors.New("daemon.json has no port")
	}
	return d, nil
}

func (h Home) WriteDaemonInfo(d DaemonInfo) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(h.DaemonJSON(), b, 0o600)
}

// RemoveDaemonInfo deletes daemon.json; a missing file is not an error.
func (h Home) RemoveDaemonInfo() error {
	return Retry(shareRetry, func() error {
		if err := os.Remove(h.DaemonJSON()); !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

// shareRetry covers a concurrent reader: on Windows os.ReadFile opens files
// without FILE_SHARE_DELETE, so a rename or delete during the read fails
// with a sharing violation.
const shareRetry = 250 * time.Millisecond

// Retry runs op until it succeeds or window has passed, returning the last error.
func Retry(window time.Duration, op func() error) error { return kit.Retry(window, op) }

// WriteAtomic replaces path with data via a temp file in the same directory.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	return kit.WriteAtomic(path, data, perm)
}

// RotateLog moves the log to .1 when it grew past 10 MB; only the file's owner may call it.
func RotateLog(path string) { kit.RotateLog(path) }

// OpenLog opens an append-only slog file logger at the level of WHATSAPP_MCP_LOG.
func OpenLog(path string) (*slog.Logger, *os.File, error) { return kit.OpenLog(server, path) }

// Tail returns up to n last lines of the file at path.
func Tail(path string, n int) []string { return kit.Tail(path, n) }
