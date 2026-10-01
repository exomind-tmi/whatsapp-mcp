package wa

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// keepGlobals restores whatsmeow's globals after a test that sets them.
func keepGlobals(t *testing.T) {
	props := proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps)
	payload := proto.Clone(store.BaseClientPayload).(*waWa6.ClientPayload)
	version := store.GetWAVersion()
	t.Cleanup(func() {
		store.DeviceProps = props
		store.BaseClientPayload = payload
		store.SetWAVersion(version)
	})
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestGlobalsWaitForVersion(t *testing.T) {
	keepGlobals(t)
	osVersion := proto.Clone(store.DeviceProps.GetVersion()).(*waCompanionReg.DeviceProps_AppVersion)
	want := store.WAVersionContainer{2, 3000, 1}
	release := make(chan struct{})
	var calls atomic.Int32
	g := newGlobals(func(ctx context.Context) (*store.WAVersionContainer, error) {
		calls.Add(1)
		<-release
		return &want, nil
	})
	g.start(context.Background(), quiet)
	g.start(context.Background(), quiet) // a no-op

	// The settings that need no network are in place at once.
	p := store.DeviceProps
	if p.GetOs() != deviceName || p.GetPlatformType() != waCompanionReg.DeviceProps_CHROME || !p.GetRequireFullSync() {
		t.Errorf("DeviceProps: os %q, platform %v, full sync %v", p.GetOs(), p.GetPlatformType(), p.GetRequireFullSync())
	}
	if !proto.Equal(p.GetVersion(), osVersion) {
		t.Errorf("DeviceProps version = %v, want whatsmeow's %v", p.GetVersion(), osVersion)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait before the version arrived = %v, want deadline exceeded", err)
	}
	close(release)
	if err := g.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.GetWAVersion(); got != want {
		t.Errorf("WA version = %v, want %v", got, want)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("version fetched %d times, want 1", n)
	}
}

func TestGlobalsKeepBuiltInVersion(t *testing.T) {
	keepGlobals(t)
	builtIn := store.GetWAVersion()
	page := strings.Repeat("<html>captive portal</html>", 100)
	g := newGlobals(func(ctx context.Context) (*store.WAVersionContainer, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("version fetched without a timeout")
		}
		return nil, errors.New("unexpected response with status 200: " + page)
	})
	var buf bytes.Buffer
	g.start(context.Background(), slog.New(slog.NewTextHandler(&buf, nil)))
	if err := g.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.GetWAVersion(); got != builtIn {
		t.Errorf("WA version = %v, want the built-in %v", got, builtIn)
	}
	if !strings.Contains(buf.String(), "level=WARN") || buf.Len() > 2*maxErrLen+200 {
		t.Errorf("want a short warning, got %d bytes:\n%s", buf.Len(), buf.String())
	}
}
