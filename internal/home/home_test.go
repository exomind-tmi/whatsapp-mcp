package home

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

func TestResolveOverride(t *testing.T) {
	dir := testutil.TempDir(t)
	t.Setenv(EnvHome, dir)
	h, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if h.Dir != filepath.Clean(dir) {
		t.Fatalf("Dir = %q, want %q", h.Dir, dir)
	}
	if got, want := h.Log("daemon.log"), filepath.Join(dir, "logs", "daemon.log"); got != want {
		t.Fatalf("Log = %q, want %q", got, want)
	}
}

func TestResolveRejectsRelative(t *testing.T) {
	t.Setenv(EnvHome, "relative/dir")
	if _, err := Resolve(); err == nil {
		t.Fatal("relative override accepted")
	}
}

func TestResolveDefault(t *testing.T) {
	t.Setenv(EnvHome, "")
	h, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(".mcp", "exomind-tmi", "whatsapp-mcp")
	if !strings.HasSuffix(h.Dir, want) || !filepath.IsAbs(h.Dir) {
		t.Fatalf("Dir = %q, want absolute path ending in %q", h.Dir, want)
	}
}

func TestEnsureCreatesLayout(t *testing.T) {
	h := Home{filepath.Join(testutil.TempDir(t), "nested", "state")}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{h.Dir, h.LogsDir(), h.BinDir()} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("%s not created: %v", d, err)
		}
	}
}

// TestEnsureMakesDirectoriesPrivate: on Unix the state directory and what is in
// it are the user's alone, also when they exist with a looser mode.
func TestEnsureMakesDirectoriesPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("access follows the user's profile on Windows")
	}
	h := Home{filepath.Join(testutil.TempDir(t), "state")}
	for _, d := range []string{h.Dir, h.LogsDir(), h.BinDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil { // the umask may have taken some
			t.Fatal(err)
		}
	}
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{h.Dir, h.LogsDir(), h.BinDir()} {
		if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s has the mode %v (%v), want drwx------", d, st.Mode(), err)
		}
	}

	fresh := Home{filepath.Join(testutil.TempDir(t), "new")}
	if err := fresh.Ensure(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(fresh.Dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("a new directory has the mode %v (%v), want drwx------", st.Mode(), err)
	}
}

// TestEnsureLeavesAPrivateDirectoryAlone: a directory that is the user's alone is
// not chmodded, as a shared one that the user cannot chmod must not stop every
// command; only one that is open to others is made 0700.
func TestEnsureLeavesAPrivateDirectoryAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("access follows the user's profile on Windows")
	}
	h := Home{filepath.Join(testutil.TempDir(t), "state")}
	for _, d := range []string{h.Dir, h.LogsDir(), h.BinDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(h.BinDir(), 0o500); err != nil { // private, but not 0700
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.BinDir(), 0o700) }) // so that the directory can be removed
	if err := h.Ensure(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(h.BinDir()); err != nil || st.Mode().Perm() != 0o500 {
		t.Errorf("a private directory has the mode %v (%v), want it left at dr-x------", st.Mode(), err)
	}
}

func TestWriteAtomicReplaces(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "f.json")
	for _, s := range []string{"one", "two"} {
		if err := WriteAtomic(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != s {
			t.Fatalf("content = %q, want %q", b, s)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestDaemonInfoRoundTrip(t *testing.T) {
	h := Home{testutil.TempDir(t)}
	if _, err := h.ReadDaemonInfo(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing daemon.json: err = %v, want ErrNotExist", err)
	}
	in := DaemonInfo{PID: 42, Port: 50123, Version: "v0.1.0", StartedAt: time.Unix(1790000000, 0).UTC()}
	if err := h.WriteDaemonInfo(in); err != nil {
		t.Fatal(err)
	}
	out, err := h.ReadDaemonInfo()
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestRemoveDaemonInfo(t *testing.T) {
	h := Home{testutil.TempDir(t)}
	if err := h.RemoveDaemonInfo(); err != nil {
		t.Fatalf("missing daemon.json: %v", err)
	}
	h.WriteDaemonInfo(DaemonInfo{PID: 1, Port: 1})
	if err := h.RemoveDaemonInfo(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.DaemonJSON()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("daemon.json left: %v", err)
	}
}

func TestRetry(t *testing.T) {
	n := 0
	err := Retry(time.Second, func() error {
		if n++; n < 3 {
			return errors.New("busy")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("err=%v n=%d", err, n)
	}
	if err := Retry(0, func() error { return errors.New("busy") }); err == nil {
		t.Fatal("last error lost")
	}
}

func TestTokenProof(t *testing.T) {
	p := TokenProof("tok", "n1")
	if p != TokenProof("tok", "n1") || p == TokenProof("tok", "n2") || p == TokenProof("other", "n1") {
		t.Fatal("proof must depend on both token and nonce, deterministically")
	}
	if strings.Contains(p, "tok") {
		t.Fatal("proof reveals the token")
	}
}

func TestReadToken(t *testing.T) {
	h := Home{testutil.TempDir(t)}
	if _, err := h.ReadToken(); err == nil {
		t.Fatal("missing token read without error")
	}
	os.WriteFile(h.TokenFile(), []byte("abc\n"), 0o600)
	if tok, err := h.ReadToken(); err != nil || tok != "abc" {
		t.Fatalf("ReadToken = %q, %v", tok, err)
	}
}

func TestTail(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "x.log")
	os.WriteFile(p, []byte("a\r\nb\nc\nd\n"), 0o600)
	got := strings.Join(Tail(p, 3), ",")
	if got != "b,c,d" {
		t.Fatalf("Tail = %q", got)
	}
	if Tail(filepath.Join(testutil.TempDir(t), "none"), 3) != nil {
		t.Fatal("Tail of missing file must be nil")
	}
}
