// Package toolstest provides a fake tools.WA and a fake tools.Archive, so tests
// of the tools and of the processes serving them do not need the real
// wa.Manager (with its store and connections). It does not import tools: the
// tools package's own tests use it.
package toolstest

import (
	"context"
	"sync"

	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// WA returns canned answers and records every call. Set the fields before
// handing it to a server.
type WA struct {
	Accs    []wa.AccountInfo
	Ticket  wa.LinkTicket
	Removed wa.RemoveResult
	Err     error // returned by Link and Remove

	// Pairs are the people whose number and LID the fake LID store knows: lid, pn,
	// lid, pn... (user parts, as in 200000000100, 70000000100). CanonicalChat
	// answers by them, as the real one does by its store.
	Pairs []string
	// Senders is what SenderJIDs answers for a text; a text that is not here is
	// named by nobody.
	Senders map[string][]string
	// People are the names Names gives, by JID, as the contact store would: a
	// person has an entry for each address that should find them.
	People map[string]string
	// LookupErr is returned by CanonicalChat and SenderJIDs, as a store that cannot be read.
	LookupErr error

	mu    sync.Mutex
	calls []Call
}

// Call is one recorded call with the arguments the tool passed on.
type Call struct {
	Method  string
	Nick    string // Link, Remove, SenderJIDs, Names
	Phone   string // Link
	QRImage bool   // Link
	Chat    string // CanonicalChat
	Sender  string // SenderJIDs
}

func (f *WA) Accounts(context.Context) []wa.AccountInfo {
	f.record(Call{Method: "Accounts"})
	return f.Accs
}

func (f *WA) Roster(context.Context) []wa.AccountInfo {
	f.record(Call{Method: "Roster"})
	return f.Accs
}

func (f *WA) Link(_ context.Context, req wa.LinkRequest) (wa.LinkTicket, error) {
	f.record(Call{Method: "Link", Nick: req.Nick, Phone: req.Phone, QRImage: req.QRImage})
	return f.Ticket, f.Err
}

func (f *WA) Remove(_ context.Context, nick string) (wa.RemoveResult, error) {
	f.record(Call{Method: "Remove", Nick: nick})
	return f.Removed, f.Err
}

func (f *WA) CanonicalChat(ctx context.Context, chat string) (wa.Chat, error) {
	f.record(Call{Method: "CanonicalChat", Chat: chat})
	if f.LookupErr != nil {
		return wa.Chat{}, f.LookupErr
	}
	return wa.CanonicalChat(ctx, pairs(f.Pairs), chat)
}

func (f *WA) SenderJIDs(_ context.Context, nick, sender string) ([]string, error) {
	f.record(Call{Method: "SenderJIDs", Nick: nick, Sender: sender})
	if f.LookupErr != nil {
		return nil, f.LookupErr
	}
	return f.Senders[sender], nil
}

func (f *WA) Names(_ context.Context, nick string) wa.Namer {
	f.record(Call{Method: "Names", Nick: nick})
	return func(jid string) string { return f.People[jid] }
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

// pairs is a LID store that knows the pairs it was made of.
type pairs []string

func (p pairs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	for i := 0; i+1 < len(p); i += 2 {
		if p[i+1] == pn.User {
			return types.NewJID(p[i], types.HiddenUserServer), nil
		}
	}
	return types.EmptyJID, nil
}

func (p pairs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	for i := 0; i+1 < len(p); i += 2 {
		if p[i] == lid.User {
			return types.NewJID(p[i+1], types.DefaultUserServer), nil
		}
	}
	return types.EmptyJID, nil
}
