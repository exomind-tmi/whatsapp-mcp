package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// gateOnce is a channel that a test cleanup closes, once, so that a hook that waits for
// it does not outlive the test.
func gateOnce(t *testing.T) (gate chan struct{}, open func()) {
	t.Helper()
	gate = make(chan struct{})
	var once sync.Once
	open = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	return gate, open
}

// TestHistoryStartSignalsTheWorkerThatIsThere: a start that finds a worker tells it to look
// again, which is all that keeps a notification queued while the worker is deciding to
// end from waiting for the next connection.
func TestHistoryStartSignalsTheWorkerThatIsThere(t *testing.T) {
	x := newHx(t, 100)
	x.m.mu.Lock()
	a := x.m.accounts["personal"]
	w := &historyWorker{wake: make(chan struct{}, 1)}
	a.history = w
	x.m.mu.Unlock()
	x.m.startHistory(a)
	if !x.m.historyMore(a, w) {
		t.Error("a worker that a start has told to look again is let end")
	}
}

// TestHistoryKeepAliveRestoredStartsTheWorker: the connection that came back without a new
// Connected (the keepalive had timed out) is a reason to import what waits.
func TestHistoryKeepAliveRestoredStartsTheWorker(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	x.announce(t, "personal", "H1", "/h/1") // the account is reconnecting: the worker that this starts ends at once
	x.idle(t, "personal")
	x.cli("personal").DangerousInternals().DispatchEvent(&events.KeepAliveRestored{})
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
		t.Errorf("Bob's messages %v", got)
	}
}

// TestHistoryCloseCancelsTheDownload: Close does not wait for a download that goes on.
func TestHistoryCloseCancelsTheDownload(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	gate, _ := gateOnce(t)
	started := make(chan struct{})
	x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
			return errors.New("released by the test")
		}
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the download", started)
	x.m.closeWait = 10 * time.Second // Close ends when the worker has, or not for this long
	closed := make(chan struct{})
	go func() { x.m.Close(); close(closed) }()
	waitFor(t, "Close to return", closed)
	if attempts, _, ok := x.queueRow(t, "personal"); !ok || attempts != 0 {
		t.Errorf("the queue row: %d attempts, %v; want the notification there, and not blamed", attempts, ok)
	}
}

// TestHistoryCloseWhileItWaitsToRetry: Close does not wait for the pause after a failed
// import.
func TestHistoryCloseWhileItWaitsToRetry(t *testing.T) {
	x := newHx(t, 100)
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
	x.bobBlob("/h/1", "G1")
	x.h.failNext("/h/1", errors.New("the media server is down"))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.within(t, 5*time.Second, "the failed attempt to be logged", func() bool {
		return strings.Contains(x.logs.String(), "history import failed; trying again later")
	})
	x.m.closeWait = 10 * time.Second
	closed := make(chan struct{})
	go func() { x.m.Close(); close(closed) }()
	waitFor(t, "Close to return", closed)
}

// TestHistoryQueueReadFailureIsPaused: a queue that cannot be read is tried again after the
// pause and not in a loop that fills the log.
func TestHistoryQueueReadFailureIsPaused(t *testing.T) {
	x := newHx(t, 100, `DROP TABLE history_queue`)
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
	x.connect("personal")
	const msg = "read the history queue"
	x.within(t, 5*time.Second, "the failed read", func() bool { return strings.Contains(x.logs.String(), msg) })
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(x.logs.String(), msg); n != 1 {
		t.Errorf("the queue was read %d times in the time of one pause", n)
	}
}

