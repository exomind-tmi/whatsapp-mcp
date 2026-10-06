package wa

import (
	"math"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// OpKind is what a received message does to the archive.
type OpKind int

const (
	OpSkip         OpKind = iota // nothing to store; the zero Op
	OpUpsert                     // a message: text, or media with its caption
	OpEdit                       // the sender changed the text of an earlier message
	OpRevoke                     // the sender deleted an earlier message for everybody
	OpHistoryNotif               // the phone announces a history sync blob to download
)

func (k OpKind) String() string {
	switch k {
	case OpSkip:
		return "skip"
	case OpUpsert:
		return "upsert"
	case OpEdit:
		return "edit"
	case OpRevoke:
		return "revoke"
	case OpHistoryNotif:
		return "history-notification"
	}
	return "unknown"
}

// Op is what Classify made of a received message. Of Row, Edit and Revoke only
// the one that Kind names is set, and not the account or the chat of it: those
// are the caller's to give (In), once it has the canonical chat.
type Op struct {
	Kind OpKind

	// Chat is the chat the operation belongs to as it was received, which is not
	// the archive's key yet (CanonicalChat). It is empty for OpSkip and
	// OpHistoryNotif.
	Chat types.JID
	// ChatName is what the sender calls themselves, when the chat is a private
	// one with them: the name that chat is shown under until a better one is
	// known. Empty for a group, for a message of our own and for no name.
	ChatName string

	Row    archive.Row
	Edit   archive.Edit
	Revoke archive.Revoke

	// Notif is the history sync announcement of OpHistoryNotif.
	Notif *waE2E.HistorySyncNotification
}

// In sets whose and where the operation is: the account's nick and the
// canonical JID of the chat.
func (o Op) In(account, chat string) Op {
	switch o.Kind {
	case OpUpsert:
		o.Row.Account, o.Row.Chat = account, chat
	case OpEdit:
		o.Edit.Account, o.Edit.Chat = account, chat
	case OpRevoke:
		o.Revoke.Account, o.Revoke.Chat = account, chat
	}
	return o
}

// NoLaterThan makes the time of the operation not later than now. A clock that
// runs ahead, or a message with a made-up date, would otherwise pin its chat at
// the top of the list for good: the chat's last message time only grows.
func (o Op) NoLaterThan(now time.Time) Op {
	switch o.Kind {
	case OpUpsert:
		o.Row.TS = capTime(o.Row.TS, now)
	case OpEdit:
		o.Edit.EditedAt = capTime(o.Edit.EditedAt, now)
	case OpRevoke:
		o.Revoke.RevokedAt = capTime(o.Revoke.RevokedAt, now)
	}
	return o
}

// capTime is t, or now when t is later, and also when t is not a time at all:
// the archive refuses a message with none, and a refusal would keep the message
// from ever being acknowledged.
func capTime(t, now time.Time) time.Time {
	if t.IsZero() || t.After(now) {
		return now
	}
	return t
}

// Classify says what a received message means for the archive. msg is the
// message as it was sent, wrappers on (events.Message.RawMessage): whatsmeow's
// own UnwrapRaw takes off the wrappers it knows (device-sent, ephemeral,
// view-once, document with caption, edited, ...) and keeps of each only a flag
// on the event, so what a wrapper says about its content (that it is view-once)
// is lost with it. Classify takes them all off itself, in whatever order and
// however deep they come, and reads what they say. A message that has been
// unwrapped already is read as well, without that. It reads only its arguments,
// and nothing of msg is changed.
//
// What the archive keeps is what people write: text, and media with its caption,
// and the words of what a business sends and of the buttons a person presses in
// answer. Reactions, polls, locations, contact cards, keys and the other notices
// of the protocol are skipped, as are the messages of places that are not
// conversations: statuses, channels (newsletters), Meta AI and the messages of
// the devices to each other (category "peer"), apart from the announcement of a
// history sync.
func Classify(info types.MessageInfo, msg *waE2E.Message) Op {
	m, viewOnce := unwrap(msg)
	if m == nil {
		return Op{}
	}
	if n := m.GetProtocolMessage().GetHistorySyncNotification(); n != nil {
		// Only our own phone may send one: it names the blob to be downloaded and
		// the key to read it with, and whatever it holds goes into the archive as
		// what the contacts said (whatsmeow ignores the others, message.go:888).
		if info.IsFromMe {
			return Op{Kind: OpHistoryNotif, Notif: n}
		}
		return Op{}
	}
	chat, ok := receivedChat(info)
	if !ok || info.ID == "" || info.Category == "peer" {
		return Op{}
	}
	sender := info.Sender.ToNonAD()
	if sender.IsEmpty() && !info.IsGroup {
		sender = chat // a private chat has no one else to be the sender
	}
	if sender.IsEmpty() {
		return Op{}
	}
	op := Op{Chat: chat, ChatName: contactName(info, chat)}

	switch p := m.GetProtocolMessage(); {
	case p == nil:
		return upsertOp(op, info, sender, m, viewOnce)
	case p.Type != nil && p.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT:
		return editOp(op, info, sender, p)
	// Type is optional and REVOKE is the zero value, so a protocol message that
	// says nothing about its type would read as a revoke through GetType.
	case p.Type != nil && p.GetType() == waE2E.ProtocolMessage_REVOKE:
		return revokeOp(op, info, sender, p)
	}
	return Op{}
}

// receivedChat is the chat a message of info belongs to, or false for the
// places the archive does not keep: a status, a message to a broadcast list of
// our own, a channel, Meta AI and whatever else has no private or group chat to
// file the message in (a chat CanonicalChat would refuse). A message that came
// by a broadcast list shows up in the chat with its sender, as it does on the
// phone.
func receivedChat(info types.MessageInfo) (types.JID, bool) {
	chat := info.Chat
	// Not types.MessageSource.IsIncomingBroadcast: it holds for a message of ours as
	// well when that carries the owner of the list (the "recipient" of the stanza,
	// message.go:114), and would file our own message to a list in the chat with
	// ourselves.
	if !info.IsFromMe && info.Chat.IsBroadcastList() {
		chat = info.Sender.ToNonAD()
	}
	switch {
	case info.Sender.IsBot() || chat.IsBot():
		return types.EmptyJID, false
	case chat.User == "" || !archivableServer(chat.Server):
		return types.EmptyJID, false
	}
	return chat, true
}

// contactName is the push name to show a private chat under: the one on the
// message of the other person. On a message of our own it is ours, which would
// name the chat after ourselves; "-" is what WhatsApp sends for none
// (message.go:59).
func contactName(info types.MessageInfo, chat types.JID) string {
	name := strings.TrimSpace(info.PushName)
	if info.IsFromMe || chat.Server == types.GroupServer || name == "-" {
		return ""
	}
	return name
}

func upsertOp(op Op, info types.MessageInfo, sender types.JID, m *waE2E.Message, viewOnce bool) Op {
	c := contentOf(m)
	if !c.ok {
		return Op{}
	}
	op.Kind = OpUpsert
	op.Row = archive.Row{
		ID: info.ID, Sender: sender.String(), FromMe: info.IsFromMe, TS: info.Timestamp,
		Text: c.text, MediaType: c.mediaType, MediaMime: c.mime, MediaName: c.name, MediaSize: c.size,
		QuotedID: c.quoted,
	}
	if c.mediaType != "" && (viewOnce || c.viewOnce) {
		// A view-once message is meant to be seen once, and not kept: the row says
		// that one came, of what kind, and that is all. Without the raw message
		// there is nothing to download either.
		op.Row.Text, op.Row.MediaName = "", ""
		return op
	}
	// proto.Marshal of a message that was just unmarshaled does not fail.
	op.Row.Raw, _ = proto.Marshal(m)
	return op
}

// editOp is an edit of the message p.Key names. The time of the edit is the
// time of the message that carries it, as for a revoke: that one is the
// server's, and the same sort of clock for all of a message's edits, which the
// latest wins by.
func editOp(op Op, info types.MessageInfo, sender types.JID, p *waE2E.ProtocolMessage) Op {
	id := p.GetKey().GetID()
	edited, _ := unwrap(p.GetEditedMessage())
	c := contentOf(edited)
	if id == "" || !c.ok {
		return Op{}
	}
	op.Kind = OpEdit
	op.Edit = archive.Edit{ID: id, Sender: sender.String(), FromMe: info.IsFromMe, Text: c.text, EditedAt: info.Timestamp}
	return op
}

// revokeOp is the deletion of the message p.Key names. The key is that of the
// message as its deleter has it: for a message of their own, fromMe is set and
// the sender of this one is also the sender of that; for one an admin deletes
// from a group, fromMe is not set and the participant is its author. The author
// matters only for the stub that a message not archived yet gets.
func revokeOp(op Op, info types.MessageInfo, sender types.JID, p *waE2E.ProtocolMessage) Op {
	key := p.GetKey()
	if key.GetID() == "" {
		return Op{}
	}
	author, fromMe := sender, info.IsFromMe
	if part, err := types.ParseJID(key.GetParticipant()); !key.GetFromMe() && err == nil && part.User != "" {
		author, fromMe = part.ToNonAD(), false
	}
	op.Kind = OpRevoke
	op.Revoke = archive.Revoke{ID: key.GetID(), Sender: author.String(), FromMe: fromMe, RevokedAt: info.Timestamp}
	return op
}

// maxWrapping bounds the nesting that unwrap follows, against a message made to
// loop its reader.
const maxWrapping = 8

// wrappers are the messages that hold another one and add nothing the archive
// keeps. Some of them whatsmeow takes off in UnwrapRaw, in a fixed order and
// once each (events/events.go:377-420); a message that has them in another order
// or deeper, and those it does not know (an album's item, a group-mention), would
// be left as a wrapper with no text or media in it, and lost. viewOnce marks
// those that make the content view-once.
var wrappers = []struct {
	inner    func(*waE2E.Message) *waE2E.Message
	viewOnce bool
}{
	{func(m *waE2E.Message) *waE2E.Message { return m.GetDeviceSentMessage().GetMessage() }, false},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetEphemeralMessage().GetMessage() }, false},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetViewOnceMessage().GetMessage() }, true},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetViewOnceMessageV2().GetMessage() }, true},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetViewOnceMessageV2Extension().GetMessage() }, true},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetDocumentWithCaptionMessage().GetMessage() }, false},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetEditedMessage().GetMessage() }, false},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetLottieStickerMessage().GetMessage() }, false},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetBotInvokeMessage().GetMessage() }, false},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetAssociatedChildMessage().GetMessage() }, false},
	{func(m *waE2E.Message) *waE2E.Message { return m.GetGroupMentionedMessage().GetMessage() }, false},
}

