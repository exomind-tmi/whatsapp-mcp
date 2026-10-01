package wa

import (
	"bytes"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func debugLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestWALogLevelsAndSub(t *testing.T) {
	var buf bytes.Buffer
	l := newWALog(debugLog(&buf), "Client")
	l.Errorf("e %d", 1)
	l.Warnf("w %d", 2)
	l.Infof("i %d", 3)
	l.Debugf("d %d", 4)
	l.Sub("AppState").Infof("nested")

	for _, want := range []string{
		`level=ERROR msg="e 1" sub=Client`,
		`level=WARN msg="w 2" sub=Client`,
		`level=INFO msg="i 3" sub=Client`,
		`level=DEBUG msg="d 4" sub=Client`,
		`level=INFO msg=nested sub=Client/AppState`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

// secret fails the test if anything formats it: a dropped line must not
// even be rendered, let alone written.
type secret struct{ t *testing.T }

func (s secret) String() string {
	s.t.Error("a secret was formatted")
	return "SECRET"
}

func TestWALogDropsSecrets(t *testing.T) {
	var buf bytes.Buffer
	l := newWALog(debugLog(&buf), "Client")
	s := secret{t}
	l.Sub("QRChannel").Debugf("Emitting QR code %s", s)
	l.Sub("Recv").Debugf("%s", s)
	l.Sub("Send").Debugf("%s", s)
	l.Sub("Send").Sub("Deeper").Debugf("%s", s)
	newWALog(debugLog(&buf), "QRChannel").Debugf("%s", s)
	l.Debugf("Errored frame hex: %s", s)
	if buf.Len() != 0 {
		t.Errorf("secrets reached the log:\n%s", buf.String())
	}

	// Only their Debug is clamped: the rest still tells what went wrong.
	l.Sub("QRChannel").Infof("closed")
	l.Sub("Recv").Warnf("odd node")
	l.Sub("Socket").Debugf("Dialing")
	for _, want := range []string{"sub=Client/QRChannel", "sub=Client/Recv", "sub=Client/Socket"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

// TestWALogSummarisesNodes logs a node the way whatsmeow's handler queue
// does for a slow handler (client.go:929): at Warn, through the root logger.
func TestWALogSummarisesNodes(t *testing.T) {
	var buf bytes.Buffer
	l := newWALog(debugLog(&buf), "Client")
	pair := &waBinary.Node{
		Tag:   "iq",
		Attrs: waBinary.Attrs{"id": "42", "type": "set", "from": types.NewJID("79161234567", types.DefaultUserServer), "notify": "Anton"},
		Content: []waBinary.Node{{Tag: "pair-device", Content: []waBinary.Node{
			{Tag: "ref", Content: []byte("2@SECRETREF")},
		}}},
	}
	l.Warnf("Node handling took %s for %s", "6s", pair)
	l.Errorf("Unknown stream error: %s", waBinary.Node{Tag: "stream:error", Attrs: waBinary.Attrs{"code": "999"}})
	l.Debugf("Got identity change for %s: %s", "x", (*waBinary.Node)(nil))

	out := buf.String()
	for _, leak := range []string{"SECRETREF", "79161234567", "Anton"} {
		if strings.Contains(out, leak) {
			t.Errorf("log shows %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{
		`msg="Node handling took 6s for <iq id=\"42\" type=\"set\"><pair-device/>"`,
		`msg="Unknown stream error: <stream:error code=\"999\">"`,
		`for x: <nil>"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// TestWhatsmeowLogSources pins the whatsmeow source the clamps above rely
// on: a module renamed, a new sub-logger or the QR logged elsewhere would
// silently open them up after an update (plan 16). On failure, re-read the
// changed lines and fix secretModules and secretDebug.
func TestWhatsmeowLogSources(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "go.mau.fi/whatsmeow").Output()
	if err != nil {
		t.Fatalf("locating whatsmeow: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	subRe := regexp.MustCompile(`\.Sub\("([^"]+)"\)`)
	subs := map[string]bool{}
	var qr, frames []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(src), "\n") {
			line = strings.TrimSpace(line)
			for _, m := range subRe.FindAllStringSubmatch(line, -1) {
				subs[m[1]] = true
			}
			if strings.Contains(line, `"Emitting QR code`) {
				qr = append(qr, line)
			}
			if strings.Contains(line, `"Errored frame hex`) {
				frames = append(frames, line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"Recv": true, "Send": true, "AppState": true, "Socket": true, "QRChannel": true}; !maps.Equal(subs, want) {
		t.Errorf("whatsmeow sub-loggers = %v, want %v", slices.Sorted(maps.Keys(subs)), slices.Sorted(maps.Keys(want)))
	}
	if len(qr) != 1 || !strings.HasPrefix(qr[0], `qrc.log.Debugf("Emitting QR code %s"`) {
		t.Errorf("the QR is no longer logged only at QRChannel's Debug: %q", qr)
	}
	for _, line := range frames {
		if !strings.HasPrefix(line, `cli.Log.Debugf("Errored frame hex: %s"`) {
			t.Errorf("raw frame logged in a way secretDebug misses: %q", line)
		}
	}
	if len(frames) == 0 {
		t.Error(`"Errored frame hex" is gone: drop it from secretDebug or follow its new wording`)
	}
}

func TestWALogSkipsFormattingBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	l := newWALog(slog.New(slog.NewTextHandler(&buf, nil)), "Client") // Info
	l.Debugf("%s", secret{t})
	if buf.Len() != 0 {
		t.Errorf("Debug written at Info:\n%s", buf.String())
	}
}
