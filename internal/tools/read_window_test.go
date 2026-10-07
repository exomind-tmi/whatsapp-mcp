package tools

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// pairKnownRig has the store know that Bob's number and LID are one person, while
// the account work, which the pair did not come through, still has the chat with
// him under his number: the archive is brought in line with the store by a message
// of the chat or by the next connect. The account personal is in line already.
func pairKnownRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, "personal", "work")
	r.wa.Pairs = []string{bobLID, bobPN}
	r.chat("personal", bobChat, bobPNJID, "Bob", false)
	r.chat("work", bobPNJID, "", "Bob", false)
	r.put(
		m{account: "personal", chat: bobChat, id: "P1", sender: bobChat, at: 10 * time.Second, text: "lunch in personal"},
		m{account: "work", chat: bobPNJID, id: "W1", sender: bobPNJID, at: 20 * time.Second, text: "lunch in work"},
		m{account: "personal", chat: bobChat, id: "P2", sender: bobChat, at: 30 * time.Second, text: "lunch again in personal"},
		m{account: "work", chat: bobPNJID, id: "W2", sender: bobPNJID, at: 40 * time.Second, text: "lunch again in work"},
	)
	return r
}

// TestAChatThatHasNotCaughtUpWithTheStore: the chat is found whichever way it is
// asked for, in each account under the JID that account has it under; the chat that
// list-chats gave is passed back as it is.
func TestAChatThatHasNotCaughtUpWithTheStore(t *testing.T) {
	r := pairKnownRig(t)
	for _, chat := range []string{bobPN, bobPNJID, bobChat, "+7 000 000 0100"} {
		if got := r.messages(map[string]any{"chat": chat, "account": "work"}); got.Chat != bobPNJID || !slices.Equal(ids(got.Messages), []string{"W1", "W2"}) {
			t.Errorf("chat %q in work: %s %v, want what work has, under the number", chat, got.Chat, ids(got.Messages))
		}
		if got := r.messages(map[string]any{"chat": chat, "account": "personal"}); got.Chat != bobChat || !slices.Equal(ids(got.Messages), []string{"P1", "P2"}) {
			t.Errorf("chat %q in personal: %s %v, want what personal has, under the LID", chat, got.Chat, ids(got.Messages))
		}
	}
	// What list-chats says is the chat of each account, as that account has it.
	var chats ChatsOut
	r.ok("list-chats", nil, &chats)
	if got := chatIDs(chats); !slices.Equal(got, []string{bobPNJID, bobChat}) {
		t.Fatalf("chats %v", got)
	}
	for _, c := range chats.Chats {
		if got := r.messages(map[string]any{"chat": c.Chat, "account": c.Account}); len(got.Messages) != 2 || got.Chat != c.Chat {
			t.Errorf("the chat %s of %s, as list-chats gave it: %d messages of %s", c.Chat, c.Account, len(got.Messages), got.Chat)
		}
	}
	// The account of the chat is not told by the one that has it in line, but by both.
	text := r.fails("get-messages", map[string]any{"chat": bobPN})
	if !strings.Contains(text, "several accounts") || !strings.Contains(text, "personal, work") {
		t.Errorf("a chat that two accounts have under two JIDs: %s", text)
	}
	// A chat that only the account behind has, under its number, is its own.
	only := newRig(t, "personal", "work")
	only.wa.Pairs = []string{bobLID, bobPN}
	only.put(m{account: "work", chat: bobPNJID, id: "W1", sender: bobPNJID, text: "only here"})
	if got := only.messages(map[string]any{"chat": bobPN}); got.Account != "work" || got.Chat != bobPNJID || len(got.Messages) != 1 {
		t.Errorf("by the number: %+v", got)
	}
}

func TestAContextOfAChatThatHasNotCaughtUpWithTheStore(t *testing.T) {
	r := pairKnownRig(t)
	got := r.around(map[string]any{"chat": bobPNJID, "account": "work", "message_id": "W1", "before": 0})
	if got.Chat != bobPNJID || !slices.Equal(contextIDs(got), []string{"W1", "W2"}) || !got.Messages[0].Target {
		t.Errorf("context in work: %+v", got)
	}
	got = r.around(map[string]any{"chat": bobPN, "account": "personal", "message_id": "P2", "after": 0, "before": 1})
	if got.Chat != bobChat || !slices.Equal(contextIDs(got), []string{"P1", "P2"}) {
		t.Errorf("context in personal: %+v", got)
	}
}

