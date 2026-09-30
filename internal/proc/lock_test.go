package proc

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

func TestLockIsExclusive(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "daemon.lock")
	first, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock: err = %v, want ErrLocked", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	again, err := TryLock(p)
	if err != nil {
		t.Fatalf("TryLock after Unlock: %v", err)
	}
	again.Unlock()
}
