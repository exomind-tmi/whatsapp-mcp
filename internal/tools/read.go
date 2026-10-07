package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// What the read tools have in common: how an account is chosen, how a chat and a
// time are read, what the agent is told of an account that is not connected, and
// what is kept of the words that come back.
//
// Everything the agent is told in an error or in notes is composed here from
// fixed words, account ids (which are 1-64 of a-z, 0-9, _ and -) and statuses.
// What other people wrote (texts, names, file names) and what the agent was given
// to look for is never put in them: it comes back as data, in the fields of the
// result, where a host and a model can tell it from our own words.

// The limits of the read tools. Their descriptions quote them, so that what an
// agent is told is what is enforced.
const (
	defaultChats, maxChats       = 50, 200
	defaultMessages, maxMessages = 50, 200
	defaultHits, maxHits         = 20, 100
	defaultAround, maxAround     = 5, 20

	// What a result carries of what other people wrote is limited. A text is at most
	// maxText characters, and any other string (the name of a person, a chat or a file,
	// a media type, a quoted id) at most maxName, each on its own. The result as a
	// whole, as the JSON that goes out, is at most maxResultBytes where the strings can
	// make it so: a host takes only so much of a tool result (Claude Code refuses more
	// than 25,000 tokens), a character is one byte or six (an escaped < or control
	// character), and the result goes out twice, as text and as structured content.
	// When it is more the long strings are cut shorter, to one common length (see
	// capResult), and a text that is cut is marked truncated; nothing is left out.
	// What does not depend on the strings (the ids and addresses, a few hundred bytes
	// for each message) is not cut, so a page of the most messages is cut the most.
	maxText        = 4000
	maxName        = 200
	maxResultBytes = 64 << 10
)

// What a result says when its strings were cut shorter than their own limits,
// because the result as a whole was big.
const (
	cutAdvice      = "texts were cut shorter than usual because the result is big (`truncated`): ask for fewer messages (limit) to get them whole, or read one message with get-message-context before=0 after=0"
	namesCutAdvice = "names were cut shorter than usual because the result is big: ask for fewer chats (limit)"
)

// moreAdvice is what a list that has reached its limit tells of the rest: where
// it is, how to narrow the list, and how to raise the limit if it can be raised.
func moreAdvice(what string, shown, most int, narrow string) string {
	advice := fmt.Sprintf("there are more %s than the %d shown, the most recent first: narrow it with %s", what, shown, narrow)
	if shown < most {
		advice += fmt.Sprintf(", or raise limit (at most %d)", most)
	}
	return advice
}

// cutNote is what the tools that show messages tell of the limits above.
var cutNote = fmt.Sprintf("A long text is cut (`truncated`): over %d characters, and sooner if the result is big.", maxText)

// groupSuffix ends the JID of a group.
const groupSuffix = "@g.us"

func isGroup(chat string) bool { return strings.HasSuffix(chat, groupSuffix) }

// AccountArg is the account of a tool that works on one: given, or told by the
// chat, or the only one there is.
type AccountArg struct {
	Account string `json:"account,omitempty" jsonschema:"account id from manage-accounts list; may be left out if only one account is linked or the chat is in one account only"`
}

// AccountsArg is the accounts of a tool that works on several: one id or a list
// of ids, all accounts by default. It is an `any` for the string or the list,
// which schemaFor says in the schema.
type AccountsArg struct {
	Account any `json:"account,omitempty" jsonschema:"account id, or a list of account ids; all accounts by default"`
}

var (
	errNoAccounts = errors.New("no accounts linked yet: call manage-accounts action=add account_id=<nickname>")
	errBadAccount = errors.New("account must be an account id or a list of account ids")

	// errReadFailed is all that an agent is told of a read that failed: the words of
	// the database and of the files are not for it (the daemon log has them).
	errReadFailed = errors.New("the archive could not be read: repeat the call, and if it fails again stop and tell the user (the daemon log has the details)")
)

