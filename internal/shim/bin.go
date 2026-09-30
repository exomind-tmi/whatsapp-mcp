package shim

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"golang.org/x/mod/semver"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
)

// keepVersions is how many versions stay in bin/; the running daemon is
// always the newest, so it is never pruned.
const keepVersions = 2

const exeRetry = 2 * time.Second

func exeName() string {
	if runtime.GOOS == "windows" {
		return "whatsapp-mcp.exe"
	}
	return "whatsapp-mcp"
}

// install copies the binary src into binDir/<version>/ unless an identical
// copy is already there. It tolerates parallel shims and a running target:
// a failed rename is fine when a sibling has put the same bytes in place.
func install(src, binDir, version string) error {
	if !semver.IsValid(version) {
		return fmt.Errorf("version %q is not semver", version)
	}
	dst := filepath.Join(binDir, version, exeName())
	if same(src, dst) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), fmt.Sprintf(".tmp-%d-%s", os.Getpid(), rand.Text()[:8]))
	defer os.Remove(tmp)
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	// A fresh exe is what antivirus and the indexer open first on Windows.
	err := home.Retry(exeRetry, func() error { return os.Rename(tmp, dst) })
	if err != nil && !same(src, dst) {
		return err
	}
	prune(binDir, keepVersions)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// same reports whether both files exist with equal size and sha256.
func same(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil || sa.Size() != sb.Size() {
		return false
	}
	ha, err1 := fileHash(a)
	hb, err2 := fileHash(b)
	return err1 == nil && err2 == nil && ha == hb
}

func fileHash(p string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// versions lists installed versions in bin/, newest first.
func versions(binDir string) []string {
	entries, _ := os.ReadDir(binDir)
	var vs []string
	for _, e := range entries {
		v := e.Name()
		if !e.IsDir() || !semver.IsValid(v) {
			continue
		}
		if st, err := os.Stat(filepath.Join(binDir, v, exeName())); err == nil && st.Mode().IsRegular() {
			vs = append(vs, v)
		}
	}
	slices.SortFunc(vs, func(a, b string) int { return semver.Compare(b, a) })
	return vs
}

// maxBin returns the newest installed binary: the one every shim starts.
func maxBin(binDir string) (string, error) {
	vs := versions(binDir)
	if len(vs) == 0 {
		return "", fmt.Errorf("no whatsapp-mcp binary in %s", binDir)
	}
	return filepath.Join(binDir, vs[0], exeName()), nil
}

// prune removes versions beyond the newest keep. Errors are ignored: a
// running exe cannot be deleted on Windows, and that is fine.
func prune(binDir string, keep int) {
	vs := versions(binDir)
	for _, v := range vs[min(keep, len(vs)):] {
		_ = os.RemoveAll(filepath.Join(binDir, v))
	}
}
