package archive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentUse is the shape of the real use, run under the race detector
// (go test -race -count=3): the live handler and the history worker write in
// transactions, tools read, a queue worker takes notifications off, and an
// account is deleted in the middle, VACUUM and all. Nothing may fail except
// what the delete says it may, and at the end the archive must be consistent.
func TestConcurrentUse(t *testing.T) {
	const (
		liveMsgs     = 150
		historyMsgs  = 600
		chunk        = 40
		notifs       = 8
		readerRounds = 60
	)
	db := openWith(t, "alice", "bob", "doomed")
	seed(t, db, "doomed")
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	fail := func(format string, args ...any) {
		select {
		case errs <- fmt.Errorf(format, args...):
		default:
		}
	}
	spawn := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	// The live handler: a message, its chat, now and then an edit, a revoke
	// and the merge of the number chat into the LID chat.
	spawn(func() {
		for i := range liveMsgs {
			id := fmt.Sprintf("live%03d", i)
			chat := pnChat // after the merge the chat is known by its LID
			if i > liveMsgs/2 {
				chat = lidChat
			}
			err := db.Tx(ctx, func(tx *Tx) error {
				if err := tx.Upsert(msg("alice", chat, id, int64(1000+i), "live message "+id)); err != nil {
					return err
				}
				if err := tx.TouchChat(ChatUpd{Account: "alice", JID: chat, PN: pnChat, LastMessageTS: at(int64(1000 + i))}); err != nil {
					return err
				}
				switch {
				case i%25 == 24:
					return tx.Edit(Edit{Account: "alice", Chat: chat, ID: id, Sender: chat, Text: "edited " + id, EditedAt: at(int64(2000 + i))})
				case i%40 == 39:
					return tx.Revoke(Revoke{Account: "alice", Chat: chat, ID: id, Sender: chat, RevokedAt: at(int64(3000 + i))})
				case i == liveMsgs/2:
					return tx.MergeChat("alice", pnChat, lidChat)
				}
				return nil
			})
			if err != nil {
				fail("live write %d: %v", i, err)
				return
			}
		}
	})

	// The history worker of bob: the queue, chunks of messages, the last chunk
	// taking the notification off.
	var pushed atomic.Int32
	spawn(func() {
		for n := range notifs {
			if err := db.QueuePush(ctx, "bob", fmt.Sprintf("h%d", n), []byte("notif")); err != nil {
				fail("push: %v", err)
				return
			}
			pushed.Add(1)
		}
	})
	var processed atomic.Int32
	spawn(func() {
		for processed.Load() < notifs {
			item, ok, err := db.QueueNext(ctx, "bob")
			if err != nil {
				fail("queue next: %v", err)
				return
			}
			if !ok {
				time.Sleep(time.Millisecond)
				continue
			}
			n := int(processed.Load())
			if n%3 == 2 && item.Attempts == 0 {
				if err := db.QueueFail(ctx, item.ID, "first try fails"); err != nil {
					fail("queue fail: %v", err)
					return
				}
				continue
			}
			for c := 0; c < historyMsgs/notifs; c += chunk {
				last := c+chunk >= historyMsgs/notifs
				err := db.Tx(ctx, func(tx *Tx) error {
					for j := c; j < c+chunk && j < historyMsgs/notifs; j++ {
						id := fmt.Sprintf("h%d-%03d", n, j)
						if err := tx.Upsert(msg("bob", group, id, int64(500+n*100+j), "history "+id)); err != nil {
							return err
						}
					}
					if err := tx.TouchChat(ChatUpd{Account: "bob", JID: group, IsGroup: true, LastMessageTS: at(900)}); err != nil {
						return err
					}
					if last {
						return tx.QueueDone(item.ID)
					}
					return nil
				})
				if err != nil {
					fail("history chunk: %v", err)
					return
				}
			}
			processed.Add(1)
		}
	})

	// The delete of another account, with VACUUM, in the middle of it all.
	spawn(func() {
		time.Sleep(20 * time.Millisecond)
		if err := db.DeleteAccount(ctx, "doomed"); err != nil && !errors.Is(err, ErrNotScrubbed) {
			fail("delete: %v", err)
		}
	})

	// The tools: every kind of read, again and again, each checking what it got.
	for r := range 6 {
		spawn(func() {
			for i := range readerRounds {
				switch (r + i) % 6 {
				case 0:
					page, err := db.Messages(ctx, MsgQuery{Account: "alice", Chat: pnChat, Limit: 30})
					if err != nil {
						fail("messages: %v", err)
						return
					}
					for k := 1; k < len(page.Messages); k++ {
						a, b := page.Messages[k-1], page.Messages[k]
						if a.TS.After(b.TS) {
							fail("messages out of order: %v then %v", a.ID, b.ID)
						}
					}
				case 1:
					if _, err := db.Search(ctx, SearchQuery{Query: "message", Limit: 10}); err != nil {
						fail("search: %v", err)
						return
					}
				case 2:
					if _, err := db.Chats(ctx, ChatQuery{Limit: 10}); err != nil {
						fail("chats: %v", err)
						return
					}
				case 3:
					if _, err := db.Accounts(ctx); err != nil {
						fail("accounts: %v", err)
						return
					}
				case 4:
					if _, err := db.AccountsForChat(ctx, pnChat); err != nil {
						fail("accounts for chat: %v", err)
						return
					}
					if _, err := db.QueueStuck(ctx, "bob"); err != nil {
						fail("queue stuck: %v", err)
						return
					}
				case 5:
					if _, err := db.Around(ctx, "alice", pnChat, "live000", 3, 3); err != nil && !errors.Is(err, ErrNoMessage) {
						fail("around: %v", err) // not there is fine: it is merged away, or not written yet
						return
					}
				}
			}
		})
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := count(t, db, `SELECT count(*) FROM messages WHERE account = 'alice'`); got != liveMsgs {
		t.Errorf("alice has %d messages, want %d", got, liveMsgs)
	}
	if got := count(t, db, `SELECT count(*) FROM messages WHERE account = 'bob'`); got != historyMsgs {
		t.Errorf("bob has %d messages, want %d", got, historyMsgs)
	}
	if got := count(t, db, `SELECT count(*) FROM messages WHERE chat_jid = ?`, pnChat); got != 0 {
		t.Errorf("%d messages are left under the number after the merge", got)
	}
	if got := count(t, db, `SELECT count(*) FROM history_queue WHERE account = 'bob'`); got != 0 {
		t.Errorf("%d notifications left in the queue", got)
	}
	if got := count(t, db, `SELECT count(*) FROM accounts WHERE nick = 'doomed'`); got != 0 {
		t.Error("the deleted account is still there")
	}
	checkFTS(t, db)
	if hits := search(t, db, SearchQuery{Query: "history", Accounts: []string{"bob"}, Limit: 1000}); len(hits) != historyMsgs {
		t.Errorf("search finds %d of bob's %d messages", len(hits), historyMsgs)
	}
}
