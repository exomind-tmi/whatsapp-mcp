package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

var searchMessagesDescription = fmt.Sprintf("Search the text of the messages, newest first: what was written, the caption of a media, and the file name of a document sent without a caption (other file names are not searched). "+
	"`query` is words that a message must all contain, in any order; a part of a word is enough. A word needs 3 or more characters: shorter ones are ignored, and a query with none is refused. "+
	"Covers all accounts unless `account` is given (an account id or a list of them). `chat` (a chat id or a phone number), `sender` (a phone number, a JID or part of a name) and `after` and `before` (times in ISO 8601 with an offset, e.g. %s, never \"now\" or a relative time) narrow it. "+
	"`limit` is %d by default, at most %d. "+
	"Each result has `account`, `chat`, `chat_name` and `id`: to read around a result pass its `account`, `chat` and `id` (as `message_id`) to get-message-context. "+
	"A result with `revoked: true` was deleted by its sender: do not quote it. %s "+
	"An account that is not connected is searched in its archive, and `notes` says so.", timeExample, defaultHits, maxHits, cutNote)

var searchMessagesTool = &mcp.Tool{
	Name:        "search-messages",
	Description: searchMessagesDescription,
	Annotations: readOnly("Search WhatsApp messages"),
	InputSchema: schemaFor[searchMessagesIn](nil),
}

type searchMessagesIn struct {
	Query string `json:"query" jsonschema:"words that a message must all contain, in any order; a part of a word is enough; at least one word of 3 or more characters"`
	AccountsArg
	Chat   string `json:"chat,omitempty" jsonschema:"only this chat: a chat id from list-chats, or a phone number"`
	Sender string `json:"sender,omitempty" jsonschema:"only messages of this person: a phone number, a JID, or part of the name they are known by"`
	After  string `json:"after,omitempty" jsonschema:"only messages from this time on: a time in ISO 8601 with an offset"`
	Before string `json:"before,omitempty" jsonschema:"only messages older than this: a time in ISO 8601 with an offset"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many results to return; 20 by default, at most 100"`
}

// HitOut is a result of search-messages: a message with the name of its chat.
type HitOut struct {
	MessageOut
	ChatName string `json:"chat_name,omitempty"`
}

// SearchOut is the search-messages output.
type SearchOut struct {
	Results []HitOut `json:"results"`
	Notes   []string `json:"notes,omitempty"`
}

var (
	errQueryTooShort = errors.New("query too short: it needs at least one word of 3 or more characters")
	errNoSender      = errors.New("sender matches no one this archive knows: give a phone number, or part of a name that list-chats shows")
)

func (d Deps) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in searchMessagesIn) (*mcp.CallToolResult, SearchOut, error) {
	var n notes
	accs, err := d.roster(ctx).selected(in.Account)
	if err != nil {
		return nil, SearchOut{}, err
	}
	q := archive.SearchQuery{Accounts: nicks(accs), Query: in.Query}
	if q.Limit, err = limitOf(in.Limit, defaultHits, maxHits, &n); err != nil {
		return nil, SearchOut{}, err
	}
	chat, err := d.filters(ctx, &q, in, accs)
	if err != nil {
		return nil, SearchOut{}, err
	}
	limit := q.Limit
	q.Limit++ // one more than asked for tells whether there are more
	hits, err := d.searchAll(ctx, q, chat)
	switch {
	case errors.Is(err, archive.ErrQueryTooShort):
		return nil, SearchOut{}, errQueryTooShort
	case err != nil:
		return nil, SearchOut{}, d.failed(err)
	}
	if len(hits) > limit {
		hits = hits[:limit]
		n.add("%s", moreAdvice("matching messages", limit, maxHits, "chat, sender, after or before"))
	}
	if len(hits) == 0 && chat.JID != "" && !d.chatKnown(ctx, q.Accounts, chat) {
		n.add("none of the accounts searched has this chat in its archive: find it with list-chats by name, or give its phone number with the country code")
	}
	out := SearchOut{Results: d.presentHits(ctx, hits)}
	shown, names := make([]*MessageOut, len(out.Results)), make([]field, len(out.Results))
	for i := range out.Results {
		shown[i], names[i] = &out.Results[i].MessageOut, field{&out.Results[i].ChatName, maxName, nil}
	}
	if capMessages(&out, shown, names...) {
		n.add(cutAdvice)
	}
	out.Notes = append(n, accountNotes(accs, time.Now())...)
	return nil, out, nil
}

