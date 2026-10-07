package archive

import (
	"fmt"
	"reflect"
	"testing"
)

// pnMsg and lidMsg are messages of alice's chat under the phone number and
// under the LID; the sender is the chat, as for a message that was received.
func pnMsg(id string, ts int64, text string) Row  { return msg("alice", pnChat, id, ts, text) }
func lidMsg(id string, ts int64, text string) Row { return msg("alice", lidChat, id, ts, text) }

func revokeIn(chat, id string, ts int64) op {
	return revoke(Revoke{Account: "alice", Chat: chat, ID: id, Sender: chat, RevokedAt: at(ts)})
}

func editIn(chat, id, text string, ts int64) op {
	return edit(Edit{Account: "alice", Chat: chat, ID: id, Sender: chat, Text: text, EditedAt: at(ts)})
}

type chatRow struct {
	PN, Name string
	IsGroup  bool
	Last     int64
}

func TestMergeChat(t *testing.T) {
	tests := []struct {
		name  string
		setup []op
		want  map[string]state // the messages that must be under the LID afterwards
		chat  *chatRow         // the chat row that must be under the LID, nil: none
	}{
		{
			name: "only under the number",
			setup: []op{up(pnMsg("a", 10, "alpha")), up(pnMsg("b", 20, "beta")),
				touch(ChatUpd{Account: "alice", JID: pnChat, PN: pnChat, Name: "Bob", LastMessageTS: at(20)})},
			want: map[string]state{
				"a": {Sender: pnChat, Text: "alpha", TS: 10, HasRaw: true},
				"b": {Sender: pnChat, Text: "beta", TS: 20, HasRaw: true},
			},
			chat: &chatRow{PN: pnChat, Name: "Bob", Last: 20},
		},
		{
			name:  "only under the LID",
			setup: []op{up(lidMsg("a", 10, "alpha")), touch(ChatUpd{Account: "alice", JID: lidChat, Name: "Bob", LastMessageTS: at(10)})},
			want:  map[string]state{"a": {Sender: lidChat, Text: "alpha", TS: 10, HasRaw: true}},
			chat:  &chatRow{Name: "Bob", Last: 10},
		},
		{
			name: "both, different messages",
			setup: []op{up(pnMsg("a", 10, "alpha")), up(lidMsg("b", 20, "beta")),
				touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Bob (number)", LastMessageTS: at(10)}),
				touch(ChatUpd{Account: "alice", JID: lidChat, LastMessageTS: at(20)})},
			want: map[string]state{
				"a": {Sender: pnChat, Text: "alpha", TS: 10, HasRaw: true},
				"b": {Sender: lidChat, Text: "beta", TS: 20, HasRaw: true},
			},
			chat: &chatRow{PN: pnChat, Name: "Bob (number)", Last: 20},
		},
		{
			name:  "the same message on both sides is one",
			setup: []op{up(pnMsg("a", 10, "alpha")), up(lidMsg("a", 10, "alpha"))},
			want:  map[string]state{"a": {Sender: lidChat, Text: "alpha", TS: 10, HasRaw: true}},
		},
		{
			// The case a merge by UPDATE OR IGNORE and DELETE loses: the PN
			// row is skipped, and the DELETE that follows takes the original
			// with it.
			name: "a stub under the LID and the original under the number",
			setup: []op{
				revokeIn(lidChat, "a", 11),
				up(Row{Account: "alice", Chat: pnChat, ID: "a", Sender: pnChat, FromMe: true, TS: at(5), Text: "secret", Raw: []byte("raw:a")}),
			},
			want: map[string]state{"a": {Sender: pnChat, FromMe: true, Text: "secret", TS: 5, HasRaw: true, Revoked: 11}},
		},
		{
			name:  "the original under the LID and a stub under the number",
			setup: []op{up(lidMsg("a", 5, "secret")), revokeIn(pnChat, "a", 11)},
			want:  map[string]state{"a": {Sender: lidChat, Text: "secret", TS: 5, HasRaw: true, Revoked: 11}},
		},
		{
			name:  "an edit stub under the LID and the original under the number",
			setup: []op{editIn(lidChat, "a", "edited", 30), up(pnMsg("a", 5, "first"))},
			want:  map[string]state{"a": {Sender: pnChat, Text: "edited", TS: 5, HasRaw: true, Edited: 30}},
		},
		{
			name:  "the later edit wins, from either side",
			setup: []op{up(lidMsg("a", 5, "first")), editIn(lidChat, "a", "edit one", 30), editIn(pnChat, "a", "edit two", 40)},
			want:  map[string]state{"a": {Sender: lidChat, Text: "edit two", TS: 5, HasRaw: true, Edited: 40}},
		},
		{
			name:  "the later edit wins when it is on the LID side",
			setup: []op{up(pnMsg("a", 5, "first")), editIn(pnChat, "a", "edit one", 30), editIn(lidChat, "a", "edit two", 40)},
			want:  map[string]state{"a": {Sender: pnChat, Text: "edit two", TS: 5, HasRaw: true, Edited: 40}},
		},
		{
			// An edit may clear a caption. The text it leaves is NULL, which the
			// other side's copy must not fill in again.
			name:  "a text that an edit cleared under the LID stays cleared",
			setup: []op{up(lidMsg("a", 5, "caption")), editIn(lidChat, "a", "", 30), up(pnMsg("a", 5, "caption"))},
			want:  map[string]state{"a": {Sender: lidChat, TS: 5, HasRaw: true, Edited: 30}},
		},
		{
			name:  "a text that an edit stub under the LID cleared stays cleared when the original comes under the number",
			setup: []op{editIn(lidChat, "a", "", 30), up(pnMsg("a", 5, "caption"))},
			want:  map[string]state{"a": {Sender: pnChat, TS: 5, HasRaw: true, Edited: 30}},
		},
		{
			name:  "a text that an edit cleared under the number stays cleared",
			setup: []op{up(lidMsg("a", 5, "caption")), up(pnMsg("a", 5, "caption")), editIn(pnChat, "a", "", 30)},
			want:  map[string]state{"a": {Sender: lidChat, TS: 5, HasRaw: true, Edited: 30}},
		},
		{
			name: "an older edit under the number does not bring back a text that a later edit cleared",
			setup: []op{up(lidMsg("a", 5, "first")), up(pnMsg("a", 5, "first")),
				editIn(pnChat, "a", "older edit", 20), editIn(lidChat, "a", "", 30)},
			want: map[string]state{"a": {Sender: lidChat, TS: 5, HasRaw: true, Edited: 30}},
		},
		{
			name:  "two stubs, an edit and a revoke",
			setup: []op{editIn(lidChat, "a", "edited", 30), revokeIn(pnChat, "a", 40)},
			want:  map[string]state{"a": {Sender: lidChat, Text: "edited", TS: 30, Edited: 30, Revoked: 40}},
		},
		{
			name: "the download of the number side is kept",
			setup: []op{up(pnMsg("a", 10, "alpha")), up(lidMsg("a", 10, "alpha")),
				func(tx *Tx) error { return tx.SetMedia("alice", pnChat, "a", "/d/a.jpg", nil) }},
			want: map[string]state{"a": {Sender: lidChat, Text: "alpha", TS: 10, HasRaw: true, MediaPath: "/d/a.jpg"}},
		},
		{
			name: "a chat of the number side with no message keeps no time",
			setup: []op{touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Bob"}),
				touch(ChatUpd{Account: "alice", JID: lidChat})},
			want: map[string]state{},
			chat: &chatRow{PN: pnChat, Name: "Bob"},
		},
		{
			name: "the group flag of the number side is kept",
			setup: []op{touch(ChatUpd{Account: "alice", JID: pnChat, IsGroup: true}),
				touch(ChatUpd{Account: "alice", JID: lidChat})},
			want: map[string]state{},
			chat: &chatRow{PN: pnChat, IsGroup: true},
		},
		{
			name: "the LID's own name wins, the number side supplies the pn and what is missing",
			setup: []op{touch(ChatUpd{Account: "alice", JID: pnChat, PN: pnChat, Name: "Old name", LastMessageTS: at(50)}),
				touch(ChatUpd{Account: "alice", JID: lidChat, Name: "New name", LastMessageTS: at(40)})},
			want: map[string]state{},
			chat: &chatRow{PN: pnChat, Name: "New name", Last: 50},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openWith(t, "alice")
			run(t, db, tt.setup...)

			run(t, db, merge("alice", pnChat, lidChat))

			if n := count(t, db, `SELECT count(*) FROM messages WHERE chat_jid = ?`, pnChat); n != 0 {
				t.Errorf("%d messages are left under the number", n)
			}
			if n := count(t, db, `SELECT count(*) FROM chats WHERE jid = ?`, pnChat); n != 0 {
				t.Errorf("the chat row is left under the number")
			}
			if n := count(t, db, `SELECT count(*) FROM messages`); n != len(tt.want) {
				t.Errorf("%d messages in all, want %d", n, len(tt.want))
			}
			for id, want := range tt.want {
				if got := stateAt(t, db, "alice", lidChat, id); got != want {
					t.Errorf("message %s = %+v, want %+v", id, got, want)
				}
			}
			assertChatRow(t, db, tt.chat)
			checkFTS(t, db)

			// A second merge, as a second message of the chat would make it,
			// finds nothing to do.
			run(t, db, merge("alice", pnChat, lidChat))
			for id, want := range tt.want {
				if got := stateAt(t, db, "alice", lidChat, id); got != want {
					t.Errorf("message %s after a second merge = %+v, want %+v", id, got, want)
				}
			}
			assertChatRow(t, db, tt.chat)
		})
	}
}

