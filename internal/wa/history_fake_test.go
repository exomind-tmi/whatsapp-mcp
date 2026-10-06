package wa

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
)

// fakeHistory is WhatsApp as the history worker meets it: the blobs the phone has
// uploaded, which a download returns by the direct path of the notification that
// names it, and the record of what the worker asked for. Built from real
// waHistorySync protos, so that whatsmeow's own parser reads them as it reads the
// phone's.
type fakeHistory struct {
	mu sync.Mutex

	blobs    map[string]*waHistorySync.HistorySync // by direct path
	failures map[string][]error                    // by direct path: what the next downloads fail with before one succeeds

	// The hooks run in the seams, on the worker's goroutine and outside mu, and make
	// a test's moment: to block the worker, to cut it off, to break the connection.
	onDownload func(ctx context.Context, cli *whatsmeow.Client, n *waE2E.HistorySyncNotification) error // a non-nil result is the download's error
	onParse    func(n int)                                                                              // before the n-th parse of the test (1-based)
	onDelete   func(n *waE2E.HistorySyncNotification)
	deleteErr  error // what the delete of a blob from the server fails with
	onReceipt  func(id string) error

	calls     []downloadCall
	deleted   []*waE2E.HistorySyncNotification
	receipts  []string
	receiptBy []*whatsmeow.Client // the client each receipt went out by
	parses    int
	order     []string // "download /h/1", "delete /h/1", "receipt H1": what happened, in order
}

type downloadCall struct {
	path string
	cli  *whatsmeow.Client
	sync bool
	at   time.Time
}

func newFakeHistory() *fakeHistory {
	return &fakeHistory{blobs: map[string]*waHistorySync.HistorySync{}, failures: map[string][]error{}}
}

// put makes the blob the phone has uploaded at path.
func (h *fakeHistory) put(path string, blob *waHistorySync.HistorySync) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.blobs[path] = blob
}

// failNext makes the next downloads of path fail with errs.
func (h *fakeHistory) failNext(path string, errs ...error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures[path] = append(h.failures[path], errs...)
}

func (h *fakeHistory) downloads() []downloadCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]downloadCall(nil), h.calls...)
}

func (h *fakeHistory) deletes() []*waE2E.HistorySyncNotification {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*waE2E.HistorySyncNotification(nil), h.deleted...)
}

func (h *fakeHistory) receipted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.receipts...)
}

func (h *fakeHistory) history() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.order...)
}

// download is WhatsApp's answer to DownloadHistorySync. With the storage flag set it
// does what the real call does before it returns: the LID pairs of the blob are in
// the store (message.go:804-806). Without it they are not, as the real call leaves
// them to a goroutine that may be late.
func (h *fakeHistory) download(cli *whatsmeow.Client, ctx context.Context, n *waE2E.HistorySyncNotification, syncStorage bool) (*waHistorySync.HistorySync, error) {
	path := n.GetDirectPath()
	h.mu.Lock()
	h.calls = append(h.calls, downloadCall{path: path, cli: cli, sync: syncStorage, at: time.Now()})
	h.order = append(h.order, "download "+path)
	hook := h.onDownload
	var failure error
	if fs := h.failures[path]; len(fs) > 0 {
		failure, h.failures[path] = fs[0], fs[1:]
	}
	blob := h.blobs[path]
	h.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, cli, n); err != nil {
			return nil, err
		}
	}
	if failure != nil {
		return nil, failure
	}
	if blob == nil {
		return nil, fmt.Errorf("the phone has uploaded nothing at %s", path)
	}
	if syncStorage {
		for _, p := range blob.GetPhoneNumberToLidMappings() {
			pn, _ := types.ParseJID(p.GetPnJID())
			lid, _ := types.ParseJID(p.GetLidJID())
			if err := cli.Store.LIDs.PutLIDMapping(ctx, lid, pn); err != nil {
				return nil, err
			}
		}
	}
	return blob, nil
}

func (f *fakeNet) downloadHistory(cli *whatsmeow.Client, ctx context.Context, n *waE2E.HistorySyncNotification, syncStorage bool) (*waHistorySync.HistorySync, error) {
	if f.history == nil {
		return nil, errors.New("this test has no history")
	}
	return f.history.download(cli, ctx, n, syncStorage)
}

func (f *fakeNet) deleteHistoryMedia(cli *whatsmeow.Client, ctx context.Context, n *waE2E.HistorySyncNotification) error {
	h := f.history
	if h == nil {
		return errors.New("this test has no history")
	}
	h.mu.Lock()
	h.deleted = append(h.deleted, n)
	h.order = append(h.order, "delete "+n.GetDirectPath())
	hook, err := h.onDelete, h.deleteErr
	h.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return err
}

