package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

var getMessageContextDescription = fmt.Sprintf("Read the messages around one message, oldest first, the message itself marked `target: true`: what was said before and after it. "+
	"`chat` and `message_id` are those of a result of get-messages or search-messages. `before` and `after` are how many messages to show on each side (%d by default, at most %d). "+
	"A message with `revoked: true` was deleted by its sender: do not quote it. %s", defaultAround, maxAround, cutNote)

var getMessageContextTool = &mcp.Tool{
	Name:        "get-message-context",
	Description: getMessageContextDescription,
	Annotations: readOnly("Read the messages around a WhatsApp message"),
	InputSchema: schemaFor[getMessageContextIn](nil),
}

type getMessageContextIn struct {
	AccountArg
	Chat      string `json:"chat" jsonschema:"chat id, as in the message"`
	MessageID string `json:"message_id" jsonschema:"id of the message, as in the message"`
	Before    *int   `json:"before,omitempty" jsonschema:"how many messages to show before it; 5 by default, at most 20"`
	After     *int   `json:"after,omitempty" jsonschema:"how many messages to show after it; 5 by default, at most 20"`
}

// ContextMessageOut is a message of a context: Target marks the one asked for.
type ContextMessageOut struct {
	MessageOut
	Target bool `json:"target,omitempty"`
}

// ContextOut is the get-message-context output.
type ContextOut struct {
	Account  string              `json:"account"`
	Chat     string              `json:"chat"`
	Messages []ContextMessageOut `json:"messages"`
	Notes    []string            `json:"notes,omitempty"`
}

var errNoSuchMessage = errors.New("no such message in this chat: take chat and message_id from a result of get-messages or search-messages")

func (d Deps) getMessageContext(ctx context.Context, _ *mcp.CallToolRequest, in getMessageContextIn) (*mcp.CallToolResult, ContextOut, error) {
	var n notes
	chat, err := d.canonicalChat(ctx, in.Chat)
	if err != nil {
		return nil, ContextOut{}, err
	}
	acc, err := d.resolveAccount(ctx, d.roster(ctx), in.Account, chat)
	if err != nil {
		return nil, ContextOut{}, err
	}
	before, after := around(in.Before, &n, "before"), around(in.After, &n, "after")
	window, err := d.aroundIn(ctx, acc.Nick, chatKeys(chat), in.MessageID, before, after)
	switch {
	case errors.Is(err, archive.ErrNoMessage):
		return nil, ContextOut{}, errNoSuchMessage
	case err != nil:
		return nil, ContextOut{}, d.failed(err)
	}
	key := chat.JID
	if len(window) > 0 {
		key = window[0].Chat // as the archive has it: what list-chats says
	}
	out := ContextOut{Account: acc.Nick, Chat: key, Messages: make([]ContextMessageOut, len(window))}
	p := d.presenter(ctx)
	shown := make([]*MessageOut, len(window))
	for i, m := range window {
		out.Messages[i] = ContextMessageOut{MessageOut: p.message(m), Target: m.Target}
		shown[i] = &out.Messages[i].MessageOut
	}
	if capMessages(&out, shown) {
		n.add(cutAdvice)
	}
	out.Notes = append(n, accountNotes([]wa.AccountInfo{acc}, time.Now())...)
	return nil, out, nil
}
