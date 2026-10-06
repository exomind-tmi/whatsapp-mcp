package archive

import (
	"context"
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNoReplace guards the one rule the triggers depend on: a row deleted by
// REPLACE does not fire messages_ad, and its text stays in the full-text
// index for ever. Of the package's sources, the string literals (the SQL) are
// searched for a REPLACE statement, so that one cannot come back with a later
// edit; the replace() function the triggers use is not one.
func TestNoReplace(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\bOR\s+REPLACE\b|\bREPLACE\s+INTO\b`)
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s scanner.Scanner
		s.Init(token.NewFileSet().AddFile(f, -1, len(src)), src, nil, 0)
		for {
			_, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			if tok == token.STRING && banned.MatchString(lit) {
				t.Errorf("%s has a REPLACE statement in %.60q", f, lit)
			}
		}
	}
}

// The three events one message can have, in the shapes the rules are tested
// with: the message at 100 with the text "hello", its edit at 200 and its
// revoke at 300.
var (
	original = msg("alice", pnChat, "m1", 100, "hello")
	edited   = Edit{Account: "alice", Chat: pnChat, ID: "m1", Sender: pnChat, Text: "hello world", EditedAt: at(200)}
	revoked  = Revoke{Account: "alice", Chat: pnChat, ID: "m1", Sender: pnChat, RevokedAt: at(300)}
)

func TestWriteRules(t *testing.T) {
	edit2 := edited
	edit2.Text, edit2.EditedAt = "third", at(250)
	other := original
	other.Text, other.Sender, other.TS = "something else", "x@s.whatsapp.net", at(5)
	revokedLater := revoked
	revokedLater.RevokedAt = at(400)

	plain := state{Sender: pnChat, Text: "hello", TS: 100, HasRaw: true}
	withEdit := state{Sender: pnChat, Text: "hello world", TS: 100, HasRaw: true, Edited: 200}
	tests := []struct {
		name string
		ops  []op
		want state
	}{
		{"a message", []op{up(original)}, plain},
		{"a redelivery changes nothing", []op{up(original), up(original)}, plain},
		{"a redelivery does not overwrite a message", []op{up(original), up(other)}, plain},
		{"an edit", []op{up(original), edit(edited)}, withEdit},
		{"an edit redelivered", []op{up(original), edit(edited), edit(edited)}, withEdit},
		{"a message redelivered after its edit keeps the edit", []op{up(original), edit(edited), up(original)}, withEdit},
		{"an edit before the message, which fills the stub and keeps the edit text", []op{edit(edited), up(original)}, withEdit},
		{"an edit of an unknown message is a stub dated by the edit",
			[]op{edit(edited)}, state{Sender: pnChat, Text: "hello world", TS: 200, Edited: 200}},
		{"two edits, the later first", []op{up(original), edit(edit2), edit(edited)},
			state{Sender: pnChat, Text: "third", TS: 100, HasRaw: true, Edited: 250}},
		{"two edits, the earlier first", []op{up(original), edit(edited), edit(edit2)},
			state{Sender: pnChat, Text: "third", TS: 100, HasRaw: true, Edited: 250}},
		{"a revoke keeps the text and the raw", []op{up(original), revoke(revoked)},
			state{Sender: pnChat, Text: "hello", TS: 100, HasRaw: true, Revoked: 300}},
		{"a revoke redelivered", []op{up(original), revoke(revoked), revoke(revoked)},
			state{Sender: pnChat, Text: "hello", TS: 100, HasRaw: true, Revoked: 300}},
		{"the first revoke time stays", []op{up(original), revoke(revoked), revoke(revokedLater)},
			state{Sender: pnChat, Text: "hello", TS: 100, HasRaw: true, Revoked: 300}},
		{"a revoke before the message, which fills the stub and keeps revoked_at", []op{revoke(revoked), up(original)},
			state{Sender: pnChat, Text: "hello", TS: 100, HasRaw: true, Revoked: 300}},
		{"a revoke of an unknown message is a stub with no text, dated by the revoke",
			[]op{revoke(revoked)}, state{Sender: pnChat, TS: 300, Revoked: 300}},
		{"an edit after the revoke still sets the text", []op{up(original), revoke(revoked), edit(edited)},
			state{Sender: pnChat, Text: "hello world", TS: 100, HasRaw: true, Edited: 200, Revoked: 300}},
		{"a redelivery after edit and revoke", []op{up(original), edit(edited), revoke(revoked), up(original)},
			state{Sender: pnChat, Text: "hello world", TS: 100, HasRaw: true, Edited: 200, Revoked: 300}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openWith(t, "alice")
			run(t, db, tt.ops...)
			if got := stateAt(t, db, "alice", pnChat, "m1"); got != tt.want {
				t.Errorf("state = %+v, want %+v", got, tt.want)
			}
			if n := count(t, db, `SELECT count(*) FROM messages`); n != 1 {
				t.Errorf("%d rows, want 1", n)
			}
			checkFTS(t, db)
		})
	}
}

// TestWriteOrderDoesNotMatter: history sync brings the edit and the revoke of
// a message in any order relative to the message and to each other, and the
// archive must end in the same state whichever it was.
func TestWriteOrderDoesNotMatter(t *testing.T) {
	events := map[string]op{"message": up(original), "edit": edit(edited), "revoke": revoke(revoked)}
	want := state{Sender: pnChat, Text: "hello world", TS: 100, HasRaw: true, Edited: 200, Revoked: 300}
	for _, order := range permutations([]string{"message", "edit", "revoke"}) {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			db := openWith(t, "alice")
			for _, name := range order {
				run(t, db, events[name])
			}
			if got := stateAt(t, db, "alice", pnChat, "m1"); got != want {
				t.Errorf("state = %+v, want %+v", got, want)
			}
			if hits := ftsHits(t, db, "hello"); hits != 1 {
				t.Errorf("the text is in the index %d times, want 1", hits)
			}
			checkFTS(t, db)
		})
	}
}

func permutations(s []string) [][]string {
	if len(s) <= 1 {
		return [][]string{append([]string(nil), s...)}
	}
	var out [][]string
	for i := range s {
		rest := append(append([]string(nil), s[:i]...), s[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{s[i]}, p...))
		}
	}
	return out
}

// TestUpsertStoresEverything: every field of a row comes back, and a stub the
// message fills takes the media fields too.
func TestUpsertStoresEverything(t *testing.T) {
	db := openWith(t, "alice")
	r := Row{Account: "alice", Chat: group, ID: "pic", Sender: lidChat, FromMe: true, TS: at(77), Text: "a caption",
		MediaType: "image", MediaMime: "image/jpeg", MediaName: "p.jpg", MediaSize: 12345, QuotedID: "q1", Raw: []byte{1, 2, 3}}

	run(t, db, revoke(Revoke{Account: "alice", Chat: group, ID: "pic", RevokedAt: at(90)}), up(r))

	want := Message{Account: "alice", Chat: group, ID: "pic", Sender: lidChat, FromMe: true, TS: at(77), Text: "a caption",
		MediaType: "image", MediaMime: "image/jpeg", MediaName: "p.jpg", MediaSize: 12345, QuotedID: "q1",
		RevokedAt: at(90), Raw: []byte{1, 2, 3}}
	got, err := db.MessageWithRaw(bg, "alice", group, "pic")
	if err != nil {
		t.Fatal(err)
	}
	got.rowid = 0
	if !reflect.DeepEqual(got, want) {
		t.Errorf("message = %+v\nwant      %+v", got, want)
	}
}

// TestUpsertWithoutRawIsStillAMessage: the raw is what separates a message
// from a stub, so a row given none gets an empty one, and a redelivery with
// other content cannot replace it as it would a stub.
func TestUpsertWithoutRawIsStillAMessage(t *testing.T) {
	db := openWith(t, "alice")
	bare := msg("alice", pnChat, "m1", 100, "hello")
	bare.Raw = nil
	changed := bare
	changed.Text = "changed"

	run(t, db, up(bare), up(changed))

	if count(t, db, `SELECT count(*) FROM messages WHERE typeof(raw) = 'blob' AND length(raw) = 0`) != 1 {
		t.Errorf("a row without raw is not stored with an empty blob")
	}
	if got := stateAt(t, db, "alice", pnChat, "m1"); got.Text != "hello" {
		t.Errorf("text = %q: the redelivery overwrote a message that has no raw", got.Text)
	}
}

func TestWriteRejectsIncompleteInput(t *testing.T) {
	db := openWith(t, "alice")
	tests := []struct {
		name string
		op   op
	}{
		{"upsert without account", up(Row{Chat: pnChat, ID: "m1", Sender: pnChat, TS: at(1)})},
		{"upsert without chat", up(Row{Account: "alice", ID: "m1", Sender: pnChat, TS: at(1)})},
		{"upsert without id: all would share a row", up(Row{Account: "alice", Chat: pnChat, Sender: pnChat, TS: at(1)})},
		{"upsert without sender", up(Row{Account: "alice", Chat: pnChat, ID: "m1", TS: at(1)})},
		{"upsert without time", up(Row{Account: "alice", Chat: pnChat, ID: "m1", Sender: pnChat})},
		{"edit without id", edit(Edit{Account: "alice", Chat: pnChat, EditedAt: at(1)})},
		{"edit without time", edit(Edit{Account: "alice", Chat: pnChat, ID: "m1"})},
		{"revoke without chat", revoke(Revoke{Account: "alice", ID: "m1", RevokedAt: at(1)})},
		{"revoke without time", revoke(Revoke{Account: "alice", Chat: pnChat, ID: "m1"})},
		{"touch without jid", touch(ChatUpd{Account: "alice"})},
		{"merge without account", merge("", pnChat, lidChat)},
		{"merge without target", merge("alice", pnChat, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := db.Tx(bg, func(tx *Tx) error { return tt.op(tx) }); err == nil {
				t.Error("no error")
			}
		})
	}
	if n := count(t, db, `SELECT (SELECT count(*) FROM messages) + (SELECT count(*) FROM chats)`); n != 0 {
		t.Errorf("%d rows stored from refused writes", n)
	}
}

// TestTx: all of a transaction's writes or none, whichever way it ends.
func TestTx(t *testing.T) {
	errBoom := errors.New("boom")
	writes := func(tx *Tx) error {
		if err := tx.Upsert(original); err != nil {
			return err
		}
		return tx.TouchChat(ChatUpd{Account: "alice", JID: pnChat, LastMessageTS: at(100)})
	}
	// A transaction that was not ended keeps the writer's one connection, so
	// the wait is bounded: a hang would say nothing of the cause.
	rows := func(db *DB) int {
		ctx, cancel := context.WithTimeout(bg, 2*time.Second)
		defer cancel()
		var n int
		err := db.w.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages) + (SELECT count(*) FROM chats) + (SELECT count(*) FROM messages_fts)`).Scan(&n)
		if err != nil {
			t.Fatalf("the writer is not free after the transaction (is it still open?): %v", err)
		}
		return n
	}

	t.Run("commit", func(t *testing.T) {
		db := openWith(t, "alice")
		if err := db.Tx(bg, writes); err != nil {
			t.Fatal(err)
		}
		if n := rows(db); n != 3 {
			t.Errorf("%d rows, want a message, its chat and its index entry", n)
		}
	})
	t.Run("an error rolls back and is returned as it is", func(t *testing.T) {
		db := openWith(t, "alice")
		err := db.Tx(bg, func(tx *Tx) error {
			if err := writes(tx); err != nil {
				return err
			}
			return errBoom
		})
		wantErrIs(t, err, errBoom)
		if n := rows(db); n != 0 {
			t.Errorf("%d rows after a rollback", n)
		}
	})
	t.Run("a failed write rolls back the ones before it", func(t *testing.T) {
		db := openWith(t, "alice")
		err := db.Tx(bg, func(tx *Tx) error {
			if err := writes(tx); err != nil {
				return err
			}
			return tx.TouchChat(ChatUpd{Account: "nobody", JID: pnChat}) // no such account
		})
		if err == nil {
			t.Fatal("a chat of an account that does not exist was stored")
		}
		if n := rows(db); n != 0 {
			t.Errorf("%d rows after a rollback", n)
		}
	})
	t.Run("a panic rolls back, is not swallowed, and leaves the writer usable", func(t *testing.T) {
		db := openWith(t, "alice")
		func() {
			defer func() {
				if p := recover(); p != "oops" {
					t.Errorf("recovered %v, want the panic to go on", p)
				}
			}()
			db.Tx(bg, func(tx *Tx) error {
				writes(tx)
				panic("oops")
			})
		}()
		if n := rows(db); n != 0 {
			t.Errorf("%d rows after a panic", n)
		}
		// A transaction left open would keep the writer's one connection for
		// ever, so the wait for it is bounded: a hang would hide the cause.
		ctx, cancel := context.WithTimeout(bg, 2*time.Second)
		defer cancel()
		if err := db.Tx(ctx, writes); err != nil {
			t.Errorf("the writer is stuck after a panic: %v", err)
		}
	})
	t.Run("a cancelled context stores nothing", func(t *testing.T) {
		db := openWith(t, "alice")
		ctx, cancel := context.WithCancel(bg)
		err := db.Tx(ctx, func(tx *Tx) error {
			err := writes(tx)
			cancel()
			return err
		})
		if err == nil {
			t.Error("a transaction whose context was cancelled committed")
		}
		if n := rows(db); n != 0 {
			t.Errorf("%d rows after a cancel", n)
		}
	})
	t.Run("leaving the goroutine rolls back and leaves the writer usable", func(t *testing.T) {
		// runtime.Goexit is what t.Fatal and t.FailNow do inside the function:
		// neither an error nor a panic, so only a deferred rollback sees it.
		db := openWith(t, "alice")
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			db.Tx(bg, func(tx *Tx) error {
				writes(tx)
				runtime.Goexit()
				return nil
			})
		}()
		wg.Wait()
		if n := rows(db); n != 0 {
			t.Errorf("%d rows after a Goexit", n)
		}
		ctx, cancel := context.WithTimeout(bg, 2*time.Second)
		defer cancel()
		if err := db.Tx(ctx, writes); err != nil {
			t.Errorf("the writer is stuck after a Goexit: %v", err)
		}
	})
	t.Run("the DB's reads do not see what the transaction has written, and its writes wait", func(t *testing.T) {
		// This is why fn must not call the DB's methods, and what the doc says.
		db := openWith(t, "alice")
		err := db.Tx(bg, func(tx *Tx) error {
			if err := writes(tx); err != nil {
				return err
			}
			_, err := db.Message(bg, "alice", pnChat, original.ID)
			wantErrIs(t, err, ErrNoMessage)
			ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
			defer cancel()
			wantErrIs(t, db.QueuePush(ctx, "alice", "q1", []byte("n")), context.DeadlineExceeded)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Message(bg, "alice", pnChat, original.ID); err != nil {
			t.Errorf("the message after the commit: %v", err)
		}
	})
}

func TestTouchChat(t *testing.T) {
	type chat struct {
		PN, Name string
		IsGroup  bool
		Last     int64 // 0: NULL
	}
	tests := []struct {
		name string
		ops  []ChatUpd
		want chat
	}{
		{"a chat with no message keeps NULL, not 0",
			[]ChatUpd{{JID: group, Name: "Team", IsGroup: true}}, chat{Name: "Team", IsGroup: true}},
		{"and still NULL after another touch with no time",
			[]ChatUpd{{JID: group, Name: "Team", IsGroup: true}, {JID: group, Name: "Team 2"}}, chat{Name: "Team 2", IsGroup: true}},
		{"the first message gives the time",
			[]ChatUpd{{JID: lidChat}, {JID: lidChat, LastMessageTS: at(50)}}, chat{Last: 50}},
		{"a later message moves it",
			[]ChatUpd{{JID: lidChat, LastMessageTS: at(50)}, {JID: lidChat, LastMessageTS: at(60)}}, chat{Last: 60}},
		{"an older one (history sync) does not",
			[]ChatUpd{{JID: lidChat, LastMessageTS: at(60)}, {JID: lidChat, LastMessageTS: at(50)}}, chat{Last: 60}},
		{"a touch with no time does not clear it",
			[]ChatUpd{{JID: lidChat, LastMessageTS: at(60)}, {JID: lidChat, Name: "Bob"}}, chat{Name: "Bob", Last: 60}},
		{"an unknown name or pn keeps the known one",
			[]ChatUpd{{JID: lidChat, PN: pnChat, Name: "Bob"}, {JID: lidChat}}, chat{PN: pnChat, Name: "Bob"}},
		{"a new name replaces the old when a group is renamed",
			[]ChatUpd{{JID: group, Name: "Old", IsGroup: true}, {JID: group, Name: "New"}}, chat{Name: "New", IsGroup: true}},
		{"a pn learned later is added",
			[]ChatUpd{{JID: lidChat}, {JID: lidChat, PN: pnChat}}, chat{PN: pnChat}},
		{"a group stays a group",
			[]ChatUpd{{JID: group, IsGroup: true}, {JID: group}}, chat{IsGroup: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openWith(t, "alice")
			for _, u := range tt.ops {
				u.Account = "alice"
				run(t, db, touch(u))
			}
			chats, err := db.Chats(bg, ChatQuery{Limit: 10})
			if err != nil || len(chats) != 1 {
				t.Fatalf("chats = %v, %v; want one", chats, err)
			}
			c := chats[0]
			got := chat{c.PN, c.Name, c.IsGroup, unixOrZero(c.LastMessageTS)}
			if got != tt.want {
				t.Errorf("chat = %+v, want %+v", got, tt.want)
			}
			if tt.want.Last == 0 && count(t, db, `SELECT count(*) FROM chats WHERE last_message_ts IS NULL`) != 1 {
				t.Error("last_message_ts is not NULL in the table")
			}
		})
	}
}

func TestSetMedia(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(original))

	tests := []struct {
		name     string
		path     string
		raw      []byte
		wantPath string
		wantRaw  string
	}{
		{name: "the path of a download", path: "/d/a.jpg", wantPath: "/d/a.jpg", wantRaw: "raw:m1"},
		{name: "nothing new keeps both", wantPath: "/d/a.jpg", wantRaw: "raw:m1"},
		{name: "a media retry changes the raw", raw: []byte("retried"), wantPath: "/d/a.jpg", wantRaw: "retried"},
		{name: "a new download replaces the path", path: "/d/b.jpg", wantPath: "/d/b.jpg", wantRaw: "retried"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := db.SetMedia(bg, "alice", pnChat, "m1", tt.path, tt.raw); err != nil {
				t.Fatal(err)
			}
			m, err := db.MessageWithRaw(bg, "alice", pnChat, "m1")
			if err != nil {
				t.Fatal(err)
			}
			if m.MediaPath != tt.wantPath || string(m.Raw) != tt.wantRaw {
				t.Errorf("path = %q, raw = %q; want %q, %q", m.MediaPath, m.Raw, tt.wantPath, tt.wantRaw)
			}
		})
	}
	t.Run("an unknown message", func(t *testing.T) {
		wantErrIs(t, db.SetMedia(bg, "alice", pnChat, "nope", "/d/c.jpg", nil), ErrNoMessage)
	})
	t.Run("a redelivery does not lose the download", func(t *testing.T) {
		run(t, db, up(original))
		if got := stateAt(t, db, "alice", pnChat, "m1"); got.MediaPath != "/d/b.jpg" {
			t.Errorf("media_path = %q after a redelivery", got.MediaPath)
		}
	})
}

// TestWritesAreScopedToTheAccount: the same chat and message id under another
// account are another message.
func TestWritesAreScopedToTheAccount(t *testing.T) {
	db := openWith(t, "alice", "bob")
	bobs := original
	bobs.Account, bobs.Text = "bob", "bob's hello"
	run(t, db, up(original), up(bobs),
		edit(Edit{Account: "bob", Chat: pnChat, ID: "m1", Text: "bob edited", EditedAt: at(200)}))

	if a, b := stateAt(t, db, "alice", pnChat, "m1"), stateAt(t, db, "bob", pnChat, "m1"); a.Text != "hello" || b.Text != "bob edited" {
		t.Errorf("alice's text %q, bob's %q", a.Text, b.Text)
	}
}
