package archive

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// searchFixture has texts that probe the trigram index: case, ё, a phone
// number, text that looks like FTS5 syntax. The id of a message is its time.
func searchFixture(t *testing.T) *DB {
	t.Helper()
	db := openWith(t, "alice", "bob")
	in := func(account, chat, id string, ts int64, text string) op {
		r := msg(account, chat, id, ts, text)
		return up(r)
	}
	fromBoss := msg("alice", group, "50", 50, "Встреча по аренде квартиры")
	fromBoss.Sender = "5551@lid"
	run(t, db,
		in("alice", pnChat, "10", 10, "Привет, как дела?"),
		in("alice", pnChat, "20", 20, "ПРИВЕТСТВУЮ вас"),
		in("alice", pnChat, "30", 30, "Ёлка зелёная"),
		in("alice", pnChat, "31", 31, "елка без ё"),
		in("alice", pnChat, "40", 40, "Позвони мне +7 999 123-45-67 вечером"),
		up(fromBoss),
		in("bob", "bobs@lid", "60", 60, "Аренда гаража, привет"),
		in("alice", pnChat, "70", 70, `100% sure; it's "quoted" text`),
		in("alice", pnChat, "80", 80, "café crème"),
		in("alice", pnChat, "90", 90, "NEAR(привет, дела)"),
		in("alice", pnChat, "95", 95, "a.b-c*d"),
		touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Anna"}),
		touch(ChatUpd{Account: "alice", JID: group, Name: "Дача", IsGroup: true}),
	)
	return db
}

func search(t *testing.T, db *DB, q SearchQuery) []Hit {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 100
	}
	hits, err := db.Search(bg, q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return hits
}

