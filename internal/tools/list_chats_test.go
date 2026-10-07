package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// TestListChats: the fields of a chat, the order (the most recent first, a chat
// with no message last), and the number, which is the chat's own and never a LID.
func TestListChats(t *testing.T) {
	r := newRig(t, "personal", "work")
	r.chat("personal", bobChat, bobPNJID, "Bob", false)
	r.put(m{account: "personal", chat: bobChat, id: "B1", sender: bobChat, at: 10 * time.Second, text: "hi"})
	r.chat("personal", carolJID, "", "Carol", false) // filed under her number
	r.put(m{account: "personal", chat: carolJID, id: "C1", sender: carolJID, at: 20 * time.Second, text: "hi"})
	r.chat("work", "200000000555@lid", "", "Eve", false) // a LID with no number known
	r.put(m{account: "work", chat: "200000000555@lid", id: "E1", sender: "200000000555@lid", at: 5 * time.Second, text: "hi"})
	r.chat("work", groupJID, "", "Family", true)
	r.put(m{account: "work", chat: groupJID, id: "G1", sender: carolJID, at: 15 * time.Second, text: "hi"})
	r.chat("work", "70000000777@s.whatsapp.net", "", "Silent", false) // never had a message

	var out ChatsOut
	r.ok("list-chats", nil, &out)
	want := []ChatOut{
		{Account: "personal", Chat: carolJID, Name: "Carol", Phone: "+" + carolPN, LastMessageAt: iso(20 * time.Second)},
		{Account: "work", Chat: groupJID, Name: "Family", IsGroup: true, LastMessageAt: iso(15 * time.Second)},
		{Account: "personal", Chat: bobChat, Name: "Bob", Phone: "+" + bobPN, LastMessageAt: iso(10 * time.Second)},
		{Account: "work", Chat: "200000000555@lid", Name: "Eve", LastMessageAt: iso(5 * time.Second)},
		{Account: "work", Chat: "70000000777@s.whatsapp.net", Name: "Silent", Phone: "+70000000777"},
	}
	if len(out.Chats) != len(want) {
		t.Fatalf("chats = %+v, want %+v", out.Chats, want)
	}
	for i := range want {
		if out.Chats[i] != want[i] {
			t.Errorf("chat %d = %+v, want %+v", i, out.Chats[i], want[i])
		}
	}
	if !strings.HasSuffix(out.Chats[0].LastMessageAt, "+03:00") {
		t.Errorf("last_message_at %q does not carry the local offset", out.Chats[0].LastMessageAt)
	}
	if len(out.Notes) != 0 {
		t.Errorf("notes = %q", out.Notes)
	}
}

