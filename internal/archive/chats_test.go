package archive

import (
	"slices"
	"strings"
	"testing"
)

func chatNames(chats []Chat) []string {
	var out []string
	for _, c := range chats {
		out = append(out, c.Account+"/"+c.Name)
	}
	return out
}

// chatsFixture: alice has five chats, bob two; one of alice's has had no
// message, one has no name.
func chatsFixture(t *testing.T) *DB {
	t.Helper()
	db := openWith(t, "alice", "bob")
	pn := func(digits string) string { return digits + "@s.whatsapp.net" }
	run(t, db,
		touch(ChatUpd{Account: "alice", JID: "1@lid", PN: pn("79991234567"), Name: "Анна Петрова", LastMessageTS: at(300)}),
		touch(ChatUpd{Account: "alice", JID: "2@lid", PN: pn("78885554433"), Name: "Ёлкин Пётр", LastMessageTS: at(200)}),
		touch(ChatUpd{Account: "alice", JID: group, Name: "Дача", IsGroup: true, LastMessageTS: at(400)}),
		touch(ChatUpd{Account: "alice", JID: "3@lid", PN: pn("77770001122"), LastMessageTS: at(100)}),
		touch(ChatUpd{Account: "alice", JID: "4@lid", Name: "Zed"}), // no message yet
		touch(ChatUpd{Account: "bob", JID: "5@lid", PN: pn("447700900123"), Name: "Anna Smith", LastMessageTS: at(250)}),
		touch(ChatUpd{Account: "bob", JID: "6@lid", Name: "Room 101", LastMessageTS: at(50)}),
	)
	return db
}

func TestChats(t *testing.T) {
	db := chatsFixture(t)
	all := []string{"alice/Дача", "alice/Анна Петрова", "bob/Anna Smith", "alice/Ёлкин Пётр", "alice/", "bob/Room 101", "alice/Zed"}

	tests := []struct {
		name string
		q    ChatQuery
		want []string
	}{
		{"all, the newest first and those without a message last", ChatQuery{Limit: 100}, all},
		{"a limit", ChatQuery{Limit: 3}, all[:3]},
		{"one account", ChatQuery{Accounts: []string{"bob"}, Limit: 100}, []string{"bob/Anna Smith", "bob/Room 101"}},
		{"two accounts", ChatQuery{Accounts: []string{"bob", "alice"}, Limit: 100}, all},
		{"an unknown account", ChatQuery{Accounts: []string{"carol"}, Limit: 100}, nil},
		{"a name", ChatQuery{Query: "Анна", Limit: 100}, []string{"alice/Анна Петрова"}},
		{"in capitals", ChatQuery{Query: "АННА", Limit: 100}, []string{"alice/Анна Петрова"}},
		{"a part of a name", ChatQuery{Query: "етров", Limit: 100}, []string{"alice/Анна Петрова"}},
		{"latin, any case", ChatQuery{Query: "aNNa", Limit: 100}, []string{"bob/Anna Smith"}},
		{"ё typed as е", ChatQuery{Query: "елкин петр", Limit: 100}, []string{"alice/Ёлкин Пётр"}},
		{"е typed as ё, which also finds the е", ChatQuery{Query: "ПЁТР", Limit: 100}, []string{"alice/Анна Петрова", "alice/Ёлкин Пётр"}},
		{"spaces around the query", ChatQuery{Query: "  дача ", Limit: 100}, []string{"alice/Дача"}},
		{"digits of a number", ChatQuery{Query: "888555", Limit: 100}, []string{"alice/Ёлкин Пётр"}},
		{"a number as it is written", ChatQuery{Query: "+7 (999) 123-45", Limit: 100}, []string{"alice/Анна Петрова"}},
		{"a number with the country code of another account's chat", ChatQuery{Query: "+44 7700", Limit: 100}, []string{"bob/Anna Smith"}},
		{"digits of a chat with no name", ChatQuery{Query: "7770001", Limit: 100}, []string{"alice/"}},
		{"a number that is not there", ChatQuery{Query: "+7 000 000", Limit: 100}, nil},
		{"a name with digits is not read as a number", ChatQuery{Query: "Room 101", Limit: 100}, []string{"bob/Room 101"}},
		{"a word with digits finds no number", ChatQuery{Query: "x7999", Limit: 100}, nil},
		{"the limit comes after the filter", ChatQuery{Query: "а", Limit: 1}, []string{"alice/Дача"}},
		{"the group has no number to find", ChatQuery{Query: "120363", Limit: 100}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chats, err := db.Chats(bg, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := chatNames(chats); !slices.Equal(got, tt.want) {
				t.Errorf("chats = %q\nwant    %q", got, tt.want)
			}
		})
	}

	t.Run("no limit is refused", func(t *testing.T) {
		_, err := db.Chats(bg, ChatQuery{})
		wantErrIs(t, err, ErrBadLimit)
	})
	t.Run("the fields", func(t *testing.T) {
		chats, err := db.Chats(bg, ChatQuery{Query: "Дача", Limit: 1})
		if err != nil || len(chats) != 1 {
			t.Fatal(chats, err)
		}
		if c := chats[0]; c.Account != "alice" || c.JID != group || !c.IsGroup || c.PN != "" || c.LastMessageTS.Unix() != 400 {
			t.Errorf("chat = %+v", c)
		}
	})
	t.Run("a chat with no message has no time", func(t *testing.T) {
		chats, err := db.Chats(bg, ChatQuery{Query: "zed", Limit: 1})
		if err != nil || len(chats) != 1 || !chats[0].LastMessageTS.IsZero() {
			t.Errorf("chats = %+v, %v; want Zed with the zero time, not 1970", chats, err)
		}
	})
}

