package wa

import (
	"math"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// The people of the tests: a contact's number and LID, our own number, a group.
const (
	bobPN    = "70000000100"
	bobLID   = "200000000100"
	carolPN  = "70000000101"
	carolLID = "200000000101"
	ownPN    = "70000000001"
	ownLID   = "200000000001"
	groupID  = "120363000000000001"
)

// t0 is when the messages of the tests were sent: long ago, so that none of
// them is in the future for the clock of a test that has not set one.
var t0 = time.Unix(1_700_000_000, 0)

func pnJID(user string) types.JID  { return types.NewJID(user, types.DefaultUserServer) }
func lidJID(user string) types.JID { return types.NewJID(user, types.HiddenUserServer) }
func groupJID() types.JID          { return types.NewJID(groupID, types.GroupServer) }

// incoming is the info of a private message from who, as whatsmeow parses one.
func incoming(id string, from types.JID) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{Chat: from, Sender: from},
		ID:            id, PushName: "Bob", Timestamp: t0,
	}
}

// outgoing is the info of a message of ours, as another device of ours sent it:
// the chat is the recipient and the sender is our own device, push name ours.
func outgoing(id string, to types.JID) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{Chat: to, Sender: types.NewADJID(ownPN, 0, 12), IsFromMe: true},
		ID:            id, PushName: "Anton", Timestamp: t0,
	}
}

// inGroup is the info of a message in the group from participant who.
func inGroup(id string, from types.JID) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{Chat: groupJID(), Sender: from, IsGroup: true},
		ID:            id, PushName: "Bob", Timestamp: t0,
	}
}

func text(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func quoting(s, id string) *waE2E.Message {
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(s), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String(id)},
	}}
}

func image(caption string) *waE2E.ImageMessage {
	m := &waE2E.ImageMessage{
		Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(1234),
		MediaKey: []byte("key"), DirectPath: proto.String("/v/t62/x"), JPEGThumbnail: []byte("thumb"),
	}
	if caption != "" {
		m.Caption = proto.String(caption)
	}
	return m
}

func document(name, caption string) *waE2E.DocumentMessage {
	m := &waE2E.DocumentMessage{Mimetype: proto.String("application/pdf"), FileLength: proto.Uint64(99), MediaKey: []byte("key")}
	if name != "" {
		m.FileName = proto.String(name)
	}
	if caption != "" {
		m.Caption = proto.String(caption)
	}
	return m
}

func protocol(typ waE2E.ProtocolMessage_Type, id string) *waE2E.ProtocolMessage {
	return &waE2E.ProtocolMessage{Type: typ.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(true)}}
}

func edit(id string, edited *waE2E.Message) *waE2E.Message {
	p := protocol(waE2E.ProtocolMessage_MESSAGE_EDIT, id)
	p.EditedMessage = edited
	return &waE2E.Message{ProtocolMessage: p}
}

func revoke(id string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: protocol(waE2E.ProtocolMessage_REVOKE, id)}
}

func future(m *waE2E.Message) *waE2E.FutureProofMessage { return &waE2E.FutureProofMessage{Message: m} }

// historyNotice is the announcement of a history sync that the phone sends.
func historyNotice() *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &waE2E.HistorySyncNotification{
			SyncType: waE2E.HistorySyncType_RECENT.Enum(), ChunkOrder: proto.Uint32(2),
		},
	}}
}