func TestListChatsQuery(t *testing.T) {
	r := newRig(t, "personal")
	r.chat("personal", bobChat, bobPNJID, "Bob Marley", false)
	r.chat("personal", carolJID, "", "Ёлка Ivanova", false)
	r.chat("personal", groupJID, "", "Room 101", true)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"marley", []string{bobChat}},
		{"BOB", []string{bobChat}},
		{"елка", []string{carolJID}}, // ё is е
		{"70000000100", []string{bobChat}},
		{"+7 000 000 0100", []string{bobChat}},
		{"101", []string{groupJID, carolJID}}, // the name of one, and the digits of the number of the other
		{"Room 101", []string{groupJID}},
		{"nobody", []string{}},
		{"", []string{bobChat, carolJID, groupJID}},
	} {
		var out ChatsOut
		r.ok("list-chats", map[string]any{"query": tc.query}, &out)
		got := []string{}
		for _, c := range out.Chats {
			got = append(got, c.Chat)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("query %q: chats %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestListChatsLimit: 50 by default, what was asked for, and no more than 200,
// which is said in a note and not refused.
func TestListChatsLimit(t *testing.T) {
	r := newRig(t, "personal")
	for i := range 230 {
		r.chat("personal", fmt.Sprintf("%d@s.whatsapp.net", 70000001000+i), "", fmt.Sprintf("Chat %d", i), false)
	}
	moreRaise := "there are more chats than the %d shown, the most recent first: narrow it with query or account, or raise limit (at most 200)"
	moreMost := "there are more chats than the 200 shown, the most recent first: narrow it with query or account"
	for _, tc := range []struct {
		name  string
		limit any
		want  int
		notes []string
	}{
		{"absent", nil, 50, []string{fmt.Sprintf(moreRaise, 50)}},
		{"zero is absent", 0, 50, []string{fmt.Sprintf(moreRaise, 50)}},
		{"asked", 7, 7, []string{fmt.Sprintf(moreRaise, 7)}},
		{"the most", 200, 200, []string{moreMost}},
		{"more than the most", 5000, 200, []string{"limit lowered to the most there is, 200", moreMost}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{}
			if tc.limit != nil {
				args["limit"] = tc.limit
			}
			var out ChatsOut
			r.ok("list-chats", args, &out)
			if len(out.Chats) != tc.want {
				t.Errorf("%d chats, want %d", len(out.Chats), tc.want)
			}
			if !slices.Equal(out.Notes, tc.notes) {
				t.Errorf("notes = %q, want %q", out.Notes, tc.notes)
			}
		})
	}
	if text := r.fails("list-chats", map[string]any{"limit": -1}); text != "limit must be positive" {
		t.Errorf("a negative limit: %s", text)
	}
}

func TestListChatsEmptyIsAnEmptyList(t *testing.T) {
	r := newRig(t, "personal")
	_, text := r.call("list-chats", nil)
	if !strings.Contains(text, `"chats":[]`) {
		t.Errorf("an account with no chats: %s", text)
	}
}

// TestListChatsAsksTheArchive: what reaches the archive, with a fake in its place.
func TestListChatsAsksTheArchive(t *testing.T) {
	arc := &toolstest.Archive{}
	w := &toolstest.WA{Accs: []wa.AccountInfo{{Nick: "personal", Status: wa.StatusConnected}, {Nick: "work", Status: wa.StatusConnected}}}
	cs := serve(t, Deps{WA: w, Archive: arc})
	call := func(args map[string]any) {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list-chats", Arguments: args}); err != nil {
			t.Fatal(err)
		}
	}
	call(map[string]any{"query": "bob", "limit": 3})
	call(map[string]any{"account": []any{"work"}})
	got := arc.Queries().Chats
	// One more than the limit is asked for: it tells whether there are more.
	want := []archive.ChatQuery{
		{Accounts: []string{"personal", "work"}, Query: "bob", Limit: 4},
		{Accounts: []string{"work"}, Limit: 51},
	}
	if len(got) != 2 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the archive was asked %+v, want %+v", got, want)
	}
}

// contactsRig is a rig with Bob's chat, which the archive calls by the push name he
// gave himself and his contact calls "Мама" (the archive does not know that), Carol's, and
// a group, with the most recent message in Carol's.
func contactsRig(t *testing.T) *rig {
	t.Helper()
	r := bobsRig(t)
	r.chat("personal", bobChat, bobPNJID, "Bobby", false)
	r.chat("personal", carolJID, "", "Carol", false)
	r.chat("personal", groupJID, "", "Family", true)
	r.put(
		m{account: "personal", chat: bobChat, id: "B1", sender: bobChat, at: 10 * time.Second, text: "hi"},
		m{account: "personal", chat: groupJID, id: "G1", sender: carolJID, at: 20 * time.Second, text: "hi"},
		m{account: "personal", chat: carolJID, id: "C1", sender: carolJID, at: 30 * time.Second, text: "hi"},
	)
	r.wa.People = map[string]string{bobChat: "Мама", groupJID: "not the name of a group"}
	r.wa.Senders = map[string][]string{"мама": {bobChat, bobPNJID}, "bob": {bobChat, bobPNJID}}
	return r
}

func chatIDs(out ChatsOut) []string {
	ids := []string{}
	for _, c := range out.Chats {
		ids = append(ids, c.Chat)
	}
	return ids
}

