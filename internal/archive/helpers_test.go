package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	pnChat  = "79991234567@s.whatsapp.net"
	lidChat = "123456789012345@lid"
	group   = "120363000000000001@g.us"
)

var (
	bg      = context.Background()
	errTest = errors.New("test error")
)

func at(n int64) time.Time { return time.Unix(n, 0) }

// openWith opens a fresh archive with the accounts.
func openWith(t *testing.T, nicks ...string) *DB {
	t.Helper()
	db := openTemp(t)
	for _, n := range nicks {
		if err := db.AddAccount(bg, n); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// msg is a received message with a raw, sent by the chat itself.
func msg(account, chat, id string, ts int64, text string) Row {
	return Row{Account: account, Chat: chat, ID: id, Sender: chat, TS: at(ts), Text: text, Raw: []byte("raw:" + id)}
}

// op is one write, so that a test can list them and run them in any order.
type op func(*Tx) error

func up(r Row) op        { return func(t *Tx) error { return t.Upsert(r) } }
func edit(e Edit) op     { return func(t *Tx) error { return t.Edit(e) } }
func revoke(r Revoke) op { return func(t *Tx) error { return t.Revoke(r) } }
func touch(c ChatUpd) op { return func(t *Tx) error { return t.TouchChat(c) } }
func merge(a, from, to string) op {
	return func(t *Tx) error { return t.MergeChat(a, from, to) }
}

// run applies every op in a transaction of its own, as separate events do.
func run(t *testing.T, db *DB, ops ...op) {
	t.Helper()
	for i, o := range ops {
		if err := db.Tx(bg, func(tx *Tx) error { return o(tx) }); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}
}

// runTx applies the ops in one transaction.
func runTx(t *testing.T, db *DB, ops ...op) {
	t.Helper()
	err := db.Tx(bg, func(tx *Tx) error {
		for _, o := range ops {
			if err := o(tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// state is the part of a message the write rules decide.
type state struct {
	Sender, Text         string
	FromMe               bool
	TS                   int64
	HasRaw               bool
	Edited, Revoked      int64
	MediaType, MediaPath string
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func stateOf(m Message) state {
	return state{m.Sender, m.Text, m.FromMe, m.TS.Unix(), m.Raw != nil, unixOrZero(m.EditedAt), unixOrZero(m.RevokedAt), m.MediaType, m.MediaPath}
}

func stateAt(t *testing.T, db *DB, account, chat, id string) state {
	t.Helper()
	m, err := db.MessageWithRaw(bg, account, chat, id)
	if err != nil {
		t.Fatalf("message %s: %v", id, err)
	}
	return stateOf(m)
}

// ftsHits is how many rows of the full-text index match the word.
func ftsHits(t *testing.T, db *DB, word string) int {
	t.Helper()
	return count(t, db, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, `"`+word+`"`)
}

// checkFTS fails unless the index has exactly one row for each text and none
// for a row that is not there: the triggers must keep up with every write.
func checkFTS(t *testing.T, db *DB) {
	t.Helper()
	if idx, texts := count(t, db, `SELECT count(*) FROM messages_fts`),
		count(t, db, `SELECT count(*) FROM messages WHERE text IS NOT NULL`); idx != texts {
		t.Errorf("%d rows in the full-text index for %d texts", idx, texts)
	}
	if n := count(t, db, `SELECT count(*) FROM messages_fts WHERE rowid NOT IN (SELECT id FROM messages WHERE text IS NOT NULL)`); n != 0 {
		t.Errorf("%d orphan rows in the full-text index", n)
	}
}

// wantErrIs fails unless err is (or wraps) want.
func wantErrIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func ids(msgs []Message) string {
	s := make([]string, len(msgs))
	for i, m := range msgs {
		s[i] = m.ID
	}
	return strings.Join(s, ",")
}

func hitIDs(hits []Hit) string {
	s := make([]string, len(hits))
	for i, h := range hits {
		s[i] = h.ID
	}
	return strings.Join(s, ",")
}

// seq makes ids "m01".."mNN".
func seq(i int) string { return fmt.Sprintf("m%02d", i) }