// unwrap is the content of m under its wrappers, and whether one of them made
// it view-once. A message with none is its own content.
func unwrap(m *waE2E.Message) (inner *waE2E.Message, viewOnce bool) {
	for range maxWrapping {
		next := (*waE2E.Message)(nil)
		for _, w := range wrappers {
			if next = w.inner(m); next != nil {
				viewOnce = viewOnce || w.viewOnce
				break
			}
		}
		if next == nil {
			break
		}
		m = next
	}
	return m, viewOnce
}

// content is what the archive keeps of a message's body.
type content struct {
	ok        bool // there is something to keep
	text      string
	mediaType string // image|video|audio|ptt|document|sticker; "" for a text
	mime      string
	name      string
	size      int64
	quoted    string // the id of the message this one answers
	viewOnce  bool   // the media's own flag
}

// contentOf reads the text or the media of m, which is not a protocol message.
// The first that the message has wins: a real message has one.
func contentOf(m *waE2E.Message) content {
	switch {
	case m.GetConversation() != "":
		return content{ok: true, text: m.GetConversation()}
	case m.GetExtendedTextMessage().GetText() != "":
		x := m.GetExtendedTextMessage()
		return content{ok: true, text: x.GetText(), quoted: x.GetContextInfo().GetStanzaID()}
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		return media("image", x.GetMimetype(), x.GetCaption(), x.GetFileLength(), x.GetContextInfo(), x.GetViewOnce())
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		return media("video", x.GetMimetype(), x.GetCaption(), x.GetFileLength(), x.GetContextInfo(), x.GetViewOnce())
	case m.GetPtvMessage() != nil: // a round video note
		x := m.GetPtvMessage()
		return media("video", x.GetMimetype(), x.GetCaption(), x.GetFileLength(), x.GetContextInfo(), x.GetViewOnce())
	case m.GetAudioMessage() != nil:
		x := m.GetAudioMessage()
		kind := "audio"
		if x.GetPTT() {
			kind = "ptt"
		}
		return media(kind, x.GetMimetype(), "", x.GetFileLength(), x.GetContextInfo(), x.GetViewOnce())
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		c := media("document", x.GetMimetype(), x.GetCaption(), x.GetFileLength(), x.GetContextInfo(), false)
		c.name = x.GetFileName()
		if c.name == "" {
			c.name = x.GetTitle()
		}
		if c.text == "" {
			c.text = c.name // what the file is called is the closest thing to a text it has
		}
		return c
	case m.GetStickerMessage() != nil:
		x := m.GetStickerMessage()
		return media("sticker", x.GetMimetype(), "", x.GetFileLength(), x.GetContextInfo(), false)
	}
	if t := businessText(m); t != "" {
		return content{ok: true, text: t}
	}
	return content{}
}

