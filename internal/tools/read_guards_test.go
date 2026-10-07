package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// Tests of guards that the other tests of the read tools leave to chance: each fails
// when the guard it names is taken out.

// TestAccountGivenWhenThereAreNoAccounts: an account that is named, when none exists,
// is told as the lack of accounts it is, with what to do about it.
func TestAccountGivenWhenThereAreNoAccounts(t *testing.T) {
	r := newRig(t)
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"get-messages", map[string]any{"chat": bobChat, "account": "personal"}},
		{"get-message-context", map[string]any{"chat": bobChat, "message_id": "M1", "account": "personal"}},
	} {
		if text := r.fails(tool.name, tool.args); !strings.Contains(text, "no accounts linked yet") {
			t.Errorf("%s: %s", tool.name, text)
		}
	}
}

// TestSeveralAccountsErrorNamesOnlyThoseWithTheChat: the error that asks for an account
// offers the accounts that have the chat, not all.
func TestSeveralAccountsErrorNamesOnlyThoseWithTheChat(t *testing.T) {
	r := newRig(t, "personal", "work", "other")
	r.put(
		m{account: "personal", chat: carolJID, id: "C1", sender: carolJID, text: "one"},
		m{account: "work", chat: carolJID, id: "C2", sender: carolJID, text: "two"},
	)
	text := r.fails("get-messages", map[string]any{"chat": carolJID})
	if !strings.Contains(text, "pass account: personal, work") || strings.Contains(text, "other") {
		t.Errorf("error = %q", text)
	}
}

// TestAnEmptyPageNamesTheChat: a chat that the account does not have is still the chat
// that was asked for, in the form that the archive would have it.
func TestAnEmptyPageNamesTheChat(t *testing.T) {
	r := bobsRig(t)
	if out := r.messages(map[string]any{"chat": "+7 999 999 99 99"}); out.Chat != "79999999999@s.whatsapp.net" || out.Account != "personal" {
		t.Errorf("chat %q of %q", out.Chat, out.Account)
	}
	if out := r.messages(map[string]any{"chat": bobPN}); out.Chat != bobChat {
		t.Errorf("chat %q", out.Chat)
	}
}

// TestAccountIDsRefusesWhatIsNotAnId: what the schema lets through is all that reaches
// accountIDs, but it does not rely on that.
func TestAccountIDsRefusesWhatIsNotAnId(t *testing.T) {
	for _, bad := range []any{5, true, []any{"a", 1}, []any{[]any{"a"}}, map[string]any{}} {
		if _, err := accountIDs(bad); err != errBadAccount {
			t.Errorf("accountIDs(%v) error %v", bad, err)
		}
	}
	if ids, err := accountIDs([]any{"a", "b"}); err != nil || !slices.Equal(ids, []string{"a", "b"}) {
		t.Errorf("%v %v", ids, err)
	}
}

// TestContextNamesTheSideThatWasLowered: the note says which side was lowered.
func TestContextNamesTheSideThatWasLowered(t *testing.T) {
	r := bobsRig(t)
	r.series("personal", bobChat, "M", 1, 5, false)
	for _, tc := range []struct {
		args map[string]any
		want []string
	}{
		{map[string]any{"before": 0, "after": 100}, []string{"after lowered to the most there is, 20"}},
		{map[string]any{"before": 100, "after": 0}, []string{"before lowered to the most there is, 20"}},
		{map[string]any{"before": 100, "after": 100}, []string{"before lowered to the most there is, 20", "after lowered to the most there is, 20"}},
	} {
		args := map[string]any{"chat": bobChat, "message_id": "M3"}
		for k, v := range tc.args {
			args[k] = v
		}
		if out := r.around(args); !slices.Equal(out.Notes, tc.want) {
			t.Errorf("%v: notes %q, want %q", tc.args, out.Notes, tc.want)
		}
	}
}

// countingWA gives each account its own names, and counts the questions.
type countingWA struct {
	*toolstest.WA
	mu     sync.Mutex
	namers map[string]int
	asked  map[[2]string]int
	names  map[string]map[string]string // by account, by JID
}

func (c *countingWA) Names(_ context.Context, nick string) wa.Namer {
	c.mu.Lock()
	c.namers[nick]++
	c.mu.Unlock()
	return func(jid string) string {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.asked[[2]string{nick, jid}]++
		return c.names[nick][jid]
	}
}

