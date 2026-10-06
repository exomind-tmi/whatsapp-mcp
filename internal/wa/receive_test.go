package wa

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

// syncBuf is a log buffer that goroutines of the Manager may write while the
// test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rx is a Manager with two accounts, personal (+ownPN, with a device) and work,
// and the archive and the fake network beside it. The tests send the events of
// a client to its own handler, which is what whatsmeow does with the messages it
// decrypts, so that the handler is the real one and everything it uses is real
// but WhatsApp.
type rx struct {
	*fixture
	m    *Manager
	fn   *fakeNet
	logs *syncBuf
	now  time.Time // the Manager's clock: a month after t0
}

// newRx builds an rx. prep are statements run on the new archive.db before the
// Manager opens it, for a test to break it in a way of its own.
func newRx(t *testing.T, prep ...string) *rx {
	t.Helper()
	dir := testutil.TempDir(t)
	path := filepath.Join(dir, "archive.db")
	db, err := archive.Open(path) // the schema, for the statements to work on
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, stmt := range prep {
		w, err := sqlitedb.Open(path, sqlitedb.Pragmas)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		w.Close()
	}
	f := &fixture{dir: dir}
	if f.db, err = archive.Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	logs := &syncBuf{}
	f.log = debugLogTo(logs)
	f.account(t, "personal", ownPN)
	f.account(t, "work", workPhone)
	f.devices(t, map[string]string{ownPN: "Anton", workPhone: ""})
	fn := &fakeNet{}
	r := &rx{fixture: f, fn: fn, logs: logs, now: t0.Add(30 * 24 * time.Hour)}
	r.m = f.start(t, fn.network(readyGlobals()))
	r.m.wg.Wait() // the connects are done
	r.m.now = func() time.Time { return r.now }
	return r
}

func (r *rx) cli(nick string) *whatsmeow.Client {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return r.m.accounts[nick].cli
}

// send gives the message to the handler of cli, as whatsmeow does, with its
// wrappers taken off, and reports whether the handler let it be acknowledged.
func send(cli *whatsmeow.Client, info types.MessageInfo, msg *waE2E.Message) (acked bool) {
	e := &events.Message{Info: info, RawMessage: msg}
	e.UnwrapRaw()
	return !cli.DangerousInternals().DispatchEvent(e)
}

// deliver is send to an account's own client.
func (r *rx) deliver(nick string, info types.MessageInfo, msg *waE2E.Message) bool {
	return send(r.cli(nick), info, msg)
}

// mustDeliver delivers the message and fails the test unless it is acknowledged.
func (r *rx) mustDeliver(t *testing.T, nick string, info types.MessageInfo, msg *waE2E.Message) {
	t.Helper()
	if !r.deliver(nick, info, msg) {
		t.Fatalf("message %s of %s was not acknowledged; log:\n%s", info.ID, nick, r.logs)
	}
}

