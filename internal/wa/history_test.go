package wa

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

const (
	bobChat   = bobPN + "@s.whatsapp.net"
	carolChat = carolPN + "@s.whatsapp.net"
	teamChat  = groupID + "@g.us"
)

// kinds is a conversation's messages of each kind the archive keeps besides text.
func kinds(chat string) []*waHistorySync.HistorySyncMsg {
	voice := &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg"), FileLength: proto.Uint64(5), PTT: proto.Bool(true)}
	video := &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), FileLength: proto.Uint64(50), Caption: proto.String("a short clip")}
	sticker := &waE2E.StickerMessage{Mimetype: proto.String("image/webp"), FileLength: proto.Uint64(7)}
	return []*waHistorySync.HistorySyncMsg{
		hmsg(chat, "K1", false, "", 1*time.Second, &waE2E.Message{ImageMessage: image("holiday photo")}),
		hmsg(chat, "K2", false, "", 2*time.Second, &waE2E.Message{DocumentMessage: document("report.pdf", "")}),
		hmsg(chat, "K3", false, "", 3*time.Second, &waE2E.Message{AudioMessage: voice}),
		hmsg(chat, "K4", false, "", 4*time.Second, &waE2E.Message{VideoMessage: video}),
		hmsg(chat, "K5", false, "", 5*time.Second, &waE2E.Message{StickerMessage: sticker}),
		hmsg(chat, "K6", false, "", 6*time.Second, quoting("an answer", "K1")),
	}
}