func rigWithNames(t *testing.T) (*rig, *countingWA) {
	t.Helper()
	r := newRig(t, "personal", "work")
	c := &countingWA{WA: r.wa, namers: map[string]int{}, asked: map[[2]string]int{},
		names: map[string]map[string]string{"personal": {bobChat: "Bob of personal", carolJID: "Carol of personal"}, "work": {bobChat: "Bob of work", carolJID: "Carol of work"}}}
	r.cs = serve(t, Deps{WA: c, Archive: r.db})
	return r, c
}

// TestNamesAreAskedOnceAndPerAccount: a person's name is asked for once per account,
// and is the name that account has: the same person may be called differently in each.
func TestNamesAreAskedOnceAndPerAccount(t *testing.T) {
	r, c := rigWithNames(t)
	for i := range 6 {
		sender := []string{bobChat, carolJID}[i%2] // two people, so that a namer is asked for more than once
		r.put(
			m{account: "personal", chat: groupJID, id: "P" + string(rune('A'+i)), sender: sender, at: time.Duration(i) * time.Second, text: "needle one"},
			m{account: "work", chat: groupJID, id: "W" + string(rune('A'+i)), sender: sender, at: time.Duration(i) * time.Second, text: "needle two"},
		)
	}
	got := map[string]string{}
	for _, h := range r.search(map[string]any{"query": "needle", "limit": 100}).Results {
		got[h.Account+h.Sender] = h.SenderName
	}
	want := map[string]string{"personal" + bobChat: "Bob of personal", "work" + bobChat: "Bob of work", "personal" + carolJID: "Carol of personal", "work" + carolJID: "Carol of work"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sender names %v, want %v: each is the name the account has", got, want)
	}
	if c.namers["personal"] != 1 || c.namers["work"] != 1 {
		t.Errorf("namers asked for: %v, want one for each account", c.namers)
	}
	for key, n := range c.asked {
		if n != 1 {
			t.Errorf("%v was asked for %d times, want once", key, n)
		}
	}
}

// TestSearchOfAChatBehindTheStoreBreaksTiesByStoring: messages of the same second in
// accounts that have the chat under two JIDs come in the order the archive gives one
// chat: the one stored later first.
func TestSearchOfAChatBehindTheStoreBreaksTiesByStoring(t *testing.T) {
	r := newRig(t, "personal", "work")
	r.wa.Pairs = []string{bobLID, bobPN}
	r.put(
		m{account: "personal", chat: bobChat, id: "P1", sender: bobChat, at: 10 * time.Second, text: "lunch in personal"},
		m{account: "work", chat: bobPNJID, id: "W1", sender: bobPNJID, at: 10 * time.Second, text: "lunch in work"},
	)
	if got := hitIDs(r.search(map[string]any{"query": "lunch", "chat": bobPN})); !slices.Equal(got, []string{"work:W1", "personal:P1"}) {
		t.Errorf("got %v, want the one stored later first", got)
	}
	r.put(m{account: "personal", chat: bobChat, id: "P2", sender: bobChat, at: 10 * time.Second, text: "lunch again in personal"})
	if got := hitIDs(r.search(map[string]any{"query": "lunch", "chat": bobPN})); !slices.Equal(got, []string{"personal:P2", "work:W1", "personal:P1"}) {
		t.Errorf("got %v", got)
	}
}

// TestListChatsWithABlankQueryDoesNotLookPeopleUp: a blank query is no name to look
// for in the contacts.
func TestListChatsWithABlankQueryDoesNotLookPeopleUp(t *testing.T) {
	r := contactsRig(t)
	for _, q := range []string{" ", "   ", "\t"} {
		r.call("list-chats", map[string]any{"query": q})
	}
	for _, c := range r.wa.Calls() {
		if c.Method == "SenderJIDs" {
			t.Errorf("a blank query was looked up in the contacts: %+v", c)
		}
	}
}

// oneAccount is the shim's stand-in with an account to read, so that a call goes on to
// the parts that must fail.
type oneAccount struct{ HandledByDaemon }

func (oneAccount) Roster(context.Context) []wa.AccountInfo {
	return []wa.AccountInfo{{Nick: "a", Status: wa.StatusConnected}}
}

// TestEveryPartTheShimPassesFailsTheCall: whatever of the stand-ins a call reaches, it
// fails there as a read that could not be done, and does not fall over.
func TestEveryPartTheShimPassesFailsTheCall(t *testing.T) {
	cs := serve(t, Deps{WA: oneAccount{}, Archive: HandledByDaemon{}})
	for name, args := range map[string]map[string]any{
		"list-chats":           {},
		"get-messages":         {"chat": bobChat},
		"get-message-context":  {"chat": bobChat, "message_id": "M1"},
		"search-messages":      {"query": "hello"},
		"search-messages/chat": {"query": "hello", "chat": bobChat},
	} {
		tool := strings.SplitN(name, "/", 2)[0]
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil || !res.IsError || ResultText(res) != errReadFailed.Error() {
			t.Errorf("%s: %v %v", name, err, res)
		}
	}
}

