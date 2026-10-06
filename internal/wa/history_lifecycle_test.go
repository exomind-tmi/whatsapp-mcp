package wa

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// TestHistoryOneWorkerPerAccount: a second Connected, the connection restored, and
// a notification that comes while the worker is at work, do not start a second
// worker: the one is woken, and two imports never run at once for an account.
func TestHistoryOneWorkerPerAccount(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	x.bobBlob("/h/2", "G2")
	var inflight, peak atomic.Int32
	gate := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		cur := inflight.Add(1)
		for p := peak.Load(); cur > p && !peak.CompareAndSwap(p, cur); p = peak.Load() {
		}
		<-gate
		inflight.Add(-1)
		return nil
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.within(t, 5*time.Second, "the first download", func() bool { return inflight.Load() == 1 })
	x.m.mu.Lock()
	first := x.m.accounts["personal"].history
	x.m.mu.Unlock()

	cli := x.cli("personal").DangerousInternals()
	for range 5 {
		cli.DispatchEvent(&events.Connected{})
		cli.DispatchEvent(&events.KeepAliveRestored{})
	}
	x.announce(t, "personal", "H2", "/h/2")
	x.m.mu.Lock()
	same := x.m.accounts["personal"].history == first
	x.m.mu.Unlock()
	if first == nil || !same {
		t.Error("the account has another worker now")
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	x.imported(t, "personal")
	if p := peak.Load(); p != 1 {
		t.Errorf("%d imports ran at once", p)
	}
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1", "G2"}) {
		t.Errorf("Bob's messages %v: the notification that came meanwhile is imported too", got)
	}
}

// TestHistoryReady: the worker works for an account that is known, connected, has a
// device, and is neither being removed nor having its device taken away, with the
// client the account has now, and for no other.
func TestHistoryReady(t *testing.T) {
	x := newHx(t, 100)
	forceStatus(x.m, "personal", StatusConnected)
	x.m.mu.Lock()
	defer x.m.mu.Unlock()
	a := x.m.accounts["personal"]
	own := a.cli
	if cli, ok := x.m.historyReady(a); !ok || cli != own || cli == nil {
		t.Fatalf("the account is not ready: %v", ok)
	}
	for name, tc := range map[string]struct {
		set, unset func()
	}{
		"being removed":    {func() { a.removing = true }, func() { a.removing = false }},
		"being unlinked":   {func() { a.unlinking = &unlink{done: make(chan struct{})} }, func() { a.unlinking = nil }},
		"not connected":    {func() { a.info.Status = StatusReconnecting }, func() { a.info.Status = StatusConnected }},
		"linking":          {func() { a.info.Status = StatusLinking }, func() { a.info.Status = StatusConnected }},
		"without a device": {func() { a.cli = nil }, func() { a.cli = own }},
		"dropped":          {func() { delete(x.m.accounts, "personal") }, func() { x.m.accounts["personal"] = a }},
	} {
		tc.set()
		if _, ok := x.m.historyReady(a); ok {
			t.Errorf("an account that is %s is ready", name)
		}
		tc.unset()
	}
	// The pairing's client is the account's while the pairing owns the status.
	pcli := x.m.accounts["work"].cli
	a.pcli = pcli
	if cli, ok := x.m.historyReady(a); !ok || cli != pcli {
		t.Errorf("with a pairing client the worker uses %p, ready %v; want %p", cli, ok, pcli)
	}
	a.pcli = nil
	x.m.cancel() // closing
	if _, ok := x.m.historyReady(a); ok {
		t.Error("an account of a Manager that is closing is ready")
	}
}

// TestHistoryCountsGrowWhileItImports: list shows the chats and messages as the
// chunks are committed, with the status as it was, the other accounts' too.
func TestHistoryCountsGrowWhileItImports(t *testing.T) {
	x := newHx(t, 10)
	x.h.put("/h/1", bulk(10, 30)) // 300 messages
	reached, release := x.pauseAt(t, 150)
	pair(t, x.m, "fresh") // an account that is linking, beside it
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the import to be half done", reached)

	mid := x.listed("personal")
	if mid.Messages == 0 || mid.Messages >= 300 || mid.Chats == 0 || mid.Status != StatusConnected || mid.Reason != "" {
		t.Errorf("list shows %+v while the import is half done: want some of the 300 messages, and the status as it was", mid)
	}
	if got := x.listed("fresh").Status; got != StatusLinking {
		t.Errorf("the account that is linking is %s", got)
	}
	release()
	x.imported(t, "personal")
	if end := x.listed("personal"); end.Messages != 300 || end.Chats != 10 || end.Status != StatusConnected {
		t.Errorf("list shows %+v after it, want 10 chats and 300 messages", end)
	}
	if got := x.listed("fresh").Status; got != StatusLinking {
		t.Errorf("the account that is linking is %s", got)
	}
}

