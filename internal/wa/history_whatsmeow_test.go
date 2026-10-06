package wa

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// The tests here let whatsmeow's own code read what the phone sends where the rest of
// them put a fake of it: its download, which is what decides what a notification that
// names no blob or one inside it does, and its parser, which is what decides which
// spelling of a chat it can read the sender of a message in.

// whatsmeowDownload makes the worker download with whatsmeow's own call, and counts
// the calls.
func (x *hx) whatsmeowDownload() *atomic.Int32 {
	calls := new(atomic.Int32)
	x.m.net.downloadHistory = func(cli *whatsmeow.Client, ctx context.Context, n *waE2E.HistorySyncNotification, synchronousStorage bool) (*waHistorySync.HistorySync, error) {
		calls.Add(1)
		return cli.DownloadHistorySync(ctx, n, synchronousStorage)
	}
	return calls
}

// announceNotification delivers n as the message id.
func (x *hx) announceNotification(t *testing.T, nick, id string, n *waE2E.HistorySyncNotification) {
	t.Helper()
	x.mustDeliver(t, nick, outgoing(id, pnJID(ownPN)), noticeOf(n))
}

// TestHistoryNotificationThatNamesNoBlobIsNothingToImport: the phone says that it has no
// history to send, or what access it has to the messages, in notifications that carry
// no blob. There is nothing to download for them, and no try would find anything: they
// are taken off the queue without a call to WhatsApp, are not counted as history that
// is missing, and the notification behind them does not wait for their pauses. The first
// one after a link carries its blob inside, and is read by whatsmeow's own download.
func TestHistoryNotificationThatNamesNoBlobIsNothingToImport(t *testing.T) {
	x := newHx(t, 100)
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour // a pause would be waited out for good
	calls := x.whatsmeowDownload()
	x.connect("personal")
	x.announceNotification(t, "personal", "N1", &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_NO_HISTORY.Enum()})
	x.announceNotification(t, "personal", "N2", &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_MESSAGE_ACCESS_STATUS.Enum()})
	x.announceNotification(t, "personal", "N3", inline(t, hblob(hconv(bobChat, "Bob", hin(bobChat, "Z1", "hello", time.Second)))))
	x.imported(t, "personal")

	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"Z1"}) {
		t.Errorf("Bob's messages %v, want the one the inline notification carried", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d downloads, want the one of the notification that has a blob", n)
	}
	if acc := x.listed("personal"); acc.HistoryStuck != 0 || acc.Reason != "" {
		t.Errorf("list shows %+v: nothing is missing", acc)
	}
	if log := x.logs.String(); strings.Contains(log, "import failed") || strings.Contains(log, "given up") {
		t.Errorf("an import is said to have failed:\n%s", log)
	}
	eventually(t, "the receipts", func() bool { return len(x.h.receipted()) == 3 })
}

// TestHistoryDeleteOfABlobInsideTheNotificationCallsNothing: there is no blob on the
// server to delete for the notification that carries it, and whatsmeow's call knows,
// which is how the worker may ask for the delete of every notification it has imported.
// The client of the test is not connected, so a call that went out would fail.
func TestHistoryDeleteOfABlobInsideTheNotificationCallsNothing(t *testing.T) {
	x := newHx(t, 100)
	n := inline(t, hblob())
	if err := liveNetwork.deleteHistoryMedia(x.cli("personal"), context.Background(), n); err != nil {
		t.Errorf("the delete of a blob that is not on the server fails with %v", err)
	}
	n.DirectPath = proto.String("/v/t62.7118-24/blob")
	if err := liveNetwork.deleteHistoryMedia(x.cli("personal"), context.Background(), n); err == nil {
		t.Error("the delete of a blob that is on the server calls nobody")
	}
}

