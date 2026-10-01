package wa

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	waBinary "go.mau.fi/whatsmeow/binary"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// secretModules log secrets at Debug: QRChannel the QR code itself
// (qrchan.go:97), Recv and Send every node they pass (client.go:867, 971).
// Their Debug never reaches the log, not even with WHATSAPP_MCP_LOG=debug
// (plan 10); their sub-loggers inherit it. The names are whatsmeow's own
// (client.go:263-264, qrchan.go:235); TestWhatsmeowLogSources notices when
// an update renames them.
var secretModules = map[string]bool{"QRChannel": true, "Recv": true, "Send": true}

// secretDebug are Debug formats of the other modules that dump a raw frame:
// a decrypted node that failed to decode (client.go:858, 864).
var secretDebug = map[string]bool{"Errored frame hex: %s": true}

// nodeAttrs are the attributes a logged node keeps: enough to tell which
// node it was and why it failed (<failure reason>, <stream:error code>),
// none of the JIDs, push names or content.
var nodeAttrs = []string{"id", "type", "xmlns", "code", "reason"}

// waLogger writes whatsmeow's printf-style log to the daemon's slog logger,
// with the whatsmeow module path as the sub attribute.
//
// Besides the Debug above, whatsmeow logs whole nodes at every level
// (client.go:929-946 for any node handled slowly, connectionevents.go:67,
// 134, 153), and a node prints its content: a <pair-device> its QR refs, a
// newsletter its message text (binary/xml.go:62-80). So every node argument
// is cut down to nodeSummary. Errors are left whole: an IQError without a
// code prints its node too (errors.go:216-219), but that is the server's
// reply to a query of ours, not a message or a pairing secret. A zerolog
// logger must never go into a context handed to whatsmeow: its zerolog.Ctx
// calls would bypass this adapter.
type waLogger struct {
	log    *slog.Logger
	module string
	quiet  bool // Debug dropped, see secretModules
}

// newWALog returns the logger for a whatsmeow component named module,
// e.g. "Client" or "Database".
func newWALog(log *slog.Logger, module string) waLog.Logger {
	return &waLogger{log: log, module: module, quiet: secretModules[module]}
}

func (w *waLogger) Sub(module string) waLog.Logger {
	return &waLogger{log: w.log, module: w.module + "/" + module, quiet: w.quiet || secretModules[module]}
}

func (w *waLogger) Errorf(msg string, args ...any) { w.logf(slog.LevelError, msg, args) }
func (w *waLogger) Warnf(msg string, args ...any)  { w.logf(slog.LevelWarn, msg, args) }
func (w *waLogger) Infof(msg string, args ...any)  { w.logf(slog.LevelInfo, msg, args) }

func (w *waLogger) Debugf(msg string, args ...any) {
	if w.quiet || secretDebug[msg] {
		return
	}
	w.logf(slog.LevelDebug, msg, args)
}

// logf formats only what the handler keeps, so whatsmeow's chatty Debug
// costs nothing at the default Info level.
func (w *waLogger) logf(level slog.Level, msg string, args []any) {
	ctx := context.Background()
	if w.log.Enabled(ctx, level) {
		w.log.Log(ctx, level, fmt.Sprintf(msg, redact(args)...), "sub", w.module)
	}
}

// redact returns args with every node replaced by its summary; a fresh
// slice, as the caller may own args.
func redact(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch n := a.(type) {
		case *waBinary.Node:
			if n != nil {
				a = nodeSummary(n)
			}
		case waBinary.Node:
			a = nodeSummary(&n)
		}
		out[i] = a
	}
	return out
}

// nodeSummary renders n as its tag, the nodeAttrs it has and the tags of
// its children, e.g. <stream:error code="401"><conflict/>.
func nodeSummary(n *waBinary.Node) string {
	var b strings.Builder
	b.WriteString("<" + n.Tag)
	for _, k := range nodeAttrs {
		if v, ok := n.Attrs[k]; ok {
			fmt.Fprintf(&b, " %s=%q", k, fmt.Sprint(v))
		}
	}
	b.WriteString(">")
	for _, c := range n.GetChildren() {
		b.WriteString("<" + c.Tag + "/>")
	}
	return b.String()
}