// TestSearchInAChatThatHasNotCaughtUpWithTheStore: a search in the chat covers the
// accounts under the JID each has it under, the newest first of all of them, and the
// limit is of the whole.
func TestSearchInAChatThatHasNotCaughtUpWithTheStore(t *testing.T) {
	r := pairKnownRig(t)
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"both accounts, by number", map[string]any{"chat": bobPN}, []string{"work:W2", "personal:P2", "work:W1", "personal:P1"}},
		{"both accounts, by the LID", map[string]any{"chat": bobChat}, []string{"work:W2", "personal:P2", "work:W1", "personal:P1"}},
		{"both accounts, by the number's JID", map[string]any{"chat": bobPNJID}, []string{"work:W2", "personal:P2", "work:W1", "personal:P1"}},
		{"the limit is of the whole", map[string]any{"chat": bobPN, "limit": 3}, []string{"work:W2", "personal:P2", "work:W1"}},
		{"one account behind", map[string]any{"chat": bobPN, "account": "work"}, []string{"work:W2", "work:W1"}},
		{"one account in line", map[string]any{"chat": bobPN, "account": []any{"personal"}}, []string{"personal:P2", "personal:P1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"query": "lunch"}
			for k, v := range tc.args {
				args[k] = v
			}
			if got := hitIDs(r.search(args)); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAChatUnderItsLIDWithItsNumberIsOneAccounts: the archive finds the account of a
// chat by its JID and by its number, which a chat that is under its LID with its
// number beside it answers twice, and is still the one account.
func TestAChatUnderItsLIDWithItsNumberIsOneAccount(t *testing.T) {
	r := newRig(t, "personal", "work")
	r.wa.Pairs = []string{bobLID, bobPN}
	r.chat("personal", bobChat, bobPNJID, "Bob", false)
	r.put(m{account: "personal", chat: bobChat, id: "P1", sender: bobChat, text: "hello"})
	for _, chat := range []string{bobPN, bobPNJID, bobChat} {
		if got := r.messages(map[string]any{"chat": chat}); got.Account != "personal" || len(got.Messages) != 1 {
			t.Errorf("chat %q: %d messages of %q", chat, len(got.Messages), got.Account)
		}
	}
}

// twoRowsRig has one account with Bob in two rows, as an archive that has not caught
// up with the store has him: the messages from before his LID was known under his
// number, the later ones under the LID. The store knows the pair (it learned it from
// another account), and the archive merges the rows only at the next message of the
// chat or at a connect.
func twoRowsRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, "personal")
	r.wa.Pairs = []string{bobLID, bobPN}
	r.put(
		m{account: "personal", chat: bobPNJID, id: "M1", sender: bobPNJID, at: 1 * time.Second, text: "needle one"},
		m{account: "personal", chat: bobPNJID, id: "M2", sender: bobPNJID, at: 2 * time.Second, text: "needle two"},
		m{account: "personal", chat: bobChat, id: "M3", sender: bobChat, at: 3 * time.Second, text: "needle three"},
	)
	return r
}

// TestTwoRowsOfOnePersonAreReadTogether: the chat of one account that is in two rows is
// read as one, whichever way it is asked for, in every tool; reading one of the rows
// would show a part of the chat and say nothing of the other.
func TestTwoRowsOfOnePersonAreReadTogether(t *testing.T) {
	r := twoRowsRig(t)
	for _, chat := range []string{bobChat, bobPNJID, bobPN, "+7 000 000 0100"} {
		got := r.messages(map[string]any{"chat": chat, "account": "personal"})
		if !slices.Equal(ids(got.Messages), []string{"M1", "M2", "M3"}) || got.Chat != bobChat {
			t.Errorf("get-messages chat=%q: %v of %s, want M1 M2 M3 of the LID, the row of the newest", chat, ids(got.Messages), got.Chat)
		}
		if got := hitIDs(r.search(map[string]any{"query": "needle", "chat": chat})); !slices.Equal(got, []string{"personal:M3", "personal:M2", "personal:M1"}) {
			t.Errorf("search chat=%q: %v", chat, got)
		}
		if got := r.messages(map[string]any{"chat": chat, "after": iso(2 * time.Second)}); !slices.Equal(ids(got.Messages), []string{"M2", "M3"}) {
			t.Errorf("get-messages chat=%q after: %v", chat, ids(got.Messages))
		}
	}
	// The window of a message is where the message is, whichever row it is in.
	for _, chat := range []string{bobChat, bobPNJID} {
		if got := r.around(map[string]any{"chat": chat, "message_id": "M2", "before": 0, "after": 0}); !slices.Equal(contextIDs(got), []string{"M2"}) {
			t.Errorf("the context of M2, asked as %s: %v", chat, got.Messages)
		}
	}
	// Every hit of a search with no chat gives a chat that its context finds.
	for _, h := range r.search(map[string]any{"query": "needle"}).Results {
		if res, text := r.call("get-message-context", map[string]any{"account": h.Account, "chat": h.Chat, "message_id": h.ID}); res.IsError {
			t.Errorf("the context of hit %s of chat %s: %s", h.ID, h.Chat, text)
		}
	}
	if text := r.fails("get-message-context", map[string]any{"chat": bobChat, "message_id": "M9"}); text != errNoSuchMessage.Error() {
		t.Errorf("a message that is in no row: %s", text)
	}
}