// TestChatsFindAChatKeptUnderItsNumber: a chat that has no LID yet is kept
// under the phone number and may have no pn of its own; its number is its jid,
// and the query finds it as it finds one that has a pn. A LID is not a number.
func TestChatsFindAChatKeptUnderItsNumber(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db,
		touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Ivan", LastMessageTS: at(100)}),
		touch(ChatUpd{Account: "alice", JID: lidChat, PN: "79990001122@s.whatsapp.net", Name: "Petr", LastMessageTS: at(50)}),
	)
	for query, want := range map[string]string{
		"79991234567":        "Ivan",
		"7999123":            "Ivan",
		"+7 (999) 123-45-67": "Ivan",
		"79990001122":        "Petr",
		"999":                "Ivan,Petr",
		"123456789012345":    "", // the digits of the LID are not a number
	} {
		chats, err := db.Chats(bg, ChatQuery{Query: query, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range chats {
			names = append(names, c.Name)
		}
		if got := strings.Join(names, ","); got != want {
			t.Errorf("Chats(%q) = %q, want %q", query, got, want)
		}
	}
}

func TestAccountsForChat(t *testing.T) {
	db := openWith(t, "alice", "bob", "carol")
	run(t, db,
		touch(ChatUpd{Account: "alice", JID: lidChat, PN: pnChat}), // known by its LID, with the number
		touch(ChatUpd{Account: "alice", JID: pnChat}),              // and, until the merge, under the number too: still one account
		touch(ChatUpd{Account: "bob", JID: pnChat}),                // not merged yet: kept under the number
		touch(ChatUpd{Account: "carol", JID: "999@lid", PN: "7000@s.whatsapp.net"}),
		touch(ChatUpd{Account: "carol", JID: group, IsGroup: true}),
		touch(ChatUpd{Account: "alice", JID: group, IsGroup: true}),
	)
	tests := []struct {
		chat string
		want []string
	}{
		{lidChat, []string{"alice"}},
		{pnChat, []string{"alice", "bob"}}, // by its pn and its jid in alice's chats, by its jid in bob's: each account once
		{group, []string{"alice", "carol"}},
		{"7000@s.whatsapp.net", []string{"carol"}},
		{"unknown@lid", nil},
		{"", nil},
	}
	for _, tt := range tests {
		got, err := db.AccountsForChat(bg, tt.chat)
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("AccountsForChat(%q) = %v, %v; want %v", tt.chat, got, err, tt.want)
		}
	}
}