// sameIDs reports whether got holds exactly the ids of want, in any order.
func sameIDs(got, want []string) bool {
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

// TestHistoryImportsWhatThePhoneKept is the whole way, with the real archive: the
// announcement the handler queues, the blob the worker downloads, and every kind of
// conversation and message it holds. The rows, what search finds, the chats with
// their names and the time of their last message, the counts list shows, and what
// the worker leaves behind: the queue empty, the blob deleted from the server once,
// the phone told once, and the log with counts and no content.
func TestHistoryImportsWhatThePhoneKept(t *testing.T) {
	x := newHx(t, 3)
	own := x.cli("personal")
	own.Store.LID = lidJID(ownLID) // whatsmeow files our own message in a LID chat under it (client.go:1028-1030)
	farFuture := 400 * 24 * time.Hour

	// Carol wrote before the history came, by her number; the blob says which LID that is.
	x.mustDeliver(t, "personal", at(incoming("C-1", pnJID(carolPN)), 0), text("a message that came live"))

	blob := withPairs(hblob(
		hconv(bobChat, "Bob (work)", append([]*waHistorySync.HistorySyncMsg{
			pushed(hin(bobChat, "M1", "hello there", 1*time.Second), "Bobby"),
			hout(bobChat, "M2", "on my way", 2*time.Second),
			hmsg(bobChat, "E1", false, "", 10*time.Second, edit("M1", text("hello, world"))),
			hmsg(bobChat, "D1", true, "", 20*time.Second, revoke("M2")),
			hmsg(bobChat, "R1", false, "", 21*time.Second, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}),
			hmsg(bobChat, "S1", false, "", 22*time.Second, nil), // a stub: no content
			hin(bobChat, "M5", "from the future", farFuture),
		}, kinds(bobChat)...)...),
		hconv(teamChat, "Team",
			hmsg(teamChat, "G1", false, bobChat, 3*time.Second, text("hi all")),
			hmsg(teamChat, "G2", true, "", 4*time.Second, text("see you")),
		),
		// A chat with no name of its own is shown under what the contact calls themselves.
		hconv("70000000555@s.whatsapp.net", "", pushed(hin("70000000555@s.whatsapp.net", "N1", "who is this", time.Second), "Dave D")),
		hconv(carolChat, "", hin(carolChat, "C0", "an old greeting", 1*time.Second)),
		hconv(carolLID+"@lid", "Carol",
			hin(carolLID+"@lid", "C1", "a newer greeting", 5*time.Second),
			hout(carolLID+"@lid", "C2", "and my answer", 6*time.Second)),
		// Not conversations of the archive.
		hconv("status@broadcast", "", hin("status@broadcast", "T1", "my day", time.Second)),
		hconv("1700000000@broadcast", "A list", hout("1700000000@broadcast", "B1", "to all of them", time.Second)),
		hconv("120363111111111111@newsletter", "News", hin("120363111111111111@newsletter", "W1", "a channel post", time.Second)),
		hconv("13135550002@bot", "Meta AI", hin("13135550002@bot", "A1", "an answer", time.Second)),
		hconv("no-jid", "", hin("no-jid", "X1", "nowhere", time.Second)),
	), carolLID, carolPN)
	x.h.put("/h/1", blob)

	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")

	if got := x.size(t, "personal"); got != [2]int{4, 9 + 2 + 1 + 4} {
		t.Fatalf("the archive has %v chats and messages; log:\n%s", got, x.logs)
	}
	bob := x.msgs(t, "personal", bobChat)
	if got := ids(bob); !sameIDs(got, []string{"M1", "M2", "K1", "K2", "K3", "K4", "K5", "K6", "M5"}) {
		t.Errorf("Bob's messages %v", got)
	}
	byID := map[string]archive.Message{}
	for _, m := range bob {
		byID[m.ID] = m
	}
	if m := byID["M1"]; m.Text != "hello, world" || !m.EditedAt.Equal(t0.Add(10*time.Second)) || !m.TS.Equal(t0.Add(time.Second)) || m.FromMe || m.Sender != bobChat {
		t.Errorf("the edited message is %+v: the edit's text and time on the original's row", m)
	}
	if m := byID["M2"]; m.Text != "on my way" || !m.RevokedAt.Equal(t0.Add(20*time.Second)) || !m.FromMe || m.Sender != ownPN+"@s.whatsapp.net" {
		t.Errorf("the revoked message is %+v: its text stays, marked", m)
	}
	for id, want := range map[string]struct{ typ, text, name string }{
		"K1": {"image", "holiday photo", ""}, "K2": {"document", "report.pdf", "report.pdf"}, "K3": {"ptt", "", ""},
		"K4": {"video", "a short clip", ""}, "K5": {"sticker", "", ""}, "K6": {"", "an answer", ""},
	} {
		if m := byID[id]; m.MediaType != want.typ || m.Text != want.text || m.MediaName != want.name {
			t.Errorf("%s is %+v, want %+v", id, m, want)
		}
	}
	if byID["K6"].QuotedID != "K1" {
		t.Errorf("the answer quotes %q, want K1", byID["K6"].QuotedID)
	}
	if m := byID["M5"]; !m.TS.Equal(x.now) {
		t.Errorf("a message of a year to come is at %v, want the clock's %v", m.TS, x.now)
	}
	for _, id := range []string{"E1", "D1", "R1", "S1"} {
		if _, ok := byID[id]; ok {
			t.Errorf("%s is a row of its own: an edit or a revoke is the original's, a reaction and a stub are not kept", id)
		}
	}
	team := x.msgs(t, "personal", teamChat)
	if got := ids(team); !equalIDs(got, []string{"G1", "G2"}) {
		t.Errorf("the group's messages %v", got)
	}
	if team[0].Sender != bobChat || team[0].FromMe || !team[1].FromMe {
		t.Errorf("the group's messages %+v: the sender is the participant", team)
	}

	// Carol is one chat, under her LID, with her number, and what was under the number moved.
	carol := carolLID + "@lid"
	carolMsgs := x.msgs(t, "personal", carol)
	if got := ids(carolMsgs); !equalIDs(got, []string{"C-1", "C0", "C1", "C2"}) {
		t.Errorf("Carol's messages under her LID: %v", got)
	}
	if got := x.msgs(t, "personal", carolChat); len(got) != 0 {
		t.Errorf("%d messages are left under Carol's number", len(got))
	}
	if c2 := carolMsgs[3]; !c2.FromMe || c2.Sender != lidJID(ownLID).String() {
		t.Errorf("our message in a LID chat is %+v, want it from our LID", c2)
	}

	chats := map[string]archive.Chat{}
	for _, c := range x.chats(t, "personal") {
		chats[c.JID] = c
	}
	if len(chats) != 4 {
		t.Fatalf("chats %+v, want Bob, the group, Dave and Carol, and nothing of the statuses, lists, channels and bots", chats)
	}
	if c := chats[bobChat]; c.Name != "Bob (work)" || c.IsGroup || !c.LastMessageTS.Equal(x.now) {
		t.Errorf("Bob's chat %+v: the phone's name beats his own, and the last message is the clamped one", c)
	}
	if c := chats[teamChat]; c.Name != "Team" || !c.IsGroup || !c.LastMessageTS.Equal(t0.Add(4*time.Second)) {
		t.Errorf("the group %+v", c)
	}
	if c := chats[pnJID("70000000555").String()]; c.Name != "Dave D" {
		t.Errorf("a chat with no name is %+v, want his push name", c)
	}
	if c := chats[carol]; c.Name != "Carol" || c.PN != carolChat || !c.LastMessageTS.Equal(t0.Add(6*time.Second)) {
		t.Errorf("Carol's chat %+v, want her LID's, with her number and the time of her last message", c)
	}

	// Search sees the edited text and not the original, and the caption.
	search := func(query string) []string {
		hits, err := x.db.Search(context.Background(), archive.SearchQuery{Accounts: []string{"personal"}, Query: query, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.ID)
		}
		return out
	}
	for query, want := range map[string]string{"world": "M1", "photo": "K1", "newer": "C1", "clip": "K4"} {
		if got := search(query); !slices.Contains(got, want) {
			t.Errorf("search %q finds %v, want %s among them", query, got, want)
		}
	}
	if got := search("there"); len(got) != 0 {
		t.Errorf("the text before the edit is still found: %v", got)
	}
	if n := x.size(t, "work"); n != [2]int{} {
		t.Errorf("the other account has %v", n)
	}

	// What list shows, the statuses untouched.
	for _, a := range x.m.Accounts(context.Background()) {
		if a.Nick == "personal" && (a.Chats != 4 || a.Messages != 16 || a.Status != StatusConnected || a.HistoryStuck != 0 || a.Reason != "") {
			t.Errorf("list shows %+v", a)
		}
	}

	// The worker's side of it.
	if calls := x.h.downloads(); len(calls) != 1 || !calls[0].sync || calls[0].cli != own {
		t.Errorf("downloads %+v: want one, by the account's client, with the LID pairs stored before the call returns", calls)
	}
	dels := x.h.deletes()
	if len(dels) != 1 || dels[0].GetDirectPath() != "/h/1" || string(dels[0].GetFileEncSHA256()) != "enc/h/1" || dels[0].GetEncHandle() != "handle/h/1" {
		t.Errorf("the blob is deleted from the server %v: want once, with the notification's own path, hash and handle", dels)
	}
	eventually(t, "the receipt", func() bool { return len(x.h.receipted()) > 0 })
	if got := x.h.receipted(); len(got) != 1 || got[0] != "H1" {
		t.Errorf("receipts %v, want one for H1", got)
	}
	if got := x.h.history(); slices.Index(got, "download /h/1") > slices.Index(got, "delete /h/1") {
		t.Errorf("order of events %v: want the blob deleted after it was downloaded", got)
	}
	log := x.logs.String()
	if !strings.Contains(log, "history sync imported") || !strings.Contains(log, "type=RECENT") || !strings.Contains(log, "conversations=5") {
		t.Errorf("the import is not in the log with its counts:\n%s", log)
	}
	for _, secret := range []string{"hello", "world", "Bobby", "Dave", "Carol", "holiday", bobPN, carolPN, carolLID, "70000000555"} {
		if strings.Contains(log, secret) {
			t.Errorf("%q is in the log:\n%s", secret, log)
		}
	}
}