func marshaled(t testing.TB, m *waE2E.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestClassify: every kind of message the archive keeps or leaves out, as the
// protobuf of the events whatsmeow dispatches. raw is the message the row must
// hold the bytes of (the one under the wrappers); nil for none.
func TestClassify(t *testing.T) {
	bob, carol := pnJID(bobPN), pnJID(carolPN)
	bobSender := bob.String()
	voice := &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus"), FileLength: proto.Uint64(5), PTT: proto.Bool(true)}
	once := proto.Bool(true)

	albumItem := &waE2E.Message{ImageMessage: image("holiday")}
	viewOnceImage := &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(77), Caption: proto.String("secret"),
		ViewOnce: once, ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("Q9")}, MediaKey: []byte("k")}
	deepest := text("deep")
	nest := func(n int) *waE2E.Message {
		m := deepest
		for range n {
			m = &waE2E.Message{EphemeralMessage: future(m)}
		}
		return m
	}
	quote := &waE2E.ContextInfo{StanzaID: proto.String("Q1")}
	ownGroup := inGroup("M2", types.NewADJID(ownPN, 0, 12))
	ownGroup.IsFromMe = true
	s := proto.String

	// What a business says in words, and the answers to it.
	interactive := &waE2E.Message{InteractiveMessage: &waE2E.InteractiveMessage{Body: &waE2E.InteractiveMessage_Body{Text: s("Your code is 123456")}}}
	template := &waE2E.Message{TemplateMessage: &waE2E.TemplateMessage{HydratedTemplate: &waE2E.TemplateMessage_HydratedFourRowTemplate{HydratedContentText: s("Order shipped")}}}
	buttons := &waE2E.Message{ButtonsMessage: &waE2E.ButtonsMessage{ContentText: s("Choose")}}
	list := &waE2E.Message{ListMessage: &waE2E.ListMessage{Title: s("Menu"), Description: s("pick one")}}
	listTitleOnly := &waE2E.Message{ListMessage: &waE2E.ListMessage{Title: s("Menu")}}
	buttonsReply := &waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: "Yes"}}}
	listReply := &waE2E.Message{ListResponseMessage: &waE2E.ListResponseMessage{Title: s("Option A")}}
	templateReply := &waE2E.Message{TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{SelectedDisplayText: s("Confirm")}}
	interactiveReply := &waE2E.Message{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{Body: &waE2E.InteractiveResponseMessage_Body{Text: s("Pay now")}}}
	businessRow := func(words string) archive.Row {
		return archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: words}
	}

	for _, tc := range []struct {
		name string
		info types.MessageInfo
		msg  *waE2E.Message

		kind   OpKind
		chat   string // as received
		name_  string
		row    archive.Row // Raw aside
		edit   archive.Edit
		revoke archive.Revoke
		raw    *waE2E.Message
	}{
		// What people write.
		{name: "text", info: incoming("M1", bob), msg: text("hello"), kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "hello"}, raw: text("hello")},
		{name: "text with a reply to a message", info: incoming("M1", bob), msg: quoting("agreed", "Q1"), kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "agreed", QuotedID: "Q1"}, raw: quoting("agreed", "Q1")},
		{name: "text in a group", info: inGroup("M1", bob), msg: text("hi all"), kind: OpUpsert, chat: groupJID().String(),
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "hi all"}, raw: text("hi all")}, // no name: the push name is the participant's
		{name: "own text: the chat is the recipient, the sender is us", info: outgoing("M1", bob), msg: text("on my way"), kind: OpUpsert, chat: bobSender,
			row: archive.Row{ID: "M1", Sender: pnJID(ownPN).String(), FromMe: true, TS: t0, Text: "on my way"}, raw: text("on my way")},
		{name: "own text in a group", info: func() types.MessageInfo {
			i := inGroup("M1", types.NewADJID(ownPN, 0, 12))
			i.IsFromMe = true
			return i
		}(), msg: text("see you"), kind: OpUpsert, chat: groupJID().String(),
			row: archive.Row{ID: "M1", Sender: pnJID(ownPN).String(), FromMe: true, TS: t0, Text: "see you"}, raw: text("see you")},
		{name: "the sender stays as it came: a LID is not turned into a number", info: func() types.MessageInfo {
			i := incoming("M1", lidJID(bobLID))
			i.SenderAlt = bob
			return i
		}(), msg: text("hi"), kind: OpUpsert, chat: lidJID(bobLID).String(), name_: "Bob",
			row: archive.Row{ID: "M1", Sender: lidJID(bobLID).String(), TS: t0, Text: "hi"}, raw: text("hi")},
		{name: "the device and the agent are dropped from the sender", info: func() types.MessageInfo {
			i := inGroup("M1", types.JID{User: bobPN, RawAgent: 0, Device: 7, Server: types.DefaultUserServer})
			return i
		}(), msg: text("hi"), kind: OpUpsert, chat: groupJID().String(),
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "hi"}, raw: text("hi")},
		{name: "a private chat message with no sender is the chat's", info: func() types.MessageInfo {
			i := incoming("M1", bob)
			i.Sender = types.EmptyJID
			return i
		}(), msg: text("hi"), kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "hi"}, raw: text("hi")},
		{name: "a group message with no sender is not attributable", info: func() types.MessageInfo {
			i := inGroup("M1", types.EmptyJID)
			return i
		}(), msg: text("hi")},
		{name: "no push name is no name", info: func() types.MessageInfo {
			i := incoming("M1", bob)
			i.PushName = "-"
			return i
		}(), msg: text("hi"), kind: OpUpsert, chat: bobSender,
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "hi"}, raw: text("hi")},
		{name: "a legacy chat server is read as is, for the identity to settle", info: func() types.MessageInfo {
			i := incoming("M1", types.NewJID(bobPN, types.LegacyUserServer))
			return i
		}(), msg: text("hi"), kind: OpUpsert, chat: bobPN + "@c.us", name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobPN + "@c.us", TS: t0, Text: "hi"}, raw: text("hi")},

		// Media.
		{name: "image with a caption", info: incoming("M1", bob), msg: &waE2E.Message{ImageMessage: image("my cat")}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "my cat", MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234},
			raw: &waE2E.Message{ImageMessage: image("my cat")}},
		{name: "image without one", info: incoming("M1", bob), msg: &waE2E.Message{ImageMessage: image("")}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234},
			raw: &waE2E.Message{ImageMessage: image("")}},
		{name: "image that answers a message", info: incoming("M1", bob), msg: &waE2E.Message{ImageMessage: func() *waE2E.ImageMessage {
			m := image("")
			m.ContextInfo = &waE2E.ContextInfo{StanzaID: proto.String("Q2")}
			return m
		}()}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234, QuotedID: "Q2"},
			raw: &waE2E.Message{ImageMessage: func() *waE2E.ImageMessage {
				m := image("")
				m.ContextInfo = &waE2E.ContextInfo{StanzaID: proto.String("Q2")}
				return m
			}()}},
		{name: "video", info: incoming("M1", bob), msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), FileLength: proto.Uint64(9000), Caption: proto.String("look")}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "look", MediaType: "video", MediaMime: "video/mp4", MediaSize: 9000},
			raw: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), FileLength: proto.Uint64(9000), Caption: proto.String("look")}}},
		{name: "a round video note is a video", info: incoming("M1", bob), msg: &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), FileLength: proto.Uint64(10)}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "video", MediaMime: "video/mp4", MediaSize: 10},
			raw: &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), FileLength: proto.Uint64(10)}}},
		{name: "audio", info: incoming("M1", bob), msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/mpeg"), FileLength: proto.Uint64(5)}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "audio", MediaMime: "audio/mpeg", MediaSize: 5},
			raw: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/mpeg"), FileLength: proto.Uint64(5)}}},
		{name: "voice message", info: incoming("M1", bob), msg: &waE2E.Message{AudioMessage: voice}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "ptt", MediaMime: "audio/ogg; codecs=opus", MediaSize: 5},
			raw: &waE2E.Message{AudioMessage: voice}},
		{name: "document with a caption: the text is the caption, the name the file's", info: incoming("M1", bob),
			msg: &waE2E.Message{DocumentMessage: document("lease.pdf", "the lease")}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "the lease", MediaType: "document", MediaMime: "application/pdf", MediaName: "lease.pdf", MediaSize: 99},
			raw: &waE2E.Message{DocumentMessage: document("lease.pdf", "the lease")}},
		{name: "document without one: the file's name is the text", info: incoming("M1", bob),
			msg: &waE2E.Message{DocumentMessage: document("lease.pdf", "")}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "lease.pdf", MediaType: "document", MediaMime: "application/pdf", MediaName: "lease.pdf", MediaSize: 99},
			raw: &waE2E.Message{DocumentMessage: document("lease.pdf", "")}},
		{name: "document with a title and no file name", info: incoming("M1", bob),
			msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Title: proto.String("Lease"), Mimetype: proto.String("application/pdf")}}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "Lease", MediaType: "document", MediaMime: "application/pdf", MediaName: "Lease"},
			raw: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Title: proto.String("Lease"), Mimetype: proto.String("application/pdf")}}},
		{name: "document still in its caption wrapper", info: incoming("M1", bob),
			msg: &waE2E.Message{DocumentWithCaptionMessage: future(&waE2E.Message{DocumentMessage: document("lease.pdf", "the lease")})}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "the lease", MediaType: "document", MediaMime: "application/pdf", MediaName: "lease.pdf", MediaSize: 99},
			raw: &waE2E.Message{DocumentMessage: document("lease.pdf", "the lease")}},
		{name: "sticker", info: incoming("M1", bob), msg: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp"), FileLength: proto.Uint64(3)}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "sticker", MediaMime: "image/webp", MediaSize: 3},
			raw: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp"), FileLength: proto.Uint64(3)}}},
		{name: "a length that does not fit a signed integer is clamped", info: incoming("M1", bob),
			msg:  &waE2E.Message{StickerMessage: &waE2E.StickerMessage{FileLength: proto.Uint64(math.MaxUint64)}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "sticker", MediaSize: math.MaxInt64},
			raw: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{FileLength: proto.Uint64(math.MaxUint64)}}},

		// The reply a message makes is kept with the media as with a text.
		{name: "audio that answers a message", info: incoming("M1", bob), msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{ContextInfo: quote}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "audio", QuotedID: "Q1"}, raw: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{ContextInfo: quote}}},
		{name: "video that answers a message", info: incoming("M1", bob), msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{ContextInfo: quote}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "video", QuotedID: "Q1"}, raw: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{ContextInfo: quote}}},
		{name: "document that answers a message", info: incoming("M1", bob), msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{ContextInfo: quote}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "document", QuotedID: "Q1"}, raw: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{ContextInfo: quote}}},
		{name: "sticker that answers a message", info: incoming("M1", bob), msg: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{ContextInfo: quote}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "sticker", QuotedID: "Q1"}, raw: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{ContextInfo: quote}}},
		{name: "a text with no text, only a reply, is nothing", info: incoming("M1", bob), msg: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: quote}}},
		{name: "the push name is trimmed", info: func() types.MessageInfo {
			i := incoming("M1", bob)
			i.PushName = "  Bob  "
			return i
		}(), msg: text("hi"), kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "hi"}, raw: text("hi")},

		// What a business says in words, and what is answered by a button: the words
		// are kept as a text; the buttons are not, and a message with none is nothing.
		{name: "business: interactive message", info: incoming("M1", bob), msg: interactive, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Your code is 123456"), raw: interactive},
		{name: "business: template", info: incoming("M1", bob), msg: template, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Order shipped"), raw: template},
		{name: "business: buttons", info: incoming("M1", bob), msg: buttons, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Choose"), raw: buttons},
		{name: "business: list, the description before the title", info: incoming("M1", bob), msg: list, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("pick one"), raw: list},
		{name: "business: list with a title only", info: incoming("M1", bob), msg: listTitleOnly, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Menu"), raw: listTitleOnly},
		{name: "business: the button that was pressed", info: incoming("M1", bob), msg: buttonsReply, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Yes"), raw: buttonsReply},
		{name: "business: the item of a list that was chosen", info: incoming("M1", bob), msg: listReply, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Option A"), raw: listReply},
		{name: "business: the button of a template that was pressed", info: incoming("M1", bob), msg: templateReply, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Confirm"), raw: templateReply},
		{name: "business: the answer to an interactive message", info: incoming("M1", bob), msg: interactiveReply, kind: OpUpsert, chat: bobSender, name_: "Bob", row: businessRow("Pay now"), raw: interactiveReply},
		{name: "business: interactive message with no words", info: incoming("M1", bob), msg: &waE2E.Message{InteractiveMessage: &waE2E.InteractiveMessage{}}},

		// View-once: the row says that one came and of what kind; what it showed is not kept.
		{name: "view-once image marked on the image", info: incoming("M1", bob), msg: &waE2E.Message{ImageMessage: viewOnceImage}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "image", MediaMime: "image/jpeg", MediaSize: 77, QuotedID: "Q9"}},
		{name: "view-once image under its wrapper", info: incoming("M1", bob), msg: &waE2E.Message{ViewOnceMessageV2: future(&waE2E.Message{ImageMessage: image("secret")})},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234}},
		{name: "view-once video", info: incoming("M1", bob), msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), Caption: proto.String("secret"), ViewOnce: once}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "video", MediaMime: "video/mp4"}},
		{name: "view-once voice message", info: incoming("M1", bob), msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg"), PTT: proto.Bool(true), ViewOnce: once}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "ptt", MediaMime: "audio/ogg"}},
		{name: "view-once round video note, marked on the note", info: incoming("M1", bob),
			msg:  &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), Caption: proto.String("secret"), ViewOnce: once}},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "video", MediaMime: "video/mp4"}},
		{name: "view-once image under the first wrapper", info: incoming("M1", bob), msg: &waE2E.Message{ViewOnceMessage: future(&waE2E.Message{ImageMessage: image("secret")})},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234}},
		{name: "view-once image under the extension wrapper", info: incoming("M1", bob), msg: &waE2E.Message{ViewOnceMessageV2Extension: future(&waE2E.Message{ImageMessage: image("secret")})},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234}},
		{name: "view-once image under another wrapper, under the view-once one", info: incoming("M1", bob),
			msg:  &waE2E.Message{ViewOnceMessageV2: future(&waE2E.Message{EphemeralMessage: future(&waE2E.Message{ImageMessage: image("secret")})})},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234}},

		// Wrappers that whatsmeow does not take off for us.
		{name: "an album's item", info: incoming("M1", bob), msg: &waE2E.Message{AssociatedChildMessage: future(albumItem)}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "holiday", MediaType: "image", MediaMime: "image/jpeg", MediaSize: 1234}, raw: albumItem},
		{name: "a message that mentions the group", info: inGroup("M1", bob), msg: &waE2E.Message{GroupMentionedMessage: future(text("@all lunch"))}, kind: OpUpsert, chat: groupJID().String(),
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "@all lunch"}, raw: text("@all lunch")},
		{name: "wrappers in an order that UnwrapRaw does not read", info: incoming("M1", bob),
			msg:  &waE2E.Message{ViewOnceMessage: future(&waE2E.Message{EphemeralMessage: future(&waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: text("nested")}})})},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "nested"}, raw: text("nested")}, // view-once is about media: a text is kept
		{name: "a lottie sticker's wrapper", info: incoming("M1", bob), msg: &waE2E.Message{LottieStickerMessage: future(&waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp")}})},
			kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, MediaType: "sticker", MediaMime: "image/webp"},
			raw: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp")}}},
		{name: "a message to a bot, in its wrapper", info: incoming("M1", bob), msg: &waE2E.Message{BotInvokeMessage: future(text("asked"))}, kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "asked"}, raw: text("asked")},
		{name: "a message nested one wrapper too deep is not followed", info: incoming("M1", bob), msg: nest(maxWrapping + 1)},
		{name: "a message nested too deep is not followed", info: incoming("M1", bob), msg: nest(maxWrapping + 4)},
		{name: "a message at the depth that is followed", info: incoming("M1", bob), msg: nest(maxWrapping), kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "deep"}, raw: deepest},

		// Edits.
		{name: "edit of a text", info: incoming("M2", bob), msg: edit("M1", text("fixed")), kind: OpEdit, chat: bobSender, name_: "Bob",
			edit: archive.Edit{ID: "M1", Sender: bobSender, Text: "fixed", EditedAt: t0}},
		{name: "edit of our own message", info: outgoing("M2", bob), msg: edit("M1", text("fixed")), kind: OpEdit, chat: bobSender,
			edit: archive.Edit{ID: "M1", Sender: pnJID(ownPN).String(), FromMe: true, Text: "fixed", EditedAt: t0}},
		{name: "edit of a caption", info: incoming("M2", bob), msg: edit("M1", &waE2E.Message{ImageMessage: image("a better one")}), kind: OpEdit, chat: bobSender, name_: "Bob",
			edit: archive.Edit{ID: "M1", Sender: bobSender, Text: "a better one", EditedAt: t0}},
		{name: "edit that takes a caption away leaves the document's name", info: incoming("M2", bob),
			msg: edit("M1", &waE2E.Message{DocumentMessage: document("lease.pdf", "")}), kind: OpEdit, chat: bobSender, name_: "Bob",
			edit: archive.Edit{ID: "M1", Sender: bobSender, Text: "lease.pdf", EditedAt: t0}},
		{name: "edit that takes a caption off an image", info: incoming("M2", bob), msg: edit("M1", &waE2E.Message{ImageMessage: image("")}), kind: OpEdit, chat: bobSender, name_: "Bob",
			edit: archive.Edit{ID: "M1", Sender: bobSender, EditedAt: t0}},
		{name: "edit under the wrapper that is left", info: incoming("M2", bob), msg: &waE2E.Message{EditedMessage: future(edit("M1", text("fixed")))}, kind: OpEdit, chat: bobSender, name_: "Bob",
			edit: archive.Edit{ID: "M1", Sender: bobSender, Text: "fixed", EditedAt: t0}},
		{name: "edit in a group", info: inGroup("M2", bob), msg: edit("M1", text("fixed")), kind: OpEdit, chat: groupJID().String(),
			edit: archive.Edit{ID: "M1", Sender: bobSender, Text: "fixed", EditedAt: t0}},
		{name: "edit whose content is in a wrapper", info: incoming("M2", bob), msg: edit("M1", &waE2E.Message{EphemeralMessage: future(text("fixed"))}), kind: OpEdit, chat: bobSender, name_: "Bob",
			edit: archive.Edit{ID: "M1", Sender: bobSender, Text: "fixed", EditedAt: t0}},
		{name: "edit with no new content", info: incoming("M2", bob), msg: edit("M1", nil)},
		{name: "edit to a content the archive does not keep", info: incoming("M2", bob), msg: edit("M1", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}})},
		{name: "edit with no message id", info: incoming("M2", bob), msg: edit("", text("fixed"))},

		// Revokes.
		{name: "revoke", info: incoming("M2", bob), msg: revoke("M1"), kind: OpRevoke, chat: bobSender, name_: "Bob",
			revoke: archive.Revoke{ID: "M1", Sender: bobSender, RevokedAt: t0}},
		{name: "revoke of our own message", info: outgoing("M2", bob), msg: revoke("M1"), kind: OpRevoke, chat: bobSender,
			revoke: archive.Revoke{ID: "M1", Sender: pnJID(ownPN).String(), FromMe: true, RevokedAt: t0}},
		{name: "revoke by an admin: the message is its participant's", info: inGroup("M2", carol), msg: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String("M1"), FromMe: proto.Bool(false), Participant: proto.String(bobPN + ":3@s.whatsapp.net")},
		}}, kind: OpRevoke, chat: groupJID().String(),
			revoke: archive.Revoke{ID: "M1", Sender: bobSender, RevokedAt: t0}},
		{name: "revoke of one's own message in a group names the participant too", info: inGroup("M2", bob), msg: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String("M1"), FromMe: proto.Bool(true), Participant: proto.String(bobPN + "@s.whatsapp.net")},
		}}, kind: OpRevoke, chat: groupJID().String(),
			revoke: archive.Revoke{ID: "M1", Sender: bobSender, RevokedAt: t0}},
		{name: "revoke of one's own message: its sender is the deleter's, as that message's key may be in another addressing", info: inGroup("M2", lidJID(bobLID)), msg: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String("M1"), FromMe: proto.Bool(true), Participant: proto.String(bobPN + "@s.whatsapp.net")},
		}}, kind: OpRevoke, chat: groupJID().String(),
			revoke: archive.Revoke{ID: "M1", Sender: lidJID(bobLID).String(), RevokedAt: t0}},
		{name: "revoke by us as an admin: the message is its participant's, and not ours", info: ownGroup, msg: adminRevoke("M1", bobPN+"@s.whatsapp.net"), kind: OpRevoke, chat: groupJID().String(),
			revoke: archive.Revoke{ID: "M1", Sender: bobSender, FromMe: false, RevokedAt: t0}},
		{name: "revoke by someone who names nobody as the author: the message is the deleter's", info: inGroup("M2", carol), msg: adminRevoke("M1", ""), kind: OpRevoke, chat: groupJID().String(),
			revoke: archive.Revoke{ID: "M1", Sender: carol.String(), RevokedAt: t0}},
		{name: "revoke with no message id", info: incoming("M2", bob), msg: revoke("")},
		{name: "a protocol message that does not say its type is not a revoke (REVOKE is the zero value)", info: incoming("M2", bob),
			msg: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: &waCommon.MessageKey{ID: proto.String("M1")}}}},
		{name: "ephemeral setting is a notice", info: incoming("M2", bob), msg: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(), EphemeralExpiration: proto.Uint32(86400)}}},

		// What is not for the archive.
		{name: "reaction", info: incoming("M2", bob), msg: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍"), Key: &waCommon.MessageKey{ID: proto.String("M1")}}}},
		{name: "poll", info: incoming("M2", bob), msg: &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("Lunch?")}}},
		{name: "poll vote", info: incoming("M2", bob), msg: &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}}},
		{name: "location", info: incoming("M2", bob), msg: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Home")}}},
		{name: "contact card", info: incoming("M2", bob), msg: &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Carol")}}},
		{name: "buttons", info: incoming("M2", bob), msg: &waE2E.Message{ButtonsMessage: &waE2E.ButtonsMessage{}}},
		{name: "template", info: incoming("M2", bob), msg: &waE2E.Message{TemplateMessage: &waE2E.TemplateMessage{}}},
		{name: "album header", info: incoming("M2", bob), msg: &waE2E.Message{AlbumMessage: &waE2E.AlbumMessage{ExpectedImageCount: proto.Uint32(3)}}},
		{name: "sender key of a group, with no message", info: inGroup("M2", bob), msg: &waE2E.Message{
			SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String(groupID)},
			MessageContextInfo:           &waE2E.MessageContextInfo{MessageSecret: []byte("secret")}}},
		{name: "empty message", info: incoming("M2", bob), msg: &waE2E.Message{}},
		{name: "empty text", info: incoming("M2", bob), msg: text("")},
		{name: "no message", info: incoming("M2", bob), msg: nil},
		{name: "a message with no id", info: incoming("", bob), msg: text("hello")},
		{name: "a message with no chat", info: func() types.MessageInfo {
			i := incoming("M2", bob)
			i.Chat = types.EmptyJID
			return i
		}(), msg: text("hello")},
		{name: "status", info: func() types.MessageInfo {
			i := incoming("M2", bob)
			i.Chat, i.IsGroup = types.StatusBroadcastJID, true
			return i
		}(), msg: &waE2E.Message{ImageMessage: image("my day")}},
		{name: "our own status", info: func() types.MessageInfo {
			i := outgoing("M2", types.StatusBroadcastJID)
			i.IsGroup = true
			return i
		}(), msg: &waE2E.Message{ImageMessage: image("my day")}},
		{name: "channel post", info: func() types.MessageInfo {
			i := incoming("M2", types.NewJID("120363111111111111", types.NewsletterServer))
			return i
		}(), msg: text("news")},
		{name: "chat with Meta AI", info: incoming("M2", pnJID("13135550002")), msg: text("an answer")},
		{name: "chat with a bot server's JID", info: incoming("M2", types.NewJID("867051314767696", types.BotServer)), msg: text("an answer")},
		{name: "Meta AI in a group", info: inGroup("M2", pnJID("13135550002")), msg: text("an answer")},
		{name: "a chat of a server the archive has no kind for", info: incoming("M2", types.NewJID("1234", types.MessengerServer)), msg: text("hi")},
		{name: "a message of one device to another", info: func() types.MessageInfo {
			i := incoming("M2", bob)
			i.Category = "peer"
			return i
		}(), msg: text("hello")},

		// Broadcast lists: one that reaches us shows in the chat with its sender.
		{name: "broadcast list message to us", info: func() types.MessageInfo {
			i := incoming("M1", bob)
			i.Chat, i.IsGroup = types.NewJID("1699999999", types.BroadcastServer), true
			return i
		}(), msg: text("sale"), kind: OpUpsert, chat: bobSender, name_: "Bob",
			row: archive.Row{ID: "M1", Sender: bobSender, TS: t0, Text: "sale"}, raw: text("sale")},
		{name: "message of ours to a broadcast list", info: func() types.MessageInfo {
			i := outgoing("M1", types.NewJID("1699999999", types.BroadcastServer))
			i.IsGroup = true
			return i
		}(), msg: text("sale")},
		// whatsmeow calls it incoming, and files it with the sender, which is us, when
		// the stanza names the owner of the list.
		{name: "message of ours to a broadcast list that names the owner", info: func() types.MessageInfo {
			i := outgoing("M1", types.NewJID("1699999999", types.BroadcastServer))
			i.IsGroup = true
			i.BroadcastListOwner = bob
			return i
		}(), msg: text("sale")},
		{name: "a chat of a server with no user", info: func() types.MessageInfo {
			i := incoming("M2", bob)
			i.Chat = types.NewJID("", types.DefaultUserServer)
			return i
		}(), msg: text("hi")},

		// History: only the phone's own announcement counts.
		{name: "history sync notification of our phone", info: func() types.MessageInfo {
			i := outgoing("H1", pnJID(ownPN))
			i.Category = "peer"
			return i
		}(), msg: historyNotice(), kind: OpHistoryNotif},
		{name: "history sync notification of somebody else", info: incoming("H1", bob), msg: historyNotice()},
		{name: "history sync notification under a wrapper", info: outgoing("H1", pnJID(ownPN)), msg: &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: historyNotice()}}, kind: OpHistoryNotif},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before []byte
			if tc.msg != nil {
				before = marshaled(t, tc.msg)
			}
			op := Classify(tc.info, tc.msg)
			if tc.msg != nil {
				if after := marshaled(t, tc.msg); string(after) != string(before) {
					t.Error("Classify changed the message")
				}
			}
			if op.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v (op %+v)", op.Kind, tc.kind, op)
			}
			if tc.kind == OpSkip {
				if !reflect.DeepEqual(op, Op{}) {
					t.Errorf("a skip carries data: %+v", op)
				}
				return
			}
			if tc.kind == OpHistoryNotif {
				if op.Notif.GetChunkOrder() != 2 || !op.Chat.IsEmpty() {
					t.Errorf("history notification op = %+v", op)
				}
				return
			}
			if op.Chat.String() != tc.chat || op.ChatName != tc.name_ {
				t.Errorf("chat %q named %q, want %q named %q", op.Chat, op.ChatName, tc.chat, tc.name_)
			}
			raw := op.Row.Raw
			op.Row.Raw = nil
			switch tc.kind {
			case OpUpsert:
				if !reflect.DeepEqual(op.Row, tc.row) {
					t.Errorf("row = %+v, want %+v", op.Row, tc.row)
				}
				if tc.raw == nil {
					if len(raw) != 0 {
						t.Errorf("raw = % x, want none", raw)
					}
				} else {
					if string(raw) != string(marshaled(t, tc.raw)) {
						t.Errorf("raw is not the message under the wrappers: %x", raw)
					}
					var got waE2E.Message
					if err := proto.Unmarshal(raw, &got); err != nil || !proto.Equal(&got, tc.raw) {
						t.Errorf("raw does not read back as %v: %v", tc.raw, err)
					}
				}
				if !reflect.DeepEqual(op.Edit, archive.Edit{}) || !reflect.DeepEqual(op.Revoke, archive.Revoke{}) {
					t.Errorf("an upsert has an edit or a revoke: %+v", op)
				}
			case OpEdit:
				if !reflect.DeepEqual(op.Edit, tc.edit) {
					t.Errorf("edit = %+v, want %+v", op.Edit, tc.edit)
				}
			case OpRevoke:
				if !reflect.DeepEqual(op.Revoke, tc.revoke) {
					t.Errorf("revoke = %+v, want %+v", op.Revoke, tc.revoke)
				}
			}
		})
	}
}