// assertChatRow checks the chat row of the LID chat; a want of nil is no row.
// A last time of 0 in want means NULL, and is read from the table itself, as
// a NULL that became a 0 would read as the same number.
func assertChatRow(t *testing.T, db *DB, want *chatRow) {
	t.Helper()
	chats, err := db.Chats(bg, ChatQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var got *chatRow
	for _, c := range chats {
		if c.JID == lidChat {
			got = &chatRow{c.PN, c.Name, c.IsGroup, unixOrZero(c.LastMessageTS)}
		}
	}
	switch {
	case want == nil && got != nil:
		t.Errorf("chat row = %+v, want none", *got)
	case want != nil && got == nil:
		t.Errorf("no chat row, want %+v", *want)
	case want != nil && *got != *want:
		t.Errorf("chat row = %+v, want %+v", *got, *want)
	}
	if want != nil && want.Last == 0 && count(t, db, `SELECT count(*) FROM chats WHERE jid = ? AND last_message_ts IS NULL`, lidChat) != 1 {
		t.Error("last_message_ts is not NULL: a chat with no message must not get a time")
	}
}

// TestMergeChatFTS: the merged text is in the index once, under the id the
// message has now, found in the LID chat and not in the number's.
func TestMergeChatFTS(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db,
		up(pnMsg("a", 10, "zebra stripes")), up(pnMsg("b", 20, "common ground")),
		up(lidMsg("b", 20, "common ground")), up(lidMsg("c", 30, "lid only")),
		revokeIn(lidChat, "d", 40), up(pnMsg("d", 35, "was revoked")),
		touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Bob"}))
	before := count(t, db, `SELECT count(*) FROM messages_fts`)

	run(t, db, merge("alice", pnChat, lidChat))

	checkFTS(t, db)
	if n := count(t, db, `SELECT count(*) FROM messages_fts`); n != before-1 { // "common ground" was on both sides
		t.Errorf("%d index rows after the merge, %d before", n, before)
	}
	for word, want := range map[string]int{"zebra": 1, "common": 1, "lid": 1, "revoked": 1} {
		hits, err := db.Search(bg, SearchQuery{Query: word, Limit: 10})
		if err != nil || len(hits) != want {
			t.Errorf("search %q: %d hits, %v; want %d", word, len(hits), err, want)
		}
		for _, h := range hits {
			if h.Chat != lidChat {
				t.Errorf("search %q found a message in %s, want the LID chat", word, h.Chat)
			}
		}
	}
}

// TestMergeChatKeepsTheOrderOfOneSecond: the messages of a second are told
// apart by the id they were stored with, and a merge must not shuffle them.
func TestMergeChatKeepsTheOrderOfOneSecond(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(pnMsg("z", 10, "first stored")), up(pnMsg("y", 10, "second stored")), up(pnMsg("x", 10, "third stored")))

	run(t, db, merge("alice", pnChat, lidChat))

	page, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: lidChat, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page.Messages); got != "z,y,x" {
		t.Errorf("order = %s, want z,y,x", got)
	}
}