func TestSearch(t *testing.T) {
	db := searchFixture(t)
	tests := []struct {
		name string
		q    SearchQuery
		want string // ids, newest first
	}{
		{"a word, the case of the text does not matter", SearchQuery{Query: "привет"}, "90,60,20,10"},
		{"a word, the case of the query does not matter", SearchQuery{Query: "ПРИВЕТ"}, "90,60,20,10"},
		{"mixed case", SearchQuery{Query: "пРиВеТ"}, "90,60,20,10"},
		{"a part of a word", SearchQuery{Query: "ривет"}, "90,60,20,10"},
		{"a longer word narrows it", SearchQuery{Query: "приветству"}, "20"},
		{"е finds ё and е", SearchQuery{Query: "елка"}, "31,30"},
		{"ё finds ё and е", SearchQuery{Query: "ёлка"}, "31,30"},
		{"Ё in capitals", SearchQuery{Query: "ЁЛКА"}, "31,30"},
		{"ё in the middle of a word", SearchQuery{Query: "зелёная"}, "30"},
		{"е for it", SearchQuery{Query: "зеленая"}, "30"},
		{"a letter of the same shape in the other alphabet is another letter", SearchQuery{Query: "прuвет"}, ""},
		{"words are ANDed, in any order", SearchQuery{Query: "дела привет"}, "90,10"},
		{"a fragment of a phone number, as words", SearchQuery{Query: "999 123"}, "40"},
		{"a fragment with the dashes, as it is written", SearchQuery{Query: "123-45"}, "40"},
		{"a whole number", SearchQuery{Query: "+7 999 123-45-67"}, "40"},
		{"digits that are not there", SearchQuery{Query: "999 124"}, ""},
		{"latin diacritics are removed", SearchQuery{Query: "cafe"}, "80"},
		{"in the query too", SearchQuery{Query: "CRÈME"}, "80"},
		{"one account", SearchQuery{Query: "привет", Accounts: []string{"bob"}}, "60"},
		{"the other account", SearchQuery{Query: "привет", Accounts: []string{"alice"}}, "90,20,10"},
		{"both accounts", SearchQuery{Query: "аренд", Accounts: []string{"alice", "bob"}}, "60,50"},
		{"no account is all of them", SearchQuery{Query: "аренд"}, "60,50"},
		{"an account that is not there", SearchQuery{Query: "привет", Accounts: []string{"carol"}}, ""},
		{"a chat", SearchQuery{Query: "аренд", Chat: group}, "50"},
		{"a chat that has none", SearchQuery{Query: "привет", Chat: group}, ""},
		{"a chat of the other account", SearchQuery{Query: "привет", Chat: "bobs@lid"}, "60"},
		{"a sender, in either of its forms", SearchQuery{Query: "аренд", Senders: []string{"5551@s.whatsapp.net", "5551@lid"}}, "50"},
		{"a sender that wrote nothing", SearchQuery{Query: "аренд", Senders: []string{"7@lid"}}, ""},
		{"after is inclusive", SearchQuery{Query: "привет", After: at(20)}, "90,60,20"},
		{"before is exclusive", SearchQuery{Query: "привет", Before: at(60)}, "20,10"},
		{"between", SearchQuery{Query: "привет", After: at(11), Before: at(90)}, "60,20"},
		{"the limit keeps the newest", SearchQuery{Query: "привет", Limit: 2}, "90,60"},
		{"a word the length of a trigram", SearchQuery{Query: "дел"}, "90,10"},
		{"short words are dropped when there is a longer one", SearchQuery{Query: "а привет я"}, "90,60,20,10"},

		// Text that looks like a query language is only text.
		{"a percent sign", SearchQuery{Query: "100%"}, "70"},
		{"quotes around a word do not stop it being found", SearchQuery{Query: `"quoted"`}, "70"},
		{"a quote that is not closed", SearchQuery{Query: `"quoted`}, "70"},
		{"punctuation as written", SearchQuery{Query: "a.b-c*d"}, "95"},
		{"a star at the end is trimmed like any punctuation, and does not make a prefix operator", SearchQuery{Query: "прив*"}, "90,60,20,10"},
		{"a star that is in the text", SearchQuery{Query: "c*d"}, "95"},
		{"a column filter is not one", SearchQuery{Query: "text:привет"}, ""},
		{"nor is the table's name", SearchQuery{Query: "messages_fts:привет"}, ""},
		{"OR is dropped as short, so both words are needed", SearchQuery{Query: "привет OR дела"}, "90,10"},
		{"AND is a word like another", SearchQuery{Query: "привет AND дела"}, ""},
		{"NOT is a word like another", SearchQuery{Query: "привет NOT дела"}, ""},
		{"a minus at the start is trimmed, not an exclusion", SearchQuery{Query: "-привет"}, "90,60,20,10"},
		{"a caret is part of the word", SearchQuery{Query: "^привет"}, ""},
		{"NEAR is only text, not the operator that would find message 10 too", SearchQuery{Query: "NEAR(привет дела)"}, "90"},
		{"NEAR that is in the text", SearchQuery{Query: "NEAR(привет,"}, "90"},
		{"brackets at the start are trimmed", SearchQuery{Query: "((привет"}, "90,60,20,10"},
		{"a quote inside a word", SearchQuery{Query: `при"вет`}, ""},
		{"a nul splits words", SearchQuery{Query: "привет\x00дела"}, "90,10"},
		{"control characters split words", SearchQuery{Query: "привет\r\n\tдела"}, "90,10"},
		{"bytes that are not UTF-8 are dropped", SearchQuery{Query: "\xff\xfe привет \xc3"}, "90,60,20,10"},
		{"and split words", SearchQuery{Query: "привет\xffдела"}, "90,10"},
		{"SQL in the query is only text", SearchQuery{Query: `'; DROP TABLE messages; --`}, ""},
		{"a SQL quote", SearchQuery{Query: `привет' OR '1'='1`}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hitIDs(search(t, db, tt.q)); got != tt.want {
				t.Errorf("hits = %q, want %q", got, tt.want)
			}
		})
	}
	if n := count(t, db, `SELECT count(*) FROM messages`); n != 11 {
		t.Errorf("%d messages after the hostile queries, want 11", n)
	}
}