// TestHistoryChatsOnTheOtherSpellingsOfAnAddress: whatsmeow reads the sender of a message
// of another person from the chat for s.whatsapp.net and lid only, and fails the others,
// c.us and the hosted ones, for want of a participant. The phone's history has chats
// under them (whatsmeow itself changes c.us in the pairs of a blob, message.go:1143-1145),
// which are the same chats the messages that come live are filed under, and lose nothing.
func TestHistoryChatsOnTheOtherSpellingsOfAnAddress(t *testing.T) {
	x := newHx(t, 100)
	x.cli("personal").Store.LID = lidJID(ownLID)
	const cu, hosted, hostedLID = "70001110001", "70001110002", "70001110003"
	chats := map[string]string{ // as the phone writes the chat: as the archive files it
		cu + "@c.us":              cu + "@s.whatsapp.net",
		hosted + "@hosted":        hosted + "@s.whatsapp.net",
		hostedLID + "@hosted.lid": hostedLID + "@lid",
	}
	var convs []*waHistorySync.Conversation
	for from := range chats {
		convs = append(convs, hconv(from, "", hin(from, "IN-"+from, "from them", time.Second), hout(from, "OUT-"+from, "mine", 2*time.Second)))
	}
	x.h.put("/h/1", hblob(convs...))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")

	for from, to := range chats {
		got := x.msgs(t, "personal", to)
		if len(got) != 2 || got[0].ID != "IN-"+from || got[0].FromMe || got[0].Sender != to || got[1].ID != "OUT-"+from || !got[1].FromMe {
			t.Errorf("%s: the chat %s has %+v, want the message of the other person, from them, and ours", from, to, got)
		}
	}
	if n := x.size(t, "personal"); n != [2]int{3, 6} {
		t.Errorf("the archive has %v chats and messages", n)
	}

}

// TestHistoryMessagesOfAConversationAreWrittenOldestFirst: a phone lists a conversation's
// messages from the newest, or in an order of its own, and the archive reads those of the
// same second in the order they were written, and names a chat that has no name of its
// own by the push name written last.
func TestHistoryMessagesOfAConversationAreWrittenOldestFirst(t *testing.T) {
	const album = 5 * time.Second
	ordered := func(hm *waHistorySync.HistorySyncMsg, n uint64) *waHistorySync.HistorySyncMsg {
		hm.MsgOrderID = proto.Uint64(n)
		return hm
	}
	for _, tc := range []struct {
		name string
		msgs []*waHistorySync.HistorySyncMsg
		want []string
		by   string // the chat's name: the push name of the newest message
	}{
		{"newest first", []*waHistorySync.HistorySyncMsg{
			pushed(hin(bobChat, "A3", "third", album), "Robert"),
			pushed(hin(bobChat, "A2", "second", album), "Robert"),
			pushed(hin(bobChat, "A1", "first", album), "Robert"),
			pushed(hin(bobChat, "OLD", "oldest", time.Second), "Bob"),
		}, []string{"OLD", "A1", "A2", "A3"}, "Robert"},
		{"oldest first", []*waHistorySync.HistorySyncMsg{
			pushed(hin(bobChat, "OLD", "oldest", time.Second), "Bob"),
			pushed(hin(bobChat, "A1", "first", album), "Robert"),
			pushed(hin(bobChat, "A2", "second", album), "Robert"),
		}, []string{"OLD", "A1", "A2"}, "Robert"},
		{"in no order", []*waHistorySync.HistorySyncMsg{
			pushed(hin(bobChat, "X", "middle", 2*time.Second), "Mid"),
			pushed(hin(bobChat, "Y", "newest", 3*time.Second), "Newest"),
			pushed(hin(bobChat, "Z", "oldest", time.Second), "Old"),
		}, []string{"Z", "X", "Y"}, "Newest"},
		{"by the order the phone numbers them", []*waHistorySync.HistorySyncMsg{
			ordered(pushed(hin(bobChat, "A2", "second", album), "Robert"), 2),
			ordered(pushed(hin(bobChat, "A1", "first", album), "Robert"), 1),
			ordered(pushed(hin(bobChat, "OLD", "oldest", time.Second), "Bob"), 0),
			ordered(pushed(hin(bobChat, "A3", "third", album), "Robert"), 3),
		}, []string{"OLD", "A1", "A2", "A3"}, "Robert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newHx(t, 100)
			x.h.put("/h/1", hblob(hconv(bobChat, "", tc.msgs...)))
			x.connect("personal")
			x.announce(t, "personal", "H1", "/h/1")
			x.imported(t, "personal")
			if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, tc.want) {
				t.Errorf("read back %v, want %v", got, tc.want)
			}
			if cs := x.chats(t, "personal"); len(cs) != 1 || cs[0].Name != tc.by {
				t.Errorf("chats %+v, want one named %q", cs, tc.by)
			}
		})
	}
}