// TestClassifyViewOnceKeepsNoContent: of a view-once message the archive holds
// the fact and the kind, not what it showed: not its caption, not the media key,
// not the thumbnail, which would all be in the raw message.
func TestClassifyViewOnceKeepsNoContent(t *testing.T) {
	img := image("the code is 1234")
	img.ViewOnce = proto.Bool(true)
	op := Classify(incoming("M1", pnJID(bobPN)), &waE2E.Message{ImageMessage: img})
	if op.Kind != OpUpsert || op.Row.MediaType != "image" {
		t.Fatalf("op = %+v, want a row of an image", op)
	}
	if op.Row.Text != "" || len(op.Row.Raw) != 0 {
		t.Errorf("the row of a view-once image holds a text %q and a raw message of %d bytes", op.Row.Text, len(op.Row.Raw))
	}
}

// TestClassifyHistoryNoticeIsOurPhonesAlone: whatever the notice names is
// downloaded and written to the archive as what the contacts said, so one that
// arrives in a chat of somebody else must be taken for nothing, however it is
// wrapped.
func TestClassifyHistoryNoticeIsOurPhonesAlone(t *testing.T) {
	for name, info := range map[string]types.MessageInfo{
		"contact":       incoming("H1", pnJID(bobPN)),
		"group":         inGroup("H1", pnJID(bobPN)),
		"contact's LID": incoming("H1", lidJID(bobLID)),
	} {
		if op := Classify(info, historyNotice()); op.Kind != OpSkip {
			t.Errorf("%s: op = %+v, want a skip", name, op)
		}
	}
	own := outgoing("H1", pnJID(ownPN))
	if op := Classify(own, historyNotice()); op.Kind != OpHistoryNotif || op.Notif.GetSyncType() != waE2E.HistorySyncType_RECENT {
		t.Errorf("our own phone's notice: op = %+v", op)
	}
}

