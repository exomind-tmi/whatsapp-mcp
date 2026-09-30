package archive

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

func TestOpenFresh(t *testing.T) {
	db, err := Open(filepath.Join(testutil.TempDir(t), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

func TestOpenPathWithURIDelimiters(t *testing.T) {
	for _, name := range []string{"a#b", "c%41d", "e f", "ж&з", "q?r"} {
		if runtime.GOOS == "windows" && strings.Contains(name, "?") {
			continue // not a legal file name there
		}
		dir := filepath.Join(testutil.TempDir(t), name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := Open(filepath.Join(dir, "archive.db"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		db.Close()
		if _, err := os.Stat(filepath.Join(dir, "archive.db")); err != nil {
			t.Fatalf("%s: archive.db not created in place: %v", name, err)
		}
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	raw, err := sql.Open("sqlite", dsn(p, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	if _, err := Open(p); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("err = %v, want ErrNewerSchema", err)
	}
}