// TestMergeChatKeepsAPositionInTheChat: a position in a chat that a client holds (the
// cursor of a page) is still one after the number's chat is merged into the LID's, or the
// messages of the second the cursor is in, that are on the other side of it, are lost.
func TestMergeChatKeepsAPositionInTheChat(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db,
		up(pnMsg("e", 5, "epsilon")),
		up(pnMsg("a", 10, "alpha")), up(pnMsg("b", 10, "beta")), up(pnMsg("c", 10, "gamma")), up(pnMsg("d", 10, "delta")))
	newest, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: pnChat, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(newest.Messages); got != "c,d" {
		t.Fatalf("the newest page is %s", got)
	}

	run(t, db, merge("alice", pnChat, lidChat))

	older, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: lidChat, Limit: 10, Before: newest.Next})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(older.Messages); got != "e,a,b" {
		t.Errorf("the page before the position, after the merge, is %q: want \"e,a,b\" (a and b were of the same second as c)", got)
	}
	checkFTS(t, db)
}

// TestMergeChatKeepsTheIDsOfWhatItMoves: a message that is only on the number's side
// has the same id (and so the same cursor) in the LID's chat; one that is on both sides
// is the LID side's, which is the one that stays.
func TestMergeChatKeepsTheIDsOfWhatItMoves(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db,
		up(pnMsg("only-pn", 10, "number side")), up(pnMsg("both", 20, "on both")),
		up(lidMsg("both", 20, "on both")), up(lidMsg("only-lid", 30, "lid side")))
	cursors := func(chat string) map[string]Cursor {
		page, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: chat, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Cursor{}
		for _, m := range page.Messages {
			out[m.ID] = m.Cursor()
		}
		return out
	}
	pn, lid := cursors(pnChat), cursors(lidChat)

	run(t, db, merge("alice", pnChat, lidChat))

	after := cursors(lidChat)
	if len(after) != 3 {
		t.Fatalf("%d messages after the merge, want 3: %v", len(after), after)
	}
	if after["only-pn"] != pn["only-pn"] {
		t.Errorf("the message of the number's side has the cursor %v, had %v", after["only-pn"], pn["only-pn"])
	}
	for _, id := range []string{"both", "only-lid"} {
		if after[id] != lid[id] {
			t.Errorf("%s has the cursor %v, had %v on the LID's side", id, after[id], lid[id])
		}
	}
	if n := count(t, db, `SELECT count(*) FROM messages WHERE chat_jid = '`+pnChat+`'`); n != 0 {
		t.Errorf("%d messages are left under the number", n)
	}
	checkFTS(t, db)
}

