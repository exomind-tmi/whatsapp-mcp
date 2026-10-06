package archive

import (
	"os"
	"testing"
	"time"
)

// TestMain removes the big fixture that perf_test.go builds once for all the
// tests of the run, after closing the database that holds it.
func TestMain(m *testing.M) {
	code := m.Run()
	perf.close()
	os.Exit(code)
}

// removeAll is os.RemoveAll that retries for a while: on Windows a scanner
// may still hold a file that was just closed.
func removeAll(dir string) {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if os.RemoveAll(dir) == nil || time.Now().After(deadline) {
			return
		}
	}
}
