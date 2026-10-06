package archive

import (
	"bytes"
	"fmt"
	"testing"
)

func push(t *testing.T, db *DB, account, msgID, notif string) {
	t.Helper()
	if err := db.QueuePush(bg, account, msgID, []byte(notif)); err != nil {
		t.Fatal(err)
	}
}

// next is QueueNext that must find something.
func next(t *testing.T, db *DB, account string) QueueItem {
	t.Helper()
	q, ok, err := db.QueueNext(bg, account)
	if err != nil || !ok {
		t.Fatalf("QueueNext(%s) = %+v, %v, %v; want an item", account, q, ok, err)
	}
	return q
}

func nothingNext(t *testing.T, db *DB, account string) {
	t.Helper()
	if q, ok, err := db.QueueNext(bg, account); err != nil || ok {
		t.Fatalf("QueueNext(%s) = %+v, %v, %v; want nothing", account, q, ok, err)
	}
}

func done(t *testing.T, db *DB, id int64) {
	t.Helper()
	if err := db.Tx(bg, func(tx *Tx) error { return tx.QueueDone(id) }); err != nil {
		t.Fatal(err)
	}
}

func fail(t *testing.T, db *DB, id int64, cause string) {
	t.Helper()
	if err := db.QueueFail(bg, id, cause); err != nil {
		t.Fatal(err)
	}
}