// TestPagingGoesBackOverTwoRowsOfOnePerson: the pages of a chat in two rows are one run
// of the chat: by time and then by the order the messages were stored in, the cursor of
// a page is the position of its oldest message in that run, and no message is skipped or
// shown twice, among those of one second as well.
func TestPagingGoesBackOverTwoRowsOfOnePerson(t *testing.T) {
	r := newRig(t, "personal")
	r.wa.Pairs = []string{bobLID, bobPN}
	r.put( // stored in this order: the order of the same second is the order of storing
		m{account: "personal", chat: bobPNJID, id: "A1", sender: bobPNJID, at: 1 * time.Second, text: "a1"},
		m{account: "personal", chat: bobPNJID, id: "A2", sender: bobPNJID, at: 5 * time.Second, text: "a2"},
		m{account: "personal", chat: bobChat, id: "B1", sender: bobChat, at: 5 * time.Second, text: "b1"},
		m{account: "personal", chat: bobPNJID, id: "A3", sender: bobPNJID, at: 5 * time.Second, text: "a3"},
		m{account: "personal", chat: bobChat, id: "B2", sender: bobChat, at: 9 * time.Second, text: "b2"},
	)
	for _, tc := range []struct {
		limit int
		want  [][]string
	}{
		{2, [][]string{{"A3", "B2"}, {"A2", "B1"}, {"A1"}}},
		{1, [][]string{{"B2"}, {"A3"}, {"B1"}, {"A2"}, {"A1"}}},
		{3, [][]string{{"B1", "A3", "B2"}, {"A1", "A2"}}},
		{5, [][]string{{"A1", "A2", "B1", "A3", "B2"}}},
		{50, [][]string{{"A1", "A2", "B1", "A3", "B2"}}},
	} {
		args := map[string]any{"chat": bobPN, "limit": tc.limit}
		for i, want := range tc.want {
			got := r.messages(args)
			if !slices.Equal(ids(got.Messages), want) {
				t.Fatalf("limit %d, page %d: %v, want %v", tc.limit, i+1, ids(got.Messages), want)
			}
			if last := i == len(tc.want)-1; last != (got.NextBefore == "") {
				t.Fatalf("limit %d, page %d: next_before %q", tc.limit, i+1, got.NextBefore)
			}
			args = map[string]any{"chat": bobPN, "limit": tc.limit, "before": got.NextBefore}
		}
	}
}

// TestSearchOfAChatThatIsNotThereSaysSo: a search of a chat that has no match says
// nothing of the chat; one of a chat that no account searched has says so.
func TestSearchOfAChatThatIsNotThereSaysSo(t *testing.T) {
	r := newRig(t, "personal")
	r.put(m{account: "personal", chat: bobChat, id: "M1", sender: bobChat, text: "needle"})
	out := r.search(map[string]any{"query": "needle", "chat": "70009999999"})
	if len(out.Results) != 0 || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "none of the accounts searched has this chat") {
		t.Errorf("results %d, notes %q: nothing says that the chat is not in the archive", len(out.Results), out.Notes)
	}
	// A chat that is there, with no match, is no such note, and neither is no chat at all.
	out = r.search(map[string]any{"query": "absent", "chat": bobChat})
	if len(out.Notes) != 0 {
		t.Errorf("a chat that is in the archive, and has no match: notes %q", out.Notes)
	}
	if out = r.search(map[string]any{"query": "absent"}); len(out.Notes) != 0 {
		t.Errorf("a search that asked for no chat, and has no match: notes %q", out.Notes)
	}
	// A chat that an account has under the number only, which the store knows the LID of,
	// is there as well: the lookup is of both of its JIDs.
	behind := pairKnownRig(t)
	if out := behind.search(map[string]any{"query": "absent", "chat": bobChat, "account": "work"}); len(out.Results) != 0 || len(out.Notes) != 0 {
		t.Errorf("a chat that is under the number: %d results, notes %q", len(out.Results), out.Notes)
	}
	// A chat that is in the archive of an account that is not searched is not there for this search.
	two := newRig(t, "personal", "work")
	two.put(m{account: "personal", chat: bobChat, id: "M1", sender: bobChat, text: "needle"})
	if out := two.search(map[string]any{"query": "needle", "chat": bobChat, "account": "work"}); len(out.Results) != 0 || len(out.Notes) != 1 {
		t.Errorf("the chat of another account: %d results, notes %q", len(out.Results), out.Notes)
	}
}
