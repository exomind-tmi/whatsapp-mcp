package toolstest

import (
	"cmp"
	"context"
	"sync"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// Archive returns canned answers and keeps the queries it was asked, for the
// tests that are about what a tool asks of the archive and what it makes of the
// answer. The tests that are about the archive's own behaviour (the order of a
// page, equal times, the cursor) use a real one.
type Archive struct {
	ChatList     []archive.Chat
	Page         archive.Page
	Window       []archive.Message
	Hits         []archive.Hit
	ChatAccounts map[string][]string // AccountsForChat, by chat
	Err          error               // returned by every method
	AroundErr    error               // returned by Around, which Err does not stand for: it may be archive.ErrNoMessage

	mu       sync.Mutex
	chats    []archive.ChatQuery
	messages []archive.MsgQuery
	arounds  []Around
	searches []archive.SearchQuery
	lookups  []string
}

// Around is one call of Around.
type Around struct {
	Account, Chat, ID string
	Before, After     int
}

func (a *Archive) Chats(_ context.Context, q archive.ChatQuery) ([]archive.Chat, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chats = append(a.chats, q)
	return a.ChatList, a.Err
}

func (a *Archive) Messages(_ context.Context, q archive.MsgQuery) (archive.Page, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, q)
	return a.Page, a.Err
}

func (a *Archive) Around(_ context.Context, account, chat, id string, before, after int) ([]archive.Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.arounds = append(a.arounds, Around{account, chat, id, before, after})
	return a.Window, cmp.Or(a.Err, a.AroundErr)
}

func (a *Archive) Search(_ context.Context, q archive.SearchQuery) ([]archive.Hit, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.searches = append(a.searches, q)
	return a.Hits, a.Err
}

func (a *Archive) AccountsForChat(_ context.Context, chat string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lookups = append(a.lookups, chat)
	return a.ChatAccounts[chat], a.Err
}

// Queries is what the archive was asked, by method.
type Queries struct {
	Chats    []archive.ChatQuery
	Messages []archive.MsgQuery
	Arounds  []Around
	Searches []archive.SearchQuery
	Lookups  []string // the chats AccountsForChat was asked about
}

func (a *Archive) Queries() Queries {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Queries{
		Chats:    append([]archive.ChatQuery(nil), a.chats...),
		Messages: append([]archive.MsgQuery(nil), a.messages...),
		Arounds:  append([]Around(nil), a.arounds...),
		Searches: append([]archive.SearchQuery(nil), a.searches...),
		Lookups:  append([]string(nil), a.lookups...),
	}
}

// Asked tells whether the archive was asked anything at all.
func (a *Archive) Asked() bool {
	q := a.Queries()
	return len(q.Chats)+len(q.Messages)+len(q.Arounds)+len(q.Searches)+len(q.Lookups) > 0
}
