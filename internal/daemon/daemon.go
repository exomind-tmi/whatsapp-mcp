// Package daemon is the single per-machine process that owns WhatsApp
// connections and the archive and serves MCP over loopback HTTP.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/proc"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

const (
	lockWait        = 5 * time.Second // covers an old daemon still shutting down during handover
	shutdownTimeout = 30 * time.Second
)

// Option changes how Run builds the daemon, for tests that run it in-process.
type Option func(*options)

type options struct{ offline bool }

// WithoutWhatsApp makes the account manager never reach WhatsApp: no version
// check, no connection (wa.Config.Offline).
func WithoutWhatsApp() Option { return func(o *options) { o.offline = true } }

// Run serves until ctx is cancelled or /admin/stop is called. Losing the
// lock race is not an error: another daemon is already serving.
func Run(ctx context.Context, h home.Home, version string, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	lock, lockErr := acquire(ctx, h.LockFile())
	if lockErr == nil {
		defer lock.Unlock()
		home.RotateLog(h.Log("daemon.log")) // only the lock holder owns the log
	}
	log, logFile, err := home.OpenLog(h.Log("daemon.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	_ = debug.SetCrashOutput(logFile, debug.CrashOptions{}) // panics land in daemon.log, not in NUL
	defer debug.SetCrashOutput(nil, debug.CrashOptions{})   // release the duplicated handle

	switch {
	case errors.Is(lockErr, proc.ErrLocked):
		log.Info("another daemon holds the lock, exiting")
		return nil
	case lockErr != nil:
		err = fmt.Errorf("lock: %w", lockErr)
	default:
		err = run(ctx, h, version, log, o)
	}
	if err != nil {
		log.Error("daemon failed", "err", err)
		return err
	}
	log.Info("stopped") // once run has closed everything
	return nil
}

// run serves while the caller holds the lock.
func run(ctx context.Context, h home.Home, version string, log *slog.Logger, o options) error {
	// Deferred first so it runs after the WA and archive close and just
	// before the unlock. A shim waiting for daemon.json to
	// vanish then finds the lock free within its 5 s retry.
	defer func() {
		if err := h.RemoveDaemonInfo(); err != nil {
			log.Warn("remove daemon.json", "err", err)
		}
	}()
	log.Info("starting", "version", version, "home", h.Dir)

	token, err := ensureToken(h)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	db, err := archive.Open(h.ArchiveDB())
	if err != nil {
		return err
	}
	defer db.Close()

	// Listen before building the Manager: the login pages it hands out
	// live on this port. Serve closes ln; the defer covers a failure before.
	// Serving starts only once the Manager is built (see wa.Manager.load).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	accounts, err := wa.NewManager(ctx, wa.Config{
		Log:       log,
		StorePath: h.StoreDB(),
		Archive:   db,
		BaseURL:   fmt.Sprintf("http://127.0.0.1:%d", port),
		Offline:   o.offline,
	})
	if err != nil {
		return err
	}
	defer func() { // after srv.Shutdown below, before db.Close
		if err := accounts.Close(); err != nil {
			log.Warn("close accounts", "err", err)
		}
	}()

	stop := make(chan struct{})
	srv := &http.Server{
		Handler: newMux(muxDeps{
			version: version,
			token:   token,
			server:  tools.NewServer(version, tools.Deps{WA: accounts}),
			stop:    stop,
			log:     log,
			login:   accounts,
			port:    port,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute, // /login/ is open to any local process: idle connections must not pile up
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	if err := h.WriteDaemonInfo(home.DaemonInfo{PID: os.Getpid(), Port: port, Version: version, StartedAt: time.Now()}); err != nil {
		srv.Close()
		return fmt.Errorf("write daemon.json: %w", err)
	}
	log.Info("listening", "port", port)

	select {
	case <-ctx.Done():
		log.Info("stopping: signal")
	case <-stop:
		log.Info("stopping: /admin/stop")
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	return nil
}

func acquire(ctx context.Context, path string) (*proc.Lock, error) {
	deadline := time.Now().Add(lockWait)
	for {
		l, err := proc.TryLock(path)
		if !errors.Is(err, proc.ErrLocked) || time.Now().After(deadline) {
			return l, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ensureToken creates the bearer token once; only the daemon, under the lock,
// ever writes it.
func ensureToken(h home.Home) (string, error) {
	t, err := h.ReadToken()
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	rand.Read(b)
	t = hex.EncodeToString(b)
	if err := home.WriteAtomic(h.TokenFile(), []byte(t), 0o600); err != nil {
		return "", err
	}
	return t, nil
}
