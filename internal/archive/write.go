package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Tx is a transaction on the writer. The live handler applies an operation,
// the chat's TouchChat and a MergeChat in one, and the history worker writes
// a chunk of messages in one; everything in it is applied or nothing is.
//
// The messages table is written only by upserts, never by INSERT OR REPLACE or
// REPLACE: the row a REPLACE deletes does not fire messages_ad (recursive
// triggers are off), so its text would stay in messages_fts as an orphan. The
// writes are idempotent and independent of the order they arrive in, because
// WhatsApp delivers at least once, and history sync brings the edit or the
// revoke of a message before the message itself.
type Tx struct {
	ctx context.Context
	tx  *sql.Tx
}

// Tx runs fn in a transaction on the writer and commits it, or rolls it back
// when fn returns an error, panics or leaves its goroutine (runtime.Goexit,
// which is what t.Fatal does): the writer has one connection, and a
// transaction left open would hold it for good. The write lock is taken at
// BEGIN (sqlitedb.Pragmas), so fn never fails half way for a lock.
//
// fn must not call the DB's methods. Those that write wait for the writer
// connection that Tx holds, and never get it; those that read do not wait, but
// read what was committed before, not what fn has written so far.
func (db *DB) Tx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() // no-op after Commit
	if err := fn(&Tx{ctx: ctx, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// A message that is already stored as a real one (it has a raw) is not
// touched by its redelivery: it may have been edited, revoked or downloaded
// since. Only a stub (raw IS NULL), which an edit or a revoke made before the
// message came, takes the original's header, content and media; its text is
// the original's unless an edit has set one already, and revoked_at stays.
const upsertSQL = `
INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text,
  media_type, media_mime, media_name, media_size, quoted_id, raw)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account, chat_jid, msg_id) DO UPDATE SET
  sender_jid = excluded.sender_jid, from_me = excluded.from_me, ts = excluded.ts,
  media_type = excluded.media_type, media_mime = excluded.media_mime, media_name = excluded.media_name,
  media_size = excluded.media_size, quoted_id = excluded.quoted_id, raw = excluded.raw,
  text = CASE WHEN messages.edited_at IS NULL THEN excluded.text ELSE messages.text END
WHERE messages.raw IS NULL`

// Upsert stores the message. Sending it again changes nothing.
func (t *Tx) Upsert(r Row) error {
	if err := needKey("upsert message", r.Account, r.Chat, r.ID); err != nil {
		return err
	}
	if r.Sender == "" || r.TS.IsZero() {
		return fmt.Errorf("upsert message of account %q: sender and time are required", r.Account)
	}
	raw := r.Raw
	if raw == nil {
		raw = []byte{}
	}
	_, err := t.tx.ExecContext(t.ctx, upsertSQL,
		r.Account, r.Chat, r.ID, r.Sender, b2i(r.FromMe), r.TS.Unix(), nullStr(r.Text),
		nullStr(r.MediaType), nullStr(r.MediaMime), nullStr(r.MediaName), nullInt(r.MediaSize),
		nullStr(r.QuotedID), raw)
	return wrap(err, "upsert message", r.Account)
}

// The latest edit wins, whatever order they come in, and an edit does not
// care whether the message was revoked: an edit that arrives after the revoke
// is still the last text the sender had, and what the archive ends up with
// must not depend on which of the two history sync happened to bring first.
const editSQL = `
INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text, edited_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account, chat_jid, msg_id) DO UPDATE SET text = excluded.text, edited_at = excluded.edited_at
WHERE messages.edited_at IS NULL OR messages.edited_at < excluded.edited_at`

// Edit stores the new text of the message, or a stub for the message if it is
// not archived yet.
func (t *Tx) Edit(e Edit) error {
	if err := needKey("edit message", e.Account, e.Chat, e.ID); err != nil {
		return err
	}
	if e.EditedAt.IsZero() {
		return fmt.Errorf("edit message of account %q: the time of the edit is required", e.Account)
	}
	at := e.EditedAt.Unix()
	_, err := t.tx.ExecContext(t.ctx, editSQL,
		e.Account, e.Chat, e.ID, e.Sender, b2i(e.FromMe), at, nullStr(e.Text), at)
	return wrap(err, "edit message", e.Account)
}

// A revoke marks the message and leaves its text and raw: the sender took it
// back, but what they sent is still the archive's to show, marked.
const revokeSQL = `
INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, revoked_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account, chat_jid, msg_id) DO UPDATE SET revoked_at = excluded.revoked_at
WHERE messages.revoked_at IS NULL`

// Revoke marks the message revoked, or stores a stub for the message if it is
// not archived yet.
func (t *Tx) Revoke(r Revoke) error {
	if err := needKey("revoke message", r.Account, r.Chat, r.ID); err != nil {
		return err
	}
	if r.RevokedAt.IsZero() {
		return fmt.Errorf("revoke message of account %q: the time of the revoke is required", r.Account)
	}
	at := r.RevokedAt.Unix()
	_, err := t.tx.ExecContext(t.ctx, revokeSQL,
		r.Account, r.Chat, r.ID, r.Sender, b2i(r.FromMe), at, at)
	return wrap(err, "revoke message", r.Account)
}

const setMediaSQL = `
UPDATE messages SET media_path = coalesce(?, media_path), raw = coalesce(?, raw)
WHERE account = ? AND chat_jid = ? AND msg_id = ?`

// SetMedia records the file a download saved (path) and/or the raw message
// after a media retry changed it; "" and nil leave the stored value as it is.
func (t *Tx) SetMedia(account, chat, id, path string, raw []byte) error {
	res, err := t.tx.ExecContext(t.ctx, setMediaSQL, nullStr(path), nullBytes(raw), account, chat, id)
	if err != nil {
		return wrap(err, "set media of message", account)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("set media of message of account %q: %w", account, ErrNoMessage)
	}
	return nil
}

// SetMedia is Tx.SetMedia on its own, for the download that has nothing else
// to write.
func (db *DB) SetMedia(ctx context.Context, account, chat, id, path string, raw []byte) error {
	return db.Tx(ctx, func(t *Tx) error { return t.SetMedia(account, chat, id, path, raw) })
}

// The chat's last_message_ts only grows, and NULL is not 0: of two values the
// NULL gives way, and with both NULL the result is NULL. max(coalesce(x, 0),
// ...) would turn a chat with no message into 1970. A later name or pn
// replaces an earlier one (a group is renamed), and an unknown one keeps it.
const touchChatSQL = `
INSERT INTO chats(account, jid, pn, name, is_group, last_message_ts) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(account, jid) DO UPDATE SET
  pn = coalesce(excluded.pn, chats.pn),
  name = coalesce(excluded.name, chats.name),
  is_group = max(chats.is_group, excluded.is_group),
  last_message_ts = max(coalesce(chats.last_message_ts, excluded.last_message_ts),
                        coalesce(excluded.last_message_ts, chats.last_message_ts))`

// TouchChat creates the chat or adds what is now known of it.
func (t *Tx) TouchChat(c ChatUpd) error {
	if c.Account == "" || c.JID == "" {
		return errors.New("touch chat: account and jid are required")
	}
	_, err := t.tx.ExecContext(t.ctx, touchChatSQL,
		c.Account, c.JID, nullStr(c.PN), nullStr(c.Name), b2i(c.IsGroup), nullTime(c.LastMessageTS))
	return wrap(err, "touch chat", c.Account)
}

// needKey refuses the empty parts of a message's key: an empty id would put
// every message of the kind on one row.
func needKey(op, account, chat, id string) error {
	if account == "" || chat == "" || id == "" {
		return fmt.Errorf("%s: account, chat and id are required", op)
	}
	return nil
}

// wrap adds what failed and for which account to a driver error. The chat and
// the sender are left out: they are phone numbers, and the error may be logged.
func wrap(err error, op, account string) error {
	switch {
	case err == nil:
		return nil
	case account == "":
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s of account %q: %w", op, account, err)
}
