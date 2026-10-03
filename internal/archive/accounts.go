package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
)

var (
	// ErrNoAccount means there is no account with that nick.
	ErrNoAccount = errors.New("no such account")
	// ErrJIDTaken means the device is already linked to another nick; the
	// error names that nick.
	ErrJIDTaken = errors.New("device already linked to another account")
)

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
// alone, so the orphan cleanup has one state to compare against.
// accounts.jid is UNIQUE: a device that another nick holds is ErrJIDTaken.
func (db *DB) SetAccountJID(ctx context.Context, nick, jid string) error {
	const op = "set jid of account"
	if jid == "" {
		return fmt.Errorf("%s %q: empty jid", op, nick)
	}
	err := db.execAccount(ctx, op, nick, `UPDATE accounts SET jid = ? WHERE nick = ?`, jid, nick)
	if err == nil || errors.Is(err, ErrNoAccount) {
		return err
	}
	// Asking who holds the jid is simpler than decoding the driver's
	// constraint error, and the error can then name the nick.
	var holder string
	if db.w.QueryRowContext(ctx, `SELECT nick FROM accounts WHERE jid = ? AND nick <> ?`, jid, nick).Scan(&holder) == nil {
		return fmt.Errorf("%s %q: %w: %s", op, nick, ErrJIDTaken, holder)
	}
	return err
}

// ErrNotScrubbed means DeleteAccount has deleted the account, and what it did after
// to leave nothing of it in the files has not all worked: it is gone for good, and
// old copies of its rows may be in the file or the WAL until the next checkpoint.
var ErrNotScrubbed = errors.New("the account is deleted, but its old copies may still be in archive.db or its WAL")

// DeleteAccount forgets the account and, by ON DELETE CASCADE, its whole
// archive: chats, messages with their full-text index, the history queue.
// secure_delete has zeroed the freed pages. The full-text index is a trigram
// one with the positions of the words, and its deletes only mark the entries
// (contentless_delete), which a merge drops: the 'optimize' that follows is what
// leaves no piece of the text in its segments. VACUUM gives the pages back to the
// file system (simpler than auto_vacuum, and a remove is rare), and the checkpoint
// moves its result into archive.db and empties the WAL, which still holds the rows
// as they were written. These three are best effort, as the delete is committed,
// and they run whatever becomes of ctx, which a client that has given up on the
// call must not turn into a delete left half scrubbed; if any of them fails, the
// error is ErrNotScrubbed. A reader that holds off the checkpoint leaves the
// VACUUM's copy of the database in the WAL; the automatic checkpoints move it
// later, and journal_size_limit (sqlitedb.Pragmas) cuts the file back.
// VACUUM cannot run inside a transaction, so it goes straight to the writer;
// while it runs, writes of the other accounts wait, and at its peak it needs
// about twice the database in extra disk space: its temp copy plus the WAL.
func (db *DB) DeleteAccount(ctx context.Context, nick string) error {
	if err := db.execAccount(ctx, "delete account", nick, `DELETE FROM accounts WHERE nick = ?`, nick); err != nil {
		return err
	}
	scrub := context.WithoutCancel(ctx)
	var errs []error
	for _, stmt := range []string{"INSERT INTO messages_fts(messages_fts) VALUES('optimize')", "VACUUM"} {
		if _, err := db.w.ExecContext(scrub, stmt); err != nil {
			errs = append(errs, err)
		}
	}
	if err := sqlitedb.Checkpoint(scrub, db.w); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrNotScrubbed, errors.Join(errs...))
	}
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
