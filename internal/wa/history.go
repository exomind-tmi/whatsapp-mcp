package wa

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// The history worker imports what the phone sends of the history of its chats. The
// phone announces each part of it in a message, which the handler of live messages
// puts in the archive's queue (queueHistory), and the worker downloads the blob the
// announcement names and writes its conversations (history_import.go). It makes the
// calls to WhatsApp that the handler may not: its files call no method of the client
// but through the seams of the network.

const (
	// historyChunk is how many messages one transaction of an import writes. The
	// writer is one connection, and a message that comes live waits for it while a
	// transaction runs, with the connection's acknowledgement held back
	// (SynchronousAck): a chunk is as long as that wait. Measured on archive.db with
	// its full-text index (TestHistoryChunkCost) a message costs 75-110 us whatever
	// the size of the chunk up to 2000, and more beyond it, so 500 hold the writer for
	// 40-55 ms; 250 would halve that for a tenth more commits. Nothing but a message
	// of the live handler waits, and it waits seconds for an acknowledgement.
	historyChunk = 500

	// historyWait bounds the download of one notification. A blob of a long history
	// is big and a laptop's link is slow, but a transfer that goes on for longer is
	// not going to end: it is a failed attempt, which is tried again later.
	historyWait = 10 * time.Minute

	// historyRetry is the pause after the first failed import of a notification, and
	// each failure after it doubles it, up to historyRetryMax. A notification is given
	// up on after archive.MaxAttempts failures, and the pauses are what make that
	// take minutes and not seconds: whatever keeps the blob from coming, the network
	// or the media server, has the time to be over. (A failure that no pause would
	// change is given up on at once, see failuresCounted.)
	historyRetry    = 30 * time.Second
	historyRetryMax = 10 * time.Minute

	// historySideWait bounds the calls that go with an import and are not worth
	// waiting long for: the receipt, and the delete of the blob from the server.
	historySideWait = 30 * time.Second
)

// historyWorker is the goroutine that imports one account's queued notifications. At
// most one runs for an account, and it runs while there is something to import and
// the account is connected: it ends when the queue is empty, when the connection
// goes, and when the account is removed, and is started again by what makes it
// useful, a notification queued (queueHistory) or the connection back (handle). Its
// fields are under Manager.mu.
type historyWorker struct {
	wake   chan struct{}      // a signal to look again; one pending is as good as many
	cancel context.CancelFunc // ends the attempt in flight; nil before the first
}

func (w *historyWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// startHistory makes sure a worker is running for the account, or tells the one that
// is to look again. A second Connected, or a notification queued while the worker is
// at work, does not start a second.
func (m *Manager) startHistory(a *account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := a.history; w != nil {
		w.signal()
		return
	}
	if m.accounts[a.nick] != a || !m.track() {
		return
	}
	w := &historyWorker{wake: make(chan struct{}, 1)}
	a.history = w
	go m.runHistory(a, w)
}

// stopHistoryLocked ends the attempt the worker has in flight, if any, and wakes the
// worker to look again: the account is being removed, or is gone, or is being linked
// to another device, whose client is the account's from then on. The worker itself
// ends at that look if the account is no longer ready (historyReady), and goes on
// with a new attempt if it is. mu must be held, and is by whoever changes what that says.
func (m *Manager) stopHistoryLocked(a *account) {
	if w := a.history; w != nil {
		if w.cancel != nil {
			w.cancel()
		}
		w.signal()
	}
}

func (m *Manager) runHistory(a *account, w *historyWorker) {
	defer m.wg.Done()
	defer m.historyEnded(a, w)
	// Not whatsmeow's goroutine, so no one recovers a panic here but this: it would
	// end the daemon. The import of a notification has its own, which counts the panic
	// as a failed attempt, so that a notification that makes the code panic is given up
	// on after a few and is not tried at every start.
	defer func() {
		if r := recover(); r != nil {
			m.panicked("history worker panicked", a, r)
		}
	}()
	for m.historyRound(a, w) {
	}
}

func (m *Manager) historyEnded(a *account, w *historyWorker) {
	m.mu.Lock()
	if a.history == w {
		a.history = nil
	}
	m.mu.Unlock()
}

// historyReady tells whether the account's history may be imported now, and with
// which client: the account's current one, which is not always the one an earlier
// round used (a relink has replaced it, see owner). The account must be known,
// connected, and neither being removed nor having its device taken away. mu must be
// held.
func (m *Manager) historyReady(a *account) (*whatsmeow.Client, bool) {
	cli := a.owner()
	ok := m.ctx.Err() == nil && m.accounts[a.nick] == a && !a.removing && a.unlinking == nil &&
		cli != nil && a.info.Status == StatusConnected
	return cli, ok
}

// historyAttempt is historyReady for a worker that is about to import, and the
// context of the attempt, which a remove cancels (stopHistoryLocked) and Close
// cancels with the Manager's. A worker that finds the account not ready ends here,
// in the same hold of mu: a start that comes after it makes a new one, and one that
// came before it has left its signal for the look it is making now.
func (m *Manager) historyAttempt(a *account, w *historyWorker) (context.Context, context.CancelFunc, *whatsmeow.Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cli, ok := m.historyReady(a)
	if !ok {
		if a.history == w {
			a.history = nil
		}
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithCancel(m.ctx)
	w.cancel = cancel
	return ctx, cancel, cli, true
}

// historyRound imports the oldest notification that waits, and says whether the
// worker is to go on.
func (m *Manager) historyRound(a *account, w *historyWorker) bool {
	if !m.historyPause(a, w) {
		return false
	}
	ctx, cancel, cli, ok := m.historyAttempt(a, w)
	if !ok {
		return false
	}
	defer cancel()
	item, found, err := m.db.QueueNext(ctx, a.nick)
	switch {
	case ctx.Err() != nil:
		return true // the next look finds why
	case err != nil:
		m.log.Warn("read the history queue; trying again later", "account", a.nick, "err", err)
		m.historyPauseFor(a, m.historyRetry)
		return true
	case !found:
		return m.historyMore(a, w)
	}
	m.importItem(ctx, a, cli, item)
	return true
}

// historyMore is the worker's last look before it ends for want of work: a
// notification queued since it last asked has left a signal, and it goes on.
func (m *Manager) historyMore(a *account, w *historyWorker) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-w.wake:
		return true
	default:
	}
	if a.history == w {
		a.history = nil
	}
	return false
}