// msgs are the messages of a chat, oldest first.
func (r *rx) msgs(t *testing.T, nick, chat string) []archive.Message {
	t.Helper()
	page, err := r.db.Messages(context.Background(), archive.MsgQuery{Account: nick, Chat: chat, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return page.Messages
}

func (r *rx) chats(t *testing.T, nick string) []archive.Chat {
	t.Helper()
	cs, err := r.db.Chats(context.Background(), archive.ChatQuery{Accounts: []string{nick}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// size is the number of chats and messages the archive has of the account.
func (r *rx) size(t *testing.T, nick string) [2]int {
	t.Helper()
	return archiveOf(t, r.fixture)[nick]
}

func ids(ms []archive.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// at is info sent d after t0.
func at(info types.MessageInfo, d time.Duration) types.MessageInfo {
	info.Timestamp = t0.Add(d)
	return info
}

var bobPNChat, bobLIDChat = bobPN + "@s.whatsapp.net", bobLID + "@lid"

// debugLogTo is a debug logger onto w.
func debugLogTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestReceiveStoresMessages: a text in and out of a private chat and of a group,
// through the real handler: the rows, the chats with the name and the time, and
// the raw message, which are what the read tools will show.
func TestReceiveStoresMessages(t *testing.T) {
	r := newRx(t)
	bob := pnJID(bobPN)
	in := text("hello")
	out := text("on my way")
	g := text("hi all")

	r.mustDeliver(t, "personal", at(incoming("M1", bob), 1*time.Second), in)
	r.mustDeliver(t, "personal", at(outgoing("M2", bob), 2*time.Second), out)
	r.mustDeliver(t, "personal", at(inGroup("G1", bob), 3*time.Second), g)
	own := outgoing("G2", types.EmptyJID)
	own.Chat, own.IsGroup, own.Sender = groupJID(), true, types.NewADJID(ownPN, 0, 12)
	r.mustDeliver(t, "personal", at(own, 4*time.Second), text("see you"))

	got := r.msgs(t, "personal", bobPNChat)
	if len(got) != 2 {
		t.Fatalf("private chat: %d messages, want 2: %+v", len(got), got)
	}
	m1, m2 := got[0], got[1]
	if m1.ID != "M1" || m1.Text != "hello" || m1.FromMe || m1.Sender != bobPNChat || !m1.TS.Equal(t0.Add(time.Second)) {
		t.Errorf("incoming = %+v", m1)
	}
	if m2.ID != "M2" || m2.Text != "on my way" || !m2.FromMe || m2.Sender != ownPN+"@s.whatsapp.net" {
		t.Errorf("own = %+v", m2)
	}
	if grp := r.msgs(t, "personal", groupID+"@g.us"); len(grp) != 2 || grp[0].Sender != bobPNChat || !grp[1].FromMe {
		t.Errorf("group = %+v", grp)
	}
	raw, err := r.db.MessageWithRaw(context.Background(), "personal", bobPNChat, "M1")
	if err != nil || !bytes.Equal(raw.Raw, marshaled(t, in)) {
		t.Errorf("raw = %x, %v; want the message's own bytes", raw.Raw, err)
	}

	byJID := map[string]archive.Chat{}
	for _, c := range r.chats(t, "personal") {
		byJID[c.JID] = c
	}
	if len(byJID) != 2 {
		t.Fatalf("chats %+v, want the private one and the group", byJID)
	}
	// Our own push name, on our own message, does not rename the chat after us.
	if c := byJID[bobPNChat]; c.Name != "Bob" || c.IsGroup || c.PN != "" || !c.LastMessageTS.Equal(t0.Add(2*time.Second)) {
		t.Errorf("private chat = %+v", c)
	}
	// The push name on a group message is the participant's, not the group's.
	if c := byJID[groupID+"@g.us"]; c.Name != "" || !c.IsGroup || !c.LastMessageTS.Equal(t0.Add(4*time.Second)) {
		t.Errorf("group = %+v", c)
	}
	if n := r.size(t, "work"); n != [2]int{} {
		t.Errorf("the other account has %v", n)
	}
}

// TestReceiveSkips: what is not for the archive is acknowledged and leaves no
// row and no chat behind.
func TestReceiveSkips(t *testing.T) {
	r := newRx(t)
	bob := pnJID(bobPN)
	status := incoming("S1", bob)
	status.Chat, status.IsGroup = types.StatusBroadcastJID, true
	peer := incoming("P1", bob)
	peer.Category = "peer"
	for name, tc := range map[string]struct {
		info types.MessageInfo
		msg  *waE2E.Message
	}{
		"reaction":                             {incoming("R1", bob), &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}},
		"poll":                                 {incoming("R2", bob), &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("Lunch?")}}},
		"location":                             {incoming("R3", bob), &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}}},
		"status":                               {status, &waE2E.Message{ImageMessage: image("my day")}},
		"channel":                              {incoming("R4", types.NewJID("120363111111111111", types.NewsletterServer)), text("news")},
		"Meta AI":                              {incoming("R5", pnJID("13135550002")), text("an answer")},
		"between devices":                      {peer, text("hello")},
		"empty":                                {incoming("R6", bob), &waE2E.Message{}},
		"sender key":                           {inGroup("R7", bob), &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String(groupID)}}},
		"an edit of a message that is no text": {incoming("R8", bob), edit("M1", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}})},
	} {
		if !r.deliver("personal", tc.info, tc.msg) {
			t.Errorf("%s: not acknowledged", name)
		}
	}
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v chats and messages of what is not for it", n)
	}
}

