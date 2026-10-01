package archive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

const (
	jid1 = "79001234567:12@s.whatsapp.net"
	jid2 = "79001234567:15@s.whatsapp.net"
)

type nickJID struct{ Nick, JID string }

func nickJIDs(t *testing.T, db *DB) []nickJID {
	t.Helper()
	accs, err := db.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := []nickJID{}
	for _, a := range accs {
		out = append(out, nickJID{a.Nick, a.JID})
	}
	return out
}

func TestAccountCRUD(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		setup   func(*DB) error // runs after alice was added
		act     func(*DB) error
		wantErr error
		want    []nickJID
	}{
		{
			name: "add new",
			act:  func(db *DB) error { return db.AddAccount(ctx, "bob") },
			want: []nickJID{{"alice", ""}, {"bob", ""}},
		},
		{
			name:  "add existing keeps the jid",
			setup: func(db *DB) error { return db.SetAccountJID(ctx, "alice", jid1) },
			act:   func(db *DB) error { return db.AddAccount(ctx, "alice") },
			want:  []nickJID{{"alice", jid1}},
		},
		{
			name: "set jid",
			act:  func(db *DB) error { return db.SetAccountJID(ctx, "alice", jid1) },
			want: []nickJID{{"alice", jid1}},
		},
		{
			name:  "set jid again on relink",
			setup: func(db *DB) error { return db.SetAccountJID(ctx, "alice", jid1) },
			act:   func(db *DB) error { return db.SetAccountJID(ctx, "alice", jid2) },
			want:  []nickJID{{"alice", jid2}},
		},
		{
			// A retry after a failure must not turn into ErrNoAccount.
			name:  "set the same jid again",
			setup: func(db *DB) error { return db.SetAccountJID(ctx, "alice", jid1) },
			act:   func(db *DB) error { return db.SetAccountJID(ctx, "alice", jid1) },
			want:  []nickJID{{"alice", jid1}},
		},
		{
			name:    "set jid of a missing account",
			act:     func(db *DB) error { return db.SetAccountJID(ctx, "bob", jid1) },
			wantErr: ErrNoAccount,
			want:    []nickJID{{"alice", ""}},
		},
		{
			name: "delete",
			act:  func(db *DB) error { return db.DeleteAccount(ctx, "alice") },
			want: []nickJID{},
		},
		{
			name:    "delete a missing account",
			act:     func(db *DB) error { return db.DeleteAccount(ctx, "bob") },
			wantErr: ErrNoAccount,
			want:    []nickJID{{"alice", ""}},
		},
		{
			name:  "add again after delete",
			setup: func(db *DB) error { return db.DeleteAccount(ctx, "alice") },
			act:   func(db *DB) error { return db.AddAccount(ctx, "alice") },
			want:  []nickJID{{"alice", ""}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTemp(t)
			if err := db.AddAccount(ctx, "alice"); err != nil {
				t.Fatal(err)
			}
			if tt.setup != nil {
				if err := tt.setup(db); err != nil {
					t.Fatal(err)
				}
			}
			if err := tt.act(db); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := nickJIDs(t, db); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("accounts = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetAccountJIDRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.AddAccount(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountJID(ctx, "alice", ""); err == nil {
		t.Fatal("an empty jid was accepted")
	}
	if n := count(t, db, `SELECT count(*) FROM accounts WHERE jid IS NULL`); n != 1 {
		t.Fatal("an empty jid replaced NULL")
	}
}

// TestSetAccountJIDTaken: one device, one nick. The error names the holder,
// and neither account changes.
func TestSetAccountJIDTaken(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	for _, nick := range []string{"alice", "bob"} {
		if err := db.AddAccount(ctx, nick); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetAccountJID(ctx, "bob", jid1); err != nil {
		t.Fatal(err)
	}

	err := db.SetAccountJID(ctx, "alice", jid1)
	if !errors.Is(err, ErrJIDTaken) || !strings.Contains(err.Error(), "bob") {
		t.Fatalf("err = %v, want ErrJIDTaken naming bob", err)
	}
	if got, want := nickJIDs(t, db), []nickJID{{"alice", ""}, {"bob", jid1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("accounts = %v, want %v", got, want)
	}
}

func TestAddAccountTimestamps(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	before := time.Now().Truncate(time.Second)
	if err := db.AddAccount(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	accs, err := db.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := accs[0].CreatedAt; c.Before(before) || c.After(time.Now()) {
		t.Fatalf("created_at = %v, want about now", c)
	}

	// A repeated add (a relink) must not reset when the account was created.
	if _, err := db.w.Exec(`UPDATE accounts SET created_at = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.AddAccount(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if accs, err = db.Accounts(ctx); err != nil {
		t.Fatal(err)
	}
	if c := accs[0].CreatedAt; !c.Equal(time.Unix(1, 0)) {
		t.Fatalf("created_at = %v after a repeated add, want it kept", c)
	}
}

// seed gives nick an account with 2 chats, 3 messages (2 with text, so 2
// FTS rows) and 1 queued history notification, written straight in SQL: the
// M2 write methods do not exist yet.
func seed(t *testing.T, db *DB, nick string) {
	t.Helper()
	if err := db.AddAccount(context.Background(), nick); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO chats(account, jid) VALUES (?1, 'c1@s.whatsapp.net'), (?1, 'c2@g.us')`,
		`INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text) VALUES
		   (?1, 'c1@s.whatsapp.net', 'm1', 'c1@s.whatsapp.net', 0, 1, 'hello from ' || ?1),
		   (?1, 'c1@s.whatsapp.net', 'm2', 'me@s.whatsapp.net', 1, 2, 'hello again ' || ?1),
		   (?1, 'c2@g.us', 'm3', 'c1@s.whatsapp.net', 0, 3, NULL)`,
		`INSERT INTO history_queue(account, msg_id, notif, created_at) VALUES (?1, 'h1', x'00', 1)`,
	} {
		if _, err := db.w.Exec(q, nick); err != nil {
			t.Fatalf("seed %s: %v", nick, err)
		}
	}
}

func count(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.w.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestAccountsCounts(t *testing.T) {
	db := openTemp(t)
	seed(t, db, "bob")
	seed(t, db, "alice")
	if err := db.AddAccount(context.Background(), "carol"); err != nil {
		t.Fatal(err)
	}
	accs, err := db.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type size struct {
		Nick            string
		Chats, Messages int
	}
	got := []size{}
	for _, a := range accs {
		got = append(got, size{a.Nick, a.Chats, a.Messages})
	}
	want := []size{{"alice", 2, 3}, {"bob", 2, 3}, {"carol", 0, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("accounts = %v, want %v", got, want)
	}
}

// TestDeleteAccountCascades checks every table keyed by the account,
// including messages_fts, which has no foreign key and is cleared only by
// the messages_ad trigger firing for the cascaded deletes.
func TestDeleteAccountCascades(t *testing.T) {
	db := openTemp(t)
	seed(t, db, "alice")
	seed(t, db, "bob")
	// The FTS rows exist before the delete, so their absence after it is
	// the cascade's doing, not the insert trigger's.
	if n := count(t, db, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'alice'`); n != 2 {
		t.Fatalf("alice has %d FTS rows before the delete, want 2", n)
	}

	if err := db.DeleteAccount(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		query string
		want  map[string]int // nick → rows
	}{
		{"accounts", `SELECT count(*) FROM accounts WHERE nick = ?`, map[string]int{"alice": 0, "bob": 1}},
		{"chats", `SELECT count(*) FROM chats WHERE account = ?`, map[string]int{"alice": 0, "bob": 2}},
		{"messages", `SELECT count(*) FROM messages WHERE account = ?`, map[string]int{"alice": 0, "bob": 3}},
		{"messages_fts", `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, map[string]int{"alice": 0, "bob": 2}},
		{"history_queue", `SELECT count(*) FROM history_queue WHERE account = ?`, map[string]int{"alice": 0, "bob": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for nick, want := range tt.want {
				if got := count(t, db, tt.query, nick); got != want {
					t.Errorf("%s: %d rows, want %d", nick, got, want)
				}
			}
		})
	}
	// Nothing orphaned that the per-nick queries above could miss.
	if n := count(t, db, `SELECT count(*) FROM messages_fts`); n != 2 {
		t.Errorf("%d FTS rows left in all, want bob's 2", n)
	}
}

// TestDeleteAccountLeavesNoTrace reads the files themselves: a deleted row
// stays readable in a freed page or in the WAL unless secure_delete and the
// checkpoint remove it.
func TestDeleteAccountLeavesNoTrace(t *testing.T) {
	const marker = "zqxjvmarker"
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db := openAt(t, p)
	seed(t, db, "alice")
	if _, err := db.w.Exec(`INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text, raw)
	  VALUES ('alice', 'c1@s.whatsapp.net', 'm9', 'c1@s.whatsapp.net', 0, 9, ?1, CAST(?1 AS BLOB))`, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		t.Fatal(err)
	}
	if !inFiles(t, p, marker) {
		t.Fatal("the marker is not in the files before the delete: the test checks nothing")
	}

	if err := db.DeleteAccount(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if inFiles(t, p, marker) {
		t.Fatal("the deleted text is still in archive.db or its WAL")
	}
}

// TestDeleteAccountShrinksFile: VACUUM gives the freed pages back to the file
// system, and what it rebuilds keeps the other account whole, including the
// FTS rows, which are tied to messages by rowid.
func TestDeleteAccountShrinksFile(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db := openAt(t, p)
	seed(t, db, "alice")
	seed(t, db, "bob")
	if _, err := db.w.Exec(`
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text, raw)
SELECT 'alice', 'c1@s.whatsapp.net', 'b' || i, 'c1@s.whatsapp.net', 0, 10 + i,
       'filler ' || hex(randomblob(100)), randomblob(300) FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	before := fileSize(t, p)

	if err := db.DeleteAccount(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if after := fileSize(t, p); after*2 > before {
		t.Errorf("archive.db is %d bytes after the delete, %d before: not shrunk", after, before)
	}
	if n := count(t, db, `PRAGMA freelist_count`); n != 0 {
		t.Errorf("%d free pages left: VACUUM did not run", n)
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM chats WHERE account = 'bob'`:         2,
		`SELECT count(*) FROM messages WHERE account = 'bob'`:      3,
		`SELECT count(*) FROM history_queue WHERE account = 'bob'`: 1,
		`SELECT count(*) FROM messages_fts f JOIN messages m ON m.id = f.rowid
		   WHERE messages_fts MATCH 'bob' AND m.account = 'bob'`: 2,
	} {
		if got := count(t, db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// TestHistoryQueueKeyedByMessage: a notification redelivered after a crash
// (plan 7.2) is queued once per account, and the key also serves the
// foreign key, so the cascade of DeleteAccount does not scan the queue.
func TestHistoryQueueKeyedByMessage(t *testing.T) {
	db := openTemp(t)
	seed(t, db, "alice") // both queue h1: the key is per account
	seed(t, db, "bob")
	const push = `INSERT INTO history_queue(account, msg_id, notif, created_at) VALUES ('alice', 'h1', x'01', 2)`
	if _, err := db.w.Exec(push + ` ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(push); err == nil {
		t.Error("a second h1 of alice was queued")
	}
	if n := count(t, db, `SELECT count(*) FROM history_queue WHERE account = 'alice'`); n != 1 {
		t.Errorf("alice has %d queued notifications, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM history_queue`); n != 2 {
		t.Errorf("%d queued notifications in all, want 2", n)
	}

	rows, err := db.w.Query(`EXPLAIN QUERY PLAN SELECT id FROM history_queue WHERE account = 'alice'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "; "), "SEARCH history_queue USING") {
		t.Errorf("lookup by account: %q, want an index search", plan)
	}
}

func inFiles(t *testing.T, p, s string) bool {
	t.Helper()
	for _, f := range []string{p, p + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}

// TestFTSFollowsText covers the triggers that keep the contentless index
// in step with messages.text, ё folded to е as the query side does.
func TestFTSFollowsText(t *testing.T) {
	db := openTemp(t)
	seed(t, db, "alice")
	steps := []struct {
		name   string
		update string // on alice's m1
		want   map[string]int
	}{
		{"insert", "", map[string]int{`"hello"`: 2}},
		{"edit", `UPDATE messages SET text = 'ёлка' WHERE msg_id = 'm1'`, map[string]int{`"hello"`: 1, `"елка"`: 1}},
		{"same text", `UPDATE messages SET text = text WHERE msg_id = 'm1'`, map[string]int{`"елка"`: 1}},
		{"text gone", `UPDATE messages SET text = NULL WHERE msg_id = 'm1'`, map[string]int{`"елка"`: 0}},
		{"text back", `UPDATE messages SET text = 'Ёж' || 'ики' WHERE msg_id = 'm1'`, map[string]int{`"ежи"`: 1}},
	}
	for _, s := range steps {
		if s.update != "" {
			if _, err := db.w.Exec(s.update); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
		}
		for q, want := range s.want {
			if got := count(t, db, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, q); got != want {
				t.Errorf("%s: MATCH %s = %d, want %d", s.name, q, got, want)
			}
		}
		// Every text, and only a text, has its row.
		if got, want := count(t, db, `SELECT count(*) FROM messages_fts`),
			count(t, db, `SELECT count(*) FROM messages WHERE text IS NOT NULL`); got != want {
			t.Errorf("%s: %d FTS rows for %d texts", s.name, got, want)
		}
	}
}
