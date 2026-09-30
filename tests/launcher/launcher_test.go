// Package launcher_test runs the plugin's launchers the way Claude hosts do:
// the command of plugin/.mcp.json with ${CLAUDE_PLUGIN_ROOT} expanded, an
// extensionless path that Windows resolves to launch-whatsapp-mcp.cmd (which
// runs launch.ps1) and Linux runs as the sh launcher. A local HTTP server plays
// GitHub Releases, and the "whatsapp-mcp" binary it serves is this test
// executable: with fakeEnv set, TestMain echoes its args and stdin instead of
// testing.
package launcher_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

const (
	fakeEnv     = "LAUNCHER_TEST_FAKE" // run as the fake whatsapp-mcp
	fakeExitEnv = "LAUNCHER_TEST_EXIT" // its exit code
	version     = "v1.2.3"
	runTimeout  = 45 * time.Second
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "" {
		os.Exit(fakeServer())
	}
	os.Exit(m.Run())
}

// fakeServer prints its args, then echoes stdin, like a trivial stdio server.
func fakeServer() int {
	in, _ := io.ReadAll(os.Stdin)
	fmt.Printf("args=%q\n", os.Args[1:])
	os.Stdout.Write(in)
	code, _ := strconv.Atoi(os.Getenv(fakeExitEnv))
	return code
}

// stdin mixes non-ASCII text (Cyrillic, emoji) and a large payload: the
// launcher must pass bytes as is.
var stdin = "{\"jsonrpc\":\"2.0\",\"method\":\"привет 👋\"}\n" + strings.Repeat("x", 100_000) + "\n"

func wantStdout() string { return "args=[\"stdio\"]\n" + stdin }

func platform(t *testing.T) (name, exe string) {
	switch {
	case runtime.GOOS == "windows":
		return "windows-amd64", "whatsapp-mcp.exe"
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return "linux-amd64", "whatsapp-mcp"
	}
	t.Skipf("no launcher for %s/%s", runtime.GOOS, runtime.GOARCH)
	return "", ""
}

type fixture struct {
	t         *testing.T
	root      string        // plugin copy, what ${CLAUDE_PLUGIN_ROOT} points to
	home      string        // WHATSAPP_MCP_HOME
	exe       string        // where the launcher installs the binary
	body      []byte        // the served binary
	delay     time.Duration // before the server answers a download
	notFound  bool
	noRanges  bool          // the server ignores Range and sends the whole body
	slowFirst bool          // the first download sends half the body, then trickles the rest
	halfSent  chan struct{} // closed once the slow first download has sent half
	mu        sync.Mutex
	dls       []download
	srv       *httptest.Server
}

// download records one download request.
type download struct {
	rng  string // its Range header
	sent int    // body bytes sent in a 2xx response
}

func newFixture(t *testing.T, opts ...func(*fixture)) *fixture {
	t.Helper()
	plat, exeName := platform(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := testutil.TempDir(t)
	f := &fixture{
		t:        t,
		root:     filepath.Join(dir, "plugin"),
		home:     filepath.Join(dir, "home"),
		body:     body,
		halfSent: make(chan struct{}),
	}
	for _, o := range opts {
		o(f) // before the server starts: its handler reads the options
	}
	f.exe = filepath.Join(f.home, "bin", version, exeName)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.dls = append(f.dls, download{rng: r.Header.Get("Range")})
		cw := &countingWriter{ResponseWriter: w, f: f, i: len(f.dls) - 1}
		f.mu.Unlock()
		time.Sleep(f.delay)
		switch {
		case f.notFound:
			http.NotFound(w, r)
		case f.slowFirst && cw.i == 0:
			f.trickle(cw)
		case f.noRanges:
			cw.Write(f.body)
		default:
			http.ServeContent(cw, r, "", time.Time{}, bytes.NewReader(f.body)) // honours Range
		}
	}))
	t.Cleanup(f.srv.Close)
	copyDir(t, filepath.Join("..", "..", "plugin"), f.root)

	sum := sha256.Sum256(body)
	f.writeRelease(release(plat, f.srv.URL+"/download/"+version+"/"+exeName, hex.EncodeToString(sum[:])))
	return f
}