// TestReceiveHistoryNoticeIsQueuedBeforeItIsAcknowledged: the announcement of a
// history sync is kept for the worker that downloads it, with the bootstrap it may
// carry inline, and only then acknowledged: whatsmeow throws away what it holds of
// a message once that is done. Sent again it is one row. Chats and messages are
// not touched, and the log has its kind and not what it carries.
func TestReceiveHistoryNoticeIsQueuedBeforeItIsAcknowledged(t *testing.T) {
	r := newRx(t)
	notice := historyNotice()
	notice.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = []byte("SECRET-PAYLOAD")
	for range 2 {
		if !r.deliver("personal", outgoing("H1", pnJID(ownPN)), notice) {
			t.Fatal("the notification was not acknowledged")
		}
	}
	q, ok, err := r.db.QueueNext(context.Background(), "personal")
	if err != nil || !ok || q.MsgID != "H1" {
		t.Fatalf("queue: %+v %v %v", q, ok, err)
	}
	var got waE2E.HistorySyncNotification
	if err := proto.Unmarshal(q.Notif, &got); err != nil || string(got.GetInitialHistBootstrapInlinePayload()) != "SECRET-PAYLOAD" || got.GetChunkOrder() != 2 {
		t.Errorf("the queued notification is %v (%v), want the one that came", &got, err)
	}
	// One of a stranger is no history notice at all, and is skipped as any message.
	if !r.deliver("personal", incoming("H2", pnJID(bobPN)), notice) {
		t.Error("a stranger's notification was not acknowledged")
	}
	if n := r.rowsOf(t, "history_queue", "personal"); n != 1 {
		t.Errorf("%d rows in the queue, want the one notification, once", n)
	}
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v chats and messages of a notification", n)
	}
	if out := r.logs.String(); !strings.Contains(out, "history sync queued") || strings.Contains(out, "SECRET") {
		t.Errorf("want the queueing logged without the content:\n%s", out)
	}
}

// TestReceiveHistoryNoticeThatCannotBeQueuedIsNotAcknowledged: with nowhere to keep
// it the notification is left unacknowledged, for WhatsApp to send it again, and
// the log says so without its content.
func TestReceiveHistoryNoticeThatCannotBeQueuedIsNotAcknowledged(t *testing.T) {
	r := newRx(t, `DROP TABLE history_queue`)
	notice := historyNotice()
	notice.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = []byte("SECRET-PAYLOAD")
	if r.deliver("personal", outgoing("H1", pnJID(ownPN)), notice) {
		t.Error("acknowledged, with nowhere to keep it")
	}
	if out := r.logs.String(); !strings.Contains(out, "queue a history sync") || strings.Contains(out, "SECRET") {
		t.Errorf("want the failure logged without the content:\n%s", out)
	}
}

// TestReceiveHistoryNoticeWithNoID: WhatsApp gives a message an id, so a notice
// without one is not a message that was sent; there is no key to queue it by, and
// it is not held against anyone.
func TestReceiveHistoryNoticeWithNoID(t *testing.T) {
	r := newRx(t)
	if !r.deliver("personal", outgoing("", pnJID(ownPN)), historyNotice()) {
		t.Error("a notification with no id was not acknowledged")
	}
	if n := r.rowsOf(t, "history_queue", "personal"); n != 0 {
		t.Errorf("%d rows in the queue", n)
	}
}

// TestReceiveClampsFutureTimestamps: a message dated after the clock is filed at
// the clock, as are an edit and a revoke, so that the chat does not stay at the
// top of the list, its last message time never going down.
func TestReceiveClampsFutureTimestamps(t *testing.T) {
	r := newRx(t)
	bob := pnJID(bobPN)
	r.mustDeliver(t, "personal", at(incoming("M1", bob), 365*24*time.Hour), text("from the future"))
	got := r.msgs(t, "personal", bobPNChat)
	if len(got) != 1 || !got[0].TS.Equal(r.now) {
		t.Fatalf("messages %+v, want one at %v", got, r.now)
	}
	if cs := r.chats(t, "personal"); len(cs) != 1 || !cs[0].LastMessageTS.Equal(r.now) {
		t.Errorf("chats %+v, want one last written at %v", cs, r.now)
	}
	r.mustDeliver(t, "personal", at(incoming("E1", bob), 400*24*time.Hour), edit("M1", text("still from the future")))
	r.mustDeliver(t, "personal", at(incoming("D1", bob), 400*24*time.Hour), revoke("M1"))
	m := r.msgs(t, "personal", bobPNChat)[0]
	if !m.EditedAt.Equal(r.now) || !m.RevokedAt.Equal(r.now) || m.Text != "still from the future" {
		t.Errorf("message %+v, want edited and revoked at %v", m, r.now)
	}
}