// TestHistoryClientChangedDuringAFailingDownloadIsNotAnAttempt: the download of a client that
// the account no longer has fails: nothing is counted against the notification, no pause
// is put before the next, and the account's client imports it at once.
func TestHistoryClientChangedDuringAFailingDownloadIsNotAnAttempt(t *testing.T) {
	x := newHx(t, 100)
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
	x.bobBlob("/h/1", "G1")
	old := x.cli("personal")
	t.Cleanup(func() { // lets Close disconnect each client once
		x.m.mu.Lock()
		x.m.accounts["personal"].cli = old
		x.m.mu.Unlock()
	})
	var swapped atomic.Bool
	x.h.onDownload = func(_ context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		if !swapped.CompareAndSwap(false, true) {
			return nil
		}
		x.m.mu.Lock() // the account's client is replaced, nobody cancels this attempt, and it fails
		x.m.accounts["personal"].cli = x.m.accounts["work"].cli
		x.m.mu.Unlock()
		return errors.New("connection reset")
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.within(t, 5*time.Second, "the notification to be imported by the client the account has", func() bool {
		return x.queued(t, "personal") == 0
	})
}

// TestHistoryDownloadErrorsAreToldApart: what last_error says depends on why the download failed.
func TestHistoryDownloadErrorsAreToldApart(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{whatsmeow.ErrMediaDownloadFailedWith403, causeGone},
		{whatsmeow.ErrMediaDownloadFailedWith404, causeGone},
		{whatsmeow.ErrMediaDownloadFailedWith410, causeGone},
		{fmt.Errorf("failed to download: %w", whatsmeow.ErrMediaDownloadFailedWith410), causeGone},
		{context.DeadlineExceeded, causeTimeout},
		{io.ErrUnexpectedEOF, causeDownload},
		{errors.New("connection reset"), causeDownload},
	} {
		if got := causeOf(downloadError(tc.err)); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.want)
		}
	}
	if got := causeOf(errors.New("not ours")); got != causeInternal {
		t.Errorf("an error that is no import's: %q", got)
	}
}

// TestHistoryDownloadIsBounded: a download that goes on past historyWait is a failed attempt.
func TestHistoryDownloadIsBounded(t *testing.T) {
	x := newHx(t, 100)
	x.m.historyWait = 30 * time.Millisecond
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
	x.bobBlob("/h/1", "G1")
	gate, _ := gateOnce(t)
	x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
			return nil
		}
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.within(t, 5*time.Second, "the attempt to be counted", func() bool {
		a, _, ok := x.queueRow(t, "personal")
		return ok && a == 1
	})
	if _, last, _ := x.queueRow(t, "personal"); last != causeTimeout {
		t.Errorf("last_error %q, want %q", last, causeTimeout)
	}
}

// TestHistoryUnreadableNotification: a queued notification that does not unmarshal is
// given up on, with fixed words, and nothing is downloaded for it: it would be as
// unreadable at the next try.
func TestHistoryUnreadableNotification(t *testing.T) {
	x := newHx(t, 100)
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
	if err := x.db.QueuePush(context.Background(), "personal", "H1", []byte{0x0a, 0xff}); err != nil {
		t.Fatal(err)
	}
	x.connect("personal")
	x.within(t, 5*time.Second, "the notification to be given up on", func() bool {
		a, _, ok := x.queueRow(t, "personal")
		return ok && a == archive.MaxAttempts
	})
	if _, last, _ := x.queueRow(t, "personal"); last != causeUnreadable {
		t.Errorf("last_error %q, want %q", last, causeUnreadable)
	}
	if n := len(x.h.downloads()); n != 0 {
		t.Errorf("%d downloads for a notification that cannot be read", n)
	}
}