// trickle sends half the body, then the rest in pieces over about two seconds.
func (f *fixture) trickle(w http.ResponseWriter) {
	w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
	rc := http.NewResponseController(w)
	half := len(f.body) / 2
	w.Write(f.body[:half])
	rc.Flush()
	close(f.halfSent)
	piece := (len(f.body)-half)/20 + 1
	for rest := f.body[half:]; len(rest) > 0; rest = rest[min(piece, len(rest)):] {
		time.Sleep(100 * time.Millisecond)
		if _, err := w.Write(rest[:min(piece, len(rest))]); err != nil {
			return
		}
		rc.Flush()
	}
}

// countingWriter adds the body bytes of a successful response to its download.
type countingWriter struct {
	http.ResponseWriter
	f      *fixture
	i      int // index in f.dls
	failed bool
}

func (w *countingWriter) WriteHeader(code int) {
	w.failed = code >= 300
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if !w.failed {
		w.f.mu.Lock()
		w.f.dls[w.i].sent += n
		w.f.mu.Unlock()
	}
	return n, err
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (f *fixture) downloads() []download {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.dls)
}

// release is a release.json with the given asset and a decoy for another
// platform, so the launcher has to pick the right one.
func release(plat, url, sha string) map[string]any {
	decoy := "linux-amd64"
	if plat == decoy {
		decoy = "windows-amd64"
	}
	return map[string]any{
		"version": version,
		"assets": map[string]any{
			decoy: map[string]string{"url": "http://127.0.0.1:1/decoy", "sha256": strings.Repeat("0", 64)},
			plat:  map[string]string{"url": url, "sha256": sha},
		},
	}
}

func (f *fixture) writeRelease(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "release.json"), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

type result struct {
	stdout, stderr string
	code           int
}

// command is the launcher as a host starts it, from an unrelated working directory.
func (f *fixture) command(ctx context.Context, env ...string) *exec.Cmd {
	f.t.Helper()
	cmd := exec.CommandContext(ctx, hostCommand(f.t, f.root))
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "WHATSAPP_MCP_HOME="+f.home, fakeEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd
}

// run runs the launcher to its end.
func (f *fixture) run(env ...string) result {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := f.command(ctx, env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			f.t.Fatalf("run launcher: %v", err)
		}
		code = ee.ExitCode()
	}
	return result{out.String(), errOut.String(), code}
}

func (f *fixture) log() string {
	b, _ := os.ReadFile(filepath.Join(f.home, "logs", "launcher.log"))
	return string(b)
}

// assertOK checks a successful run: the server's output only, byte for byte.
func (f *fixture) assertOK(r result) {
	f.t.Helper()
	if r.code != 0 || r.stdout != wantStdout() {
		f.t.Fatalf("exit %d, stdout %d bytes (want %d)\nstdout head: %.200q\nstderr: %s\nlog:\n%s",
			r.code, len(r.stdout), len(wantStdout()), r.stdout, r.stderr, f.log())
	}
}