// historyPauseFor makes the account's next import wait d from now.
func (m *Manager) historyPauseFor(a *account, d time.Duration) {
	m.mu.Lock()
	a.historyNext = time.Now().Add(d)
	m.mu.Unlock()
}

// historyPause waits out the pause after a failed import. It is the account's, not
// the worker's: a worker that ends with the connection, and the one that starts with
// it again, are not a new chance for a notification that has just failed. A signal
// wakes it to see whether the worker is still wanted (a remove is a signal), and it
// goes back to waiting. It returns false when the worker is not.
//
// The pause is real time and not the Manager's clock, which a test sets to a date of
// its own.
func (m *Manager) historyPause(a *account, w *historyWorker) bool {
	for {
		m.mu.Lock()
		d := time.Until(a.historyNext)
		m.mu.Unlock()
		if d <= 0 {
			return true
		}
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-w.wake:
			m.mu.Lock()
			_, ok := m.historyReady(a)
			if !ok && a.history == w {
				a.history = nil
			}
			m.mu.Unlock()
			if !ok {
				t.Stop()
				return false
			}
		case <-m.ctx.Done():
			t.Stop()
			return false
		}
		t.Stop()
	}
}

// historyDelay is the pause after the failures-th failed attempt on a notification:
// base for the first, doubled by each after it, never more than limit.
func historyDelay(base, limit time.Duration, failures int) time.Duration {
	d := base
	for range max(failures, 1) - 1 {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	return min(d, limit)
}

// historyFailed accounts for an import that failed. It counts as an attempt on the
// notification (QueueFail; for some failures as all that are left, see
// failuresCounted), and puts a pause before the next, unless it is not the
// notification's fault: the worker was stopped or the Manager is closing, the account
// has another client now (the one the import began with has been replaced, or taken
// away), or the connection is down. The connection dropping while a blob downloads
// is what a network does, and a notification that is given up on is lost for good
// (QueueFail), so what it costs the notification is nothing: the worker ends, and
// the connection back starts another.
func (m *Manager) historyFailed(ctx context.Context, a *account, cli *whatsmeow.Client, item archive.QueueItem, err error) {
	if ctx.Err() != nil || errors.Is(err, errStaleClient) || errors.Is(err, context.Canceled) {
		return
	}
	m.mu.Lock()
	live, ok := m.historyReady(a)
	m.mu.Unlock()
	if !ok || live != cli {
		m.log.Debug("history import interrupted: the connection is down or the account changed", "account", a.nick, "err", errText(err))
		return
	}
	cause := causeOf(err)
	wctx, cancel := context.WithTimeout(m.ctx, m.writeWait)
	defer cancel()
	tries := failuresCounted(cause, item.Attempts)
	var qerr error
	for range tries {
		if qerr = m.db.QueueFail(wctx, item.ID, cause); qerr != nil {
			break
		}
	}
	if qerr != nil && m.ctx.Err() == nil {
		m.log.Warn("record a failed history import", "account", a.nick, "err", qerr)
	}
	attempts := item.Attempts + tries
	// A row whose failure could not be recorded is offered again by the queue, so it is
	// not given up on whatever the count says: it waits out the pause, which is what
	// keeps a queue that cannot be written to from being downloaded in a loop.
	if qerr == nil && attempts >= archive.MaxAttempts {
		// The row is not offered again, and the next one need not wait for it.
		m.log.Error("history sync given up on: part of the older messages will be missing", "account", a.nick, "attempts", attempts, "cause", cause, "err", errText(err))
		return
	}
	delay := historyDelay(m.historyRetry, m.historyRetryMax, attempts)
	m.historyPauseFor(a, delay)
	m.log.Warn("history import failed; trying again later", "account", a.nick, "attempt", attempts, "in", delay, "cause", cause, "err", errText(err))
}

// failuresCounted is how many of a notification's attempts a failure of that cause
// uses up, given the attempts it has used. One, but for the failures that another
// try would meet again: a blob the media server says is gone and a notification that
// cannot be read. Tried at each of the attempts that are left, with the pauses
// between them, they would keep every notification behind them waiting for minutes
// to get the same answer. The queue counts failures and has no call to give a row up
// at once, so the rest of the attempts are counted together.
func failuresCounted(cause string, attempts int) int {
	switch cause {
	case causeGone, causeUnreadable:
		return archive.MaxAttempts - attempts
	}
	return 1
}

// ackHistory tells the phone that an announcement of a history sync has arrived, as
// whatsmeow does for each of them unless the receipt is switched off, which newClient
// does so that it is not sent for an announcement we have not kept (message.go:
// 892-906). It goes out once for each delivery that has been queued, before and
// whatever becomes of the download: whatsmeow sends it before it downloads the blob
// too, so the phone is told that the message has arrived, not that its history has.
// A notification delivered again is one row (QueuePush), and gets a receipt again, as
// whatsmeow gives one to each delivery; the one for the first may be what was lost.
// It fails only if the connection is gone, which whatsmeow does not retry either, and
// is logged.
func (m *Manager) ackHistory(a *account, cli *whatsmeow.Client, msgID string) {
	m.mu.Lock()
	ok := m.track()
	m.mu.Unlock()
	if !ok {
		return
	}
	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				m.panicked("history receipt panicked", a, r)
			}
		}()
		ctx, cancel := context.WithTimeout(m.ctx, historySideWait)
		defer cancel()
		if err := m.net.historyReceipt(cli, ctx, types.MessageID(msgID)); err != nil && m.ctx.Err() == nil {
			m.log.Warn("send the receipt of a history sync", "account", a.nick, "err", errText(err))
		}
	}()
}

