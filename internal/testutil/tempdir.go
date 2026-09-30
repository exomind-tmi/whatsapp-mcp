// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TempDir is t.TempDir for tests that write files. On Windows a scanner
// (Defender) may briefly hold a just-deleted file, which leaves the directory
// non-empty for a few milliseconds and fails t.TempDir's cleanup; this one
// retries the removal for up to two seconds.
func TempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", strings.NewReplacer("/", "_", "\\", "_").Replace(t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove temp dir: %v", err)
				return
			}
		}
	})
	return dir
}
