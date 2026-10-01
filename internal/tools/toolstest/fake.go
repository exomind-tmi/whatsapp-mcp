// Package toolstest provides a fake tools.WA, so tests of the tools and of
// the processes serving them do not need the real wa.Manager (with its
// store and connections). It does not import tools: the tools package's own
// tests use it.
package toolstest

import (
	"context"
	"sync"

	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// WA returns canned answers and records every call. Set the fields before
// handing it to a server.
type WA struct {
	Accs    []wa.AccountInfo
	Ticket  wa.LinkTicket
	Removed wa.RemoveResult
	Err     error // returned by Link and Remove

	mu    sync.Mutex
	calls []Call
}

// Call is one recorded call with the arguments the tool passed on.
type Call struct {
	Method string
	Nick   string // Link, Remove
	Phone  string // Link
}

func (f *WA) Accounts(context.Context) []wa.AccountInfo {
	f.record(Call{Method: "Accounts"})
	return f.Accs
}

func (f *WA) Link(_ context.Context, nick, phone string) (wa.LinkTicket, error) {
	f.record(Call{Method: "Link", Nick: nick, Phone: phone})
	return f.Ticket, f.Err
}

func (f *WA) Remove(_ context.Context, nick string) (wa.RemoveResult, error) {
	f.record(Call{Method: "Remove", Nick: nick})
	return f.Removed, f.Err
}

// Calls lists the calls so far, in order.
func (f *WA) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *WA) record(c Call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}
