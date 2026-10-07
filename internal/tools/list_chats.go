package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

var listChatsDescription = fmt.Sprintf("List the chats in the archive, the most recently active first. Each has `account`, `chat` (the chat id: pass it to get-messages, get-message-context and search-messages), `name`, `phone` (when known), `is_group` and `last_message_at`. "+
	"Start here to find a chat: `query` finds chats by part of the name or of the phone number. "+
	"Covers all accounts unless `account` is given (an account id or a list of them). `limit` is %d by default, at most %d.", defaultChats, maxChats)

var listChatsTool = &mcp.Tool{
	Name:        "list-chats",
	Description: listChatsDescription,
	Annotations: readOnly("List WhatsApp chats"),
	InputSchema: schemaFor[listChatsIn](nil),
}

// readOnly is the annotations of a tool that only reads the local archive: the
// shim may repeat it after a broken connection, and it reaches nothing outside.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false), Title: title}
}

type listChatsIn struct {
	AccountsArg
	Query string `json:"query,omitempty" jsonschema:"keep the chats whose name or phone number contains this; case does not matter"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many chats to return; 50 by default, at most 200"`
}

// ChatOut is a chat of list-chats. Chat is the id the other tools take.
type ChatOut struct {
	Account       string `json:"account"`
	Chat          string `json:"chat"`
	Name          string `json:"name,omitempty"`
	Phone         string `json:"phone,omitempty"`
	IsGroup       bool   `json:"is_group"`
	LastMessageAt string `json:"last_message_at,omitempty"` // none for a chat without a message
}

// ChatsOut is the list-chats output.
type ChatsOut struct {
	Chats []ChatOut `json:"chats"`
	Notes []string  `json:"notes,omitempty"`
}

func (d Deps) listChats(ctx context.Context, _ *mcp.CallToolRequest, in listChatsIn) (*mcp.CallToolResult, ChatsOut, error) {
	var n notes
	accs, err := d.roster(ctx).selected(in.Account)
	if err != nil {
		return nil, ChatsOut{}, err
	}
	limit, err := limitOf(in.Limit, defaultChats, maxChats, &n)
	if err != nil {
		return nil, ChatsOut{}, err
	}
	chats, err := d.chatsMatching(ctx, accs, in.Query, limit, &n)
	if err != nil {
		return nil, ChatsOut{}, err
	}
	p := d.presenter(ctx)
	out := ChatsOut{Chats: make([]ChatOut, len(chats))}
	for i, c := range chats {
		out.Chats[i] = ChatOut{
			Account:       c.Account,
			Chat:          c.JID,
			Name:          p.chatName(c.Account, c.JID, c.Name),
			Phone:         chatPhone(c),
			IsGroup:       c.IsGroup,
			LastMessageAt: isoTime(c.LastMessageTS),
		}
	}
	if capNames(&out) {
		n.add(namesCutAdvice)
	}
	out.Notes = append(n, accountNotes(accs, time.Now())...)
	return nil, out, nil
}

// capNames limits the names of the chats of one result (see capResult).
func capNames(out *ChatsOut) bool {
	names := make([]field, len(out.Chats))
	for i := range out.Chats {
		names[i] = field{&out.Chats[i].Name, maxName, nil}
	}
	return capResult(out, names)
}

// address is a JID of an account's archive.
type address struct{ account, jid string }

// chatsMatching is the first limit chats, the most recent first, that the query
// finds: those the archive matches itself, by the name it has of the chat and by
// the digits of its number, and those of people whose name in the account's
// contacts has the query in it, which the archive does not know. The chats of the
// people come among the archive's by the time of their last message, so they are
// taken from all the chats; no more than limit of the archive's own can be among
// the first limit of the two together.
func (d Deps) chatsMatching(ctx context.Context, accs []wa.AccountInfo, query string, limit int, n *notes) ([]archive.Chat, error) {
	// One more than limit is asked for: it tells whether there are more, which a list
	// without a next page must not leave the agent to guess.
	matched, err := d.archiveChats(ctx, archive.ChatQuery{Accounts: nicks(accs), Query: query, Limit: limit + 1})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) != "" {
		if matched, err = d.withPeople(ctx, accs, matched, query, n); err != nil {
			return nil, err
		}
	}
	if len(matched) > limit {
		matched = matched[:limit]
		n.add("%s", moreAdvice("chats", limit, maxChats, "query or account"))
	}
	return matched, nil
}

// withPeople is matched with the chats of the people that the query names in the
// contacts of the accounts.
func (d Deps) withPeople(ctx context.Context, accs []wa.AccountInfo, matched []archive.Chat, query string, n *notes) ([]archive.Chat, error) {
	people, tooMany, err := d.peopleNamed(ctx, accs, query)
	if err != nil {
		return nil, err
	}
	if tooMany {
		n.add("the query fits too many people in the contacts to look them up by name: only the names of the chats and the numbers were searched")
	}
	if len(people) > 0 {
		return d.chatsOfPeople(ctx, accs, matched, people)
	}
	return matched, nil
}

func (d Deps) archiveChats(ctx context.Context, q archive.ChatQuery) ([]archive.Chat, error) {
	chats, err := d.Archive.Chats(ctx, q)
	if err != nil {
		return nil, d.failed(err)
	}
	return chats, nil
}

// everyChat is a limit that no archive reaches.
const everyChat = math.MaxInt32

// chatsOfPeople is matched and the chats of people, in the order of the archive
// (the most recent first). A chat is a person's by the address it is filed under
// or by the number beside it: the contacts may know them by either.
func (d Deps) chatsOfPeople(ctx context.Context, accs []wa.AccountInfo, matched []archive.Chat, people map[address]bool) ([]archive.Chat, error) {
	every, err := d.archiveChats(ctx, archive.ChatQuery{Accounts: nicks(accs), Limit: everyChat})
	if err != nil {
		return nil, err
	}
	have := make(map[address]bool, len(matched))
	for _, c := range matched {
		have[address{c.Account, c.JID}] = true
	}
	var out []archive.Chat
	for _, c := range every {
		if have[address{c.Account, c.JID}] || people[address{c.Account, c.JID}] || (c.PN != "" && people[address{c.Account, c.PN}]) {
			out = append(out, c)
		}
	}
	return out, nil
}

// peopleNamed is the addresses of the people that the query names: whoever of an
// account's contacts has it in their name. A query that is not a name of people (the
// JID of a group) adds none, and one that fits too many of them adds none either,
// which tooMany says.
func (d Deps) peopleNamed(ctx context.Context, accs []wa.AccountInfo, query string) (map[address]bool, bool, error) {
	people, tooMany := map[address]bool{}, false
	for _, a := range accs {
		jids, err := d.WA.SenderJIDs(ctx, a.Nick, query)
		switch {
		case errors.Is(err, wa.ErrTooManyPeople):
			tooMany = true
		case errors.Is(err, wa.ErrNotAPerson):
		case err != nil:
			return nil, false, d.failed(err)
		}
		for _, j := range jids {
			people[address{a.Nick, j}] = true
		}
	}
	return people, tooMany, nil
}

// chatPhone is the number of a private chat, as "+7999...": the number the chat
// has beside its LID, or the one it is filed under. A chat that is known by a LID
// alone has none, and a LID is never shown as one.
func chatPhone(c archive.Chat) string {
	jid := c.PN
	if jid == "" && strings.HasSuffix(c.JID, "@s.whatsapp.net") {
		jid = c.JID
	}
	if user, _, _ := strings.Cut(jid, "@"); user != "" {
		return "+" + user
	}
	return ""
}
