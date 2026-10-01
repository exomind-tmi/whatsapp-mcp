package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoAccount means there is no account with that nick.
var ErrNoAccount = errors.New("no such account")

// Account is a row of accounts with the size of its archive. The nick is
// validated by the caller (wa.ValidNick): archive sits below wa and must not
// import it.
type Account struct {
	Nick      string
	JID       string // full AD-JID of the device in store.db; "" until first linked
	CreatedAt time.Time
	Chats     int
	Messages  int
}

// Accounts lists the accounts by nick. Both counts are index-only scans:
// chats and messages are keyed by account first.
func (db *DB) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := db.w.QueryContext(ctx, `
SELECT a.nick, a.jid, a.created_at,
  (SELECT count(*) FROM chats c WHERE c.account = a.nick),
  (SELECT count(*) FROM messages m WHERE m.account = a.nick)
FROM accounts a ORDER BY a.nick`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var (
			a       Account
			jid     sql.NullString
			created int64
		)
		if err := rows.Scan(&a.Nick, &jid, &created, &a.Chats, &a.Messages); err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		a.JID, a.CreatedAt = jid.String, time.Unix(created, 0)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, nil
}

// AddAccount creates the account with no device yet. It is idempotent: a
// relink adds the same nick again, and its jid and created_at are kept.
func (db *DB) AddAccount(ctx context.Context, nick string) error {
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO accounts(nick, created_at) VALUES (?, ?) ON CONFLICT(nick) DO NOTHING`,
		nick, time.Now().Unix()); err != nil {
		return fmt.Errorf("add account %q: %w", nick, err)
	}
	return nil
}

// SetAccountJID records the device the account is linked to; a relink
// replaces it. jid is the full AD-JID (with the device number), which
// store.db's GetDevice needs. An empty one is refused: "not linked" is NULL
// alone, so the orphan cleanup of plan 6.1 has one state to compare against.
func (db *DB) SetAccountJID(ctx context.Context, nick, jid string) error {
	if jid == "" {
		return fmt.Errorf("set jid of account %q: empty jid", nick)
	}
	return db.execAccount(ctx, "set jid of account", nick, `UPDATE accounts SET jid = ? WHERE nick = ?`, jid, nick)
}

// DeleteAccount forgets the account and, by ON DELETE CASCADE, its whole
// archive: chats, messages with their full-text index, the history queue.
// secure_delete has zeroed the freed pages; the checkpoint also empties the
// WAL, which still holds the rows as they were written. It is best effort:
// the delete is committed, and SQLite checkpoints on its own soon anyway.
func (db *DB) DeleteAccount(ctx context.Context, nick string) error {
	if err := db.execAccount(ctx, "delete account", nick, `DELETE FROM accounts WHERE nick = ?`, nick); err != nil {
		return err
	}
	_, _ = db.w.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return nil
}

// execAccount runs a statement meant to touch the row of one account and
// reports ErrNoAccount when there was none; op and nick prefix the error.
func (db *DB) execAccount(ctx context.Context, op, nick, query string, args ...any) error {
	res, err := db.w.ExecContext(ctx, query, args...)
	var n int64
	if err == nil {
		n, err = res.RowsAffected()
	}
	switch {
	case err != nil:
		return fmt.Errorf("%s %q: %w", op, nick, err)
	case n == 0:
		return fmt.Errorf("%s %q: %w", op, nick, ErrNoAccount)
	}
	return nil
}