// TestHistoryCrashBetweenChunks: the daemon dies after the first chunk is committed
// and before the second: the notification is still queued, not counted against, and
// the chunk is in the archive. The next start imports it again from the beginning,
// and the archive has each message once, whole: nothing lost, nothing doubled.
func TestHistoryCrashBetweenChunks(t *testing.T) {
	x := newHx(t, 5)
	x.h.put("/h/1", bulk(1, 20))
	reached, release := x.pauseAt(t, 6) // the first chunk is the chat and four messages
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the first chunk", reached)

	x.m.cancel() // what is left of the process
	release()
	x.m.Close()

	if got := x.size(t, "personal"); got != [2]int{1, 4} {
		t.Fatalf("after the crash the archive has %v chats and messages, want what the first chunk wrote", got)
	}
	if attempts, _, ok := x.queueRow(t, "personal"); !ok || attempts != 0 {
		t.Errorf("the queue row: %d attempts, %v; want the notification there, and not blamed", attempts, ok)
	}
	if n := len(x.h.deletes()); n != 0 {
		t.Errorf("the blob is deleted from the server after a crash in the middle of it (%d times)", n)
	}

	// The daemon starts again, on the same files.
	m2 := x.fixture.start(t, x.fn.network(readyGlobals()))
	m2.now = func() time.Time { return x.now }
	m2.historyChunk = 5
	m2.historyRetry, m2.historyRetryMax = time.Millisecond, 4*time.Millisecond
	y := &hx{rx: &rx{fixture: x.fixture, m: m2, fn: x.fn, logs: x.logs, now: x.now}, h: x.h}
	y.h.mu.Lock()
	y.h.onParse = nil
	y.h.mu.Unlock()
	y.connect("personal") // the rows left by the run before are imported at the connection
	y.imported(t, "personal")

	if got := y.size(t, "personal"); got != [2]int{1, 20} {
		t.Errorf("after the restart the archive has %v chats and messages, want the 20 once", got)
	}
	got := y.msgs(t, "personal", phoneNumber(0)+"@s.whatsapp.net")
	for i, m := range got {
		if want := fmt.Sprintf("B0-%d", i); m.ID != want {
			t.Errorf("message %d is %s, want %s", i, m.ID, want)
		}
	}
	if n := len(y.h.deletes()); n != 1 {
		t.Errorf("the blob is deleted %d times, want once, after the last chunk", n)
	}
	if n := len(y.h.downloads()); n != 2 {
		t.Errorf("%d downloads, want the one before the crash and the one after", n)
	}
}

// TestHistoryManyNotificationsInOrder: notifications that wait are imported oldest
// first, one after the other, and one that comes while the worker is at it goes to the
// end of the line.
func TestHistoryManyNotificationsInOrder(t *testing.T) {
	x := newHx(t, 100)
	const n = 6
	for i := 1; i <= n+1; i++ {
		x.bobBlob(fmt.Sprintf("/h/%d", i), fmt.Sprintf("A%d", i), fmt.Sprintf("B%d", i))
		if i <= n {
			x.announce(t, "personal", fmt.Sprintf("H%d", i), fmt.Sprintf("/h/%d", i))
		}
	}
	var late atomic.Bool
	x.h.onDownload = func(_ context.Context, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification) error {
		if notif.GetDirectPath() == "/h/3" && late.CompareAndSwap(false, true) { // while it is at the third
			if !send(cli, outgoing(fmt.Sprintf("H%d", n+1), pnJID(ownPN)), notice(fmt.Sprintf("/h/%d", n+1))) {
				return errors.New("the late notification was not acknowledged")
			}
		}
		return nil
	}
	x.connect("personal")
	x.imported(t, "personal")

	var got []string
	for _, c := range x.h.downloads() {
		got = append(got, c.path)
	}
	want := []string{"/h/1", "/h/2", "/h/3", "/h/4", "/h/5", "/h/6", "/h/7"}
	if !slices.Equal(got, want) {
		t.Errorf("imported in the order %v, want %v", got, want)
	}
	if k := len(x.msgs(t, "personal", bobChat)); k != 2*(n+1) {
		t.Errorf("%d messages, want %d", k, 2*(n+1))
	}
	x.within(t, 5*time.Second, "the receipts", func() bool { return len(x.h.receipted()) == n+1 })
	if r := x.h.receipted(); len(r) != n+1 {
		t.Errorf("receipts %v, want one for each of the %d", r, n+1)
	}
}