// TestHistoryConversationsKnowTheirOtherAddress: a conversation names the other address
// of its chat itself, the phone JID of a LID chat and the LID of a number's, and whatsmeow
// keeps only the pairs the blob lists apart from the conversations. The chat is filed by
// the pair all the same, and the number finds it.
func TestHistoryConversationsKnowTheirOtherAddress(t *testing.T) {
	const lid, number = "5550009", "70005550009"
	for _, tc := range []struct {
		name string
		conv func() *waHistorySync.Conversation
		live bool // a message came by the number before
	}{
		{"a LID chat with its number", func() *waHistorySync.Conversation {
			c := hconv(lid+"@lid", "Eve", hin(lid+"@lid", "E1", "hi", 2*time.Second))
			c.PnJID = proto.String(number + "@s.whatsapp.net")
			return c
		}, false},
		{"a number's chat with its LID, which had a message by the number", func() *waHistorySync.Conversation {
			c := hconv(number+"@s.whatsapp.net", "Eve", hin(number+"@s.whatsapp.net", "E1", "hi", 2*time.Second))
			c.LidJID = proto.String(lid + "@lid")
			return c
		}, true},
		{"a LID chat with its number in the old spelling", func() *waHistorySync.Conversation {
			c := hconv(lid+"@lid", "Eve", hin(lid+"@lid", "E1", "hi", 2*time.Second))
			c.PnJID = proto.String(number + "@c.us")
			return c
		}, false},
		{"the old spelling of a number's chat", func() *waHistorySync.Conversation {
			c := hconv(number+"@c.us", "Eve", hin(number+"@c.us", "E1", "hi", 2*time.Second))
			c.LidJID = proto.String(lid + "@lid")
			return c
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newHx(t, 100)
			if tc.live {
				x.mustDeliver(t, "personal", at(incoming("LIVE", pnJID(number)), time.Second), text("by number"))
			}
			x.h.put("/h/1", &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{tc.conv()}})
			x.connect("personal")
			x.announce(t, "personal", "H1", "/h/1")
			x.imported(t, "personal")

			cs := x.chats(t, "personal")
			if len(cs) != 1 || cs[0].JID != lid+"@lid" || cs[0].PN != number+"@s.whatsapp.net" || cs[0].Name != "Eve" {
				t.Fatalf("chats %+v, want one under the LID, with the number and the name", cs)
			}
			want := []string{"E1"}
			if tc.live {
				want = []string{"LIVE", "E1"}
			}
			if got := ids(x.msgs(t, "personal", lid+"@lid")); !equalIDs(got, want) {
				t.Errorf("under the LID %v, want %v", got, want)
			}
			got, err := CanonicalChat(context.Background(), lidsOf(x.cli("personal")), "+"+number)
			if err != nil || got.JID != lid+"@lid" {
				t.Errorf("the number is the chat %+v, %v, want the one under the LID", got, err)
			}
		})
	}
}

// TestHistoryConversationsDoNotMakePairsOfWhatIsNoPair: the address a conversation names
// as its other is a pair only when it is a LID for a number or the number of a LID: not
// its own address again, not a group, not something else.
func TestHistoryConversationsDoNotMakePairsOfWhatIsNoPair(t *testing.T) {
	x := newHx(t, 100)
	same := hconv(bobChat, "Bob", hin(bobChat, "M1", "one", time.Second))
	same.PnJID = proto.String(bobChat)
	group := hconv(teamChat, "Team", hmsg(teamChat, "G1", false, bobChat, time.Second, text("hi")))
	group.PnJID, group.LidJID = proto.String(bobChat), proto.String(carolLID+"@lid")
	garbage := hconv(carolChat, "Carol", hin(carolChat, "C1", "two", time.Second))
	garbage.LidJID = proto.String("not a jid@@")
	x.h.put("/h/1", &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{same, group, garbage}})
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")

	lids := lidsOf(x.cli("personal"))
	for _, jid := range []types.JID{pnJID(bobPN), pnJID(carolPN)} {
		if lid, err := lids.GetLIDForPN(context.Background(), jid); err != nil || !lid.IsEmpty() {
			t.Errorf("%s has the LID %s, %v: no pair was named", jid, lid, err)
		}
	}
	if n := x.size(t, "personal"); n != [2]int{3, 3} {
		t.Errorf("the archive has %v chats and messages", n)
	}
}