func stuck(t *testing.T, db *DB, account string) Stuck {
	t.Helper()
	s, err := db.QueueStuck(bg, account)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQueueLifecycle(t *testing.T) {
	db := openWith(t, "alice", "bob")
	nothingNext(t, db, "alice")
	push(t, db, "alice", "h1", "notif 1")
	push(t, db, "alice", "h2", "notif 2")
	push(t, db, "bob", "h1", "bob's notif")

	first := next(t, db, "alice")
	if first.MsgID != "h1" || !bytes.Equal(first.Notif, []byte("notif 1")) || first.Attempts != 0 {
		t.Fatalf("first = %+v, want h1 with its notification and no attempts", first)
	}
	if b := next(t, db, "bob"); b.MsgID != "h1" || string(b.Notif) != "bob's notif" {
		t.Errorf("bob's queue = %+v: the accounts' queues are mixed", b)
	}

	t.Run("a notification pushed again is queued once", func(t *testing.T) {
		push(t, db, "alice", "h1", "the same, redelivered")
		if n := count(t, db, `SELECT count(*) FROM history_queue WHERE account = 'alice'`); n != 2 {
			t.Errorf("%d queued, want 2", n)
		}
		if got := next(t, db, "alice"); string(got.Notif) != "notif 1" {
			t.Errorf("the redelivery replaced the notification: %q", got.Notif)
		}
	})

	t.Run("a failure counts, and the notification is tried again first", func(t *testing.T) {
		fail(t, db, first.ID, "download: 403")
		got := next(t, db, "alice")
		if got.ID != first.ID || got.Attempts != 1 {
			t.Errorf("next = %+v, want h1 with 1 attempt", got)
		}
		if s := stuck(t, db, "alice"); s != (Stuck{}) {
			t.Errorf("stuck = %+v after one failure", s)
		}
	})

	t.Run("done takes it off, and twice is fine", func(t *testing.T) {
		done(t, db, first.ID)
		done(t, db, first.ID)
		if got := next(t, db, "alice"); got.MsgID != "h2" {
			t.Errorf("next = %+v, want h2", got)
		}
	})

	t.Run("failing a notification that is gone is not an error", func(t *testing.T) {
		fail(t, db, first.ID, "late")
	})

	t.Run("a pushed again after done is a new one", func(t *testing.T) {
		// A notification that was processed and removed, then delivered again
		// (WhatsApp resends until the receipt is seen) is processed again;
		// the history it carries is idempotent.
		push(t, db, "alice", "h1", "again")
		if n := count(t, db, `SELECT count(*) FROM history_queue WHERE account = 'alice'`); n != 2 {
			t.Errorf("%d queued, want 2", n)
		}
	})
}

func TestQueuePoison(t *testing.T) {
	db := openWith(t, "alice", "bob")
	push(t, db, "alice", "h1", "bad")
	push(t, db, "alice", "h2", "good")
	push(t, db, "bob", "h1", "bobs")

	bad := next(t, db, "alice")
	for i := 1; i <= MaxAttempts; i++ {
		if got := next(t, db, "alice"); got.ID != bad.ID || got.Attempts != i-1 {
			t.Fatalf("round %d: next = %+v, want the bad one with %d attempts", i, got, i-1)
		}
		fail(t, db, bad.ID, fmt.Sprintf("error %d", i))
	}

	if got := next(t, db, "alice"); got.MsgID != "h2" {
		t.Errorf("next = %+v, want the good one: the poison one is skipped", got)
	}
	if s := stuck(t, db, "alice"); s != (Stuck{Count: 1, LastError: "error 5"}) {
		t.Errorf("stuck = %+v, want one with the last error", s)
	}
	if s := stuck(t, db, "bob"); s != (Stuck{}) {
		t.Errorf("bob is stuck: %+v", s)
	}
	if n := count(t, db, `SELECT count(*) FROM history_queue WHERE account = 'alice'`); n != 2 {
		t.Errorf("%d rows, want the poison one kept to be reported", n)
	}

	good := next(t, db, "alice")
	done(t, db, good.ID)
	nothingNext(t, db, "alice")
	if s := stuck(t, db, "alice"); s.Count != 1 {
		t.Errorf("stuck = %+v after the rest was done, want it still reported", s)
	}
}

// TestQueueStuckReportsTheNewest: several are counted, the last error is the
// newest one's.
func TestQueueStuckReportsTheNewest(t *testing.T) {
	db := openWith(t, "alice")
	for i := 1; i <= 3; i++ {
		push(t, db, "alice", fmt.Sprintf("h%d", i), "n")
	}
	for i := 1; i <= 3; i++ {
		var id int64
		if err := db.w.QueryRow(`SELECT id FROM history_queue WHERE msg_id = ?`, fmt.Sprintf("h%d", i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for range MaxAttempts {
			fail(t, db, id, fmt.Sprintf("failed h%d", i))
		}
	}
	if s := stuck(t, db, "alice"); s != (Stuck{Count: 3, LastError: "failed h3"}) {
		t.Errorf("stuck = %+v, want 3 and the newest's error", s)
	}
}

// TestQueueDoneWithTheLastChunk: the notification is taken off in the
// transaction of the last chunk, so a crash between the two cannot lose either.
func TestQueueDoneWithTheLastChunk(t *testing.T) {
	db := openWith(t, "alice")
	push(t, db, "alice", "h1", "n")
	item := next(t, db, "alice")

	err := db.Tx(bg, func(tx *Tx) error {
		if err := tx.Upsert(pnMsg("a", 10, "alpha")); err != nil {
			return err
		}
		if err := tx.QueueDone(item.ID); err != nil {
			return err
		}
		return errTest
	})
	wantErrIs(t, err, errTest)
	if got := next(t, db, "alice"); got.ID != item.ID {
		t.Errorf("the notification was taken off by a rolled back transaction")
	}
	if n := count(t, db, `SELECT count(*) FROM messages`); n != 0 {
		t.Errorf("%d messages from a rolled back chunk", n)
	}

	runTx(t, db, up(pnMsg("a", 10, "alpha")), func(tx *Tx) error { return tx.QueueDone(item.ID) })
	nothingNext(t, db, "alice")
	if n := count(t, db, `SELECT count(*) FROM messages`); n != 1 {
		t.Errorf("%d messages, want the chunk", n)
	}
}

func TestQueueRefusals(t *testing.T) {
	db := openWith(t, "alice")
	for name, err := range map[string]error{
		"an account that does not exist": db.QueuePush(bg, "nobody", "h1", []byte("n")),
		"no message id":                  db.QueuePush(bg, "alice", "", []byte("n")),
		"no account":                     db.QueuePush(bg, "", "h1", []byte("n")),
	} {
		if err == nil {
			t.Errorf("%s: pushed", name)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM history_queue`); n != 0 {
		t.Errorf("%d queued from refused pushes", n)
	}
}
