package archive

import (
	"context"
	"database/sql"
	"strings"
	"unicode"
)

// ChatQuery selects chats for list-chats.
type ChatQuery struct {
	Accounts []string // empty: all accounts
	// Query keeps the chats whose name contains it, ignoring case and ё/е, and,
	// when it reads as a phone number (digits, spaces, + - and brackets), the
	// chats whose number contains its digits. Empty keeps all.
	Query string
	Limit int
}

// Chats lists the chats, the most recently active first and those without a
// message last. The query is matched in Go, not in SQL: SQLite's lower() and
// LIKE know only ASCII case, so they would miss "привет" in "Привет", and the
// number of chats is thousands, not millions. SQLite sorts them, the rows are
// read in the order they are wanted in, and the read stops at the limit: what
// a narrow limit saves is the reading of the rest into Go values, not the sort.
func (db *DB) Chats(ctx context.Context, q ChatQuery) ([]Chat, error) {
	if q.Limit <= 0 {
		return nil, ErrBadLimit
	}
	query := "SELECT account, jid, pn, name, is_group, last_message_ts FROM chats"
	var args []any
	if len(q.Accounts) > 0 {
		var clause string
		clause, args = inClause("account", q.Accounts, args)
		query += " WHERE " + clause
	}
	query += " ORDER BY last_message_ts DESC NULLS LAST, account, jid"
	rows, err := db.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap(err, "list chats", "")
	}
	defer rows.Close()
	match := newChatMatcher(q.Query)
	var out []Chat
	for len(out) < q.Limit && rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, wrap(err, "list chats", "")
		}
		if match(c) {
			out = append(out, c)
		}
	}
	return out, wrap(rows.Err(), "list chats", "")
}

func scanChat(rows *sql.Rows) (Chat, error) {
	var (
		c        Chat
		pn, name sql.Null[string]
		last     sql.Null[int64]
	)
	if err := rows.Scan(&c.Account, &c.JID, &pn, &name, &c.IsGroup, &last); err != nil {
		return Chat{}, err
	}
	c.PN, c.Name, c.LastMessageTS = pn.V, name.V, fromUnix(last)
	return c, nil
}

// yo folds the letter ё into е, as the full-text index does with the text of
// messages (remove_diacritics leaves it alone), so that a person who types
// either finds both.
var yo = strings.NewReplacer("ё", "е", "Ё", "Е")

func fold(s string) string { return strings.ToLower(yo.Replace(s)) }

// newChatMatcher is the filter of ChatQuery.Query.
func newChatMatcher(query string) func(Chat) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return func(Chat) bool { return true }
	}
	name, digits := fold(query), phoneDigits(query)
	return func(c Chat) bool {
		return strings.Contains(fold(c.Name), name) ||
			(digits != "" && strings.Contains(chatDigits(c), digits))
	}
}

// chatDigits is the number of the chat: its pn, or, for a chat that is kept
// under the phone number itself (no LID is known for it), its jid. A LID is
// not a number and is never searched as one.
func chatDigits(c Chat) string {
	if c.PN == "" && strings.HasSuffix(c.JID, pnServer) {
		return jidDigits(c.JID)
	}
	return jidDigits(c.PN)
}

// pnServer is the suffix of a PN-JID.
const pnServer = "@s.whatsapp.net"

// phoneDigits is the digits of the query if it is written as a phone number
// and nothing else, so that "Room 101" does not find every number with 101 in
// it; otherwise "".
func phoneDigits(query string) string {
	var digits strings.Builder
	for _, r := range query {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' || r == '-' || r == '(' || r == ')' || unicode.IsSpace(r):
		default:
			return ""
		}
	}
	return digits.String()
}

// jidDigits is the number of a PN-JID, "79991234567@s.whatsapp.net".
func jidDigits(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	return user
}

// AccountsForChat is the accounts that have the chat, which is how a tool
// picks the account when the caller gave a chat and no account. The chat is
// matched by its JID or by its PN, so that one given by number finds the
// chat the LID has not been learned for yet. It reads chats, not messages:
// without an account a lookup by chat_jid in messages would scan them all.
func (db *DB) AccountsForChat(ctx context.Context, jid string) ([]string, error) {
	rows, err := db.r.QueryContext(ctx, accountsForChatSQL, jid)
	if err != nil {
		return nil, wrap(err, "find accounts of a chat", "")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, wrap(err, "find accounts of a chat", "")
		}
		out = append(out, a)
	}
	return out, wrap(rows.Err(), "find accounts of a chat", "")
}

const accountsForChatSQL = `SELECT DISTINCT account FROM chats WHERE jid = ?1 OR pn = ?1 ORDER BY account`

// inClause appends "col IN (?, ...)" for vals to a WHERE under construction
// and the values to its arguments.
func inClause(col string, vals []string, args []any) (string, []any) {
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(vals)), ", ")
	for _, v := range vals {
		args = append(args, v)
	}
	return col + " IN (" + marks + ")", args
}
