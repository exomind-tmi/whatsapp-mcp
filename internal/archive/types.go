package archive

import (
	"database/sql"
	"errors"
	"time"
)

var (
	// ErrNoMessage means the archive has no such message.
	ErrNoMessage = errors.New("no such message")
	// ErrBadLimit means a query asked for a page of no rows or fewer (or, for
	// Around, for fewer than none): a missing limit is a bug in the caller,
	// and failing is better than guessing a page size.
	ErrBadLimit = errors.New("invalid limit")
)

// Row is a message as the classifier saw it, everything Upsert stores. Times
// are kept in unix seconds, which is also why they are time.Time here and not
// in the columns: the driver would send a time.Time as TEXT, which STRICT
// refuses.
type Row struct {
	Account, Chat, ID string // Chat is the canonical JID; ID is the WhatsApp message id
	Sender            string // as received: a quote needs it verbatim
	FromMe            bool
	TS                time.Time
	Text              string // text, caption or document name; "" is stored as NULL
	MediaType         string // image|video|audio|ptt|document|sticker; "" for a text
	MediaMime         string
	MediaName         string
	MediaSize         int64 // 0 is stored as NULL
	QuotedID          string
	// Raw is proto.Marshal(waE2E.Message). It is what tells a message from the
	// stub an edit or a revoke made before it, so a nil one is stored as an
	// empty blob: the row is still a message, and a redelivery cannot
	// overwrite it.
	Raw []byte
}

// Edit is a message edited to Text at EditedAt. Sender and FromMe matter only
// when the edit came before the message itself (history sync does not keep the
// order): a stub is stored then, dated by the edit until the original fills it.
type Edit struct {
	Account, Chat, ID string
	Sender            string
	FromMe            bool
	Text              string
	EditedAt          time.Time
}

// Revoke is a message the sender deleted for everybody at RevokedAt. Its text
// and raw stay: a revoked message is marked, not erased. Sender and FromMe are
// used only for a stub of a message not archived yet, as in Edit.
type Revoke struct {
	Account, Chat, ID string
	Sender            string
	FromMe            bool
	RevokedAt         time.Time
}

// ChatUpd is what is learned of a chat. An empty PN or Name and a zero
// LastMessageTS change nothing that is stored; a chat that has never had a
// message keeps a NULL last_message_ts, never 0, or it would be listed as
// written in 1970.
type ChatUpd struct {
	Account, JID  string // JID is the canonical one
	PN            string // PN-JID (digits@s.whatsapp.net), for display and search
	Name          string
	IsGroup       bool
	LastMessageTS time.Time
}

// Message is a stored message as the read tools show it. Raw is left out of
// every query but MessageWithRaw: it is the widest column, and no list needs it.
type Message struct {
	Account, Chat, ID string
	Sender            string
	FromMe            bool
	TS                time.Time
	Text              string
	MediaType         string
	MediaMime         string
	MediaName         string
	MediaSize         int64
	MediaPath         string // the last downloaded file
	QuotedID          string
	EditedAt          time.Time // zero: never edited
	RevokedAt         time.Time // zero: not revoked
	Raw               []byte    // only from MessageWithRaw; empty for a stub, and for a message stored with none
	Target            bool      // only in Around: the message that was asked for

	rowid int64 // what Cursor is made of
}

// Cursor is the position of the message in its chat's order.
func (m Message) Cursor() Cursor { return Cursor{TS: m.TS.Unix(), ID: m.rowid} }

// Hit is a search result: a message with the name of its chat, which a result
// that mixes chats of several accounts needs and a page of one chat does not.
type Hit struct {
	Message
	ChatName string
}

// Chat is a row of chats.
type Chat struct {
	Account, JID  string
	PN            string
	Name          string
	IsGroup       bool
	LastMessageTS time.Time // zero: no message yet
}

// QueueItem is a history sync notification waiting for the worker.
type QueueItem struct {
	ID       int64
	MsgID    string // the message that carried the notification
	Notif    []byte // proto.Marshal(HistorySyncNotification)
	Attempts int
}

// nullTime is a time for a nullable column: the zero time is NULL.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// nullStr is a string for a nullable column: "" is NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt is a number for a nullable column: 0 is NULL.
func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// nullBytes is a blob for a nullable column: an empty one is NULL.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fromUnix reads a nullable time column.
func fromUnix(n sql.Null[int64]) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.V, 0)
}
