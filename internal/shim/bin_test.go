package shim

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/mod/semver"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

func TestSemverOrderOfBuildVersions(t *testing.T) {
	// Each rebuild is newer than the previous; a release beats its dev builds.
	ordered := []string{"v0.3.0", "v0.3.1-dev.1790000000", "v0.3.1-dev.1790000001", "v0.3.1", "v0.10.0"}
	for i := 1; i < len(ordered); i++ {
		if semver.Compare(ordered[i-1], ordered[i]) >= 0 {
			t.Errorf("%s should be older than %s", ordered[i-1], ordered[i])
		}
	}
}

func writeExe(t *testing.T, binDir, version, content string) {
	t.Helper()
	p := filepath.Join(binDir, version, exeName())
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMaxBin(t *testing.T) {
	bin := testutil.TempDir(t)
	if _, err := maxBin(bin); err == nil {
		t.Fatal("empty bin/ must be an error")
	}
	for _, v := range []string{"v0.3.0", "v0.3.1-dev.1790000000", "v0.10.0", "v0.9.9"} {
		writeExe(t, bin, v, v)
	}
	os.MkdirAll(filepath.Join(bin, "v0.99.0"), 0o700) // no binary inside
	os.MkdirAll(filepath.Join(bin, "latest"), 0o700)  // not semver
	got, err := maxBin(bin)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(bin, "v0.10.0", exeName()); got != want {
		t.Fatalf("maxBin = %s, want %s", got, want)
	}
}

func TestInstall(t *testing.T) {
	root := testutil.TempDir(t)
	bin := filepath.Join(root, "bin")
	src := filepath.Join(root, "src.exe")
	os.WriteFile(src, []byte("build-1"), 0o755)

	if err := install(src, bin, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(bin, "v0.1.0", exeName())
	if !same(src, dst) {
		t.Fatal("installed copy differs")
	}
	// Idempotent: an identical copy is left alone.
	st1, _ := os.Stat(dst)
	if err := install(src, bin, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(dst)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("identical copy was rewritten")
	}
	// A different binary under the same version replaces the copy.
	os.WriteFile(src, []byte("build-2"), 0o755)
	if err := install(src, bin, "v0.1.0"); err != nil || !same(src, dst) {
		t.Fatalf("replace: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dst)); len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
	if err := install(src, bin, "not-semver"); err == nil {
		t.Fatal("non-semver version accepted")
	}
}

func TestPruneKeepsNewestAndOwn(t *testing.T) {
	bin := testutil.TempDir(t)
	for _, v := range []string{"v0.2.0", "v0.1.0", "v0.4.0", "v0.3.0"} {
		writeExe(t, bin, v, v)
		os.WriteFile(filepath.Join(bin, v+".download.lock"), nil, 0o600)
	}
	prune(bin, 2, "v0.1.0")
	if got := versions(bin); !slices.Equal(got, []string{"v0.4.0", "v0.3.0", "v0.1.0"}) {
		t.Fatalf("versions after prune = %v", got)
	}
	if _, err := os.Stat(filepath.Join(bin, "v0.2.0.download.lock")); !os.IsNotExist(err) {
		t.Errorf("lock of a pruned version left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bin, "v0.1.0.download.lock")); err != nil {
		t.Errorf("lock of a kept version removed: %v", err)
	}
}
