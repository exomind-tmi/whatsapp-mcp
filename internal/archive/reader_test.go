package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

// TestReaderCannotWrite: the reader pool is read-only, both ways the DSN
// says so: the second still holds when the first is switched off.
func TestReaderCannotWrite(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(original))

	writes := []string{
		`INSERT INTO accounts(nick, created_at) VALUES ('mallory', 1)`,
		`UPDATE messages SET text = 'changed'`,
		`DELETE FROM messages`,
		`DROP TABLE messages`,
		`CREATE TABLE evil(x)`,
		`INSERT INTO messages_fts(messages_fts) VALUES('optimize')`,
		`PRAGMA user_version = 99`,
	}
	for _, q := range writes {
		if _, err := db.r.Exec(q); err == nil {
			t.Errorf("a write through the reader pool succeeded: %s", q)
		}
	}
	// With query_only off, the file is still opened read-only.
	conn, err := db.r.Conn(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(bg, `PRAGMA query_only = 0`); err != nil {
		t.Fatal(err)
	}
	for _, q := range writes[:3] {
		if _, err := conn.ExecContext(bg, q); err == nil || !strings.Contains(err.Error(), "readonly") {
			t.Errorf("without query_only: %s: err = %v, want a read-only error", q, err)
		}
	}
	if got := stateAt(t, db, "alice", pnChat, "m1"); got.Text != "hello" {
		t.Errorf("message = %+v after the refused writes", got)
	}
}

// TestReaderSeesEveryCommit: a read that begins after a commit returned sees
// it, on whichever of the reader connections it lands.
func TestReaderSeesEveryCommit(t *testing.T) {
	db := openWith(t, "alice")
	for i := range 60 {
		id := seq(i)
		run(t, db, up(msg("alice", pnChat, id, int64(100+i), "round "+id)))
		page, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: pnChat, Limit: 1})
		if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != id {
			t.Fatalf("after writing %s the newest message is %v, %v", id, page.Messages, err)
		}
		if hits := search(t, db, SearchQuery{Query: "round " + id}); len(hits) != 1 {
			t.Fatalf("after writing %s a search finds %d", id, len(hits))
		}
	}
	// The same through the other kinds of read.
	run(t, db, touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Bob"}))
	if chats, err := db.Chats(bg, ChatQuery{Query: "bob", Limit: 5}); err != nil || len(chats) != 1 {
		t.Errorf("chats = %v, %v", chats, err)
	}
	push(t, db, "alice", "h1", "n")
	next(t, db, "alice")
	if accs, err := db.Accounts(bg); err != nil || accs[0].Messages != 60 {
		t.Errorf("accounts = %+v, %v", accs, err)
	}
}

// TestReadersDoNotWaitForTheWriter: a transaction that is open, the writer
// busy with a chunk of a history sync, does not hold the reads up, and the reads
// do not see what it has not committed.
func TestReadersDoNotWaitForTheWriter(t *testing.T) {
	db := openWith(t, "alice")
	run(t, db, up(original), touch(ChatUpd{Account: "alice", JID: pnChat, Name: "Bob"}))
	inside, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- db.Tx(bg, func(tx *Tx) error {
			if err := tx.Upsert(pnMsg("pending", 200, "uncommitted words")); err != nil {
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	reads := map[string]func() error{
		"Messages": func() error {
			_, err := db.Messages(ctx, MsgQuery{Account: "alice", Chat: pnChat, Limit: 5})
			return err
		},
		"Search":   func() error { _, err := db.Search(ctx, SearchQuery{Query: "hello", Limit: 5}); return err },
		"Chats":    func() error { _, err := db.Chats(ctx, ChatQuery{Limit: 5}); return err },
		"Accounts": func() error { _, err := db.Accounts(ctx); return err },
		"Message":  func() error { _, err := db.Message(ctx, "alice", pnChat, "m1"); return err },
		"QueueNext": func() error {
			_, _, err := db.QueueNext(ctx, "alice")
			return err
		},
		// Every read of the DB is here: one that waited for the writer would
		// be stuck behind a history chunk.
		"MessageWithRaw": func() error { _, err := db.MessageWithRaw(ctx, "alice", pnChat, "m1"); return err },
		"Around":         func() error { _, err := db.Around(ctx, "alice", pnChat, "m1", 2, 2); return err },
		"AccountsForChat": func() error {
			_, err := db.AccountsForChat(ctx, pnChat)
			return err
		},
		"QueueStuck": func() error { _, err := db.QueueStuck(ctx, "alice"); return err },
	}
	for name, read := range reads {
		if err := read(); err != nil {
			t.Errorf("%s while a transaction is open: %v", name, err)
		}
	}
	if hits, err := db.Search(ctx, SearchQuery{Query: "uncommitted", Limit: 5}); err != nil || len(hits) != 0 {
		t.Errorf("a search while the transaction is open found %d hits, %v: it must not see what is not committed", len(hits), err)
	}

	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if hits := search(t, db, SearchQuery{Query: "uncommitted"}); len(hits) != 1 {
		t.Errorf("a search does not see the message after the commit")
	}
}

// TestCloseLeavesNoWAL: the writer closes last, so that SQLite checkpoints
// and deletes the WAL, which a read-only connection that closed last could
// not. A forgotten -wal is what the next start would have to recover and a
// backup of the file alone would miss.
func TestCloseLeavesNoWAL(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAccount(bg, "alice"); err != nil {
		t.Fatal(err)
	}
	run(t, db, up(original))
	// Every reader connection is open when it closes.
	var wg sync.WaitGroup
	for range readers * 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				db.Messages(bg, MsgQuery{Account: "alice", Chat: pnChat, Limit: 5})
			}
		}()
	}
	wg.Wait()
	if _, err := os.Stat(p + "-wal"); err != nil {
		t.Fatalf("no WAL while open (%v): the test checks nothing", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p + "-wal", p + "-shm"} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after Close: %v, want it gone", filepath.Base(f), err)
		}
	}
	if err := db.Close(); err != nil {
		t.Errorf("a second Close = %v", err)
	}
	db = openAt(t, p) // and what was written is in the file
	if got := stateAt(t, db, "alice", pnChat, "m1"); got.Text != "hello" {
		t.Errorf("after reopening: %+v", got)
	}
}