// timeExample is what a time looks like, for the error that asks for one.
const timeExample = "2026-10-07T18:30:00+03:00"

// failed is the answer to an error of the archive or the store: the log gets
// the error, the agent a reason that holds nothing of it.
func (d Deps) failed(err error) error {
	if d.Log != nil {
		d.Log.Warn("a read tool failed", "err", err)
	}
	return errReadFailed
}

// roster is the accounts that exist, by nick.
type roster []wa.AccountInfo

func (d Deps) roster(ctx context.Context) roster { return d.WA.Roster(ctx) }

func (r roster) find(id string) (wa.AccountInfo, bool) {
	i := slices.IndexFunc(r, func(a wa.AccountInfo) bool { return a.Nick == id })
	if i < 0 {
		return wa.AccountInfo{}, false
	}
	return r[i], true
}

func (r roster) ids() string { return strings.Join(nicks(r), ", ") }

func nicks(accs []wa.AccountInfo) []string {
	out := make([]string, len(accs))
	for i, a := range accs {
		out[i] = a.Nick
	}
	return out
}

// unknown is the answer to an account that does not exist. The id given is told
// back only if it is one in form: anything else is not an id, and the agent may
// have it from the content of a message.
func (r roster) unknown(id string) error {
	if len(r) == 0 {
		return errNoAccounts
	}
	if wa.ValidNick(id) {
		return fmt.Errorf("no account %q: the accounts are %s", id, r.ids())
	}
	return fmt.Errorf("not an account id: the accounts are %s", r.ids())
}

// resolveAccount is the account that a tool works on when it works on one: the
// one given, which must exist; else the only one there is; else the only one that
// has the chat (the zero chat is none). Otherwise the agent is asked to choose.
func (d Deps) resolveAccount(ctx context.Context, r roster, arg string, chat wa.Chat) (wa.AccountInfo, error) {
	switch {
	case arg != "":
		a, ok := r.find(arg)
		if !ok {
			return wa.AccountInfo{}, r.unknown(arg)
		}
		return a, nil
	case len(r) == 0:
		return wa.AccountInfo{}, errNoAccounts
	case len(r) == 1:
		return r[0], nil
	case chat.JID != "":
		return d.accountOfChat(ctx, r, chat)
	}
	return wa.AccountInfo{}, fmt.Errorf("several accounts are linked, so pass account: %s", r.ids())
}

func (d Deps) accountOfChat(ctx context.Context, r roster, chat wa.Chat) (wa.AccountInfo, error) {
	var have []wa.AccountInfo // the archive may still hold an account that was forgotten
	for _, key := range chatKeys(chat) {
		found, err := d.Archive.AccountsForChat(ctx, key)
		if err != nil {
			return wa.AccountInfo{}, d.failed(err)
		}
		for _, n := range found {
			if a, ok := r.find(n); ok && !slices.ContainsFunc(have, func(h wa.AccountInfo) bool { return h.Nick == n }) {
				have = append(have, a)
			}
		}
	}
	switch len(have) {
	case 0:
		return wa.AccountInfo{}, fmt.Errorf("no account has this chat in its archive: find it with list-chats, or pass account (%s)", r.ids())
	case 1:
		return have[0], nil
	}
	return wa.AccountInfo{}, fmt.Errorf("the chat is in several accounts, so pass account: %s", roster(have).ids())
}

// chatKeys are the JIDs that an account's archive may have the chat under: the
// canonical one and, when the store knows the LID of a number, the number itself.
// A chat is filed under its LID when a message of it comes, or when the archive is
// next brought in line with the store (at a connect), so for a while after the store
// has learned a pair, what was written before stays under the number, in the
// accounts that the pair did not come through.
func chatKeys(c wa.Chat) []string {
	if c.PN == "" {
		return []string{c.JID}
	}
	return []string{c.JID, c.PN}
}

