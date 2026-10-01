// Package home owns the state directory: its location, the files in it and
// how they are written. Nothing here depends on the current directory, which
// is system32 when Claude Desktop launches the server.
package home

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvHome overrides the state directory.
const EnvHome = "WHATSAPP_MCP_HOME"

// Home is the state directory, ~/.mcp/exomind-tmi/whatsapp-mcp by default.
type Home struct{ Dir string }

// Resolve picks the state directory from WHATSAPP_MCP_HOME or the user profile.
func Resolve() (Home, error) {
	if d := os.Getenv(EnvHome); d != "" {
		if !filepath.IsAbs(d) {
			return Home{}, fmt.Errorf("%s must be an absolute path, got %q", EnvHome, d)
		}
		return Home{filepath.Clean(d)}, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return Home{}, fmt.Errorf("locate user profile: %w", err)
	}
	return Home{filepath.Join(u, ".mcp", "exomind-tmi", "whatsapp-mcp")}, nil
}

// Ensure creates the state directory and its fixed subdirectories.
func (h Home) Ensure() error {
	for _, d := range []string{h.Dir, h.LogsDir(), h.BinDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (h Home) LockFile() string   { return filepath.Join(h.Dir, "daemon.lock") }
func (h Home) DaemonJSON() string { return filepath.Join(h.Dir, "daemon.json") }
func (h Home) TokenFile() string  { return filepath.Join(h.Dir, "token") }
func (h Home) ArchiveDB() string  { return filepath.Join(h.Dir, "archive.db") }
func (h Home) StoreDB() string    { return filepath.Join(h.Dir, "store.db") } // whatsmeow's device keys
func (h Home) BinDir() string     { return filepath.Join(h.Dir, "bin") }
func (h Home) LogsDir() string    { return filepath.Join(h.Dir, "logs") }
func (h Home) Log(name string) string {
	return filepath.Join(h.LogsDir(), name)
}

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

// Retry runs op until it succeeds or window has passed, returning the last
// error. It is for Windows file operations that fail while another process
// (a reader, an antivirus scanner, the indexer) briefly holds the file.
func Retry(window time.Duration, op func() error) error {
	deadline := time.Now().Add(window)
	for {
		err := op()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// WriteAtomic replaces path with data via a temp file in the same directory.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return Retry(shareRetry, func() error { return os.Rename(tmp, path) })
}