// TestHistoryBlobIsDeletedAfterTheLastCommit: when the blob is deleted from the server the
// whole of it is in the archive and the notification is off the queue; and a last chunk
// that fails deletes nothing.
func TestHistoryBlobIsDeletedAfterTheLastCommit(t *testing.T) {
	t.Run("what the archive holds when it is deleted", func(t *testing.T) {
		x := newHx(t, 2)
		x.bobBlob("/h/1", "M1", "M2", "M3", "M4", "M5")
		type snap struct{ queued, messages int }
		var mu sync.Mutex
		var snaps []snap
		x.h.onDelete = func(*waE2E.HistorySyncNotification) {
			_, found, _ := x.db.QueueNext(context.Background(), "personal")
			page, _ := x.db.Messages(context.Background(), archive.MsgQuery{Account: "personal", Chat: bobChat, Limit: 100})
			s := snap{messages: len(page.Messages)}
			if found {
				s.queued = 1
			}
			mu.Lock()
			snaps = append(snaps, s)
			mu.Unlock()
		}
		x.connect("personal")
		x.announce(t, "personal", "H1", "/h/1")
		x.imported(t, "personal")
		mu.Lock()
		defer mu.Unlock()
		if len(snaps) != 1 || snaps[0] != (snap{queued: 0, messages: 5}) {
			t.Errorf("the archive at the time of the delete: %+v, want one, with all 5 messages and the queue empty", snaps)
		}
	})
	t.Run("a last chunk that fails", func(t *testing.T) {
		x := newHx(t, 100, refuseSQL) // one chunk, which is the last
		x.bobBlob("/h/1", "M1", "M2", "M3")
		x.refuse(t, refuseMessageM)
		x.connect("personal")
		x.announce(t, "personal", "H1", "/h/1")
		x.imported2(t, "personal", "the notification to be given up on")
		if n := len(x.h.deletes()); n != 0 {
			t.Errorf("the blob is deleted from the server %d times though its last chunk was never written", n)
		}
		if got := x.size(t, "personal"); got != [2]int{} {
			t.Errorf("the archive has %v chats and messages of a chunk that failed", got)
		}
	})
}

// TestHistoryOtherAccountsChatsAreLeftAlone: the history of one account files its chats by
// LID and does not touch what another account has of the same person.
func TestHistoryOtherAccountsChatsAreLeftAlone(t *testing.T) {
	x := newHx(t, 100)
	x.mustDeliver(t, "work", at(incoming("W1", pnJID(carolPN)), time.Second), text("work's message"))
	lid := carolLID + "@lid"
	x.h.put("/h/1", withPairs(hblob(hconv(lid, "Carol", hin(lid, "C1", "a greeting", 5*time.Second))), carolLID, carolPN))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	if got := ids(x.msgs(t, "personal", lid)); !equalIDs(got, []string{"C1"}) {
		t.Errorf("personal has %v under Carol's LID", got)
	}
	if got := ids(x.msgs(t, "work", carolChat)); !equalIDs(got, []string{"W1"}) {
		t.Errorf("work has %v under Carol's number: its chat was moved by another account's import", got)
	}
	if got := ids(x.msgs(t, "work", lid)); len(got) != 0 {
		t.Errorf("work has %v under Carol's LID", got)
	}
}

// groupSyncOver waits for the fetch of the groups that a Connected starts, which files
// the chats by what the store knows then.
func (x *hx) groupSyncOver(t *testing.T, nick string) {
	t.Helper()
	x.within(t, 5*time.Second, "the group sync to be over", func() bool {
		x.m.mu.Lock()
		defer x.m.mu.Unlock()
		return !x.m.accounts[nick].syncingGroups
	})
}

