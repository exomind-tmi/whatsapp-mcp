package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

var getMessagesDescription = fmt.Sprintf("Read the messages of one chat, oldest first: the newest `limit` (%d by default, at most %d) unless you page back. "+
	"`chat` is a chat id from list-chats, or a phone number such as +7 999 123-45-67. "+
	"To read further back, call again with `before` set to the `next_before` of the result (it is absent when there is nothing older); `before` and `after` also take a time in ISO 8601 with an offset, e.g. %s, never \"now\" or a relative time. "+
	"Each message has `account`, `chat` and `id`, and `sender_name` when the sender's name is known. "+
	"A message with `revoked: true` was deleted by its sender: do not quote it. %s Media is described (`media`), not returned. "+
	"An account that is not connected is read from its archive, and `notes` says so.", defaultMessages, maxMessages, timeExample, cutNote)

var getMessagesTool = &mcp.Tool{
	Name:        "get-messages",
	Description: getMessagesDescription,
	Annotations: readOnly("Read WhatsApp messages"),
	InputSchema: schemaFor[getMessagesIn](nil),
}

type getMessagesIn struct {
	AccountArg
	Chat   string `json:"chat" jsonschema:"chat id from list-chats, or a phone number such as +7 999 123-45-67"`
	Before string `json:"before,omitempty" jsonschema:"read the messages older than this: the next_before of a previous result, or a time in ISO 8601 with an offset"`
	After  string `json:"after,omitempty" jsonschema:"read nothing older than this: a time in ISO 8601 with an offset"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many messages to return; 50 by default, at most 200"`
}

// MessagesOut is the get-messages output. NextBefore is the position of the
// oldest message returned, to be passed back as `before` for the page before.
type MessagesOut struct {
	Account    string       `json:"account"`
	Chat       string       `json:"chat"`
	Messages   []MessageOut `json:"messages"`
	NextBefore string       `json:"next_before,omitempty"`
	Notes      []string     `json:"notes,omitempty"`
}

func (d Deps) getMessages(ctx context.Context, _ *mcp.CallToolRequest, in getMessagesIn) (*mcp.CallToolResult, MessagesOut, error) {
	var n notes
	chat, err := d.canonicalChat(ctx, in.Chat)
	if err != nil {
		return nil, MessagesOut{}, err
	}
	acc, err := d.resolveAccount(ctx, d.roster(ctx), in.Account, chat)
	if err != nil {
		return nil, MessagesOut{}, err
	}
	q := archive.MsgQuery{Account: acc.Nick}
	if q.Limit, err = limitOf(in.Limit, defaultMessages, maxMessages, &n); err != nil {
		return nil, MessagesOut{}, err
	}
	if in.Before != "" {
		if q.Before, err = parseBefore(in.Before); err != nil {
			return nil, MessagesOut{}, err
		}
	}
	if in.After != "" {
		if q.After, err = parseTime("after", in.After); err != nil {
			return nil, MessagesOut{}, err
		}
	}
	page, err := d.messagesOf(ctx, q, chatKeys(chat))
	if err != nil {
		return nil, MessagesOut{}, d.failed(err)
	}
	key := chat.JID
	if last := len(page.Messages) - 1; last >= 0 {
		key = page.Messages[last].Chat // as the archive has it: what list-chats says
	}
	out := MessagesOut{Account: acc.Nick, Chat: key, Messages: d.presenter(ctx).messages(page.Messages), NextBefore: page.Next.String()}
	if capMessages(&out, refs(out.Messages)) {
		n.add(cutAdvice)
	}
	if len(out.Messages) == 0 {
		n.add("no messages: the chat may not be in this account's archive (list-chats finds chats), or none are in the range asked for")
	}
	out.Notes = append(n, accountNotes([]wa.AccountInfo{acc}, time.Now())...)
	return nil, out, nil
}

// refs is pointers to the messages, for capMessages to cut in place.
func refs(ms []MessageOut) []*MessageOut {
	out := make([]*MessageOut, len(ms))
	for i := range ms {
		out[i] = &ms[i]
	}
	return out
}