// chatKnown tells whether any of the accounts has the chat in its archive, under
// any of its JIDs. A failed read says it does (an empty result then is the failure's
// to explain, not the chat's).
func (d Deps) chatKnown(ctx context.Context, accounts []string, c wa.Chat) bool {
	for _, key := range chatKeys(c) {
		have, err := d.Archive.AccountsForChat(ctx, key)
		if err != nil || slices.ContainsFunc(have, func(a string) bool { return slices.Contains(accounts, a) }) {
			return true
		}
	}
	return false
}

// messagesOf is the page of q read under each JID that the chat may be filed under
// (chatKeys), and put together. An account whose archive has not caught up with the
// store may have the chat in two rows, a number's and a LID's, and a page of one of
// them would be a part of the chat that says nothing of the other. The ids of the
// messages are of one table, so (ts, id) orders those of both rows.
func (d Deps) messagesOf(ctx context.Context, q archive.MsgQuery, keys []string) (archive.Page, error) {
	if len(keys) == 1 {
		q.Chat = keys[0]
		return d.Archive.Messages(ctx, q)
	}
	var all []archive.Message
	more := false
	for _, key := range keys {
		q.Chat = key
		p, err := d.Archive.Messages(ctx, q)
		if err != nil {
			return archive.Page{}, err
		}
		all = append(all, p.Messages...)
		more = more || !p.Next.IsZero()
	}
	slices.SortStableFunc(all, func(a, b archive.Message) int { return compareCursors(a.Cursor(), b.Cursor()) })
	if over := len(all) - q.Limit; over > 0 {
		all, more = all[over:], true // the oldest are for the page before
	}
	var next archive.Cursor
	if more && len(all) > 0 {
		next = all[0].Cursor()
	}
	return archive.Page{Messages: all, Next: next}, nil
}

func compareCursors(a, b archive.Cursor) int {
	return cmp.Or(cmp.Compare(a.TS, b.TS), cmp.Compare(a.ID, b.ID))
}

// aroundIn is the window around a message that is in the chat, whichever of its
// JIDs it is filed under.
func (d Deps) aroundIn(ctx context.Context, account string, keys []string, id string, before, after int) ([]archive.Message, error) {
	for _, key := range keys {
		window, err := d.Archive.Around(ctx, account, key, id, before, after)
		if !errors.Is(err, archive.ErrNoMessage) {
			return window, err
		}
	}
	return nil, archive.ErrNoMessage
}

// selected is the accounts an AccountsArg names: all of them if it is empty.
func (r roster) selected(arg any) ([]wa.AccountInfo, error) {
	ids, err := accountIDs(arg)
	if err != nil {
		return nil, err
	}
	if len(r) == 0 {
		return nil, errNoAccounts
	}
	if len(ids) == 0 {
		return r, nil
	}
	var out []wa.AccountInfo
	for _, id := range ids {
		a, ok := r.find(id)
		if !ok {
			return nil, r.unknown(id)
		}
		if !slices.ContainsFunc(out, func(o wa.AccountInfo) bool { return o.Nick == id }) {
			out = append(out, a)
		}
	}
	return out, nil
}

// accountIDs reads what a JSON "account" decoded into: nothing, an id, or a list
// of them (the schema has refused anything else already).
func accountIDs(arg any) ([]string, error) {
	switch v := arg.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []string{v}, nil
	case []any:
		ids := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, errBadAccount
			}
			ids = append(ids, s)
		}
		return ids, nil
	}
	return nil, errBadAccount
}

// canonicalChat is the identity the archive files the chat under, from a JID or
// a phone number.
func (d Deps) canonicalChat(ctx context.Context, chat string) (wa.Chat, error) {
	// A chat that is neither is the agent's to put right, and what it says holds
	// nothing of what was given; the LID store failing is not.
	if _, err := wa.ParseChat(chat); err != nil {
		return wa.Chat{}, fmt.Errorf("%v; list-chats gives the chat id of a name", err)
	}
	c, err := d.WA.CanonicalChat(ctx, chat)
	if err != nil {
		return wa.Chat{}, d.failed(err)
	}
	return c, nil
}

