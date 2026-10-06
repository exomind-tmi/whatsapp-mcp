package archive

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCursor(t *testing.T) {
	good := []struct {
		in   string
		want Cursor
	}{
		{"1700000000_42", Cursor{1700000000, 42}},
		{"0_1", Cursor{0, 1}},
		{"5_0", Cursor{5, 0}}, // a time: before everything of second 5
	}
	for _, tt := range good {
		got, err := ParseCursor(tt.in)
		if err != nil || got != tt.want || got.String() != tt.in {
			t.Errorf("ParseCursor(%q) = %v, %v; String = %q; want %v and the same string back", tt.in, got, err, got.String(), tt.want)
		}
	}
	for _, in := range []string{
		"", "_", "5", "5_", "_5", "5_6_7", "-5_6", "5_-6", "+5_6", "5_+6", "a_b", "5.5_6", " 5_6", "5_6 ",
		"1_000_2", "0x10_5", "5_0x10", "0b1_1", "99999999999999999999_1", "9223372036854775808_1",
		"0_0", // no position: it has no spelling (see below)
	} {
		if got, err := ParseCursor(in); err == nil {
			t.Errorf("ParseCursor(%q) = %v, want an error", in, got)
		}
	}
	if !(Cursor{}).IsZero() || (Cursor{TS: 1}).IsZero() || (Cursor{ID: 1}).IsZero() {
		t.Error("IsZero is wrong")
	}
	// The Next of the last page is the zero Cursor. A tool that forgot to look
	// at IsZero would pass its String to the model, and the model would pass it
	// back: if that read as "the start", the newest page would come again for
	// ever. "" is not a token the model could mistake for one, and not a cursor.
	if s := (Cursor{}).String(); s != "" {
		t.Errorf("the zero Cursor is %q, want \"\"", s)
	}
}

// pagedChat stores 28 messages of one chat whose times collide on purpose
// (runs of equal seconds), a few of them stored late as history sync does, and
// returns their ids in the order the chat must show them: by time, and within
// a second by the order they were stored in.
func pagedChat(t *testing.T, db *DB, account, chat string) (order []string) {
	t.Helper()
	type stored struct {
		id string
		ts int64
	}
	var all []stored
	add := func(ts int64, n int) {
		for range n {
			s := stored{fmt.Sprintf("p%02d", len(all)), ts}
			all = append(all, s)
			run(t, db, up(msg(account, chat, s.id, s.ts, "text of "+s.id)))
		}
	}
	add(100, 5)
	add(101, 1)
	add(102, 9)
	add(103, 1)
	add(104, 8)
	add(100, 2) // history sync: older, but stored last
	add(102, 2)
	sort.SliceStable(all, func(i, j int) bool { return all[i].ts < all[j].ts })
	for _, s := range all {
		order = append(order, s.id)
	}
	return order
}

// TestMessagesPaging walks a chat back page by page, at several page sizes:
// every message turns up once, in order, however the seconds collide.
func TestMessagesPaging(t *testing.T) {
	db := openWith(t, "alice", "bob")
	want := pagedChat(t, db, "alice", pnChat)
	pagedChat(t, db, "bob", pnChat)                              // another account, the same ids
	run(t, db, up(msg("alice", group, "g1", 102, "other chat"))) // and another chat

	for _, size := range []int{1, 2, 3, 5, 9, 27, 28, 29, 100} {
		t.Run(fmt.Sprint("pages of ", size), func(t *testing.T) {
			var got []string // newest page first, each page oldest first
			var pages [][]string
			q := MsgQuery{Account: "alice", Chat: pnChat, Limit: size}
			for range 100 {
				page, err := db.Messages(bg, q)
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Messages) > size {
					t.Fatalf("a page of %d for a limit of %d", len(page.Messages), size)
				}
				var ids []string
				for _, m := range page.Messages {
					ids = append(ids, m.ID)
				}
				pages = append(pages, ids)
				if page.Next.IsZero() {
					break
				}
				cur, err := ParseCursor(page.Next.String()) // as a tool passes it on
				if err != nil {
					t.Fatal(err)
				}
				q.Before = cur
			}
			for i := len(pages) - 1; i >= 0; i-- {
				got = append(got, pages[i]...)
			}
			if !slices.Equal(got, want) {
				t.Errorf("walked %v\nwant   %v", got, want)
			}
			if wantPages := (len(want) + size - 1) / size; len(pages) != wantPages {
				t.Errorf("%d pages, want %d: the last one promised another that was empty", len(pages), wantPages)
			}
		})
	}
}

