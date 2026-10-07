package tools

import (
	"os"
	"testing"
	"time"
)

// TestMain fixes the time zone: the tools write times with the offset of the
// machine, and a test that reads them back must not depend on which one.
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("test", 3*3600)
	os.Exit(m.Run())
}
