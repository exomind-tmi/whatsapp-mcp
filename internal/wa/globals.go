package wa

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// deviceName is the name the phone shows for us under Linked devices, a
// default the user can rename on the phone. The phone writes it in brackets
// after the platform type ("Google Chrome (Claude, Windows)"), so it has none
// of its own.
func deviceName() string { return deviceNameFor(runtime.GOOS) }

func deviceNameFor(goos string) string {
	if name := osName(goos); name != "" {
		return "Claude, " + name
	}
	return "Claude"
}

// pairCodeName is the display name for pairing by phone number: the server
// takes only "Browser (OS)" of common ones and answers 400 otherwise
// (PairPhone, pair-code.go:88-89); Chrome matches the platform type set in
// start.
func pairCodeName() string { return pairCodeNameFor(runtime.GOOS) }

func pairCodeNameFor(goos string) string {
	if name := osName(goos); name != "" {
		return "Chrome (" + name + ")"
	}
	return "Chrome (Linux)"
}

// osName is how the phone names goos; "" for one it has no name for.
func osName(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return ""
}

// versionTimeout bounds the check of the current WhatsApp Web version.
const versionTimeout = 10 * time.Second

// maxErrLen caps a logged version-check error: on an unexpected status
// GetLatestVersion puts the whole response body into it (update.go:56),
// e.g. a captive portal's page.
const maxErrLen = 200

// waGlobals applies whatsmeow's process-wide client settings. They are
// package globals that Connect reads without a lock (clientpayload.go:93-100,
// 159-166), so they are written once and every Connect waits for ready.
type waGlobals struct {
	once  sync.Once
	ready chan struct{}
	fetch func(context.Context) (*store.WAVersionContainer, error)
}

var globals = newGlobals(func(ctx context.Context) (*store.WAVersionContainer, error) {
	return whatsmeow.GetLatestVersion(ctx, nil)
})

func newGlobals(fetch func(context.Context) (*store.WAVersionContainer, error)) *waGlobals {
	return &waGlobals{ready: make(chan struct{}), fetch: fetch}
}

// start applies the settings on the first call; later calls do nothing.
// The version check runs in the background, so a daemon without network
// starts at once and falls back to the version built into whatsmeow. ctx
// must be the daemon's: the check runs once per process, and a shorter ctx
// that ends early leaves the built-in version for good.
func (g *waGlobals) start(ctx context.Context, log *slog.Logger) {
	g.once.Do(func() {
		// SetOSInfo sets a version too; whatsmeow's own (0.1.0) is the one
		// WhatsApp is known to take.
		v := store.DeviceProps.GetVersion()
		store.SetOSInfo(deviceName(), [3]uint32{v.GetPrimary(), v.GetSecondary(), v.GetTertiary()})
		store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()
		// Travels only in the registration payload (clientpayload.go:168-188),
		// so it holds for accounts linked from now on and cannot be changed
		// for those already linked.
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		go g.setVersion(ctx, log)
	})
}

func (g *waGlobals) setVersion(ctx context.Context, log *slog.Logger) {
	defer close(g.ready)
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	v, err := g.fetch(ctx)
	if err != nil {
		msg := err.Error()
		if len(msg) > maxErrLen {
			msg = strings.ToValidUTF8(msg[:maxErrLen], "") + "..."
		}
		log.Warn("WhatsApp Web version check failed, using the built-in one", "version", store.GetWAVersion().String(), "err", msg)
		return
	}
	store.SetWAVersion(*v)
	log.Info("WhatsApp Web version", "version", v.String())
}

// wait returns once the settings are final, or with ctx's error. It must
// precede every Connect; it blocks until ctx ends if start was never called.
func (g *waGlobals) wait(ctx context.Context) error {
	select {
	case <-g.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