// TestHistoryEditIsNotMistakenForTheOriginal: ParseWebMessage gives an edit the id
// and the content of the message it edits (client.go:1059-1062), so read from what it
// parses it would be the original sent again, and the edit would be lost. The raw
// message it keeps says what it is, and the worker reads that, whichever comes first
// in the conversation: the messages are written oldest first, so an edit comes before
// its message only when it has the same time to the second.
func TestHistoryEditIsNotMistakenForTheOriginal(t *testing.T) {
	web := hmsg(bobChat, "E1", false, "", 10*time.Second, edit("M1", text("fixed")))
	evt, err := newHx(t, 100).cli("personal").ParseWebMessage(pnJID(bobPN), web.Message)
	if err != nil || evt.Info.ID != "M1" || evt.Message.GetConversation() != "fixed" {
		t.Fatalf("the premise: ParseWebMessage gives %+v, %v", evt, err)
	}
	sameSecond := hmsg(bobChat, "E1", false, "", time.Second, edit("M1", text("fixed")))
	for name, tc := range map[string]struct {
		msgs   []*waHistorySync.HistorySyncMsg
		editAt time.Duration
	}{
		"the edit after the message": {[]*waHistorySync.HistorySyncMsg{hin(bobChat, "M1", "original", time.Second), web}, 10 * time.Second},
		"the edit before it":         {[]*waHistorySync.HistorySyncMsg{sameSecond, hin(bobChat, "M1", "original", time.Second)}, time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			x := newHx(t, 100)
			x.h.put("/h/1", hblob(hconv(bobChat, "", tc.msgs...)))
			x.connect("personal")
			x.announce(t, "personal", "H1", "/h/1")
			x.imported(t, "personal")
			got := x.msgs(t, "personal", bobChat)
			if len(got) != 1 || got[0].Text != "fixed" || !got[0].EditedAt.Equal(t0.Add(tc.editAt)) || !got[0].TS.Equal(t0.Add(time.Second)) {
				t.Errorf("messages %+v, want the original, edited, at its own time", got)
			}
			if cs := x.chats(t, "personal"); len(cs) != 1 || !cs[0].LastMessageTS.Equal(t0.Add(time.Second)) {
				t.Errorf("chats %+v: an edit is no new message of the chat", cs)
			}
		})
	}
}

