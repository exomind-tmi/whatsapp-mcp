package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// MessageOut is a message as the read tools show it. Account, Chat and ID are
// enough to ask for it again (get-message-context). Whatever came from other
// people (the text, the names, the file name) is in the fields of data and
// nowhere else. There is no raw message and no path of a downloaded file: a
// read shows what was said, not how WhatsApp carried it nor where this computer
// keeps it.
type MessageOut struct {
	Account    string    `json:"account"`
	Chat       string    `json:"chat"`
	ID         string    `json:"id"`
	Sender     string    `json:"sender"`
	SenderName string    `json:"sender_name,omitempty"`
	FromMe     bool      `json:"from_me"`
	At         string    `json:"at"`
	Text       *string   `json:"text"` // null for a media message without a caption
	Truncated  bool      `json:"truncated,omitempty"`
	Media      *MediaOut `json:"media,omitempty"`
	QuotedID   string    `json:"quoted_id,omitempty"`
	EditedAt   string    `json:"edited_at,omitempty"`
	Revoked    bool      `json:"revoked,omitempty"` // the sender deleted it for everyone
	RevokedAt  string    `json:"revoked_at,omitempty"`
}

// MediaOut describes the attachment of a message.
type MediaOut struct {
	Type string `json:"type"`
	Mime string `json:"mime,omitempty"`
	Name string `json:"name,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// presenter makes MessageOuts. It asks for the name of a sender once, and only
// for the accounts that have a message to show.
type presenter struct {
	d     Deps
	ctx   context.Context
	names map[string]wa.Namer  // by account
	named map[[2]string]string // by account and sender
}

func (d Deps) presenter(ctx context.Context) *presenter {
	return &presenter{d: d, ctx: ctx, names: map[string]wa.Namer{}, named: map[[2]string]string{}}
}

func (p *presenter) name(account, sender string) string {
	key := [2]string{account, sender}
	if n, ok := p.named[key]; ok {
		return n
	}
	namer, ok := p.names[account]
	if !ok {
		namer = p.d.WA.Names(p.ctx, account)
		p.names[account] = namer
	}
	n := plain(namer(sender))
	p.named[key] = n
	return n
}

// chatName is what a private chat is called: the name its person has in the
// account's contacts, which is the one WhatsApp shows first, or else the name the
// archive has of the chat. A group has only the archive's.
func (p *presenter) chatName(account, chat, archived string) string {
	if isGroup(chat) {
		return plain(archived)
	}
	if n := p.name(account, chat); n != "" {
		return n
	}
	return plain(archived)
}

func (p *presenter) message(m archive.Message) MessageOut {
	out := MessageOut{
		Account:   m.Account,
		Chat:      m.Chat,
		ID:        m.ID,
		Sender:    m.Sender,
		FromMe:    m.FromMe,
		At:        isoTime(m.TS),
		QuotedID:  plain(m.QuotedID),
		EditedAt:  isoTime(m.EditedAt),
		Revoked:   !m.RevokedAt.IsZero(),
		RevokedAt: isoTime(m.RevokedAt),
	}
	if !m.FromMe { // our own name is the user's, and from_me says it
		out.SenderName = p.name(m.Account, m.Sender)
	}
	if m.Text != "" {
		text := plain(m.Text)
		out.Text = &text
	}
	if m.MediaType != "" {
		out.Media = &MediaOut{Type: m.MediaType, Mime: plain(m.MediaMime), Name: plain(m.MediaName), Size: m.MediaSize}
	}
	return out
}

func (p *presenter) messages(ms []archive.Message) []MessageOut {
	out := make([]MessageOut, len(ms))
	for i, m := range ms {
		out[i] = p.message(m)
	}
	return out
}

// plain is s without the tag characters (U+E0000-U+E007F). They spell ASCII, and show
// as nothing: a message may talk to a model in them behind the back of the user, who
// reads the same message in WhatsApp and sees none of it. What other people wrote is
// data for the agent, and it is to be the data that the user can see. (The flags of
// England, Scotland and Wales are made of them, and show as a black flag.)
func plain(s string) string {
	if !strings.ContainsFunc(s, hidden) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if hidden(r) {
			return -1
		}
		return r
	}, s)
}

func hidden(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

// field is a string of a result that other people wrote, a text or a name, with
// the most it may have on its own.
type field struct {
	s    *string
	most int
	cut  *bool // set when s is cut; nil for a name, which is cut without a mark
}

// fields are the strings of the message that the limits of a result cover.
func (m *MessageOut) fields() []field {
	fs := []field{{&m.SenderName, maxName, nil}, {&m.QuotedID, maxName, nil}}
	if m.Text != nil {
		fs = append(fs, field{m.Text, maxText, &m.Truncated})
	}
	if m.Media != nil {
		fs = append(fs, field{&m.Media.Name, maxName, nil}, field{&m.Media.Mime, maxName, nil})
	}
	return fs
}

// capMessages limits the strings of the messages of one result, and those of
// more (see capResult); out is the result, which holds them.
func capMessages(out any, msgs []*MessageOut, more ...field) (cutShort bool) {
	var fs []field
	for _, m := range msgs {
		fs = append(fs, m.fields()...)
	}
	return capResult(out, append(fs, more...))
}

// capResult cuts the strings fs of one result, out, to what the limits allow: none
// longer than its own most, and the result, as the JSON that a tool answers with,
// not more than maxResultBytes (its notes, which come after, are not in the
// measure). The size is measured, not estimated: a character may be one byte or six
// (<, >, & and the control characters are escaped), and the mark of a cut text is in
// it. When the result is too big the long strings are cut to one common length, the
// longest that fits, so that the short ones, which are most of them, stay whole and
// no message is left out; a text that was cut is marked truncated. They are cut at a
// character, not in the middle of one. The answer says whether the common length is
// below the limit of a text, that is whether something was cut that the limits of a
// field alone would have let be.
func capResult(out any, fs []field) (cutShort bool) {
	orig := make([]string, len(fs))
	for i, f := range fs {
		orig[i] = *f.s
	}
	changed := false
	apply := func(limit int) {
		changed = false
		for i, f := range fs {
			*f.s = cutRunes(orig[i], min(limit, f.most))
			cut := len(*f.s) < len(orig[i])
			changed = changed || cut
			if f.cut != nil {
				*f.cut = cut
			}
		}
	}
	limit := largestFit(maxText, func(limit int) bool {
		apply(limit)
		b, err := json.Marshal(out)
		return err == nil && len(b) <= maxResultBytes
	})
	apply(limit)
	return changed && limit < maxText
}

// largestFit is the largest n in 0..most for which fits(n) is true, by bisection,
// or 0 if there is none: fits is true up to some n and false after it.
func largestFit(most int, fits func(n int) bool) int {
	lo, hi := 0, most
	for lo < hi {
		if mid := (lo + hi + 1) / 2; fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func cutRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}
