package archive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

func TestOpenFresh(t *testing.T) {
	db, err := Open(filepath.Join(testutil.TempDir(t), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

// TestOpenEscapesPath only checks that Open goes through sqlitedb; the
// escaping itself is tested there.
func TestOpenEscapesPath(t *testing.T) {
	dir := filepath.Join(testutil.TempDir(t), "a#b")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(dir, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(filepath.Join(dir, "archive.db")); err != nil {
		t.Fatalf("archive.db not created in place: %v", err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	raw, err := sqlitedb.Open(p, "")
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