// TestHistoryTwiceIsOnce: the same history imported again, as a new notification
// of the same blob and as the same notification delivered again once it is done,
// leaves the archive as it was: no row twice, nothing of an edit or a revoke undone,
// and what was edited live in between kept.
func TestHistoryTwiceIsOnce(t *testing.T) {
	x := newHx(t, 2)
	x.h.put("/h/1", hblob(hconv(bobChat, "Bob",
		hin(bobChat, "M1", "one", 1*time.Second),
		hin(bobChat, "M2", "two", 2*time.Second),
		hmsg(bobChat, "E1", false, "", 3*time.Second, edit("M1", text("one, edited"))),
		hin(bobChat, "M3", "three", 4*time.Second),
		hmsg(bobChat, "D1", false, "", 5*time.Second, revoke("M3")))))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	first := x.msgs(t, "personal", bobChat)
	if len(first) != 3 || first[0].Text != "one, edited" || first[2].RevokedAt.IsZero() {
		t.Fatalf("the first import: %+v", first)
	}
	// Something that came live after it.
	x.mustDeliver(t, "personal", at(incoming("E2", pnJID(bobPN)), time.Minute), edit("M2", text("two, edited live")))

	for _, id := range []string{"H2", "H1"} { // the same blob under a new notification, and the first notification again
		x.announce(t, "personal", id, "/h/1")
		x.imported(t, "personal")
	}
	got := x.msgs(t, "personal", bobChat)
	if len(got) != 3 || got[0].Text != "one, edited" || got[1].Text != "two, edited live" || got[2].RevokedAt.IsZero() {
		t.Errorf("after the same history three times: %+v", got)
	}
	if n := x.size(t, "personal"); n != [2]int{1, 3} {
		t.Errorf("the archive has %v chats and messages", n)
	}
	if n := len(x.h.downloads()); n != 3 {
		t.Errorf("%d downloads, want one for each notification", n)
	}
}

// TestHistorySkipsWhatCannotBeRead: a message that whatsmeow cannot read (a group's
// that names no sender) or that has no time is left out and counted, and the rest of
// the notification goes in: failing it would lose all, and it would fail the same way
// at each try.
func TestHistorySkipsWhatCannotBeRead(t *testing.T) {
	x := newHx(t, 100)
	noTime := hin(bobChat, "T0", "without a time", 0)
	noTime.Message.MessageTimestamp = nil
	x.h.put("/h/1", hblob(
		hconv(teamChat, "Team",
			hmsg(teamChat, "G1", false, "", 2*time.Second, text("from nobody")), // no participant
			hmsg(teamChat, "G2", false, bobChat, 3*time.Second, text("from Bob"))),
		hconv(bobChat, "", noTime, hin(bobChat, "M1", "fine", 5*time.Second))))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", teamChat)); !equalIDs(got, []string{"G2"}) {
		t.Errorf("the group has %v", got)
	}
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"M1"}) {
		t.Errorf("Bob has %v", got)
	}
	log := x.logs.String()
	if !strings.Contains(log, "unparsed=2") || !strings.Contains(log, "could not be read and are missing") {
		t.Errorf("the two messages that were left out are not counted in the log:\n%s", log)
	}
	if len(x.h.deletes()) != 1 {
		t.Errorf("the notification is imported, and its blob deleted: %v", x.h.deletes())
	}
}

