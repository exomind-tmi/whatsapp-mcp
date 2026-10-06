package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrQueryTooShort means the query has no word of three characters or more.
// The full-text index is made of trigrams, so a shorter word cannot be found
// in it; the caller is told, instead of being given an empty result that
// looks like "no such message".
var ErrQueryTooShort = errors.New("search needs at least one word of 3 or more characters")

// SearchQuery is a full-text search over messages.
type SearchQuery struct {
	Accounts []string  // empty: all accounts
	Query    string    // words, all of which a message must contain, in any order; see matchExpr
	Chat     string    // canonical JID; empty: any chat
	Senders  []string  // sender JIDs, as stored (a person has a PN and a LID form); empty: anybody
	After    time.Time // the message is not older than this (inclusive); zero: no bound
	Before   time.Time // and is older than this (exclusive); zero: no bound
	Limit    int
}

// maxWords bounds the words of a query that count: a person types a few, and
// each one more is another phrase for the index to intersect.
const maxWords = 16

// matchExpr turns what a person typed into an FTS5 expression. The query is
// never passed to FTS5 as it is: its syntax (quotes, NEAR, AND/OR/NOT, '*',
// '^', 'column:', brackets) would make a stray character a syntax error or
// let the person write a query of their own. Instead:
//   - control characters and invalid UTF-8 are separators, as spaces are;
//   - ё and Ё become е and Е, as in the text the index was made of;
//   - the punctuation at the ends of a word is cut off (see trimEdges);
//   - a word shorter than 3 characters is dropped, and no word left is
//     ErrQueryTooShort;
//   - each word becomes a phrase in double quotes (a quote inside is doubled),
//     in which every character is literal, and the phrases are ANDed. The
//     trigram index searches for the substring, so "100%" and "a.b-c" are
//     found as written, and so is a part of a word, "ривет" in "привет".
func matchExpr(query string) (string, error) {
	query = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(query, " "))
	var phrases []string
	for _, w := range strings.Fields(yo.Replace(query)) {
		w = trimEdges(w)
		if utf8.RuneCountInString(w) < 3 {
			continue
		}
		phrases = append(phrases, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
		if len(phrases) == maxWords {
			break
		}
	}
	if len(phrases) == 0 {
		return "", ErrQueryTooShort
	}
	return strings.Join(phrases, " "), nil
}

// trimEdges takes the punctuation off the ends of a word, so that the quotes of
// "аренда гаража", the question mark of оплата? and the brackets of (гараж)
// do not become part of the text sought: the index finds a substring, and a
// message does not have a quote where the person only marked a phrase. What is
// trimmed is a substring of the word, so everything that matched the word
// still matches, and a query that was answered is never answered with an
// error: a word whose core would be left under 3 characters ("50%", "(8)") is
// kept as it is, and is searched as written. Symbols (+ = $ ^) are not
// punctuation and stay: "1+1=2" is searched as written.
func trimEdges(w string) string {
	t := strings.TrimFunc(w, unicode.IsPunct)
	if utf8.RuneCountInString(t) < 3 {
		return w
	}
	return t
}

// Search finds the messages that contain all the words of the query, newest
// first. The ranking is done in the inner query, and the name of the chat is
// joined only for the rows that survive its LIMIT.
func (db *DB) Search(ctx context.Context, q SearchQuery) ([]Hit, error) {
	if q.Limit <= 0 {
		return nil, ErrBadLimit
	}
	match, err := matchExpr(q.Query)
	if err != nil {
		return nil, err
	}
	query, args := searchSQL(q, match)
	rows, err := db.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap(err, "search messages", "")
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var name sql.Null[string]
		m, err := scanMessage(rows, &name)
		if err != nil {
			return nil, wrap(err, "search messages", "")
		}
		out = append(out, Hit{Message: m, ChatName: name.V})
	}
	return out, wrap(rows.Err(), "search messages", "")
}

func searchSQL(q SearchQuery, match string) (string, []any) {
	where, args := []string{"messages_fts MATCH ?"}, []any{match}
	add := func(cond string, vals ...any) {
		where, args = append(where, cond), append(args, vals...)
	}
	addIn := func(col string, vals []string) {
		if len(vals) > 0 {
			var clause string
			clause, args = inClause(col, vals, args)
			where = append(where, clause)
		}
	}
	addIn("m.account", q.Accounts)
	if q.Chat != "" {
		add("m.chat_jid = ?", q.Chat)
	}
	addIn("m.sender_jid", q.Senders)
	if !q.After.IsZero() {
		add("m.ts >= ?", q.After.Unix())
	}
	if !q.Before.IsZero() {
		add("m.ts < ?", q.Before.Unix())
	}
	return `SELECT ` + msgCols + `, c.name FROM (
  SELECT ` + msgCols + ` FROM messages_fts f JOIN messages m ON m.id = f.rowid
  WHERE ` + strings.Join(where, " AND ") + `
  ORDER BY m.ts DESC, m.id DESC LIMIT ?) m
LEFT JOIN chats c ON c.account = m.account AND c.jid = m.chat_jid
ORDER BY m.ts DESC, m.id DESC`, append(args, q.Limit)
}