// TestReceiveRedeliveryChangesNothing: WhatsApp delivers at least once, and the
// phone sends a message again that it never saw acknowledged.
func TestReceiveRedeliveryChangesNothing(t *testing.T) {
	r := newRx(t)
	bob := pnJID(bobPN)
	orig := at(incoming("M1", bob), time.Second)
	for range 3 {
		r.mustDeliver(t, "personal", orig, text("hello"))
	}
	if got := r.msgs(t, "personal", bobPNChat); len(got) != 1 {
		t.Fatalf("%d rows after three deliveries", len(got))
	}
	// The original that comes again does not undo what came after it.
	r.mustDeliver(t, "personal", at(incoming("E1", bob), 10*time.Second), edit("M1", text("hello, world")))
	r.mustDeliver(t, "personal", at(incoming("D1", bob), 20*time.Second), revoke("M1"))
	for range 2 {
		r.mustDeliver(t, "personal", orig, text("hello"))
		r.mustDeliver(t, "personal", at(incoming("E1", bob), 10*time.Second), edit("M1", text("hello, world")))
		r.mustDeliver(t, "personal", at(incoming("D1", bob), 20*time.Second), revoke("M1"))
	}
	got := r.msgs(t, "personal", bobPNChat)
	if len(got) != 1 || got[0].Text != "hello, world" || !got[0].EditedAt.Equal(t0.Add(10*time.Second)) || !got[0].RevokedAt.Equal(t0.Add(20*time.Second)) {
		t.Errorf("messages %+v, want the edited and revoked one, once", got)
	}
	if n := r.size(t, "personal"); n != [2]int{1, 1} {
		t.Errorf("the archive has %v chats and messages", n)
	}
}

// permutations of s, each a new slice.
func permutations[T any](s []T) [][]T {
	if len(s) <= 1 {
		return [][]T{slices.Clone(s)}
	}
	var out [][]T
	for i := range s {
		rest := slices.Concat(s[:i:i], s[i+1:])
		for _, p := range permutations(rest) {
			out = append(out, append([]T{s[i]}, p...))
		}
	}
	return out
}

// TestReceiveEditAndRevokeInAnyOrder: the message, its edit and its revoke come
// in whatever order the network and the phone's history put them in, and the
// archive ends with the same one message: the original's header and raw message,
// the edit's text, both marks.
func TestReceiveEditAndRevokeInAnyOrder(t *testing.T) {
	bob := pnJID(bobPN)
	type step struct {
		name string
		info types.MessageInfo
		msg  *waE2E.Message
	}
	steps := []step{
		{"original", at(incoming("M1", bob), 1*time.Second), text("original")},
		{"first edit", at(incoming("E1", bob), 10*time.Second), edit("M1", text("first edit"))},
		{"second edit", at(incoming("E2", bob), 20*time.Second), edit("M1", text("second edit"))},
		{"revoke", at(incoming("D1", bob), 30*time.Second), revoke("M1")},
	}
	for _, order := range permutations(steps) {
		var names []string
		for _, s := range order {
			names = append(names, s.name)
		}
		t.Run(strings.Join(names, ", "), func(t *testing.T) {
			r := newRx(t)
			for _, s := range order {
				r.mustDeliver(t, "personal", s.info, s.msg)
			}
			got := r.msgs(t, "personal", bobPNChat)
			if len(got) != 1 {
				t.Fatalf("%d messages: %+v", len(got), got)
			}
			m := got[0]
			if m.Text != "second edit" || !m.EditedAt.Equal(t0.Add(20*time.Second)) || !m.RevokedAt.Equal(t0.Add(30*time.Second)) {
				t.Errorf("message %+v, want the latest edit's text, and both marks", m)
			}
			if !m.TS.Equal(t0.Add(time.Second)) || m.Sender != bobPNChat || m.FromMe {
				t.Errorf("message %+v: the header is the original's, and not the stub's", m)
			}
			withRaw, err := r.db.MessageWithRaw(context.Background(), "personal", bobPNChat, "M1")
			if err != nil || !bytes.Equal(withRaw.Raw, marshaled(t, text("original"))) {
				t.Errorf("raw = %x, %v; want the original's", withRaw.Raw, err)
			}
			cs := r.chats(t, "personal")
			if len(cs) != 1 || !cs[0].LastMessageTS.Equal(t0.Add(time.Second)) {
				t.Errorf("chats %+v, want one, last written when the message was (an edit or a revoke is no new message)", cs)
			}
		})
	}
}