// TestMergeChatLeavesOthersAlone: other chats of the account, and the same
// chat of another account, are not touched.
func TestMergeChatLeavesOthersAlone(t *testing.T) {
	db := openWith(t, "alice", "bob")
	bobs := pnMsg("a", 10, "bob's")
	bobs.Account = "bob"
	run(t, db, up(pnMsg("a", 10, "alice's")), up(msg("alice", group, "g1", 5, "in a group")), up(bobs),
		touch(ChatUpd{Account: "bob", JID: pnChat, Name: "Bobs Bob"}))

	run(t, db, merge("alice", pnChat, lidChat))

	if got := stateAt(t, db, "bob", pnChat, "a"); got.Text != "bob's" {
		t.Errorf("bob's message = %+v", got)
	}
	if got := stateAt(t, db, "alice", group, "g1"); got.Text != "in a group" {
		t.Errorf("alice's group message = %+v", got)
	}
	if n := count(t, db, `SELECT count(*) FROM chats WHERE account = 'bob' AND jid = ?`, pnChat); n != 1 {
		t.Errorf("bob lost his chat")
	}
}

// TestMergeChatIntoItself: nothing is merged and nothing is lost.
func TestMergeChatIntoItself(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(lidMsg("a", 10, "alpha")), touch(ChatUpd{Account: "alice", JID: lidChat, Name: "Bob"}))

	run(t, db, merge("alice", lidChat, lidChat))

	if got := stateAt(t, db, "alice", lidChat, "a"); got.Text != "alpha" {
		t.Errorf("message = %+v: merging a chat into itself deleted it", got)
	}
	assertChatRow(t, db, &chatRow{Name: "Bob"})
}

// TestMergeChatInOneTx: the merge is part of the caller's transaction, and
// rolled back with it.
func TestMergeChatInOneTx(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(pnMsg("a", 10, "alpha")))
	err := db.Tx(bg, func(tx *Tx) error {
		if err := tx.MergeChat("alice", pnChat, lidChat); err != nil {
			return err
		}
		return errTest
	})
	wantErrIs(t, err, errTest)
	if got := stateAt(t, db, "alice", pnChat, "a"); got.Text != "alpha" {
		t.Errorf("message = %+v: the merge was not rolled back", got)
	}
	if n := count(t, db, `SELECT count(*) FROM messages WHERE chat_jid = ?`, lidChat); n != 0 {
		t.Errorf("%d messages under the LID after a rollback", n)
	}
	checkFTS(t, db)
}