// businessText is what a message of a business says in words, and what a person
// answers it by a button: the code of a bank, the notice of a delivery, a choice
// made in a bot. Only the words are kept, the buttons are not, and they come out
// as a text of the chat. The first of them that a message has wins, as in
// contentOf.
func businessText(m *waE2E.Message) string {
	list := m.GetListMessage()
	for _, s := range []string{
		m.GetInteractiveMessage().GetBody().GetText(),
		m.GetTemplateMessage().GetHydratedTemplate().GetHydratedContentText(),
		m.GetButtonsMessage().GetContentText(),
		list.GetDescription(),
		list.GetTitle(),
		m.GetButtonsResponseMessage().GetSelectedDisplayText(),
		m.GetListResponseMessage().GetTitle(),
		m.GetTemplateButtonReplyMessage().GetSelectedDisplayText(),
		m.GetInteractiveResponseMessage().GetBody().GetText(),
	} {
		if s != "" {
			return s
		}
	}
	return ""
}

func media(kind, mime, caption string, size uint64, ci *waE2E.ContextInfo, viewOnce bool) content {
	return content{
		ok: true, text: caption, mediaType: kind, mime: mime, size: clampSize(size),
		quoted: ci.GetStanzaID(), viewOnce: viewOnce,
	}
}

// clampSize is a file length as the archive's signed integer: one that does not
// fit, which only a made-up message has, would turn negative.
func clampSize(n uint64) int64 { return int64(min(n, math.MaxInt64)) }
