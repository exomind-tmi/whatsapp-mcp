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

// WaitForLog fails the test unless the file at path holds want within five
// seconds, for a line that a goroutine of the code under test writes.
func WaitForLog(t testing.TB, path, want string) {
	t.Helper()
	var b []byte
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, _ = os.ReadFile(path)
		if strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has no %q:\n%s", path, want, b)
		}
	}
}