// TestMergeChatKeepsEveryField: what the number side has and the LID side
// lacks arrives whole, the media fields and the quote too, not only the ones
// the table above looks at.
func TestMergeChatKeepsEveryField(t *testing.T) {
	db := openWith(t, "alice")
	orig := Row{Account: "alice", Chat: pnChat, ID: "a", Sender: pnChat, TS: at(5), Text: "caption",
		MediaType: "image", MediaMime: "image/jpeg", MediaName: "p.jpg", MediaSize: 777, QuotedID: "q1", Raw: []byte("RAW")}
	run(t, db, revokeIn(lidChat, "a", 11), up(orig),
		func(tx *Tx) error { return tx.SetMedia("alice", pnChat, "a", "/d/p.jpg", nil) })

	run(t, db, merge("alice", pnChat, lidChat))

	got, err := db.MessageWithRaw(bg, "alice", lidChat, "a")
	if err != nil {
		t.Fatal(err)
	}
	got.rowid = 0
	want := Message{Account: "alice", Chat: lidChat, ID: "a", Sender: pnChat, TS: at(5), Text: "caption",
		MediaType: "image", MediaMime: "image/jpeg", MediaName: "p.jpg", MediaSize: 777, MediaPath: "/d/p.jpg", QuotedID: "q1",
		RevokedAt: at(11), Raw: []byte("RAW")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %+v\nwant   %+v", got, want)
	}
}

// TestMergeChatKeepsTheLIDRevokeTime: a message revoked on both sides keeps the
// time the LID side has, the same on every merge.
func TestMergeChatKeepsTheLIDRevokeTime(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(lidMsg("a", 5, "x text")), revokeIn(lidChat, "a", 11), up(pnMsg("a", 5, "x text")), revokeIn(pnChat, "a", 22))

	run(t, db, merge("alice", pnChat, lidChat))

	if got := stateAt(t, db, "alice", lidChat, "a"); got.Revoked != 11 {
		t.Errorf("revoked_at = %d, want 11", got.Revoked)
	}
}

// TestMergeChatTakesTheTimeOfTheNumberChat: the LID chat may be known by its
// name before it has had a message, while the number chat has the time; the
// merge brings the time over, and the chat is not left with none.
func TestMergeChatTakesTheTimeOfTheNumberChat(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db,
		touch(ChatUpd{Account: "alice", JID: lidChat, Name: "Bob"}),
		touch(ChatUpd{Account: "alice", JID: pnChat, LastMessageTS: at(50)}))

	run(t, db, merge("alice", pnChat, lidChat))

	assertChatRow(t, db, &chatRow{PN: pnChat, Name: "Bob", Last: 50})
}

// TestOrderIndependenceWithMerge: the three events of one message (the
// message, its edit, its revoke) arrive in any order, each under the number or
// under the LID, and the chat is merged at any point between them (the events
// after it come under the LID, as they do once the LID is known). The archive
// ends in one state whatever happened, whether the edit changes the text or
// clears it.
func TestOrderIndependenceWithMerge(t *testing.T) {
	if raceBuild {
		t.Skip("one goroutine: the race detector only makes it a hundred times slower")
	}
	const sender = "5551@s.whatsapp.net"
	for _, tt := range []struct{ name, edited string }{
		{"an edit", "hello world"},
		{"an edit that clears the text", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := state{Sender: sender, Text: tt.edited, TS: 100, HasRaw: true, Edited: 200, Revoked: 300}
			events := map[string]func(account, chat string) op{
				"message": func(a, c string) op {
					return up(Row{Account: a, Chat: c, ID: "m", Sender: sender, TS: at(100), Text: "hello", Raw: []byte("raw")})
				},
				"edit": func(a, c string) op {
					return edit(Edit{Account: a, Chat: c, ID: "m", Sender: sender, Text: tt.edited, EditedAt: at(200)})
				},
				"revoke": func(a, c string) op {
					return revoke(Revoke{Account: a, Chat: c, ID: "m", Sender: sender, RevokedAt: at(300)})
				},
			}
			db := openTemp(t)
			n := 0
			for sides := 0; sides < 8; sides++ {
				for _, order := range permutations([]string{"message", "edit", "revoke"}) {
					for mergeAfter := 0; mergeAfter <= 3; mergeAfter++ {
						n++
						account := fmt.Sprintf("c%d", n)
						if err := db.AddAccount(bg, account); err != nil {
							t.Fatal(err)
						}
						for i, ev := range order {
							if i == mergeAfter {
								run(t, db, merge(account, pnChat, lidChat))
							}
							chat := pnChat
							if sides>>i&1 == 1 || i >= mergeAfter {
								chat = lidChat
							}
							run(t, db, events[ev](account, chat))
						}
						run(t, db, merge(account, pnChat, lidChat))
						if got := stateAt(t, db, account, lidChat, "m"); got != want {
							t.Errorf("sides=%03b order=%v merge after %d: state = %+v, want %+v", sides, order, mergeAfter, got, want)
						}
					}
				}
			}
			checkFTS(t, db)
		})
	}
}