func (f *fakeNet) historyReceipt(cli *whatsmeow.Client, ctx context.Context, id types.MessageID) error {
	h := f.history
	if h == nil {
		return nil // the tests that have no history queue notices and do not look at receipts
	}
	h.mu.Lock()
	h.receipts = append(h.receipts, string(id))
	h.receiptBy = append(h.receiptBy, cli)
	h.order = append(h.order, "receipt "+string(id))
	hook := h.onReceipt
	h.mu.Unlock()
	if hook != nil {
		return hook(string(id))
	}
	return nil
}

// parseWebMessage is whatsmeow's own: the parser reads the client's ids and goes to
// no server. It counts, for the tests that cut an import off in the middle.
func (f *fakeNet) parseWebMessage(cli *whatsmeow.Client, chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error) {
	if h := f.history; h != nil {
		h.mu.Lock()
		h.parses++
		n, hook := h.parses, h.onParse
		h.mu.Unlock()
		if hook != nil {
			hook(n)
		}
	}
	return cli.ParseWebMessage(chat, msg)
}

// The blobs of the tests.

// hmsg is a message of a history as the phone stores it: in chat (a JID), sent by
// participant if it is a group's, at d after t0.
func hmsg(chat, id string, fromMe bool, participant string, d time.Duration, m *waE2E.Message) *waHistorySync.HistorySyncMsg {
	key := &waCommon.MessageKey{RemoteJID: proto.String(chat), FromMe: proto.Bool(fromMe), ID: proto.String(id)}
	if participant != "" {
		key.Participant = proto.String(participant)
	}
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key: key, Message: m, MessageTimestamp: proto.Uint64(uint64(t0.Add(d).Unix())),
	}}
}

// pushed is hm from a contact who calls themselves name.
func pushed(hm *waHistorySync.HistorySyncMsg, name string) *waHistorySync.HistorySyncMsg {
	hm.Message.PushName = proto.String(name)
	return hm
}

// hin is a text that the other person of a private chat sent.
func hin(chat, id, body string, d time.Duration) *waHistorySync.HistorySyncMsg {
	return hmsg(chat, id, false, "", d, text(body))
}

// hout is a text of ours.
func hout(chat, id, body string, d time.Duration) *waHistorySync.HistorySyncMsg {
	return hmsg(chat, id, true, "", d, text(body))
}

// hconv is a conversation; name "" is one the phone gives none.
func hconv(id, name string, msgs ...*waHistorySync.HistorySyncMsg) *waHistorySync.Conversation {
	c := &waHistorySync.Conversation{ID: proto.String(id), Messages: msgs}
	if name != "" {
		c.Name = proto.String(name)
	}
	return c
}

func hblob(convs ...*waHistorySync.Conversation) *waHistorySync.HistorySync {
	return &waHistorySync.HistorySync{
		SyncType:      waHistorySync.HistorySync_RECENT.Enum(),
		ChunkOrder:    proto.Uint32(1),
		Conversations: convs,
	}
}

// withPairs is blob that carries the LID pairs of the people (lid, pn, lid, pn, ...).
func withPairs(blob *waHistorySync.HistorySync, pairs ...string) *waHistorySync.HistorySync {
	for i := 0; i < len(pairs); i += 2 {
		blob.PhoneNumberToLidMappings = append(blob.PhoneNumberToLidMappings, &waHistorySync.PhoneNumberToLIDMapping{
			LidJID: proto.String(pairs[i] + "@lid"), PnJID: proto.String(pairs[i+1] + "@s.whatsapp.net"),
		})
	}
	return blob
}

// notice is the announcement of the blob at path, as the phone's own device sends
// it: what the handler of a live message queues.
func notice(path string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &waE2E.HistorySyncNotification{
			SyncType: waE2E.HistorySyncType_RECENT.Enum(), ChunkOrder: proto.Uint32(1),
			DirectPath: proto.String(path), FileEncSHA256: []byte("enc" + path), EncHandle: proto.String("handle" + path),
		},
	}}
}

// noticeOf is the announcement n, as the message that carries it.
func noticeOf(n *waE2E.HistorySyncNotification) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:                    waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: n,
	}}
}

// inline is the announcement that carries blob itself, as the first one after a link
// does: compressed as the phone does, for whatsmeow's own DownloadHistorySync to read.
func inline(t *testing.T, blob *waHistorySync.HistorySync) *waE2E.HistorySyncNotification {
	t.Helper()
	raw, err := proto.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &waE2E.HistorySyncNotification{
		SyncType: waE2E.HistorySyncType_INITIAL_BOOTSTRAP.Enum(), InitialHistBootstrapInlinePayload: buf.Bytes(),
	}
}