// TestListChatsNamesPeopleByTheirContacts: a private chat is called by the name its
// person has in the account's contacts, which WhatsApp shows first, and the archive's
// name when there is none; a group by the archive's, always.
func TestListChatsNamesPeopleByTheirContacts(t *testing.T) {
	var out ChatsOut
	contactsRig(t).ok("list-chats", nil, &out)
	names := map[string]string{}
	for _, c := range out.Chats {
		names[c.Chat] = c.Name
	}
	if want := map[string]string{bobChat: "Мама", carolJID: "Carol", groupJID: "Family"}; fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

// TestListChatsFindsPeopleByTheirContacts: the query finds a chat by the name of its
// person in the contacts as well as by the names and the numbers of the archive; each
// chat once, and in the order of the most recent first.
func TestListChatsFindsPeopleByTheirContacts(t *testing.T) {
	r := contactsRig(t)
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"a name that only the contacts have", "мама", []string{bobChat}},
		{"a name of the archive and of the contacts", "bob", []string{bobChat}},
		{"a name of the archive alone", "carol", []string{carolJID}},
		{"nobody", "nobody", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out ChatsOut
			r.ok("list-chats", map[string]any{"query": tc.query}, &out)
			if got := chatIDs(out); !slices.Equal(got, tc.want) {
				t.Errorf("chats %v, want %v", got, tc.want)
			}
		})
	}

	// The contacts may know a person by the number alone, and the archive keep the
	// chat under the LID with the number beside it: it is the same chat.
	r.wa.Senders["a nickname"] = []string{bobPNJID}
	var out ChatsOut
	r.ok("list-chats", map[string]any{"query": "a nickname"}, &out)
	if got := chatIDs(out); !slices.Equal(got, []string{bobChat}) {
		t.Errorf("a person known by the number: chats %v", got)
	}

	// The contacts and the archive both add to what is found, and the limit comes after.
	r.wa.Senders["a"] = []string{bobChat, carolJID}
	r.ok("list-chats", map[string]any{"query": "a", "limit": 1}, &out) // carol's name has an "a", and so do the contacts of bob and carol
	if got := chatIDs(out); !slices.Equal(got, []string{carolJID}) {
		t.Errorf("a limit of 1: chats %v, want the most recent only", got)
	}
	r.ok("list-chats", map[string]any{"query": "a"}, &out)
	if got := chatIDs(out); !slices.Equal(got, []string{carolJID, groupJID, bobChat}) { // "Family" has an "a" too
		t.Errorf("chats %v", got)
	}
}

// TestListChatsQueryThatFitsTooManyPeople: a name that fits more people than can be
// looked up still finds what the archive matches, and says that the contacts were not
// searched; a query that names no person (a group's JID) says nothing, and a store
// that cannot be read is not the agent's.
func TestListChatsQueryThatFitsTooManyPeople(t *testing.T) {
	r := contactsRig(t)
	for _, tc := range []struct {
		name     string
		err      error
		wantNote bool
		wantErr  string
	}{
		{"too many", wa.ErrTooManyPeople, true, ""},
		{"a group", wa.ErrNotAPerson, false, ""},
		{"the store fails", errors.New("sqlite: database is locked (store.db)"), false, errReadFailed.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.wa.LookupErr = tc.err
			defer func() { r.wa.LookupErr = nil }()
			if tc.wantErr != "" {
				if text := r.fails("list-chats", map[string]any{"query": "carol"}); text != tc.wantErr {
					t.Errorf("error %q, want %q", text, tc.wantErr)
				}
				return
			}
			var out ChatsOut
			r.ok("list-chats", map[string]any{"query": "carol"}, &out)
			if got := chatIDs(out); !slices.Equal(got, []string{carolJID}) {
				t.Errorf("chats %v, want what the archive matches", got)
			}
			if (len(out.Notes) == 1 && strings.Contains(out.Notes[0], "too many people")) != tc.wantNote || (len(out.Notes) == 0) == tc.wantNote {
				t.Errorf("notes = %q, want a note about too many people: %v", out.Notes, tc.wantNote)
			}
		})
	}
}
