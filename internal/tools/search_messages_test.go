package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

func hitIDs(out SearchOut) []string {
	ids := make([]string, len(out.Results))
	for i, h := range out.Results {
		ids[i] = h.Account + ":" + h.ID
	}
	return ids
}

// TestSearchMessages: the results of a search over several accounts, newest first
// (the order of storing breaking a tie), with what each is: the account, the chat
// and its name, the id, who said it, when, the text.
func TestSearchMessages(t *testing.T) {
	r := twoAccounts(t)
	r.chat("personal", bobChat, bobPNJID, "Bob", false)
	r.chat("work", carolJID, "", "Carol", false)
	r.chat("work", groupJID, "", "Office", true)
	r.wa.People = map[string]string{bobChat: "Bob Marley"}
	r.put(
		m{account: "personal", chat: bobChat, id: "P1", sender: bobChat, at: 10 * time.Second, text: "the rent is due"},
		m{account: "work", chat: carolJID, id: "W1", sender: carolJID, at: 30 * time.Second, text: "rent for the office"},
		m{account: "personal", chat: bobChat, id: "P2", sender: ownJID, fromMe: true, at: 20 * time.Second, text: "I will pay the rent tomorrow"},
		m{account: "work", chat: groupJID, id: "W2", sender: carolJID, at: 30 * time.Second, text: "rent again"},
	)
	out := r.search(map[string]any{"query": "rent"})
	if got, want := hitIDs(out), []string{"work:W2", "work:W1", "personal:P2", "personal:P1"}; !slices.Equal(got, want) {
		t.Fatalf("results %v, want %v", got, want)
	}
	byID := map[string]HitOut{}
	for _, h := range out.Results {
		byID[h.ID] = h
	}
	if h := byID["P1"]; h.Chat != bobChat || h.ChatName != "Bob Marley" || h.Sender != bobChat || h.SenderName != "Bob Marley" || h.FromMe ||
		h.At != iso(10*time.Second) || *h.Text != "the rent is due" || h.Account != "personal" {
		t.Errorf("P1 = %+v", h)
	}
	if h := byID["P2"]; !h.FromMe || h.SenderName != "" || h.Sender != ownJID {
		t.Errorf("P2 = %+v", h)
	}
	// The other party of a private chat has the name of the chat when nothing
	// better is known; in a group the chat's name is not anyone's.
	if h := byID["W1"]; h.SenderName != "Carol" || h.ChatName != "Carol" {
		t.Errorf("W1 = %+v", h)
	}
	if h := byID["W2"]; h.SenderName != "" || h.Chat != groupJID || h.ChatName != "Office" {
		t.Errorf("W2 = %+v", h)
	}
}

