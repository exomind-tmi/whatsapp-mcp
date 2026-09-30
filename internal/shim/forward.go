package shim

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/client"
	"github.com/exomind-tmi/whatsapp-mcp/internal/home"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools"
)

const (
	msgOutdated = "whatsapp-mcp was updated and this session's tool list is outdated: ask the user to restart the session " +
		"(Claude Desktop: Quit from the tray icon and open it again)."
	msgUnconfirmed = "the connection to the whatsapp-mcp daemon broke during the call, so it is unknown whether it took effect " +
		"(for a send: the message may or may not have been sent). Check the result, e.g. with get-messages, before retrying. Error: "
)

// forwarder sends every tools/call to the daemon over one cached MCP session.
type forwarder struct {
	h       home.Home
	version string
	log     *slog.Logger
	// known and readOnly describe this build's tools (tools.Known and
	// tools.ReadOnly); tests substitute them.
	known, readOnly func(name string) bool

	// lock guards sess and port. It is a one-slot semaphore rather than a
	// sync.Mutex so that a call waiting for a daemon start honours its ctx.
	lock chan struct{}
	sess *mcp.ClientSession
	port int
}

func newForwarder(h home.Home, version string, log *slog.Logger) *forwarder {
	return &forwarder{h: h, version: version, log: log, known: tools.Known, readOnly: tools.ReadOnly, lock: make(chan struct{}, 1)}
}

func (f *forwarder) acquire(ctx context.Context) error {
	select {
	case f.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *forwarder) release() { <-f.lock }

// detach forgets the cached session and closes it in the background: Close
// waits for the session's calls in flight, which must not hold the lock.
// The caller holds the lock.
func (f *forwarder) detach() {
	if old := f.sess; old != nil {
		f.sess = nil
		go old.Close()
	}
}

// intercept is a receiving middleware: every tools/call goes to the daemon,
// so the shim's own (stub) handlers are never reached.
func (f *forwarder) intercept(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); ok && method == "tools/call" {
			return f.call(ctx, call.Params)
		}
		return next(ctx, method, req)
	}
}

func (f *forwarder) call(ctx context.Context, p *mcp.CallToolParamsRaw) (*mcp.CallToolResult, error) {
	params := &mcp.CallToolParams{Meta: p.Meta, Name: p.Name}
	if len(p.Arguments) > 0 {
		params.Arguments = p.Arguments
	}
	for attempt := 0; ; attempt++ {
		sess, err := f.session(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			f.log.Error("no daemon", "err", err)
			return errorResult(err.Error()), nil
		}
		res, err := sess.CallTool(ctx, params)
		if err == nil {
			return res, nil
		}
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) && !sdkLocal(rpcErr) {
			if rpcErr.Code == jsonrpc.CodeInvalidParams && strings.HasPrefix(rpcErr.Message, "unknown tool") && f.known(p.Name) {
				return errorResult(msgOutdated), nil
			}
			return nil, err // a protocol answer from the daemon: pass it through
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		f.log.Warn("transport error", "tool", p.Name, "attempt", attempt, "err", err)
		f.drop(ctx, sess)
		if attempt > 0 || !f.readOnly(p.Name) {
			return errorResult(msgUnconfirmed + err.Error()), nil
		}
	}
}

// session returns a session to a live daemon at least as new as this shim,
// starting or replacing the daemon when needed. Calls wait here while a
// daemon is being started.
func (f *forwarder) session(ctx context.Context) (*mcp.ClientSession, error) {
	if err := f.acquire(ctx); err != nil {
		return nil, err
	}
	defer f.release()
	if f.sess != nil {
		if info, err := client.Probe(ctx, f.h); err == nil && info.Port == f.port && f.current(info) {
			return f.sess, nil
		}
		f.detach()
	}
	info, err := f.ensureDaemon(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := client.Connect(ctx, f.h, info.Port, f.version)
	if err != nil {
		return nil, err
	}
	f.log.Info("connected to daemon", "version", info.Version, "port", info.Port)
	f.sess, f.port = sess, info.Port
	return sess, nil
}

func (f *forwarder) drop(ctx context.Context, sess *mcp.ClientSession) {
	if f.acquire(ctx) != nil {
		return // cancelled: the next call that fails on sess drops it
	}
	defer f.release()
	if f.sess == sess {
		f.detach()
	}
}

// close runs once the stdio session has ended and warm-up is cancelled, so
// the lock is free or about to be.
func (f *forwarder) close() {
	f.lock <- struct{}{}
	defer f.release()
	if f.sess != nil {
		f.sess.Close()
		f.sess = nil
	}
}

// sdkLocal reports whether the SDK made the error up on this side instead of
// receiving it from the daemon: -32003 client closing, -32004 server closing,
// -32005 "rejected by transport", which wraps a failed POST (the daemon died
// or dropped the connection mid-call). The SDK does not export these codes.
func sdkLocal(e *jsonrpc.Error) bool {
	return e.Code >= -32005 && e.Code <= -32003
}

func errorResult(msg string) *mcp.CallToolResult {
	var r mcp.CallToolResult
	r.SetError(errors.New(msg))
	return &r
}