// TestReceiveViewOnceWrapperCountsWhateverTheMediaSays: whatsmeow takes the
// view-once wrapper off the message it dispatches and keeps a flag of it, and the
// image inside need not say that it is view-once; the handler reads the wrapper
// from the message as it was sent, and what a view-once image showed is not kept.
func TestReceiveViewOnceWrapperCountsWhateverTheMediaSays(t *testing.T) {
	for name, wrap := range map[string]func(*waE2E.Message) *waE2E.Message{
		"v2":           func(m *waE2E.Message) *waE2E.Message { return &waE2E.Message{ViewOnceMessageV2: future(m)} },
		"v1":           func(m *waE2E.Message) *waE2E.Message { return &waE2E.Message{ViewOnceMessage: future(m)} },
		"v2 extension": func(m *waE2E.Message) *waE2E.Message { return &waE2E.Message{ViewOnceMessageV2Extension: future(m)} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRx(t)
			r.mustDeliver(t, "personal", at(incoming("V1", pnJID(bobPN)), time.Second), wrap(&waE2E.Message{ImageMessage: image("the code is 1234")}))
			m, err := r.db.MessageWithRaw(context.Background(), "personal", bobPNChat, "V1")
			if err != nil {
				t.Fatal(err)
			}
			if m.MediaType != "image" || m.Text != "" || len(m.Raw) != 0 {
				t.Errorf("a view-once image: type %q, caption %q, %d bytes of raw message", m.MediaType, m.Text, len(m.Raw))
			}
		})
	}
}

// TestReceiveEditOfAPlaceholderResendIsAnEdit: a message the phone sends again as a
// web message comes out of whatsmeow's parser (ParseWebMessage) as the new content
// under the original's id when it is an edit, and the raw message is all that says it
// is one. It must be an edit of the original, and not the original sent again.
func TestReceiveEditOfAPlaceholderResendIsAnEdit(t *testing.T) {
	r := newRx(t)
	cli := r.cli("personal")
	r.mustDeliver(t, "personal", at(incoming("ORIG", pnJID(bobPN)), time.Second), text("original"))
	web := &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{RemoteJID: proto.String(bobPNChat), FromMe: proto.Bool(false), ID: proto.String("E1")},
		MessageTimestamp: proto.Uint64(uint64(t0.Add(10 * time.Second).Unix())),
		PushName:         proto.String("Bob"),
		Message:          edit("ORIG", text("fixed")),
	}
	evt, err := cli.ParseWebMessage(pnJID(bobPN), web)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Info.ID != "ORIG" {
		t.Fatalf("whatsmeow gave the id %q: the premise of the test is wrong", evt.Info.ID)
	}
	if cli.DangerousInternals().DispatchEvent(evt) {
		t.Fatal("not acknowledged")
	}
	m, err := r.db.Message(context.Background(), "personal", bobPNChat, "ORIG")
	if err != nil || m.Text != "fixed" || m.EditedAt.IsZero() {
		t.Errorf("message %+v, %v; want the edited one", m, err)
	}
}

// TestReceiveRefusedResendIsLogged: a message of the phone's resend that the archive
// refuses is not given again whatever the handler says, and the log does not promise
// that it will be; one that came the usual way is given again, and the log says so.
func TestReceiveRefusedResendIsLogged(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(*events.Message)
		lost    bool
	}{
		"a web message":                {func(*events.Message) {}, true}, // what ParseWebMessage makes of one
		"the answer to a request":      {func(e *events.Message) { e.SourceWebMsg = nil; e.UnavailableRequestID = "REQ1" }, true},
		"a message that came as usual": {func(e *events.Message) { e.SourceWebMsg = nil }, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRx(t, refuseSQL)
			r.refuse(t, refuseChats)
			cli := r.cli("personal")
			web := &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{RemoteJID: proto.String(bobPNChat), FromMe: proto.Bool(false), ID: proto.String("M1")},
				MessageTimestamp: proto.Uint64(uint64(t0.Unix())),
				Message:          text("SECRET-TEXT"),
			}
			evt, err := cli.ParseWebMessage(pnJID(bobPN), web)
			if err != nil {
				t.Fatal(err)
			}
			tc.prepare(evt)
			if !cli.DangerousInternals().DispatchEvent(evt) {
				t.Fatal("the message was acknowledged")
			}
			out := r.logs.String()
			lost, promised := strings.Contains(out, "is not delivered again: it is lost"), strings.Contains(out, "will be delivered again")
			if lost != tc.lost || promised == tc.lost || strings.Contains(out, "SECRET") {
				t.Errorf("lost %v, promised %v, want lost %v:\n%s", lost, promised, tc.lost, out)
			}
		})
	}
}

// TestReceiveOwnMessageToMetaAI: our own message to Meta AI is in a chat that is
// the bot's, and not for the archive.
func TestReceiveOwnMessageToMetaAI(t *testing.T) {
	r := newRx(t)
	r.mustDeliver(t, "personal", at(outgoing("B1", pnJID("13135550002")), time.Second), text("question for the bot"))
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v after a message of ours to Meta AI", n)
	}
}