// TestHistoryTwoAccountsAtOnce: each account has a worker of its own, and they run
// together: each import waits for the other's to be under way, which two that took
// turns could not do. Each account gets what its phone sent and nothing else.
func TestHistoryTwoAccountsAtOnce(t *testing.T) {
	x := newHx(t, 3)
	x.bobBlob("/h/p", "P1", "P2", "P3", "P4")
	carol := hblob(hconv(carolChat, "Carol", hin(carolChat, "W1", "one", time.Second), hin(carolChat, "W2", "two", 2*time.Second)))
	x.h.put("/h/w", carol)
	var barrier sync.WaitGroup
	barrier.Add(2)
	x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		barrier.Done()
		both := make(chan struct{})
		go func() { barrier.Wait(); close(both) }()
		select {
		case <-both:
			return nil
		case <-time.After(3 * time.Second):
			return errors.New("the other account's import is not under way")
		}
	}
	x.connect("personal")
	x.connect("work")
	x.announce(t, "personal", "H1", "/h/p")
	x.announce(t, "work", "H1", "/h/w")
	x.imported(t, "personal")
	x.imported(t, "work")

	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"P1", "P2", "P3", "P4"}) {
		t.Errorf("personal has %v", got)
	}
	if got := ids(x.msgs(t, "work", carolChat)); !equalIDs(got, []string{"W1", "W2"}) {
		t.Errorf("work has %v", got)
	}
	if n := x.size(t, "personal"); n != [2]int{1, 4} {
		t.Errorf("personal has %v chats and messages", n)
	}
	if n := x.size(t, "work"); n != [2]int{1, 2} {
		t.Errorf("work has %v chats and messages", n)
	}
}

// TestHistoryRemovalMidImport: the account is removed while its history is half in:
// the worker stops, writes no more, leaves no row of the account, and ends (no goroutine
// of the Manager is left); and if its nick is taken by a new account in the meantime,
// the new one gets nothing of the old one's history.
func TestHistoryRemovalMidImport(t *testing.T) {
	setup := func(t *testing.T) (*hx, func(), *account) {
		x := newHx(t, 5)
		x.h.put("/h/1", bulk(1, 40))
		reached, release := x.pauseAt(t, 11)
		x.connect("personal")
		x.announce(t, "personal", "H1", "/h/1")
		waitFor(t, "the import to be under way", reached)
		if got := x.size(t, "personal"); got[1] == 0 || got[1] >= 40 {
			t.Fatalf("the premise: %v chats and messages, want some of the 40 messages", got)
		}
		x.m.mu.Lock()
		old := x.m.accounts["personal"]
		x.m.mu.Unlock()
		return x, release, old
	}
	ended := func(x *hx, old *account) func() bool {
		return func() bool {
			x.m.mu.Lock()
			defer x.m.mu.Unlock()
			return old.history == nil
		}
	}

	t.Run("the account is gone", func(t *testing.T) {
		x, release, old := setup(t)
		if res := remove(t, x.m, "personal"); res.err != nil {
			t.Fatal(res.err)
		}
		release()
		x.within(t, 5*time.Second, "the worker to end", ended(x, old))
		requireEnded(t, x.m)
		if n := x.size(t, "personal"); n != [2]int{} {
			t.Errorf("the removed account has %v chats and messages", n)
		}
		for _, table := range []string{"history_queue", "messages", "chats"} {
			if n := x.rowsOf(t, table, "personal"); n != 0 {
				t.Errorf("%d rows of the removed account in %s", n, table)
			}
		}
		if n := len(x.h.deletes()); n != 0 {
			t.Errorf("the blob of a removed account is deleted from the server (%d times)", n)
		}
		if log := x.logs.String(); strings.Contains(log, "history import failed") {
			t.Errorf("the removal is reported as a failure of the import:\n%s", log)
		}
	})

	t.Run("its nick is taken again", func(t *testing.T) {
		x, release, old := setup(t)
		if res := remove(t, x.m, "personal"); res.err != nil {
			t.Fatal(res.err)
		}
		if _, err := x.m.admit(context.Background(), "personal", kindChat, ""); err != nil {
			t.Fatal(err)
		}
		release()
		x.within(t, 5*time.Second, "the worker to end", ended(x, old))
		if n := x.size(t, "personal"); n != [2]int{} {
			t.Errorf("the new account has %v chats and messages of the old one", n)
		}
		if n := x.rowsOf(t, "history_queue", "personal"); n != 0 {
			t.Errorf("the new account has %d notifications of the old one", n)
		}
	})
}

