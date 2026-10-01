// Package shim is the stdio MCP server a Claude host launches. It answers
// initialize and tools/list itself, instantly and without a daemon, and
// forwards every tools/call to the daemon, starting it when needed.
package shim

import (
	"context"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
)

// Run serves MCP on t until the client disconnects (EOF on stdin).
func Run(ctx context.Context, h home.Home, version string, t mcp.Transport) error {
	home.RotateLog(h.Log("shim.log"))
	log, logFile, err := home.OpenLog(h.Log("shim.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	_ = debug.SetCrashOutput(logFile, debug.CrashOptions{})
	defer debug.SetCrashOutput(nil, debug.CrashOptions{}) // release the duplicated handle
	log.Info("shim started", "version", version)

	f := newForwarder(h, version, log)
	s := tools.NewServer(version, tools.Deps{WA: tools.HandledByDaemon{}})
	s.AddReceivingMiddleware(f.intercept)

	// Warm up: start or hand over the daemon while the client initializes.
	// EOF on stdin cancels it: nobody is left to use that daemon.
	warmCtx, cancelWarm := context.WithCancel(ctx)
	go func() {
		if _, err := f.session(warmCtx); err != nil && warmCtx.Err() == nil {
			log.Error("warm-up", "err", err)
		}
	}()
	err = s.Run(ctx, t)
	cancelWarm()
	f.close()
	log.Info("shim stopped", "err", err)
	return err
}