// filters reads the sender and the times of the search into q, and returns the
// chat, which searchAll puts in the queries (the zero chat is none).
func (d Deps) filters(ctx context.Context, q *archive.SearchQuery, in searchMessagesIn, accs []wa.AccountInfo) (chat wa.Chat, err error) {
	if in.Chat != "" {
		if chat, err = d.canonicalChat(ctx, in.Chat); err != nil {
			return wa.Chat{}, err
		}
	}
	if in.Sender != "" {
		if q.Senders, err = d.senders(ctx, accs, in.Sender); err != nil {
			return wa.Chat{}, err
		}
	}
	if in.After != "" {
		if q.After, err = parseTime("after", in.After); err != nil {
			return wa.Chat{}, err
		}
	}
	if in.Before != "" {
		if q.Before, err = parseTime("before", in.Before); err != nil {
			return wa.Chat{}, err
		}
	}
	return chat, nil
}

// searchAll is the search of q in the chat, if there is one. The archive takes one
// JID of a chat, and an account may have the chat under either of its two (see
// chatKeys) until it has caught up with the store, so there is a search for each, and
// their results are merged into the newest limit. A JID that an account does not
// have the chat under finds nothing there.
func (d Deps) searchAll(ctx context.Context, q archive.SearchQuery, chat wa.Chat) ([]archive.Hit, error) {
	if chat.JID == "" {
		return d.Archive.Search(ctx, q)
	}
	keys := chatKeys(chat)
	var all []archive.Hit
	for _, key := range keys {
		part := q
		part.Chat = key
		hits, err := d.Archive.Search(ctx, part)
		if err != nil {
			return nil, err
		}
		all = append(all, hits...)
	}
	if len(keys) > 1 {
		slices.SortStableFunc(all, func(a, b archive.Hit) int { return -compareCursors(a.Cursor(), b.Cursor()) })
		all = all[:min(q.Limit, len(all))]
	}
	return all, nil
}

// senders is the addresses of the people that sender names, in any of the
// accounts searched: a person has the same JIDs whichever account talks to them.
func (d Deps) senders(ctx context.Context, accs []wa.AccountInfo, sender string) ([]string, error) {
	var all []string
	for _, a := range accs {
		jids, err := d.WA.SenderJIDs(ctx, a.Nick, sender)
		switch {
		case errors.Is(err, wa.ErrNotAPerson), errors.Is(err, wa.ErrTooManyPeople):
			return nil, err // for the agent: what it says holds nothing of the sender
		case err != nil:
			return nil, d.failed(err)
		}
		all = append(all, jids...)
	}
	slices.Sort(all)
	all = slices.Compact(all)
	if len(all) == 0 {
		return nil, errNoSender
	}
	return all, nil
}

func (d Deps) presentHits(ctx context.Context, hits []archive.Hit) []HitOut {
	p := d.presenter(ctx)
	out := make([]HitOut, len(hits))
	for i, h := range hits {
		m, chatName := p.message(h.Message), p.chatName(h.Account, h.Chat, h.ChatName)
		// The other party of a private chat is who said what is not ours, and the
		// chat is called by their name when nothing better is known.
		if m.SenderName == "" && !m.FromMe && !isGroup(m.Chat) {
			m.SenderName = chatName
		}
		out[i] = HitOut{MessageOut: m, ChatName: chatName}
	}
	return out
}