// The tests of the worker.

// hx is an rx with the phone's history behind its network.
type hx struct {
	*rx
	h *fakeHistory
}

// newHx is an rx whose personal account is connected, and whose worker has chunks of
// chunk messages. The log is what the tests read the worker's account of itself from.
func newHx(t *testing.T, chunk int, prep ...string) *hx {
	t.Helper()
	r := newRx(t, prep...)
	h := newFakeHistory()
	r.fn.history = h
	r.m.historyChunk = chunk
	r.m.historyRetry, r.m.historyRetryMax = time.Millisecond, 4*time.Millisecond
	return &hx{rx: r, h: h}
}

// connect has the client of the account say that it is connected, which is what
// starts its worker.
func (x *hx) connect(nick string) {
	x.cli(nick).DangerousInternals().DispatchEvent(&events.Connected{})
}

// announce delivers the notice of the blob at path as the message id, and fails
// the test if it was not acknowledged.
func (x *hx) announce(t *testing.T, nick, id, path string) {
	t.Helper()
	x.mustDeliver(t, nick, outgoing(id, pnJID(ownPN)), notice(path))
}

// queued is how many notifications wait in the queue of the account.
func (x *hx) queued(t *testing.T, nick string) int {
	t.Helper()
	return x.rowsOf(t, "history_queue", nick)
}

// idle waits until the worker of the account has ended: it has imported what
// there was, and ends when there is no more.
func (x *hx) idle(t *testing.T, nick string) {
	t.Helper()
	x.within(t, 30*time.Second, "the history worker to end", func() bool {
		x.m.mu.Lock()
		defer x.m.mu.Unlock()
		a := x.m.accounts[nick]
		return a == nil || a.history == nil
	})
}

// within is eventually with a limit of the test's choosing: an import of thousands
// of messages takes longer than two seconds under the race detector.
func (x *hx) within(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(limit); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; log:\n%s", what, x.logs)
		}
	}
}

// imported waits for the queue of the account to be empty and its worker ended.
func (x *hx) imported(t *testing.T, nick string) {
	t.Helper()
	x.idle(t, nick)
	if n := x.queued(t, nick); n != 0 {
		t.Fatalf("%d notifications are still queued; log:\n%s", n, x.logs)
	}
}

// pauseAt holds the worker at the n-th message it parses, before the parse, until
// release is called (once, or never, which a test cleanup does): at that moment the
// chunks before the one the message is in are committed and it is not. reached is
// closed when the worker is there.
func (x *hx) pauseAt(t *testing.T, n int) (reached <-chan struct{}, release func()) {
	t.Helper()
	at, gate := make(chan struct{}), make(chan struct{})
	var once, free sync.Once
	x.h.mu.Lock()
	x.h.onParse = func(i int) {
		if i == n {
			once.Do(func() { close(at) })
			<-gate
		}
	}
	x.h.mu.Unlock()
	release = func() { free.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return at, release
}

// waitFor waits up to 5 s for a channel to be closed.
func waitFor(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// queueRow is what the queue holds of the account's oldest notification: how many
// times it has failed, and what it said of the last. ok is false if the queue is
// empty.
func (x *hx) queueRow(t *testing.T, nick string) (attempts int, lastError string, ok bool) {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(x.dir, "archive.db"), sqlitedb.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var last sql.NullString
	err = db.QueryRow(`SELECT attempts, last_error FROM history_queue WHERE account = ? ORDER BY id LIMIT 1`, nick).Scan(&attempts, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return attempts, last.String, true
}

// phoneNumber is the n-th number of the bulk blobs.
func phoneNumber(n int) string { return fmt.Sprintf("7000100%04d", n) }

// bulk is a blob of convs private conversations of per messages each, texts all
// different, at times that go up.
func bulk(convs, per int) *waHistorySync.HistorySync {
	blob := hblob()
	for c := range convs {
		chat := phoneNumber(c) + "@s.whatsapp.net"
		msgs := make([]*waHistorySync.HistorySyncMsg, 0, per)
		for i := range per {
			id := fmt.Sprintf("B%d-%d", c, i)
			d := time.Duration(c*per+i) * time.Second
			if i%2 == 0 {
				msgs = append(msgs, hin(chat, id, "message "+id, d))
			} else {
				msgs = append(msgs, hout(chat, id, "answer "+id, d))
			}
		}
		blob.Conversations = append(blob.Conversations, hconv(chat, fmt.Sprintf("Contact %d", c), msgs...))
	}
	return blob
}
