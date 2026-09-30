package shim

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/proc"
)

const (
	startWait = 10 * time.Second
	pollEvery = 200 * time.Millisecond
	tailLines = 20
	// maxHandovers bounds the retries when an older shim's daemon wins the
	// lock race against the one this shim started.
	maxHandovers = 3
)

// ensureDaemon returns a live daemon at least as new as this shim, replacing
// an older one: the newest binary in bin/ always wins.
func (f *forwarder) ensureDaemon(ctx context.Context) (home.DaemonInfo, error) {
	info, err := client.Probe(ctx, f.h)
	running := err == nil
	for attempt := 0; ; attempt++ {
		if running && f.current(info) {
			return info, nil
		}
		if attempt == maxHandovers {
			return info, fmt.Errorf("daemon %s is older than this shim (%s) and could not be replaced", info.Version, f.version)
		}
		if info, err = f.replaceDaemon(ctx, info, running); err != nil {
			return info, err
		}
		running = true
	}
}

// current reports whether the daemon is at least as new as this shim.
func (f *forwarder) current(info home.DaemonInfo) bool {
	return semver.Compare(info.Version, f.version) >= 0
}

// replaceDaemon stops the running (older) daemon, if any, and starts max(bin).
func (f *forwarder) replaceDaemon(ctx context.Context, info home.DaemonInfo, running bool) (home.DaemonInfo, error) {
	// Install first, so the newer version is in bin/ before the old one stops.
	if err := f.installSelf(); err != nil {
		if vs := versions(f.h.BinDir()); len(vs) == 0 || semver.Compare(vs[0], f.version) < 0 {
			return info, fmt.Errorf("install into %s: %w", f.h.BinDir(), err)
		}
		f.log.Warn("install self failed, bin/ has a version as new", "err", err)
	}
	if running {
		f.log.Info("handover: stopping older daemon", "daemon", info.Version, "shim", f.version)
		if err := client.StopDaemon(ctx, f.h, info); err != nil {
			if ctx.Err() != nil {
				return info, ctx.Err()
			}
			// Fine if it is gone anyway (died, or a sibling shim replaced it).
			if now, perr := client.Probe(ctx, f.h); perr == nil && now.PID == info.PID {
				return info, fmt.Errorf("stop older daemon %s: %w", info.Version, err)
			}
		}
	}
	return f.startDaemon(ctx)
}

func (f *forwarder) installSelf() error {
	self, err := selfExe()
	if err != nil {
		return err
	}
	return install(self, f.h.BinDir(), f.version)
}

// selfExe is the path to read this process's own binary from. On Linux the
// file behind os.Executable may have been deleted or replaced by a newer
// build since start; /proc/self/exe always yields the running image.
func selfExe() (string, error) {
	if runtime.GOOS == "linux" {
		return "/proc/self/exe", nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

// startDaemon spawns max(bin) detached and waits for /healthz.
func (f *forwarder) startDaemon(ctx context.Context) (home.DaemonInfo, error) {
	exe, err := maxBin(f.h.BinDir())
	if err != nil {
		return home.DaemonInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return home.DaemonInfo{}, err // nobody waits for this daemon any more
	}
	f.log.Info("starting daemon", "exe", exe)
	// Double spawn via launch-daemon (see LaunchDaemon): it prints the
	// daemon's pid and exits at once.
	var out bytes.Buffer
	cmd, err := proc.Spawn(exe, []string{"launch-daemon"}, f.h.Dir, &out)
	if err != nil {
		return home.DaemonInfo{}, fmt.Errorf("start daemon: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return home.DaemonInfo{}, f.startFailure(fmt.Sprintf("launch-daemon failed: %v", err))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		return home.DaemonInfo{}, f.startFailure(fmt.Sprintf("launch-daemon printed %q, want a pid", out.String()))
	}

	deadline := time.Now().Add(startWait)
	for {
		// This may be another shim's daemon that won the lock race; the
		// caller checks its version.
		if info, err := client.Probe(ctx, f.h); err == nil {
			return info, nil
		}
		// Our daemon is gone and nobody answers: it crashed at start (a lock
		// race loser also exits, but only after the winner is up).
		if !proc.Alive(pid) {
			if info, err := client.Probe(ctx, f.h); err == nil {
				return info, nil
			}
			return home.DaemonInfo{}, f.startFailure("daemon exited during start")
		}
		if time.Now().After(deadline) {
			return home.DaemonInfo{}, f.startFailure(fmt.Sprintf("daemon did not answer within %s", startWait))
		}
		select {
		case <-ctx.Done():
			return home.DaemonInfo{}, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

func (f *forwarder) startFailure(reason string) error {
	logPath := f.h.Log("daemon.log")
	return fmt.Errorf("the whatsapp-mcp daemon failed to start: %s. Last lines of %s:\n%s",
		reason, logPath, strings.Join(home.Tail(logPath, tailLines), "\n"))
}