// TestInstructionsSayNotToQuoteADeletedMessage: the server's own words tell the agent not
// to quote what was deleted.
func TestInstructionsSayNotToQuoteADeletedMessage(t *testing.T) {
	if !strings.Contains(Instructions, "do not quote it to the other person") {
		t.Errorf("the instructions do not say it: %s", Instructions)
	}
}

// TestHostileQueryIsNotRepeatedInNotes: what the agent was given to look for does not
// come back in a note either.
func TestHostileQueryIsNotRepeatedInNotes(t *testing.T) {
	r := contactsRig(t)
	r.wa.LookupErr = wa.ErrTooManyPeople
	for _, h := range hostile {
		var out ChatsOut
		r.ok("list-chats", map[string]any{"query": h}, &out)
		if len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "too many people") {
			t.Fatalf("notes = %q", out.Notes)
		}
		if strings.Contains(out.Notes[0], h) {
			t.Errorf("the note repeats the query %q: %s", h, out.Notes[0])
		}
	}
}

// failing is an archive whose later calls fail, with the first (who has the chat) answering.
type failing struct {
	*toolstest.Archive
	messages, around, chats error
}

func (f failing) AccountsForChat(ctx context.Context, jid string) ([]string, error) {
	if f.chats != nil {
		return nil, f.chats
	}
	return f.Archive.AccountsForChat(ctx, jid)
}

func (f failing) Messages(ctx context.Context, q archive.MsgQuery) (archive.Page, error) {
	if f.messages != nil {
		return archive.Page{}, f.messages
	}
	return f.Archive.Messages(ctx, q)
}

func (f failing) Around(ctx context.Context, account, chat, id string, before, after int) ([]archive.Message, error) {
	if f.around != nil {
		return nil, f.around
	}
	return f.Archive.Around(ctx, account, chat, id, before, after)
}

// TestLaterArchiveFailuresAreNotTheAgents: an archive that fails after the lookup of the
// chat is no more the agent's business than one that fails at the lookup: the agent gets
// a reason that holds nothing of it, and the log the error.
func TestLaterArchiveFailuresAreNotTheAgents(t *testing.T) {
	sqlErr := errors.New(`SQL logic error: no such table: messages (1) at C:\Users\someone\.mcp\archive.db`)
	for _, tc := range []struct {
		tool string
		args map[string]any
		arc  failing
	}{
		{"get-messages", map[string]any{"chat": bobChat}, failing{Archive: &toolstest.Archive{}, messages: sqlErr}},
		{"get-message-context", map[string]any{"chat": bobChat, "message_id": "M1"}, failing{Archive: &toolstest.Archive{}, around: sqlErr}},
	} {
		w := &toolstest.WA{Accs: []wa.AccountInfo{{Nick: "personal", Status: wa.StatusConnected}}}
		log := &syncLog{}
		cs := serve(t, Deps{WA: w, Archive: tc.arc, Log: slog.New(slog.NewTextHandler(log, nil))})
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil || !res.IsError || ResultText(res) != errReadFailed.Error() {
			t.Errorf("%s: %v %q", tc.tool, err, ResultText(res))
		}
		if !strings.Contains(log.String(), "no such table") {
			t.Errorf("%s: the log lacks the error: %s", tc.tool, log.String())
		}
	}
}

// TestSearchOfAChatWhoseLookupFailsDoesNotSayItIsNotThere: an empty result of a search in
// a chat is said to be of a chat that no account has only when the archive has said so; a
// failed lookup says nothing, and the result is no worse for it.
func TestSearchOfAChatWhoseLookupFailsDoesNotSayItIsNotThere(t *testing.T) {
	w := &toolstest.WA{Accs: []wa.AccountInfo{{Nick: "personal", Status: wa.StatusConnected}}}
	arc := failing{Archive: &toolstest.Archive{}, chats: errors.New("SQL logic error: database is locked")}
	cs := serve(t, Deps{WA: w, Archive: arc})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search-messages", Arguments: map[string]any{"query": "hello", "chat": bobChat}})
	if err != nil || res.IsError {
		t.Fatalf("%v %v", err, ResultText(res))
	}
	if text := ResultText(res); strings.Contains(text, "none of the accounts searched") {
		t.Errorf("a failed lookup is told as a chat that is not there: %s", text)
	}
}
