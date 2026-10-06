package sqlitedb

import (
	"context"
	"errors"
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
		"journal_mode":       "wal",
		"busy_timeout":       "10000",
		"synchronous":        "1", // NORMAL
		"secure_delete":      "1",
		"journal_size_limit": "16777216",
	} {
		var got string
		if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s = %q, %v; want %q", pragma, got, err, want)
		}
	}
}

// TestCheckpoint: a checkpoint that a reader holds off is an error, not the silent
// success it is to the driver, and one that is not held off empties the WAL.
func TestCheckpoint(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(testutil.TempDir(t), "x.db")
	db, err := Open(p, "_pragma=journal_mode(WAL)&_pragma=busy_timeout(200)&_pragma=secure_delete(1)&_txlock=immediate") // Pragmas, with a short wait
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(x); INSERT INTO t VALUES ('a'), ('b')`); err != nil {
		t.Fatal(err)
	}
	reader, err := db.Conn(ctx) // its snapshot is older than the writes that follow
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var n int
	if _, err := reader.ExecContext(ctx, `BEGIN DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('c'); DELETE FROM t WHERE x = 'a'`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("the driver reports the held-off checkpoint after all: %v; Checkpoint is not needed", err)
	}
	if err := Checkpoint(ctx, db); !errors.Is(err, ErrCheckpointBusy) {
		t.Errorf("Checkpoint with a reader = %v, want ErrCheckpointBusy", err)
	}
	if _, err := reader.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if err := Checkpoint(ctx, db); err != nil {
		t.Errorf("Checkpoint without a reader = %v", err)
	}
	if fi, err := os.Stat(p + "-wal"); err != nil || fi.Size() != 0 {
		t.Errorf("the WAL after the checkpoint: %v, %v; want it empty", fi, err)
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