// TestHistoryPairsFileTheChatsByTheImportAlone: as TestHistoryPairsFileTheChatsThatHaveNoMessageInIt,
// with the group sync of the connection out of the way, which would otherwise file the chat
// if it ran after the download and hide a worker that does not.
func TestHistoryPairsFileTheChatsByTheImportAlone(t *testing.T) {
	x := newHx(t, 100)
	x.mustDeliver(t, "personal", at(incoming("C1", pnJID(carolPN)), time.Second), text("sent by number"))
	x.h.put("/h/1", withPairs(hblob(), carolLID, carolPN))
	x.connect("personal")
	x.groupSyncOver(t, "personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	cs := x.chats(t, "personal")
	if len(cs) != 1 || cs[0].JID != carolLID+"@lid" || cs[0].PN != carolChat {
		t.Fatalf("chats %+v, want one under her LID, with her number", cs)
	}
}

// TestHistoryNamesOfConversations: the phone's name of a chat, trimmed, else its display
// name; and no name at all is none.
func TestHistoryNamesOfConversations(t *testing.T) {
	x := newHx(t, 100)
	dave := "70000000555@s.whatsapp.net"
	conv := func(id string, name, display *string) *waHistorySync.Conversation {
		c := hconv(id, "", hin(id, "X-"+id[:8], "hello", time.Second))
		c.Name, c.DisplayName = name, display
		return c
	}
	x.h.put("/h/1", hblob(
		conv(bobChat, nil, proto.String("Bob Display")),
		conv(carolChat, proto.String("  Carol  "), proto.String("ignored")),
		conv(dave, proto.String("   "), proto.String(" Dave ")),
	))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	names := map[string]string{}
	for _, c := range x.chats(t, "personal") {
		names[c.JID] = c.Name
	}
	want := map[string]string{bobChat: "Bob Display", carolChat: "Carol", dave: "Dave"}
	for jid, name := range want {
		if names[jid] != name {
			t.Errorf("the chat %s is named %q, want %q", jid, names[jid], name)
		}
	}
}

// TestHistoryChunksAreWrittenInTheChatsFinalPlace: what a chunk writes is under the chat's LID
// from the commit of the chunk, as are the rows that were under its number before, and not
// only once the whole notification is in.
func TestHistoryChunksAreWrittenInTheChatsFinalPlace(t *testing.T) {
	x := newHx(t, 3)
	x.mustDeliver(t, "personal", at(incoming("L1", pnJID(carolPN)), time.Second), text("live, by number"))
	var msgs []*waHistorySync.HistorySyncMsg
	for i := 1; i <= 8; i++ {
		msgs = append(msgs, hin(carolChat, fmt.Sprintf("C%d", i), "an old one", time.Duration(i+1)*time.Second))
	}
	x.h.put("/h/1", withPairs(hblob(hconv(carolChat, "Carol", msgs...)), carolLID, carolPN))
	reached, release := x.pauseAt(t, 6) // the chat and C1, C2 are committed, and C3 to C5 are not
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the first chunk", reached)
	lid := carolLID + "@lid"
	if got := ids(x.msgs(t, "personal", lid)); !equalIDs(got, []string{"L1", "C1", "C2"}) {
		t.Errorf("under her LID after the first chunk: %v", got)
	}
	if got := ids(x.msgs(t, "personal", carolChat)); len(got) != 0 {
		t.Errorf("under her number after the first chunk: %v", got)
	}
	release()
	x.imported(t, "personal")
}

// TestHistoryLogCounts: the log of an import says what it did, in numbers.
func TestHistoryLogCounts(t *testing.T) {
	x := newHx(t, 2)
	nested := &waE2E.Message{ProtocolMessage: notice("/h/inner").ProtocolMessage}
	x.h.put("/h/1", hblob(
		hconv(bobChat, "Bob",
			hin(bobChat, "M1", "one", 1*time.Second), hin(bobChat, "M2", "two", 2*time.Second), hin(bobChat, "M3", "three", 3*time.Second),
			hin(bobChat, "M4", "four", 4*time.Second), hin(bobChat, "M5", "five", 5*time.Second),
			hmsg(bobChat, "R1", false, "", 6*time.Second, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("x")}}),
			hmsg(bobChat, "S1", false, "", 7*time.Second, nil),
			hmsg(bobChat, "N1", true, "", 8*time.Second, nested)),
		hconv(teamChat, "Team", hmsg(teamChat, "G1", false, "", 2*time.Second, text("from nobody"))),
	))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	log := x.logs.String()
	for _, want := range []string{"conversations=2", "messages=5", "skipped=3", "unparsed=1", "chunks=4"} {
		if !strings.Contains(log, want) {
			t.Errorf("the import's log lacks %s:\n%s", want, log)
		}
	}
}

// TestHistoryReceiptGoesByTheClientThatGotTheAnnouncement: the phone is told by the connection that
// brought the message.
func TestHistoryReceiptGoesByTheClientThatGotTheAnnouncement(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	x.within(t, 5*time.Second, "the receipt", func() bool { return len(x.h.receipted()) == 1 })
	x.h.mu.Lock()
	defer x.h.mu.Unlock()
	if len(x.h.receiptBy) != 1 || x.h.receiptBy[0] != x.cli("personal") {
		t.Errorf("the receipt went out by %v, want the account's client", x.h.receiptBy)
	}
}