func TestUseAfterClose(t *testing.T) {
	db := openWith(t, "alice")
	db.Close()
	if _, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: pnChat, Limit: 1}); err == nil {
		t.Error("a read after Close succeeded")
	}
	if err := db.Tx(bg, func(tx *Tx) error { return tx.Upsert(original) }); err == nil {
		t.Error("a write after Close succeeded")
	}
}

// warmReaders makes every connection of the reader pool open, as a busy
// server leaves it, so that the delete is made with all of them there.
func warmReaders(t *testing.T, db *DB) {
	t.Helper()
	conns := make([]interface{ Close() error }, 0, readers)
	for range readers {
		c, err := db.r.Conn(bg) // all held at once: the pool must open a new one each time
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExecContext(bg, `SELECT count(*) FROM messages`); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Close()
	}
	if got := db.r.Stats().OpenConnections; got != readers {
		t.Fatalf("%d reader connections open, want %d", got, readers)
	}
}

// TestDeleteAccountWithIdleReaders: readers that are open but not reading
// hold nothing, and the scrub is as complete as without them.
func TestDeleteAccountWithIdleReaders(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db := openAt(t, p)
	seed(t, db, "alice")
	seed(t, db, "bob")
	insertRareText(t, db, "alice")
	if _, err := db.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	warmReaders(t, db)
	if n := trigramsInFiles(t, p); n != len(ftsTrigrams) {
		t.Fatalf("%d of %d trigrams in the files before the delete: the test checks nothing", n, len(ftsTrigrams))
	}

	if err := db.DeleteAccount(bg, "alice"); err != nil {
		t.Fatalf("DeleteAccount = %v with idle readers", err)
	}
	if n := trigramsInFiles(t, p); n != 0 {
		t.Errorf("%d of %d trigrams of the deleted text are still in the files", n, len(ftsTrigrams))
	}
	if n := count(t, db, `PRAGMA freelist_count`); n != 0 {
		t.Errorf("%d free pages left: VACUUM did not run", n)
	}
	accs, err := db.Accounts(bg) // the readers see the file as VACUUM made it
	if err != nil || len(accs) != 1 || accs[0].Nick != "bob" || accs[0].Messages != 3 {
		t.Errorf("accounts after the delete = %+v, %v", accs, err)
	}
	if hits := search(t, db, SearchQuery{Query: "bob"}); len(hits) != 2 {
		t.Errorf("bob has %d hits after the VACUUM, want 2", len(hits))
	}
	if hits, err := db.Search(bg, SearchQuery{Query: "alice", Limit: 5}); err != nil || len(hits) != 0 {
		t.Errorf("alice has %d hits after the delete, %v", len(hits), err)
	}
}

// TestDeleteAccountWaitsForAReader: a read that is in the middle of its rows
// when the checkpoint comes is waited for (busy_timeout), not given up on:
// the scrub is complete, and DeleteAccount says so.
func TestDeleteAccountWaitsForAReader(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db := openAt(t, p)
	seed(t, db, "alice")
	seed(t, db, "bob")
	insertRareText(t, db, "alice")

	rows, err := db.r.QueryContext(bg, `SELECT msg_id FROM messages WHERE account = 'bob'`)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("no rows")
	}
	time.AfterFunc(400*time.Millisecond, func() { rows.Close() })

	start := time.Now()
	err = db.DeleteAccount(bg, "alice")
	if err != nil {
		t.Fatalf("DeleteAccount = %v: a reader that finishes in a moment was not waited for", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Errorf("DeleteAccount returned in %v, before the reader was done: the checkpoint did not wait", waited)
	}
	if n := trigramsInFiles(t, p); n != 0 {
		t.Errorf("%d trigrams of the deleted text are still in the files", n)
	}
}

// TestDeleteAccountWithAStuckReader: a reader that is not done within
// busy_timeout leaves old copies in the WAL; the account is gone all the
// same, and the caller is told by ErrNotScrubbed, as it is for a reader of
// another process.
func TestDeleteAccountWithAStuckReader(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db := openAt(t, p)
	seed(t, db, "alice")
	seed(t, db, "bob")
	if _, err := db.w.Exec(`PRAGMA busy_timeout = 200`); err != nil { // the one connection of the writer
		t.Fatal(err)
	}
	reader, err := db.r.Conn(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(bg, `BEGIN DEFERRED`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := reader.QueryRowContext(bg, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	err = db.DeleteAccount(bg, "alice")
	if !errors.Is(err, ErrNotScrubbed) || !errors.Is(err, sqlitedb.ErrCheckpointBusy) {
		t.Fatalf("DeleteAccount with a reader of the pool = %v, want ErrNotScrubbed with the busy checkpoint", err)
	}
	if got := count(t, db, `SELECT count(*) FROM accounts WHERE nick = 'alice'`); got != 0 {
		t.Error("the account is still there")
	}
	if _, err := reader.ExecContext(bg, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAccount(bg, "alice"); !errors.Is(err, ErrNoAccount) {
		t.Errorf("a second DeleteAccount = %v, want ErrNoAccount", err)
	}
}