// TestSearchEdgePunctuation: the way a person marks a phrase or ends a
// sentence (quotes, brackets, a question mark) is not part of the text sought:
// the index finds a substring, and a message does not have the quote that the
// query put before its first word. Symbols and the punctuation inside a word
// are text, as before.
func TestSearchEdgePunctuation(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db,
		up(msg("alice", pnChat, "1", 1, "Сдаётся аренда гаража на Пушкина")),
		up(msg("alice", pnChat, "2", 2, "Когда оплата за гараж")),
		up(msg("alice", pnChat, "3", 3, "Привет, как дела")),
		up(msg("alice", pnChat, "4", 4, "1+1=2 and price_$5")),
		up(msg("alice", pnChat, "5", 5, `He said "no way" twice`)),
		up(msg("alice", pnChat, "6", 6, "скидка 50% до пункта (8)")),
	)
	tests := []struct{ query, want string }{
		{`"аренда гаража"`, "1"},
		{`оплата?`, "2"},
		{`Привет!`, "3"},
		{`(гараж)`, "2,1"},
		{`гараж.`, "2,1"},
		{`«гараж»`, "2,1"},
		{`'гараж'`, "2,1"},
		{`гаража;`, "1"},
		{`...гараж...`, "2,1"},
		{`аренда гаража`, "1"},
		{`1+1=2`, "4"},
		{`price_$5`, "4"},
		{`"no way"`, "5"},
		{`гараж?!`, "2,1"},  // all the punctuation of an end
		{`--привет--`, "3"}, // the minus signs are punctuation
		{`"в аренда"`, "1"}, // a short word is dropped, with its quote
		{`50%`, "6"},        // the core is 2 characters: searched as written, as before
		{`(8)`, "6"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := hitIDs(search(t, db, SearchQuery{Query: tt.query})); got != tt.want {
				t.Errorf("hits = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSearchLongQuery: a query of many words is bounded, not an error.
func TestSearchLongQuery(t *testing.T) {
	db := searchFixture(t)
	query := strings.Repeat("привет ", 5000)
	if got := hitIDs(search(t, db, SearchQuery{Query: query})); got != "90,60,20,10" {
		t.Errorf("hits = %q", got)
	}
	var words []string
	for i := range 3000 {
		words = append(words, "слово"+strings.Repeat("я", i%7))
	}
	if got := hitIDs(search(t, db, SearchQuery{Query: strings.Join(words, " ")})); got != "" {
		t.Errorf("hits = %q", got)
	}
}

// TestSearchTooShort: the trigram index cannot find less than three
// characters, and says so, rather than answer as if nothing were there.
func TestSearchTooShort(t *testing.T) {
	db := searchFixture(t)
	for _, q := range []string{"", "   ", "а", "ив", "ab cd", "a b c", "\x00", `"`, `""`, "++", "ё", "\xff"} {
		_, err := db.Search(bg, SearchQuery{Query: q, Limit: 10})
		if !errors.Is(err, ErrQueryTooShort) {
			t.Errorf("Search(%q) = %v, want ErrQueryTooShort", q, err)
		}
	}
	if !strings.Contains(ErrQueryTooShort.Error(), "3") {
		t.Errorf("the error does not say how many: %v", ErrQueryTooShort)
	}
	t.Run("a limit is needed", func(t *testing.T) {
		_, err := db.Search(bg, SearchQuery{Query: "привет"})
		wantErrIs(t, err, ErrBadLimit)
	})
}

func TestSearchResult(t *testing.T) {
	db := searchFixture(t)
	hits := search(t, db, SearchQuery{Query: "аренд", Accounts: []string{"alice"}})
	if len(hits) != 1 {
		t.Fatalf("hits = %v", hitIDs(hits))
	}
	h := hits[0]
	if h.Account != "alice" || h.Chat != group || h.ID != "50" || h.Sender != "5551@lid" || h.Text != "Встреча по аренде квартиры" ||
		h.TS.Unix() != 50 || h.ChatName != "Дача" || h.Raw != nil {
		t.Errorf("hit = %+v", h)
	}
	if cur := h.Cursor(); cur.TS != 50 || cur.ID == 0 {
		t.Errorf("cursor = %v", cur)
	}

	t.Run("a chat the archive has no row for has no name", func(t *testing.T) {
		hits := search(t, db, SearchQuery{Query: "гаража"})
		if len(hits) != 1 || hits[0].ChatName != "" {
			t.Errorf("hits = %+v", hits)
		}
	})
}

// TestSearchFollowsWrites: an edit, a revoke and a merge show in the search
// at once, and leave nothing of the old text.
func TestSearchFollowsWrites(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(pnMsg("a", 10, "first version")))
	if got := hitIDs(search(t, db, SearchQuery{Query: "version"})); got != "a" {
		t.Fatalf("hits = %q", got)
	}

	run(t, db, editIn(pnChat, "a", "second text", 20))
	for word, want := range map[string]string{"version": "", "second": "a"} {
		if got := hitIDs(search(t, db, SearchQuery{Query: word})); got != want {
			t.Errorf("after the edit, %q finds %q, want %q", word, got, want)
		}
	}

	run(t, db, revokeIn(pnChat, "a", 30))
	hits := search(t, db, SearchQuery{Query: "second"})
	if len(hits) != 1 || hits[0].RevokedAt.Unix() != 30 || hits[0].EditedAt.Unix() != 20 {
		t.Errorf("a revoked message: hits = %+v, want it found, marked", hits)
	}
}

func TestSearchAndChatListFoldAlike(t *testing.T) {
	// The fold of the chat list and the one of the index are two code paths;
	// the same person typing the same thing must get the same answer from both.
	db := openWith(t, "alice")
	run(t, db, up(pnMsg("a", 10, "Ёжик в тумане")),
		touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Ёжик в тумане", LastMessageTS: at(10)}))
	for _, q := range []string{"ёжик", "ЕЖИК", "ежик", "ЁЖИК"} {
		hits := search(t, db, SearchQuery{Query: q})
		chats, err := db.Chats(bg, ChatQuery{Query: q, Limit: 10})
		if err != nil || len(hits) != 1 || len(chats) != 1 {
			t.Errorf("%q: %d hits, %d chats, %v; want one of each", q, len(hits), len(chats), err)
		}
	}
}

func TestMatchExpr(t *testing.T) {
	tests := []struct {
		in, want string
		err      error
	}{
		{"привет", `"привет"`, nil},
		{"  привет   мир  ", `"привет" "мир"`, nil},
		{"Ёлка", `"Елка"`, nil},
		{`a"b"c`, `"a""b""c"`, nil},
		{"привет а", `"привет"`, nil},
		{"ab", "", ErrQueryTooShort},
		{"", "", ErrQueryTooShort},
		{"a\x00b cde", `"cde"`, nil},
		{"\xffabc", `"abc"`, nil},
		{strings.Repeat("слово ", 100), strings.TrimSpace(strings.Repeat(`"слово" `, maxWords)), nil},
	}
	for _, tt := range tests {
		got, err := matchExpr(tt.in)
		if got != tt.want || !errors.Is(err, tt.err) {
			t.Errorf("matchExpr(%q) = %q, %v; want %q, %v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

// TestSearchOrderIsTotal: messages of one second come back newest-stored
// first, and the same on every run.
func TestSearchOrderIsTotal(t *testing.T) {
	db := openWith(t, "alice", "bob")
	for i := range 12 {
		account := []string{"alice", "bob"}[i%2]
		run(t, db, up(msg(account, pnChat, seq(i), 100, "same second "+seq(i))))
	}
	want := make([]string, 0, 12)
	for i := 11; i >= 0; i-- {
		want = append(want, seq(i))
	}
	for range 3 {
		if got := hitIDs(search(t, db, SearchQuery{Query: "same second"})); got != strings.Join(want, ",") {
			t.Fatalf("hits = %s\nwant   %v", got, want)
		}
	}
	if got := hitIDs(search(t, db, SearchQuery{Query: "same second", Limit: 3})); !slices.Equal(strings.Split(got, ","), want[:3]) {
		t.Errorf("limited hits = %s", got)
	}
}