// TestHistoryNotificationsWithNoConversations: a push-name notification is imported
// as nothing, the blob deleted and the queue emptied, and a notification inside a
// history is not followed: only the live handler queues one.
func TestHistoryNotificationsWithNoConversations(t *testing.T) {
	x := newHx(t, 100)
	nested := &waE2E.Message{ProtocolMessage: notice("/h/inner").ProtocolMessage}
	x.h.put("/h/1", &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_PUSH_NAME.Enum()})
	x.h.put("/h/2", hblob(hconv(teamChat, "Team", hmsg(teamChat, "N1", true, "", time.Second, nested))))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.announce(t, "personal", "H2", "/h/2")
	x.imported(t, "personal")
	if n := x.size(t, "personal"); n != [2]int{1, 0} {
		t.Errorf("the archive has %v chats and messages, want the group's chat and no message", n)
	}
	if got := x.h.downloads(); len(got) != 2 {
		t.Errorf("%d downloads: want the two notifications and not the one inside", len(got))
	}
}

// TestHistoryPairsFileTheChatsThatHaveNoMessageInIt: a blob that brings the LID of a
// person, and none of their messages, still has their chat, which came live under
// their number, filed under the LID: the pair is in the store when the download
// returns, and the chats are filed by what the store knows once the notification is in.
func TestHistoryPairsFileTheChatsThatHaveNoMessageInIt(t *testing.T) {
	x := newHx(t, 100)
	x.mustDeliver(t, "personal", at(incoming("C1", pnJID(carolPN)), time.Second), text("sent by number"))
	if cs := x.chats(t, "personal"); len(cs) != 1 || cs[0].JID != carolChat {
		t.Fatalf("the premise: chats %+v, want one under her number", cs)
	}
	x.h.put("/h/1", withPairs(hblob(), carolLID, carolPN))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")

	carol := carolLID + "@lid"
	cs := x.chats(t, "personal")
	if len(cs) != 1 || cs[0].JID != carol || cs[0].PN != carolChat {
		t.Fatalf("chats %+v, want one under her LID, with her number", cs)
	}
	if got := ids(x.msgs(t, "personal", carol)); !equalIDs(got, []string{"C1"}) {
		t.Errorf("her messages under the LID: %v", got)
	}
}

// TestHistoryUsesNoMethodOfTheClient: the worker reaches whatsmeow only through the
// seams of the network, as the handler's files do not reach it at all. The clients of
// the tests are never connected, so a direct call would fail there; the code is looked
// at as well.
func TestHistoryUsesNoMethodOfTheClient(t *testing.T) {
	call := regexp.MustCompile(`\bcli\.[A-Z]\w*\(`)
	for _, name := range []string{"history.go", "history_import.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if loc := call.FindIndex(src); loc != nil {
			t.Errorf("%s calls a method of the client: %q", name, src[loc[0]:loc[1]])
		}
		// A client is never kept either: the worker takes the account's current one each time.
		if bytes.Contains(src, []byte("a.cli")) {
			t.Errorf("%s reads a.cli: the client to use is a.owner()", name)
		}
	}
}

// TestHistoryDelay: the pause after a failed import doubles up to its limit, and
// a count that is large, or none, does not overflow or go negative.
func TestHistoryDelay(t *testing.T) {
	const base, limit = 30 * time.Second, 10 * time.Minute
	for failures, want := range map[int]time.Duration{
		0: base, 1: base, 2: 60 * time.Second, 3: 2 * time.Minute, 4: 4 * time.Minute, 5: 8 * time.Minute,
		6: limit, 7: limit, 1000: limit,
	} {
		if got := historyDelay(base, limit, failures); got != want {
			t.Errorf("after %d failures: %v, want %v", failures, got, want)
		}
	}
	if got := historyDelay(time.Hour, time.Minute, 1); got != time.Minute {
		t.Errorf("a first pause above the limit is %v, want the limit", got)
	}
}

// TestHistoryErrorsHideTheAddressOfTheBlob: whatsmeow's errors carry the whole
// address of the media download, and the log is for what happened and not where to
// fetch an encrypted blob.
func TestHistoryErrorsHideTheAddressOfTheBlob(t *testing.T) {
	err := fmt.Errorf("failed to download: %w", &url.Error{Op: "Get", URL: "https://mmg.example/v/t62/secret-path?oh=SECRET&oe=1", Err: io.ErrUnexpectedEOF})
	text := errText(&importError{cause: causeDownload, err: err})
	if strings.Contains(text, "SECRET") || strings.Contains(text, "secret-path") || !strings.Contains(text, "<url>") || !strings.Contains(text, "unexpected EOF") {
		t.Errorf("the error says %q", text)
	}
}
