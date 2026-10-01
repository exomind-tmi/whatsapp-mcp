package sqlitedb

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

func TestDSN(t *testing.T) {
	cases := []struct{ path, query, want string }{
		{"/home/u/s.db", "", "file:///home/u/s.db"},
		{"/a#b/c%41d/e f/q?r/s.db", "_pragma=foreign_keys(1)", "file:///a%23b/c%2541d/e%20f/q%3Fr/s.db?_pragma=foreign_keys(1)"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			struct{ path, query, want string }{`C:\Users\a#b\s.db`, "", "file:///C:/Users/a%23b/s.db"},
			struct{ path, query, want string }{`\\server\share\a#b\s.db`, "", "file:////server/share/a%23b/s.db"})
	}
	for _, tc := range cases {
		if got := DSN(filepath.FromSlash(tc.path), tc.query); got != tc.want {
			t.Errorf("DSN(%q, %q) = %q, want %q", tc.path, tc.query, got, tc.want)
		}
	}
}

// TestOpenInPlace checks the escaping against the driver itself: the file
// lands exactly at the path and the query still reaches SQLite.
func TestOpenInPlace(t *testing.T) {
	for _, name := range []string{"a#b", "c%41d", "e f", "ж&з", "q?r"} {
		if runtime.GOOS == "windows" && strings.Contains(name, "?") {
			continue // not a legal file name there
		}
		dir := filepath.Join(testutil.TempDir(t), name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		openInPlace(t, dir, filepath.Join(dir, "x.db"))
	}
}

// TestPragmas checks the driver applies every shared setting: it ignores a
// misspelt pragma without a word.
func TestPragmas(t *testing.T) {
	db, err := Open(filepath.Join(testutil.TempDir(t), "x.db"), Pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for pragma, want := range map[string]string{
		"journal_mode":  "wal",
		"busy_timeout":  "10000",
		"synchronous":   "1", // NORMAL
		"secure_delete": "1",
	} {
		var got string
		if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s = %q, %v; want %q", pragma, got, err, want)
		}
	}
}

// TestOpenUNC reaches a temp dir through the administrative share, the way
// a redirected profile is reached. Skipped where that share is unavailable.
func TestOpenUNC(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("UNC paths are Windows-only")
	}
	dir := testutil.TempDir(t)
	unc := `\\localhost\` + strings.Replace(dir, ":", "$", 1)
	if _, err := os.Stat(unc); err != nil {
		t.Skipf("no administrative share: %v", err)
	}
	openInPlace(t, dir, filepath.Join(unc, "x.db"))
}

// openInPlace opens path and checks the database landed in dir as x.db.
func openInPlace(t *testing.T, dir, path string) {
	t.Helper()
	db, err := Open(path, "_pragma=user_version(7)")
	if err != nil {
		t.Fatal(err)
	}
	var v int
	err = db.QueryRow("PRAGMA user_version").Scan(&v)
	db.Close()
	if err != nil || v != 7 {
		t.Fatalf("%s: user_version = %d, %v; want 7 from the query", path, v, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.db")); err != nil {
		t.Fatalf("%s: database not created in place: %v", path, err)
	}
}