func TestSearchQueryMatching(t *testing.T) {
	r := newRig(t, "personal")
	r.put(
		m{account: "personal", chat: bobChat, id: "A", sender: bobChat, at: 1 * time.Second, text: "Привет, как дела?"},
		m{account: "personal", chat: bobChat, id: "B", sender: bobChat, at: 2 * time.Second, text: "Ёлка стоит дорого"},
		m{account: "personal", chat: bobChat, id: "C", sender: bobChat, at: 3 * time.Second, text: "дела идут, привет всем"},
		m{account: "personal", chat: bobChat, id: "D", sender: bobChat, at: 4 * time.Second, text: "report.pdf", media: "document", name: "report.pdf"},
		m{account: "personal", chat: bobChat, id: "E", sender: bobChat, at: 5 * time.Second, text: "100% sure; a+b=c"},
	)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"привет", []string{"personal:C", "personal:A"}},
		{"ПРИВЕТ", []string{"personal:C", "personal:A"}},
		{"ривет", []string{"personal:C", "personal:A"}}, // a part of a word
		{"привет дела", []string{"personal:C", "personal:A"}},
		{"дела привет", []string{"personal:C", "personal:A"}},
		{"елка", []string{"personal:B"}}, // ё is е
		{"ЕЛКА", []string{"personal:B"}},
		{"report", []string{"personal:D"}}, // the name of a document is its text
		{"100%", []string{"personal:E"}},
		{"a+b=c", []string{"personal:E"}},
		{`привет "OR" NEAR(`, nil}, // the syntax of the index is not the person's
		{"привет AND NOT дела", nil},
		{"нет такого", nil},
	} {
		if got := hitIDs(r.search(map[string]any{"query": tc.query})); !slices.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("query %q: %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestSearchQueryTooShort: a query with no word of three characters is an error
// that says so, and not an empty list that reads as "nothing found".
func TestSearchQueryTooShort(t *testing.T) {
	r := newRig(t, "personal")
	r.put(m{account: "personal", chat: bobChat, id: "A", sender: bobChat, text: "ok no go be it"})
	for _, q := range []string{"", " ", "ab", "a b c", "ок да", "!!", "\x00\x01", "до на по", "a\tb"} {
		res, text := r.call("search-messages", map[string]any{"query": q})
		if !res.IsError || text != errQueryTooShort.Error() {
			t.Errorf("query %q: isError=%v %s", q, res.IsError, text)
		}
	}
	// A short word beside a long one is dropped, and the long one is searched.
	r.put(m{account: "personal", chat: bobChat, id: "B", sender: bobChat, at: time.Second, text: "hello there"})
	if got := hitIDs(r.search(map[string]any{"query": "a hello"})); !slices.Equal(got, []string{"personal:B"}) {
		t.Errorf("a short word beside a long one: %v", got)
	}
	if text := r.fails("search-messages", map[string]any{"query": "ab", "account": "nobody"}); strings.Contains(text, "too short") {
		t.Errorf("an unknown account is the first thing wrong: %s", text)
	}
}

func TestSearchLimit(t *testing.T) {
	r := newRig(t, "personal")
	msgs := make([]m, 130)
	for i := range msgs {
		msgs[i] = m{account: "personal", chat: bobChat, id: fmt.Sprintf("M%03d", i), sender: bobChat, at: time.Duration(i) * time.Second, text: "needle " + fmt.Sprint(i)}
	}
	r.put(msgs...)
	more := "there are more matching messages than the %d shown, the most recent first: narrow it with chat, sender, after or before"
	moreRaise, moreMost := more+", or raise limit (at most 100)", fmt.Sprintf(more, 100)
	lowered := "limit lowered to the most there is, 100"
	for _, tc := range []struct {
		limit any
		want  int
		notes []string
	}{
		{nil, 20, []string{fmt.Sprintf(moreRaise, 20)}},
		{5, 5, []string{fmt.Sprintf(moreRaise, 5)}},
		{100, 100, []string{moreMost}},
		{101, 100, []string{lowered, moreMost}},
		{9999, 100, []string{lowered, moreMost}},
	} {
		args := map[string]any{"query": "needle"}
		if tc.limit != nil {
			args["limit"] = tc.limit
		}
		out := r.search(args)
		if len(out.Results) != tc.want || !slices.Equal(out.Notes, tc.notes) {
			t.Errorf("limit %v: %d results, notes %q, want %q", tc.limit, len(out.Results), out.Notes, tc.notes)
		}
		if out.Results[0].ID != "M129" { // the newest first
			t.Errorf("limit %v: the first is %s", tc.limit, out.Results[0].ID)
		}
	}
	if text := r.fails("search-messages", map[string]any{"query": "needle", "limit": -1}); text != "limit must be positive" {
		t.Errorf("negative limit: %s", text)
	}
}

func TestSearchByChatAccountAndTime(t *testing.T) {
	r := twoAccounts(t)
	r.put(
		m{account: "personal", chat: bobChat, id: "P1", sender: bobChat, at: 10 * time.Second, text: "lunch plan"},
		m{account: "personal", chat: bobChat, id: "P2", sender: bobChat, at: 20 * time.Second, text: "lunch moved"},
		m{account: "personal", chat: "70000000102@s.whatsapp.net", id: "P3", sender: "70000000102@s.whatsapp.net", at: 30 * time.Second, text: "lunch with dave"},
		m{account: "work", chat: carolJID, id: "W1", sender: carolJID, at: 40 * time.Second, text: "lunch order"},
	)
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"all", map[string]any{}, []string{"work:W1", "personal:P3", "personal:P2", "personal:P1"}},
		{"an account", map[string]any{"account": "work"}, []string{"work:W1"}},
		{"a chat by its LID", map[string]any{"chat": bobChat}, []string{"personal:P2", "personal:P1"}},
		{"a chat by its number, which is the LID of", map[string]any{"chat": "+7 000 000 0100"}, []string{"personal:P2", "personal:P1"}},
		{"a chat with no LID known, by number", map[string]any{"chat": carolPN}, []string{"work:W1"}},
		{"a chat and an account that does not have it", map[string]any{"chat": bobChat, "account": "work"}, nil},
		{"after is inclusive", map[string]any{"after": iso(20 * time.Second)}, []string{"work:W1", "personal:P3", "personal:P2"}},
		{"before is exclusive", map[string]any{"before": iso(20 * time.Second)}, []string{"personal:P1"}},
		{"both", map[string]any{"after": iso(15 * time.Second), "before": iso(35 * time.Second)}, []string{"personal:P3", "personal:P2"}},
		{"in UTC", map[string]any{"before": base.Add(20 * time.Second).UTC().Format(time.RFC3339)}, []string{"personal:P1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"query": "lunch"}
			for k, v := range tc.args {
				args[k] = v
			}
			if got := hitIDs(r.search(args)); !slices.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"now", "yesterday", "2026-10-07", "1h ago", "12:00"} {
		for _, field := range []string{"after", "before"} {
			text := r.fails("search-messages", map[string]any{"query": "lunch", field: bad})
			if !strings.Contains(text, timeExample) || !strings.HasPrefix(text, field+" must be a time") {
				t.Errorf("%s=%q: %s", field, bad, text)
			}
		}
	}
	if text := r.fails("search-messages", map[string]any{"query": "lunch", "chat": "Bob"}); !strings.Contains(text, "not a chat") {
		t.Errorf("a chat that is not one: %s", text)
	}
}

// TestSearchBySender: a person is found by whichever address their messages were
// filed under, number or LID, and the names that the WA side knows them by; the
// archive is asked for all of them.
func TestSearchBySender(t *testing.T) {
	r := twoAccounts(t)
	r.put(
		m{account: "personal", chat: groupJID, id: "G1", sender: bobPNJID, at: 1 * time.Second, text: "from the number"},
		m{account: "personal", chat: groupJID, id: "G2", sender: bobChat, at: 2 * time.Second, text: "from the LID"},
		m{account: "personal", chat: groupJID, id: "G3", sender: carolJID, at: 3 * time.Second, text: "from someone else"},
		m{account: "work", chat: groupJID, id: "G4", sender: bobChat, at: 4 * time.Second, text: "from the LID, in work"},
	)
	r.wa.Senders = map[string][]string{
		"Bob":             {bobChat, bobPNJID},
		"+7 000 000 0100": {bobChat, bobPNJID},
		"carol":           {carolJID},
		"Dup":             {bobChat, bobChat},
	}
	for sender, want := range map[string][]string{
		"Bob":             {"work:G4", "personal:G2", "personal:G1"},
		"+7 000 000 0100": {"work:G4", "personal:G2", "personal:G1"},
		"carol":           {"personal:G3"},
		"Dup":             {"work:G4", "personal:G2"},
	} {
		if got := hitIDs(r.search(map[string]any{"query": "from", "sender": sender})); !slices.Equal(got, want) {
			t.Errorf("sender %q: %v, want %v", sender, got, want)
		}
	}
	if got := hitIDs(r.search(map[string]any{"query": "from", "sender": "Bob", "account": "personal"})); !slices.Equal(got, []string{"personal:G2", "personal:G1"}) {
		t.Errorf("sender and account: %v", got)
	}
	// The sender is looked up in each account searched, and nowhere else.
	var asked []string
	for _, c := range r.wa.Calls() {
		if c.Method == "SenderJIDs" && c.Sender == "carol" {
			asked = append(asked, c.Nick)
		}
	}
	if !slices.Equal(asked, []string{"personal", "work"}) {
		t.Errorf("SenderJIDs asked for the accounts %v", asked)
	}

	for _, tc := range []struct {
		name   string
		sender string
		err    error
		want   string
	}{
		{"no one is named so", "Mallory", nil, errNoSender.Error()},
		{"a group", "Bob", wa.ErrNotAPerson, wa.ErrNotAPerson.Error()},
		{"too many", "Bob", wa.ErrTooManyPeople, wa.ErrTooManyPeople.Error()},
		{"the store fails", "Bob", errors.New("sqlite: database is locked (store.db)"), errReadFailed.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.wa.LookupErr = tc.err
			defer func() { r.wa.LookupErr = nil }()
			if text := r.fails("search-messages", map[string]any{"query": "from", "sender": tc.sender}); text != tc.want {
				t.Errorf("error %q, want %q", text, tc.want)
			}
		})
	}
}

// TestSearchShowsRevokedAndEdited: a result that was deleted by its sender says so.
func TestSearchShowsRevokedAndEdited(t *testing.T) {
	r := bobsRig(t)
	r.put(
		m{account: "personal", chat: bobChat, id: "A", sender: bobChat, at: time.Second, text: "secret plan"},
		m{account: "personal", chat: bobChat, id: "B", sender: bobChat, at: 2 * time.Second, text: "old words"},
	)
	if err := r.db.Tx(context.Background(), func(tx *archive.Tx) error {
		if err := tx.Revoke(archive.Revoke{Account: "personal", Chat: bobChat, ID: "A", RevokedAt: base.Add(time.Minute)}); err != nil {
			return err
		}
		return tx.Edit(archive.Edit{Account: "personal", Chat: bobChat, ID: "B", Text: "new words", EditedAt: base.Add(2 * time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	out := r.search(map[string]any{"query": "plan"})
	if len(out.Results) != 1 || !out.Results[0].Revoked || out.Results[0].RevokedAt != iso(time.Minute) || *out.Results[0].Text != "secret plan" {
		t.Errorf("revoked: %+v", out.Results)
	}
	out = r.search(map[string]any{"query": "words"})
	if len(out.Results) != 1 || out.Results[0].EditedAt != iso(2*time.Minute) || *out.Results[0].Text != "new words" || out.Results[0].Revoked {
		t.Errorf("edited: %+v", out.Results)
	}
	if got := r.search(map[string]any{"query": "old"}); len(got.Results) != 0 {
		t.Errorf("the text before the edit is found: %+v", got.Results)
	}
}

// TestSearchAsksTheArchive: the query, the accounts and the filters that reach the
// archive, with a fake in its place.
func TestSearchAsksTheArchive(t *testing.T) {
	arc := &toolstest.Archive{}
	w := &toolstest.WA{Accs: []wa.AccountInfo{{Nick: "personal", Status: wa.StatusConnected}, {Nick: "work", Status: wa.StatusConnected}},
		Pairs: []string{bobLID, bobPN}, Senders: map[string][]string{"Bob": {bobPNJID, bobChat, bobPNJID}}}
	r := &rig{t: t, wa: w, cs: serve(t, Deps{WA: w, Archive: arc})}
	r.search(map[string]any{"query": "  hello ", "account": []any{"work"}, "chat": bobPNJID, "sender": "Bob",
		"after": "2026-10-07T12:00:00Z", "before": "2026-10-07T15:00:00+03:00", "limit": 7})
	r.search(map[string]any{"query": "hello"})
	utc := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	got := arc.Queries().Searches
	if len(got) != 3 {
		t.Fatalf("the archive was asked %+v", got)
	}
	// A chat is asked for under each JID that an account may have it under, and one more
	// than the limit is asked for: it tells whether there are more.
	for i, key := range []string{bobChat, bobPNJID} {
		first := got[i]
		if first.Query != "  hello " || !slices.Equal(first.Accounts, []string{"work"}) || first.Chat != key ||
			!slices.Equal(first.Senders, []string{bobChat, bobPNJID}) || !first.After.Equal(utc) || !first.Before.Equal(utc) || first.Limit != 8 {
			t.Errorf("query %d = %+v", i, first)
		}
	}
	if last := got[2]; !slices.Equal(last.Accounts, []string{"personal", "work"}) || last.Limit != 21 || last.Chat != "" || last.Senders != nil ||
		!last.After.IsZero() || !last.Before.IsZero() {
		t.Errorf("the last query = %+v", last)
	}
}