// assertFailed checks a failed run: non-zero exit, a reason on stderr and in
// the log, nothing on stdout, and neither a binary nor a partial download
// left behind.
func (f *fixture) assertFailed(r result, reason string) {
	f.t.Helper()
	if r.code == 0 || r.stdout != "" || !strings.Contains(r.stderr, reason) {
		f.t.Fatalf("want failure with %q, got exit %d\nstdout: %.200q\nstderr: %s", reason, r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(f.log(), reason) {
		f.t.Errorf("launcher.log lacks %q:\n%s", reason, f.log())
	}
	if _, err := os.Stat(f.exe); err == nil {
		f.t.Errorf("binary left at %s", f.exe)
	}
	f.assertNoPartFiles()
}

func (f *fixture) assertNoPartFiles() {
	f.t.Helper()
	parts, _ := filepath.Glob(filepath.Join(f.home, "bin", "*.part"))
	if len(parts) > 0 {
		f.t.Errorf("partial downloads left: %v", parts)
	}
}

func (f *fixture) assertHits(want int) {
	f.t.Helper()
	if got := len(f.downloads()); got != want {
		f.t.Errorf("downloads: got %d, want %d", got, want)
	}
}

func TestDownloadVerifyRunThenReuse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.assertOK(f.run())
	f.assertHits(1)
	f.assertNoPartFiles()
	got, err := os.ReadFile(f.exe)
	if err != nil || !bytes.Equal(got, f.body) {
		t.Fatalf("installed binary differs from the served one (err %v)", err)
	}

	f.assertOK(f.run())
	f.assertHits(1) // reused, not downloaded again
}

func TestBadHashRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plat, _ := platform(t)
	f.writeRelease(release(plat, f.srv.URL+"/x", strings.Repeat("ab", 32)))
	f.assertFailed(f.run(), "SHA-256 mismatch")
	f.assertHits(1)
}

func TestDownloadFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(f *fixture) { f.notFound = true })
	f.assertFailed(f.run(), "")
	if len(f.downloads()) == 0 {
		t.Error("no download attempted")
	}
}

func TestUnreleasedPlaceholder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plat, _ := platform(t)
	f.writeRelease(map[string]any{"version": "v0.0.0", "assets": map[string]any{}})
	f.assertFailed(f.run(), "has no "+plat+" build")
	f.assertHits(0)
}

func TestConcurrentLaunchersDownloadOnce(t *testing.T) {
	t.Parallel()
	// A slow download keeps the lock taken while the other launchers start.
	f := newFixture(t, func(f *fixture) { f.delay = 2 * time.Second })
	const n = 4
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { results[i] = f.run() })
	}
	wg.Wait()
	for _, r := range results {
		f.assertOK(r)
	}
	f.assertHits(1)
	f.assertNoPartFiles()
}

// A host kills a launcher that has not answered within its MCP start timeout,
// and the downloader the launcher started may outlive it. The next launcher
// must wait for that download, not run a second one alongside it, then use or
// resume what it left.
func TestKilledLauncherResumes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(f *fixture) { f.slowFirst = true })
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	first := f.command(ctx)
	first.Stdin = nil // no pipes: the surviving downloader would hold them and block Wait
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.halfSent:
	case <-ctx.Done():
		t.Fatal("the launcher never started the download")
	}
	// The script logs its pid: on Windows that is powershell.exe under the .cmd.
	m := regexp.MustCompile(`pid=(\d+) downloading`).FindStringSubmatch(f.log())
	if m == nil {
		t.Fatalf("no download in the log:\n%s", f.log())
	}
	pid, _ := strconv.Atoi(m[1])
	if p, err := os.FindProcess(pid); err != nil || p.Kill() != nil {
		t.Fatalf("cannot kill launcher pid %d: %v", pid, err)
	}
	first.Wait()

	f.assertOK(f.run()) // while the survivor still trickles
	sent := 0
	for _, d := range f.downloads() {
		sent += d.sent
	}
	if sent != len(f.body) {
		t.Errorf("sent %d body bytes, want one body of %d: %+v\nlog:\n%s", sent, len(f.body), f.downloads(), f.log())
	}
	f.assertNoPartFiles()
}

