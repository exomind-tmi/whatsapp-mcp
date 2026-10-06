package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// msgCols is every column a read tool shows; raw is not among them.
const msgCols = `m.id, m.account, m.chat_jid, m.msg_id, m.sender_jid, m.from_me, m.ts, m.text,
  m.media_type, m.media_mime, m.media_name, m.media_size, m.media_path, m.quoted_id, m.edited_at, m.revoked_at`

// scanMessage reads a row of msgCols, then extra (what the caller selected
// after them).
func scanMessage(s interface{ Scan(...any) error }, extra ...any) (Message, error) {
	var (
		m                                        Message
		ts                                       int64
		text, mtype, mmime, mname, mpath, quoted sql.Null[string]
		msize, edited, revoked                   sql.Null[int64]
	)
	dest := append([]any{&m.rowid, &m.Account, &m.Chat, &m.ID, &m.Sender, &m.FromMe, &ts, &text,
		&mtype, &mmime, &mname, &msize, &mpath, &quoted, &edited, &revoked}, extra...)
	if err := s.Scan(dest...); err != nil {
		return Message{}, err
	}
	m.TS = time.Unix(ts, 0)
	m.Text, m.MediaType, m.MediaMime, m.MediaName = text.V, mtype.V, mmime.V, mname.V
	m.MediaSize, m.MediaPath, m.QuotedID = msize.V, mpath.V, quoted.V
	m.EditedAt, m.RevokedAt = fromUnix(edited), fromUnix(revoked)
	return m, nil
}

// MsgQuery is a page of one chat's messages, going back in time.
type MsgQuery struct {
	Account, Chat string
	Before        Cursor    // the page ends before this position; zero: at the newest message
	After         time.Time // and does not reach back before this time (inclusive); zero: no bound
	Limit         int
}

// Page is a page of messages, oldest first.
type Page struct {
	Messages []Message
	Next     Cursor // the position to pass as Before for the page of older ones; zero if there is none
}

// Messages is the newest Limit messages of the chat that are before the cursor
// and not before After, in chronological order. The order is (ts, id) both ways
// (see Cursor), which messages_chat_ts serves without a sort.
func (db *DB) Messages(ctx context.Context, q MsgQuery) (Page, error) {
	if q.Limit <= 0 {
		return Page{}, ErrBadLimit
	}
	// One row more than asked tells whether there are older ones, so the last
	// page does not promise an empty one.
	msgs, err := chatMessages(ctx, db.r, chatPage{account: q.Account, chat: q.Chat, cursor: q.Before, since: q.After, limit: q.Limit + 1})
	if err != nil {
		return Page{}, err
	}
	var next Cursor
	if len(msgs) > q.Limit {
		msgs = msgs[:q.Limit]
		next = msgs[len(msgs)-1].Cursor()
	}
	slices.Reverse(msgs)
	return Page{Messages: msgs, Next: next}, nil
}

// chatPage is one directional read of a chat from a position: older ones
// (newest first), or newer ones (oldest first).
type chatPage struct {
	account, chat string
	newer         bool
	cursor        Cursor    // exclusive; zero: from the end
	since         time.Time // older ones only
	limit         int
}

func (p chatPage) sql() (string, []any) {
	q := "SELECT " + msgCols + " FROM messages m WHERE m.account = ? AND m.chat_jid = ?"
	args := []any{p.account, p.chat}
	cmp, dir := "<", "DESC"
	if p.newer {
		cmp, dir = ">", "ASC"
	}
	if !p.cursor.IsZero() {
		q += " AND (m.ts, m.id) " + cmp + " (?, ?)"
		args = append(args, p.cursor.TS, p.cursor.ID)
	}
	if !p.since.IsZero() {
		q += " AND m.ts >= ?"
		args = append(args, p.since.Unix())
	}
	return q + " ORDER BY m.ts " + dir + ", m.id " + dir + " LIMIT ?", append(args, p.limit)
}

// querier is what the reader pool and a transaction on it have in common, so
// that the same read runs on either.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// chatMessages returns the page in the order it was read.
func chatMessages(ctx context.Context, q querier, p chatPage) ([]Message, error) {
	query, args := p.sql()
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap(err, "read messages", p.account)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, wrap(err, "read messages", p.account)
		}
		out = append(out, m)
	}
	return out, wrap(rows.Err(), "read messages", p.account)
}

const (
	messageSQL    = `SELECT ` + msgCols + ` FROM messages m WHERE m.account = ? AND m.chat_jid = ? AND m.msg_id = ?`
	messageRawSQL = `SELECT ` + msgCols + `, m.raw FROM messages m WHERE m.account = ? AND m.chat_jid = ? AND m.msg_id = ?`
)

// Message is one message, by the unique key it has: the account, the chat and
// the id.
func (db *DB) Message(ctx context.Context, account, chat, id string) (Message, error) {
	return message(ctx, db.r, account, chat, id)
}

func message(ctx context.Context, q querier, account, chat, id string) (Message, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, messageSQL, account, chat, id))
	return m, messageErr(err, account)
}

// MessageWithRaw is Message with its Raw, which a reply (the quoted message)
// and a download (the media keys) are made from. A stub has none.
func (db *DB) MessageWithRaw(ctx context.Context, account, chat, id string) (Message, error) {
	var raw []byte
	m, err := scanMessage(db.r.QueryRowContext(ctx, messageRawSQL, account, chat, id), &raw)
	m.Raw = raw
	return m, messageErr(err, account)
}

func messageErr(err error, account string) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read message of account %q: %w", account, ErrNoMessage)
	case err != nil:
		return wrap(err, "read message", account)
	}
	return nil
}

// Around is the message with up to before messages ahead of it and after
// behind it, oldest first; the message itself is marked Target. Neighbours
// are taken by the same (ts, id) order as pages, so equal times neither hide
// nor repeat any. The three reads share one snapshot: on separate ones a
// merge of the chat between them would leave the message without its
// neighbours.
func (db *DB) Around(ctx context.Context, account, chat, id string, before, after int) ([]Message, error) {
	if before < 0 || after < 0 {
		return nil, ErrBadLimit
	}
	// ReadOnly makes the driver begin a plain deferred transaction whatever the
	// DSN says about the lock of a write transaction: nothing here writes.
	tx, err := db.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, wrap(err, "read messages", account)
	}
	defer tx.Rollback() // nothing to commit
	target, err := message(ctx, tx, account, chat, id)
	if err != nil {
		return nil, err
	}
	target.Target = true
	older, err := neighbours(ctx, tx, chatPage{account: account, chat: chat, cursor: target.Cursor(), limit: before})
	if err != nil {
		return nil, err
	}
	newer, err := neighbours(ctx, tx, chatPage{account: account, chat: chat, cursor: target.Cursor(), newer: true, limit: after})
	if err != nil {
		return nil, err
	}
	slices.Reverse(older)
	return append(append(older, target), newer...), nil
}

func neighbours(ctx context.Context, q querier, p chatPage) ([]Message, error) {
	if p.limit == 0 {
		return nil, nil
	}
	return chatMessages(ctx, q, p)
}