var cursorRe = regexp.MustCompile(`^[0-9]+_[0-9]+$`)

var errBadBefore = fmt.Errorf("before must be the next_before of a previous page, or a time in ISO 8601 with an offset, e.g. %s; a relative time such as \"now\" is not understood: compute the time", timeExample)

// parseBefore reads the `before` of get-messages: a page's next_before, which
// is passed back as it was, or a time.
func parseBefore(s string) (archive.Cursor, error) {
	if cursorRe.MatchString(s) {
		if c, err := archive.ParseCursor(s); err == nil {
			return c, nil
		}
	} else if t, err := time.Parse(time.RFC3339, s); err == nil {
		return beforeTime(wholeSecond(t)), nil
	}
	return archive.Cursor{}, errBadBefore
}

// wholeSecond is t rounded up to a second. The archive keeps the seconds of its
// messages, and a message of the second 03 is older than 03.5, and not from 02.5 on
// when it is of 02: both limits are read at the second above a time with a fraction.
func wholeSecond(t time.Time) time.Time {
	if t.Nanosecond() == 0 {
		return t
	}
	return t.Truncate(time.Second).Add(time.Second)
}

// beforeTime is the position of the start of t: what is before it is what is
// older. The zero Cursor is no position, and would be the newest page, so the
// epoch itself, before which there is nothing, is put a second earlier.
func beforeTime(t time.Time) archive.Cursor {
	c := archive.Cursor{TS: t.Unix()}
	if c.IsZero() {
		c.TS = -1
	}
	return c
}

// parseTime reads a time of a filter. Nothing relative: the agent, which knows
// the date, computes it.
func parseTime(field, s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a time in ISO 8601 with an offset, e.g. %s; a relative time such as \"now\" or \"-1h\" is not understood: compute the time", field, timeExample)
	}
	return wholeSecond(t), nil
}

// notes are what a result tells the agent besides the data: in our own words.
type notes []string

func (n *notes) add(format string, args ...any) { *n = append(*n, fmt.Sprintf(format, args...)) }

// limitOf is the size of a page asked for: the default if none, and the most
// there is if more.
func limitOf(asked, def, most int, n *notes) (int, error) {
	switch {
	case asked < 0:
		return 0, errors.New("limit must be positive")
	case asked == 0:
		return def, nil
	case asked > most:
		n.add("limit lowered to the most there is, %d", most)
		return most, nil
	}
	return asked, nil
}

// around is how many messages of a context are asked for on one side: the
// default if none, and never fewer than none or more than the most there is.
func around(asked *int, n *notes, side string) int {
	switch {
	case asked == nil:
		return defaultAround
	case *asked < 0:
		return 0
	case *asked > maxAround:
		n.add("%s lowered to the most there is, %d", side, maxAround)
		return maxAround
	}
	return *asked
}

// accountNotes tells of the accounts a result is of that are not connected: what
// is read is the archive as it was when the account was last connected. The
// reading itself is fine, so this is no error.
func accountNotes(accs []wa.AccountInfo, now time.Time) notes {
	var out notes
	for _, a := range accs {
		if a.Status != wa.StatusConnected {
			out.add("account %s is %s: this is the archive up to when it was last connected; %s", a.Nick, a.Status, connectHint(a, now))
		}
	}
	return out
}

// connectHint is what can be done about an account that is not connected, or
// what to expect of it.
func connectHint(a wa.AccountInfo, now time.Time) string {
	switch a.Status {
	case wa.StatusNeedsLink, wa.StatusReplaced, wa.StatusError:
		if step := repairStep(a, now); step != "" {
			return "to receive new messages: " + step
		}
		return "it receives no new messages for now (manage-accounts action=list says why)"
	case wa.StatusClientOutdated:
		return "it cannot connect until whatsapp-mcp is updated"
	}
	return "newer messages come once it is connected (manage-accounts action=list shows its state)"
}