// TestHistoryRemovalCancelsTheDownload: a download that is in flight when the account
// is removed is cancelled, not waited for: the removal does not wait for it, and the
// worker ends at once with nothing counted.
func TestHistoryRemovalCancelsTheDownload(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	started := make(chan struct{})
	x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		close(started)
		<-ctx.Done() // a download that goes on until it is told to stop
		return ctx.Err()
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the download", started)
	x.m.mu.Lock()
	old := x.m.accounts["personal"]
	x.m.mu.Unlock()

	start := time.Now()
	if res := remove(t, x.m, "personal"); res.err != nil {
		t.Fatal(res.err)
	}
	x.within(t, 5*time.Second, "the worker to end", func() bool {
		x.m.mu.Lock()
		defer x.m.mu.Unlock()
		return old.history == nil
	})
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the removal and the end of the worker took %v", d)
	}
	requireEnded(t, x.m)
}

// TestHistoryRemovalWhileItWaitsToRetry: a worker that is waiting out the pause after
// a failed import ends when the account is removed, and does not hold on for the pause.
func TestHistoryRemovalWhileItWaitsToRetry(t *testing.T) {
	x := newHx(t, 100)
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
	x.bobBlob("/h/1", "G1")
	x.h.failNext("/h/1", errors.New("the media server is down"))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	x.within(t, 5*time.Second, "the failed attempt to be counted", func() bool {
		a, _, ok := x.queueRow(t, "personal")
		return ok && a == 1
	})
	if res := remove(t, x.m, "personal"); res.err != nil {
		t.Fatal(res.err)
	}
	requireEnded(t, x.m)
	if n := len(x.h.downloads()); n != 1 {
		t.Errorf("%d downloads: the pause was not waited out", n)
	}
}

// TestHistoryStopIsNotAFailure: an attempt that is stopped (the account's removal begins
// and may yet fail, a pairing starts) is not an attempt on the notification: nothing is
// counted, no pause is put before the next, and the next begins at once if the account
// is as ready as before.
func TestHistoryStopIsNotAFailure(t *testing.T) {
	x := newHx(t, 100)
	x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour // a pause would be the end of the test
	x.bobBlob("/h/1", "G1")
	started := make(chan struct{})
	var calls atomic.Int32
	x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		if calls.Add(1) > 1 {
			return nil
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the download", started)
	x.m.mu.Lock()
	x.m.stopHistoryLocked(x.m.accounts["personal"])
	x.m.mu.Unlock()
	x.within(t, 5*time.Second, "the second attempt to import it", func() bool {
		return x.queued(t, "personal") == 0
	})
	if got := ids(x.msgs(t, "personal", bobChat)); !equalIDs(got, []string{"G1"}) {
		t.Errorf("Bob's messages %v", got)
	}
}

// TestHistoryRemovalIsFeltAtOnce: the import stops when the removal begins, and does
// not wait for it to end, which a device that hangs on logout would make long; and a
// worker that is waiting out a pause ends when the account is dropped, though nothing
// else has told it.
func TestHistoryRemovalIsFeltAtOnce(t *testing.T) {
	t.Run("the download is cancelled when the removal begins", func(t *testing.T) {
		x := newHx(t, 100)
		x.bobBlob("/h/1", "G1")
		started, stopped := make(chan struct{}), make(chan struct{})
		x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
			close(started)
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		}
		x.connect("personal")
		x.announce(t, "personal", "H1", "/h/1")
		waitFor(t, "the download", started)
		if _, err := x.m.beginRemove(context.Background(), "personal"); err != nil { // and no further
			t.Fatal(err)
		}
		waitFor(t, "the download to be cancelled", stopped)
		x.idle(t, "personal")
	})

	t.Run("the worker that waits to retry ends when the account is dropped", func(t *testing.T) {
		x := newHx(t, 100)
		x.m.historyRetry, x.m.historyRetryMax = time.Hour, time.Hour
		x.bobBlob("/h/1", "G1")
		x.h.failNext("/h/1", errors.New("the media server is down"))
		x.connect("personal")
		x.announce(t, "personal", "H1", "/h/1")
		x.within(t, 5*time.Second, "the failed attempt to be counted", func() bool {
			a, _, ok := x.queueRow(t, "personal")
			return ok && a == 1
		})
		x.m.mu.Lock()
		old := x.m.accounts["personal"]
		x.m.dropAccount("personal")
		x.m.mu.Unlock()
		x.within(t, 5*time.Second, "the worker to end", func() bool {
			x.m.mu.Lock()
			defer x.m.mu.Unlock()
			return old.history == nil
		})
		requireEnded(t, x.m)
	})
}