// A server that ignores Range cannot resume a partial download, so the
// launcher downloads from scratch (on Windows curl.exe falls back to
// Invoke-WebRequest).
func TestUnresumablePartial(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(f *fixture) { f.noRanges = true })
	if err := os.MkdirAll(filepath.Join(f.home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "bin", version+".part"), f.body[:len(f.body)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	f.assertOK(f.run())
	f.assertHits(2)
	f.assertNoPartFiles()
}

func TestExitCodePropagates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.run(fakeExitEnv + "=7")
	if r.code != 7 || r.stdout != wantStdout() {
		t.Fatalf("exit %d (want 7), stdout ok: %v, stderr: %s", r.code, r.stdout == wantStdout(), r.stderr)
	}
}

// Without curl.exe on PATH, launch.ps1 falls back to Invoke-WebRequest.
func TestInvokeWebRequestFallback(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("launch.ps1 only")
	}
	t.Parallel()
	f := newFixture(t)
	f.assertOK(f.run("PATH=" + testutil.TempDir(t)))
	f.assertHits(1)
}

// The sh launcher parses release.json with sed, so it must accept exactly
// what scripts/release-json.sh prints. Linux only: on Windows "bash" may be
// the WSL stub, and launch.ps1 uses a real JSON parser anyway.
func TestGeneratedReleaseJSON(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sh launcher only")
	}
	t.Parallel()
	f := newFixture(t)
	dir := testutil.TempDir(t)
	linux := filepath.Join(dir, "whatsapp-mcp_"+version+"_linux_amd64")
	windows := filepath.Join(dir, "whatsapp-mcp_"+version+"_windows_amd64.exe")
	if err := os.WriteFile(linux, f.body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(windows, []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join("..", "..", "scripts", "release-json.sh"), version,
		"windows-amd64="+windows, "linux-amd64="+linux)
	cmd.Env = append(os.Environ(), "RELEASE_URL_BASE="+f.srv.URL+"/download/"+version)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("release-json.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "release.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	f.assertOK(f.run())
	f.assertHits(1)
}

// The committed release.json is either the pre-release placeholder or pins
// both platforms to assets of the matching GitHub Release.
func TestCommittedReleaseJSON(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "plugin", "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rel struct {
		Version string `json:"version"`
		Assets  map[string]struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		t.Fatal(err)
	}
	if rel.Version == "v0.0.0" && len(rel.Assets) == 0 {
		return
	}
	for plat, ext := range map[string]string{"windows-amd64": ".exe", "linux-amd64": ""} {
		a := rel.Assets[plat]
		want := fmt.Sprintf("https://github.com/exomind-tmi/whatsapp-mcp/releases/download/%s/whatsapp-mcp_%s_%s%s",
			rel.Version, rel.Version, strings.Replace(plat, "-", "_", 1), ext)
		if a.URL != want || len(a.SHA256) != 64 {
			t.Errorf("%s: got %+v, want url %s and a sha256", plat, a, want)
		}
	}
}

// hostCommand is the command of plugin/.mcp.json with ${CLAUDE_PLUGIN_ROOT}
// expanded and, on Windows, resolved through PATHEXT as the hosts do; it fails
// unless that picks the .cmd launcher.
func hostCommand(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.MCPServers["whatsapp"]
	if !ok || len(s.Args) > 0 || len(s.Env) > 0 {
		t.Fatalf("want server whatsapp without args and env (Desktop drops user_config), got %+v", cfg.MCPServers)
	}
	name := strings.ReplaceAll(s.Command, "${CLAUDE_PLUGIN_ROOT}", root)
	if strings.Contains(name, "$") {
		t.Fatalf("command %q uses more than ${CLAUDE_PLUGIN_ROOT}", s.Command)
	}
	if runtime.GOOS == "windows" {
		for _, ext := range filepath.SplitList(os.Getenv("PATHEXT")) {
			if _, err := os.Stat(name + ext); err == nil {
				name += ext
				break
			}
		}
		if !strings.EqualFold(filepath.Ext(name), ".cmd") {
			t.Fatalf("the command resolves to %s, want the .cmd launcher", name)
		}
	}
	return name
}

// copyDir copies a tree keeping file modes: the sh launcher must stay executable.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, st.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}