func TestMessagesQuery(t *testing.T) {
	db := openWith(t, "alice")
	order := pagedChat(t, db, "alice", pnChat)
	atSecond := func(from, to int64) []string {
		var ids []string
		for _, id := range order {
			var ts int64
			db.w.QueryRow(`SELECT ts FROM messages WHERE account = 'alice' AND msg_id = ?`, id).Scan(&ts)
			if ts >= from && ts < to {
				ids = append(ids, id)
			}
		}
		return ids
	}
	tests := []struct {
		name string
		q    MsgQuery
		want []string
		more bool
	}{
		{"the newest", MsgQuery{Limit: 3}, order[len(order)-3:], true},
		{"everything", MsgQuery{Limit: 100}, order, false},
		{"exactly all", MsgQuery{Limit: len(order)}, order, false},
		{"a time is a cursor before every message of its second", MsgQuery{Before: Cursor{TS: 102}, Limit: 100}, atSecond(0, 102), false},
		{"after is inclusive", MsgQuery{After: at(103), Limit: 100}, atSecond(103, 200), false},
		{"after bounds the walk back, and says there is no more", MsgQuery{After: at(103), Limit: 4}, atSecond(103, 200)[len(atSecond(103, 200))-4:], true},
		{"a window", MsgQuery{After: at(101), Before: Cursor{TS: 103}, Limit: 100}, atSecond(101, 103), false},
		{"nothing before the first", MsgQuery{Before: Cursor{TS: 100}, Limit: 5}, nil, false},
		{"nothing at all after the last", MsgQuery{After: at(999), Limit: 5}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.Account, tt.q.Chat = "alice", pnChat
			page, err := db.Messages(bg, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range page.Messages {
				got = append(got, m.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("messages = %v\nwant       %v", got, tt.want)
			}
			if page.Next.IsZero() == tt.more {
				t.Errorf("Next = %v, want one: %v", page.Next, tt.more)
			}
		})
	}

	t.Run("no limit is refused", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			_, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: pnChat, Limit: n})
			wantErrIs(t, err, ErrBadLimit)
		}
	})
	t.Run("an unknown chat is empty", func(t *testing.T) {
		page, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: "nobody@s.whatsapp.net", Limit: 5})
		if err != nil || len(page.Messages) != 0 || !page.Next.IsZero() {
			t.Errorf("page = %+v, %v", page, err)
		}
	})
}

func TestMessageFields(t *testing.T) {
	db := openWith(t, "alice")
	row := Row{Account: "alice", Chat: group, ID: "m1", Sender: lidChat, FromMe: true, TS: at(77), Text: "caption",
		MediaType: "image", MediaMime: "image/png", MediaName: "a.png", MediaSize: 99, QuotedID: "q", Raw: []byte("RAW")}
	run(t, db, up(row), edit(Edit{Account: "alice", Chat: group, ID: "m1", Text: "caption 2", EditedAt: at(80)}), revoke(Revoke{Account: "alice", Chat: group, ID: "m1", RevokedAt: at(90)}))
	if err := db.SetMedia(bg, "alice", group, "m1", "/d/a.png", nil); err != nil {
		t.Fatal(err)
	}

	want := Message{Account: "alice", Chat: group, ID: "m1", Sender: lidChat, FromMe: true, TS: at(77), Text: "caption 2",
		MediaType: "image", MediaMime: "image/png", MediaName: "a.png", MediaSize: 99, MediaPath: "/d/a.png", QuotedID: "q",
		EditedAt: at(80), RevokedAt: at(90)}
	check := func(name string, got Message, wantRaw string) {
		t.Helper()
		if string(got.Raw) != wantRaw {
			t.Errorf("%s: raw = %q, want %q", name, got.Raw, wantRaw)
		}
		got.Raw, got.rowid = nil, 0
		if got.TS.Unix() != want.TS.Unix() || got.EditedAt.Unix() != want.EditedAt.Unix() || got.RevokedAt.Unix() != want.RevokedAt.Unix() {
			t.Errorf("%s: times = %v %v %v", name, got.TS, got.EditedAt, got.RevokedAt)
		}
		got.TS, got.EditedAt, got.RevokedAt = want.TS, want.EditedAt, want.RevokedAt
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v\nwant %+v", name, got, want)
		}
	}
	plain, err := db.Message(bg, "alice", group, "m1")
	if err != nil {
		t.Fatal(err)
	}
	check("Message", plain, "") // no raw: the lists must not carry it
	full, err := db.MessageWithRaw(bg, "alice", group, "m1")
	if err != nil {
		t.Fatal(err)
	}
	check("MessageWithRaw", full, "RAW")

	page, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: group, Limit: 5})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("Messages = %+v, %v", page, err)
	}
	check("Messages", page.Messages[0], "")
	if page.Messages[0].Cursor() != full.Cursor() || full.Cursor().IsZero() {
		t.Errorf("cursors differ: %v and %v", page.Messages[0].Cursor(), full.Cursor())
	}

	t.Run("a stub has no raw", func(t *testing.T) {
		run(t, db, revoke(Revoke{Account: "alice", Chat: group, ID: "stub", Sender: lidChat, RevokedAt: at(5)}))
		m, err := db.MessageWithRaw(bg, "alice", group, "stub")
		if err != nil || m.Raw != nil || m.Text != "" || m.RevokedAt.IsZero() {
			t.Errorf("stub = %+v, %v", m, err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		for _, c := range []struct{ account, chat, id string }{
			{"alice", group, "nope"}, {"alice", pnChat, "m1"}, {"bob", group, "m1"},
		} {
			_, err := db.Message(bg, c.account, c.chat, c.id)
			wantErrIs(t, err, ErrNoMessage)
			_, err = db.MessageWithRaw(bg, c.account, c.chat, c.id)
			wantErrIs(t, err, ErrNoMessage)
		}
	})
}

func TestAround(t *testing.T) {
	db := openWith(t, "alice")
	order := pagedChat(t, db, "alice", pnChat) // 28 messages, runs of equal seconds
	run(t, db, up(msg("alice", group, "other", 102, "another chat")))
	pos := func(id string) int { return slices.Index(order, id) }
	window := func(id string, before, after int) []string {
		i := pos(id)
		return order[max(0, i-before):min(len(order), i+after+1)]
	}

	tests := []struct {
		id            string
		before, after int
	}{
		{order[12], 3, 3}, // in the middle of a second that many messages share
		{order[12], 10, 10},
		{order[12], 0, 0},
		{order[12], 0, 4},
		{order[12], 4, 0},
		{order[0], 5, 5},  // at the start
		{order[27], 5, 5}, // at the end
		{order[0], 0, 0},
		{order[13], 100, 100}, // more than there is
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s -%d +%d", tt.id, tt.before, tt.after), func(t *testing.T) {
			msgs, err := db.Around(bg, "alice", pnChat, tt.id, tt.before, tt.after)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			targets := 0
			for _, m := range msgs {
				got = append(got, m.ID)
				if m.Target {
					targets++
					if m.ID != tt.id {
						t.Errorf("%s is the target, want %s", m.ID, tt.id)
					}
				}
			}
			if want := window(tt.id, tt.before, tt.after); !slices.Equal(got, want) {
				t.Errorf("around = %v\nwant     %v", got, want)
			}
			if targets != 1 {
				t.Errorf("%d targets, want 1", targets)
			}
		})
	}

	t.Run("a message of another chat is not a neighbour", func(t *testing.T) {
		msgs, err := db.Around(bg, "alice", group, "other", 5, 5)
		if err != nil || len(msgs) != 1 || !msgs[0].Target {
			t.Errorf("around = %v, %v; want only the target", ids(msgs), err)
		}
	})
	t.Run("unknown message", func(t *testing.T) {
		_, err := db.Around(bg, "alice", pnChat, "nope", 3, 3)
		wantErrIs(t, err, ErrNoMessage)
	})
	t.Run("negative sizes", func(t *testing.T) {
		_, err := db.Around(bg, "alice", pnChat, order[3], -1, 3)
		wantErrIs(t, err, ErrBadLimit)
		_, err = db.Around(bg, "alice", pnChat, order[3], 3, -1)
		wantErrIs(t, err, ErrBadLimit)
	})
}