// TestHistoryWritesOnlyForTheCurrentClient: what an import has read with a client that
// is no longer the account's is not written, whether or not anything has cancelled it:
// the chunk that is next is refused (as every write of a client's events is,
// enterEvent), the attempt is not counted against the notification, and the import
// begins again with the client the account has.
func TestHistoryWritesOnlyForTheCurrentClient(t *testing.T) {
	x := newHx(t, 5)
	x.h.put("/h/1", bulk(1, 40))
	reached, release := x.pauseAt(t, 11)
	old := x.cli("personal")
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the import to be under way", reached)

	x.m.mu.Lock() // the account's client is replaced, and nobody tells the worker
	next := x.m.accounts["work"].cli
	x.m.accounts["personal"].cli = next
	x.m.mu.Unlock()
	release()
	x.imported(t, "personal")
	x.m.mu.Lock()
	x.m.accounts["personal"].cli = old // not under test: lets Close disconnect each client once
	x.m.mu.Unlock()

	calls := x.h.downloads()
	if len(calls) != 2 || calls[0].cli != old || calls[1].cli != next {
		t.Fatalf("downloads %+v: want the old client's, and then the one the account has", calls)
	}
	if got := x.size(t, "personal"); got != [2]int{1, 40} {
		t.Errorf("the archive has %v chats and messages, want the 40 once", got)
	}
	if log := x.logs.String(); strings.Contains(log, "history import failed") {
		t.Errorf("the replaced client is reported as a failure of the import:\n%s", log)
	}
}

// TestHistoryRelinkCancelsTheDownload: the client a download is in flight with is not
// the account's once a new device is being linked, and the download is cancelled, not
// waited for, and not counted against the notification.
func TestHistoryRelinkCancelsTheDownload(t *testing.T) {
	x := newHx(t, 100)
	x.bobBlob("/h/1", "G1")
	started := make(chan struct{})
	var once sync.Once
	x.h.onDownload = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the download", started)

	forceStatus(x.m, "personal", StatusNeedsLink) // the device is unlinked on the phone
	pair(t, x.m, "personal")
	x.idle(t, "personal")
	if attempts, _, ok := x.queueRow(t, "personal"); !ok || attempts != 0 {
		t.Errorf("the queue row: %d attempts, %v; want the notification there, and not blamed", attempts, ok)
	}
	if n := len(x.h.downloads()); n != 1 {
		t.Errorf("%d downloads", n)
	}
}

// TestHistoryRelinkMidImport: the account is linked again while its history is half
// in: what the old client read is not written for the new one (enterEvent), the
// attempt is not counted, and the new client imports the notification, from its start,
// once, whole.
func TestHistoryRelinkMidImport(t *testing.T) {
	x := newHx(t, 5)
	x.h.put("/h/1", bulk(1, 40))
	reached, release := x.pauseAt(t, 11)
	old := x.cli("personal")
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the import to be under way", reached)

	// The old device is unlinked on the phone, and the account is linked again.
	forceStatus(x.m, "personal", StatusNeedsLink)
	s := pair(t, x.m, "personal")
	q := x.fn.qr(t, 0)
	q.ch <- code("2@a", time.Minute)
	scan(t, q, types.NewADJID(ownPN, 0, 13))
	eventually(t, "paired", stateIs(s, pairPaired))
	q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	<-s.done

	release()
	x.imported(t, "personal")
	calls := x.h.downloads()
	if len(calls) != 2 || calls[0].cli != old || calls[1].cli != q.cli {
		t.Fatalf("downloads %+v: want the old client's, and then the new one's", calls)
	}
	if got := x.size(t, "personal"); got != [2]int{1, 40} {
		t.Errorf("the archive has %v chats and messages, want the 40 once", got)
	}
	if n := len(x.h.deletes()); n != 1 {
		t.Errorf("the blob is deleted %d times, want once", n)
	}
	if got := infoOf(x.m, "personal").Status; got != StatusConnected {
		t.Errorf("status %s", got)
	}
	if log := x.logs.String(); strings.Contains(log, "history import failed") {
		t.Errorf("the swap of the client is reported as a failure of the import:\n%s", log)
	}
}

