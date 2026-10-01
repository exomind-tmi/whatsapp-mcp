package wa

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

// TestOpenStore runs whatsmeow's schema upgrade on modernc: it refuses to
// run without foreign keys, so passing also proves the DSN turns them on.
// The second open finds the schema current.
func TestOpenStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempDir(t), "a#b", "store.db")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	log := newWALog(slog.New(slog.NewTextHandler(io.Discard, nil)), "Database")
	for range 2 {
		c, err := openStore(ctx, path, log)
		if err != nil {
			t.Fatal(err)
		}
		devs, err := c.GetAllDevices(ctx)
		if err != nil || len(devs) != 0 {
			t.Errorf("GetAllDevices = %d devices, %v; want none", len(devs), err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("store.db not at its path: %v", err)
	}
}