// TestAroundMatchesPaging: the neighbours Around gives are the ones a walk
// of Messages passes, for every message of the chat.
func TestAroundMatchesPaging(t *testing.T) {
	db := openWith(t, "alice")
	order := pagedChat(t, db, "alice", pnChat)
	for i, id := range order {
		msgs, err := db.Around(bg, "alice", pnChat, id, 2, 2)
		if err != nil {
			t.Fatal(err)
		}
		if want := order[max(0, i-2):min(len(order), i+3)]; !strings.EqualFold(ids(msgs), strings.Join(want, ",")) {
			t.Errorf("around %s = %s, want %v", id, ids(msgs), want)
		}
	}
}

// TestAroundIsOneSnapshot: the message and the ones around it are read from one
// state of the archive. While a writer merges the chat (the messages move from
// the number to the LID and back), a window that was read in three statements
// would have found the message before the merge and its neighbours after it:
// the message alone. Either the whole window or no message is the answer.
func TestAroundIsOneSnapshot(t *testing.T) {
	db := openWith(t, "alice")
	seed := func(tx *Tx) error {
		for i, id := range []string{"a", "b", "c"} {
			if err := tx.Upsert(msg("alice", pnChat, id, int64(i+1), id+" text")); err != nil {
				return err
			}
		}
		return nil
	}
	if err := db.Tx(bg, seed); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				writerDone <- nil
				return
			default:
			}
			err := db.Tx(bg, func(tx *Tx) error { return tx.MergeChat("alice", pnChat, lidChat) })
			if err == nil { // put the chat back under the number
				_, err = db.w.ExecContext(bg, `DELETE FROM messages WHERE chat_jid = ?`, lidChat)
			}
			if err == nil {
				err = db.Tx(bg, seed)
			}
			if err != nil {
				writerDone <- err
				return
			}
		}
	}()

	var full, torn atomic.Int64
	deadline := time.Now().Add(10 * time.Second)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) && torn.Load() == 0 && full.Load() < 1000 {
				msgs, err := db.Around(bg, "alice", pnChat, "b", 1, 1)
				switch {
				case errors.Is(err, ErrNoMessage): // merged away at that moment
				case err != nil:
					t.Error(err)
					return
				case len(msgs) == 3:
					full.Add(1)
				default:
					torn.Add(1)
					t.Errorf("around b = %s, want a,b,c or no message", ids(msgs))
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	t.Logf("%d whole windows", full.Load())
}