func TestOpNoLaterThan(t *testing.T) {
	now := t0.Add(time.Hour)
	later, earlier := now.Add(48*time.Hour), now.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		op   Op
		got  func(Op) time.Time
		want time.Time
	}{
		{"upsert from the future", Op{Kind: OpUpsert, Row: archive.Row{TS: later}}, func(o Op) time.Time { return o.Row.TS }, now},
		{"upsert from the past", Op{Kind: OpUpsert, Row: archive.Row{TS: earlier}}, func(o Op) time.Time { return o.Row.TS }, earlier},
		{"upsert from now", Op{Kind: OpUpsert, Row: archive.Row{TS: now}}, func(o Op) time.Time { return o.Row.TS }, now},
		{"upsert with no time", Op{Kind: OpUpsert}, func(o Op) time.Time { return o.Row.TS }, now},
		{"edit from the future", Op{Kind: OpEdit, Edit: archive.Edit{EditedAt: later}}, func(o Op) time.Time { return o.Edit.EditedAt }, now},
		{"edit from the past", Op{Kind: OpEdit, Edit: archive.Edit{EditedAt: earlier}}, func(o Op) time.Time { return o.Edit.EditedAt }, earlier},
		{"revoke from the future", Op{Kind: OpRevoke, Revoke: archive.Revoke{RevokedAt: later}}, func(o Op) time.Time { return o.Revoke.RevokedAt }, now},
		{"revoke from the past", Op{Kind: OpRevoke, Revoke: archive.Revoke{RevokedAt: earlier}}, func(o Op) time.Time { return o.Revoke.RevokedAt }, earlier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got(tc.op.NoLaterThan(now)); !got.Equal(tc.want) {
				t.Errorf("time = %v, want %v", got, tc.want)
			}
		})
	}
	// What is not an operation on the archive has nothing to cap.
	if got := (Op{Kind: OpSkip}).NoLaterThan(now); !reflect.DeepEqual(got, Op{Kind: OpSkip}) {
		t.Errorf("a skip changed: %+v", got)
	}
}

func TestOpIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   Op
		got  func(Op) [2]string
	}{
		{"upsert", Op{Kind: OpUpsert}, func(o Op) [2]string { return [2]string{o.Row.Account, o.Row.Chat} }},
		{"edit", Op{Kind: OpEdit}, func(o Op) [2]string { return [2]string{o.Edit.Account, o.Edit.Chat} }},
		{"revoke", Op{Kind: OpRevoke}, func(o Op) [2]string { return [2]string{o.Revoke.Account, o.Revoke.Chat} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got(tc.op.In("work", "c@lid")); got != [2]string{"work", "c@lid"} {
				t.Errorf("account and chat = %v", got)
			}
		})
	}
	if got := (Op{}).In("work", "c@lid"); !reflect.DeepEqual(got, Op{}) {
		t.Errorf("a skip was given a place: %+v", got)
	}
}

func TestOpKindString(t *testing.T) {
	for k, want := range map[OpKind]string{OpSkip: "skip", OpUpsert: "upsert", OpEdit: "edit", OpRevoke: "revoke", OpHistoryNotif: "history-notification", OpKind(99): "unknown"} {
		if got := k.String(); got != want {
			t.Errorf("OpKind(%d) = %q, want %q", int(k), got, want)
		}
	}
}
