package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxAttempts is how many times a queued notification may fail before it is
// left alone: a notification that cannot be downloaded or parsed must not be
// retried for ever, nor hold up the ones behind it.
const MaxAttempts = 5

// QueuePush queues a history sync notification of the account, to be
// downloaded by its worker. msgID is the id of the message that carried it:
// the notification redelivered after a crash (before the receipt went out)
// is the same row, so pushing it again is a no-op, and the caller can send
// the receipt either way.
func (db *DB) QueuePush(ctx context.Context, account, msgID string, notif []byte) error {
	if account == "" || msgID == "" {
		return errors.New("queue push: account and message id are required")
	}
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO history_queue(account, msg_id, notif, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		account, msgID, notif, time.Now().Unix())
	return wrap(err, "queue a history notification", account)
}

// QueueNext is the oldest notification of the account that has not failed
// MaxAttempts times, or false when there is none.
func (db *DB) QueueNext(ctx context.Context, account string) (QueueItem, bool, error) {
	var q QueueItem
	err := db.r.QueryRowContext(ctx, queueNextSQL, account, MaxAttempts).Scan(&q.ID, &q.MsgID, &q.Notif, &q.Attempts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return QueueItem{}, false, nil
	case err != nil:
		return QueueItem{}, false, wrap(err, "next history notification", account)
	}
	return q, true, nil
}

const queueNextSQL = `
SELECT id, msg_id, notif, attempts FROM history_queue
WHERE account = ? AND attempts < ? ORDER BY id LIMIT 1`

// QueueFail records a failed attempt on the notification. A notification that
// is gone (its account was removed meanwhile) is not an error.
//
// Each call counts: after MaxAttempts of them QueueNext never offers the
// notification again, and a redelivery of it changes nothing (QueuePush keeps
// the row that is there). So it is for an attempt that was made and failed, not
// for a context that was cancelled or a daemon that is stopping, and the pause
// between attempts is the caller's. cause is what manage-accounts list shows:
// no JIDs or numbers in it.
func (db *DB) QueueFail(ctx context.Context, id int64, cause string) error {
	if _, err := db.w.ExecContext(ctx,
		`UPDATE history_queue SET attempts = attempts + 1, last_error = ? WHERE id = ?`, cause, id); err != nil {
		return fmt.Errorf("fail history notification: %w", err)
	}
	return nil
}

// QueueDone takes the notification off the queue. It belongs in the Tx of the
// last chunk of the notification's messages, so that a crash leaves either
// the chunk and the notification, which is processed again (idempotently),
// or both done. Done twice is not an error.
func (t *Tx) QueueDone(id int64) error {
	if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM history_queue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("finish history notification: %w", err)
	}
	return nil
}

// Stuck is what is left in the queue that the worker has given up on.
type Stuck struct {
	Count     int
	LastError string // of the newest of them
}

// QueueStuck tells how many notifications of the account have failed
// MaxAttempts times, for the reason manage-accounts list gives: they are never
// retried, and the user should know that part of the history is missing.
func (db *DB) QueueStuck(ctx context.Context, account string) (Stuck, error) {
	var (
		s    Stuck
		last sql.Null[string]
	)
	err := db.r.QueryRowContext(ctx, queueStuckSQL, account, MaxAttempts).Scan(&s.Count, &last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Stuck{}, nil
	case err != nil:
		return Stuck{}, wrap(err, "stuck history notifications", account)
	}
	s.LastError = last.V
	return s, nil
}

// The window function counts all the stuck rows before LIMIT picks the newest.
const queueStuckSQL = `
SELECT count(*) OVER (), last_error FROM history_queue
WHERE account = ? AND attempts >= ? ORDER BY id DESC LIMIT 1`