// TestHistoryConversationWithAnUnreadableJID: a conversation whose id is not a JID is left out
// with its messages, and the rest of the notification goes in.
func TestHistoryConversationWithAnUnreadableJID(t *testing.T) {
	x := newHx(t, 100)
	bad := "7.0.1@s.whatsapp.net" // whatsmeow's ParseJID refuses two dots
	x.h.put("/h/1", hblob(
		hconv(bad, "Nobody", hin(bad, "X1", "nowhere", time.Second)),
		hconv(bobChat, "Bob", hin(bobChat, "M1", "fine", 2*time.Second)),
	))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.imported(t, "personal")
	if got := x.size(t, "personal"); got != [2]int{1, 1} {
		t.Errorf("the archive has %v chats and messages, want Bob's chat and message only", got)
	}
}

// TestHistoryReadyForTheAccountThatIsThereNotAnotherOfTheNick: a worker whose account was replaced by
// another of the same nick is not ready for it.
func TestHistoryReadyForTheAccountThatIsThereNotAnotherOfTheNick(t *testing.T) {
	x := newHx(t, 100)
	forceStatus(x.m, "personal", StatusConnected)
	x.m.mu.Lock()
	defer x.m.mu.Unlock()
	old := x.m.accounts["personal"]
	x.m.accounts["personal"] = x.m.accounts["work"]
	defer func() { x.m.accounts["personal"] = old }()
	if _, ok := x.m.historyReady(old); ok {
		t.Error("the worker of an account that has been replaced under its nick is ready")
	}
}

// TestHistoryRoundLooksAgainAtTheEnd: a worker that finds the queue empty and has been signalled
// meanwhile (a notification was queued after it asked) goes on, and one that has not been ends
// and is forgotten.
func TestHistoryRoundLooksAgainAtTheEnd(t *testing.T) {
	x := newHx(t, 100)
	forceStatus(x.m, "personal", StatusConnected)
	x.m.mu.Lock()
	a := x.m.accounts["personal"]
	w := &historyWorker{wake: make(chan struct{}, 1)}
	a.history = w
	x.m.mu.Unlock()
	w.signal()
	if !x.m.historyRound(a, w) {
		t.Error("a worker that was signalled while it asked for the next notification ends")
	}
	if x.m.historyRound(a, w) {
		t.Error("a worker with nothing to import and no signal goes on")
	}
	x.m.mu.Lock()
	defer x.m.mu.Unlock()
	if a.history != nil {
		t.Error("a worker that has ended is still the account's")
	}
}

// TestHistoryDefaultChunkIsSmall: the chunk a Manager imports with by default, which is what holds
// the writer while a live message waits, is a few hundred messages and not thousands. The
// interleaving test at 10k messages cannot tell without the race detector, where a transaction
// of the whole import takes longer than its bound.
func TestHistoryDefaultChunkIsSmall(t *testing.T) {
	r := newRx(t) // newHx would set the chunk
	h := newFakeHistory()
	r.fn.history = h
	x := &hx{rx: r, h: h}
	if r.m.historyChunk != historyChunk || historyChunk > 1000 {
		t.Fatalf("the Manager imports in chunks of %d, the constant is %d: want at most 1000", r.m.historyChunk, historyChunk)
	}
	x.h.put("/h/1", bulk(1, 1300))
	reached, release := x.pauseAt(t, 1100) // two full chunks of the default are in, a chunk of a thousand or more is not
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	select { // a minute, not waitFor's five seconds: under the race detector a thousand messages take that long
	case <-reached:
	case <-time.After(time.Minute):
		t.Fatal("timed out waiting for the import to be under way")
	}
	if got := x.size(t, "personal"); got[1] < 500 || got[1] >= 1100 {
		t.Errorf("the archive has %v chats and messages while the 1100th is read: want the chunks before it", got)
	}
	release()
	x.imported(t, "personal")
}