// TestHistoryCloseMidImport: Close in the middle of an import stops the worker
// without writing more or counting an attempt, and returns when it has.
func TestHistoryCloseMidImport(t *testing.T) {
	x := newHx(t, 5)
	x.h.put("/h/1", bulk(1, 40))
	reached, release := x.pauseAt(t, 11)
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")
	waitFor(t, "the import to be under way", reached)
	before := x.size(t, "personal")

	closed := make(chan struct{})
	go func() { x.m.Close(); close(closed) }()
	x.within(t, 5*time.Second, "Close to begin", func() bool { return x.m.ctx.Err() != nil })
	release()
	waitFor(t, "Close to return", closed)

	if got := x.size(t, "personal"); got != before {
		t.Errorf("the archive went from %v to %v chats and messages after Close", before, got)
	}
	if attempts, _, ok := x.queueRow(t, "personal"); !ok || attempts != 0 {
		t.Errorf("the queue row: %d attempts, %v; want the notification there, and not blamed", attempts, ok)
	}
	if n := len(x.h.deletes()); n != 0 {
		t.Errorf("the blob is deleted after Close (%d times)", n)
	}
	requireEnded(t, x.m)
}

// TestHistoryLiveMessagesGetThroughAnImport: ten thousand messages in a notification
// are written in chunks, and a message that comes live meanwhile gets the writer
// between two of them: none is lost, and none waits for the import, for longer than
// a bound that is a few chunks' time (the real Close and the acknowledgement of a
// message wait for it).
func TestHistoryLiveMessagesGetThroughAnImport(t *testing.T) {
	x := newHx(t, historyChunk)
	x.h.put("/h/1", bulk(100, 100))
	x.connect("personal")
	x.announce(t, "personal", "H1", "/h/1")

	var waits []time.Duration
	bob := pnJID(bobPN)
	running := func() bool {
		x.m.mu.Lock()
		defer x.m.mu.Unlock()
		return x.m.accounts["personal"].history != nil
	}
	start := time.Now()
	for i := 0; running(); i++ {
		began := time.Now()
		// A time that has nothing to do with the order they come in.
		if !x.deliver("personal", at(incoming(fmt.Sprintf("L%d", i), bob), time.Duration((i*7919)%100000)*time.Second), text(fmt.Sprintf("live %d", i))) {
			t.Fatalf("live message %d was not acknowledged", i)
		}
		waits = append(waits, time.Since(began))
		time.Sleep(time.Millisecond)
	}
	took := time.Since(start)
	x.imported(t, "personal")

	if len(waits) < 3 {
		t.Fatalf("only %d live messages came while the import ran (%v): not a test of the interleaving", len(waits), took)
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	median, worst := waits[len(waits)/2], waits[len(waits)-1]
	t.Logf("10000 messages in %v; %d live messages, median wait %v, worst %v", took.Round(time.Millisecond), len(waits), median, worst)
	// A few seconds, and more for an import that is slow as a whole (a loaded runner, the
	// race detector): a writer held for the whole import would make a message wait for all
	// of it, and this is a fifth.
	if limit := max(3*time.Second, took/5); worst > limit {
		t.Errorf("a live message waited %v for the writer during an import of %v", worst, took.Round(time.Millisecond))
	}
	if got := x.size(t, "personal"); got != [2]int{100 + 1, 10000 + len(waits)} {
		t.Errorf("the archive has %v chats and messages, want 10000 of the history and the %d that came live", got, len(waits))
	}
	if got := len(x.msgs(t, "personal", bobChat)); got != len(waits) {
		t.Errorf("%d live messages are stored, %d came", got, len(waits))
	}
}