// What last_error says of a failed notification: fixed words, since manage-accounts
// list shows them to the agent, and neither a path of the media server nor a JID
// is for it.
const (
	causeUnreadable = "the queued notification is unreadable"
	causeDownload   = "download or decoding failed"
	causeGone       = "download failed: the file is no longer on the server"
	causeTimeout    = "download failed: timed out"
	causeWrite      = "archive write failed"
	causeInternal   = "internal error"
)

// importError is a failure of an import with what last_error says of it.
type importError struct {
	cause string
	err   error
}

func (e *importError) Error() string { return e.cause + ": " + e.err.Error() }
func (e *importError) Unwrap() error { return e.err }

// downloadError is the failure of DownloadHistorySync: the media server may no longer
// have the blob, and that is worth telling apart from a network that failed.
func downloadError(err error) error {
	cause := causeDownload
	switch {
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403),
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404),
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		cause = causeGone
	case errors.Is(err, context.DeadlineExceeded):
		cause = causeTimeout
	}
	return &importError{cause: cause, err: err}
}

func causeOf(err error) string {
	var ie *importError
	if errors.As(err, &ie) {
		return ie.cause
	}
	return causeInternal
}

// errText is err for the log without the address of a media download, which
// whatsmeow's errors carry as a url.Error, query and all.
func errText(err error) string {
	s := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		s = strings.ReplaceAll(s, strconv.Quote(ue.URL), `"<url>"`)
	}
	return s
}

// stuckHistory reads how many notifications each of the accounts has given up on.
func (m *Manager) stuckHistory(ctx context.Context, accs []archive.Account) map[string]archive.Stuck {
	out := map[string]archive.Stuck{}
	for _, a := range accs {
		s, err := m.db.QueueStuck(ctx, a.Nick)
		if err != nil {
			if ctx.Err() == nil && m.ctx.Err() == nil {
				m.log.Warn("count the history notifications given up on", "account", a.Nick, "err", err)
			}
			continue
		}
		if s.Count > 0 {
			out[a.Nick] = s
		}
	}
	return out
}

// withStuckHistory is info with the history that is missing, which list shows as the
// reason of an account that has no other: a connected account with part of its
// history lost is still connected, and one that is linking, or needs to be linked
// again, has a worse thing to be told first.
func withStuckHistory(info AccountInfo, s archive.Stuck) AccountInfo {
	info.HistoryStuck = s.Count
	if info.Reason == "" && (info.Status == StatusConnected || info.Status == StatusReconnecting) {
		info.Reason = stuckReason(s)
	}
	return info
}

func stuckReason(s archive.Stuck) string {
	what := "1 part of the history sync"
	if s.Count != 1 {
		what = fmt.Sprintf("%d parts of the history sync", s.Count)
	}
	reason := what + " could not be imported: some older messages are missing"
	if s.LastError != "" {
		reason += " (" + s.LastError + ")"
	}
	return reason
}
