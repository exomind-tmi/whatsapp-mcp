package wa

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// errStaleClient ends an import whose client is no longer the account's, or whose
// account is gone: what it has read with that client is not to be written for the
// account now, and is read again with the one that is.
var errStaleClient = errors.New("the account's client has changed")

// importStats is what an import did, for the log.
type importStats struct {
	conversations int // archived conversations
	messages      int // operations written: messages, edits and revokes
	skipped       int // conversations and messages that are not for the archive: statuses, reactions, stubs, ...
	unparsed      int // messages that whatsmeow could not read, or that have no time
	chunks        int // transactions
}

// importItem imports one queued notification and settles its account of the outcome.
func (m *Manager) importItem(ctx context.Context, a *account, cli *whatsmeow.Client, item archive.QueueItem) {
	start := time.Now()
	st, notif, err := m.importNotification(ctx, a, cli, item)
	if err != nil {
		m.historyFailed(ctx, a, cli, item, err)
		return
	}
	m.mu.Lock()
	a.historyNext = time.Time{}
	m.mu.Unlock()
	// Counts and kinds, never what was said.
	m.log.Info("history sync imported", "account", a.nick,
		"type", notif.GetSyncType().String(), "chunk", notif.GetChunkOrder(),
		"conversations", st.conversations, "messages", st.messages, "skipped", st.skipped, "unparsed", st.unparsed,
		"chunks", st.chunks, "took", time.Since(start).Round(time.Millisecond))
	if st.unparsed > 0 {
		m.log.Warn("some messages of a history sync could not be read and are missing", "account", a.nick, "count", st.unparsed)
	}
}

// importNotification downloads the blob a queued notification names and writes it
// to the archive, in chunks, the last of which takes the notification off the queue.
// A panic is a failure like any other.
//
// What comes after the commit of the last chunk is not the import's: the blob is
// deleted from the server, which the data being safe makes it fit to do, and the
// chats are filed by the LID pairs that the download has stored (reconcileChats).
// Neither is done when a chunk has failed, nor does a failure of either fail the
// import: the notification is off the queue by then.
func (m *Manager) importNotification(ctx context.Context, a *account, cli *whatsmeow.Client, item archive.QueueItem) (st importStats, notif *waE2E.HistorySyncNotification, err error) {
	defer func() {
		if r := recover(); r != nil {
			m.panicked("history import panicked", a, r)
			err = &importError{cause: causeInternal, err: fmt.Errorf("panic: %v", r)}
		}
	}()
	notif = &waE2E.HistorySyncNotification{}
	if err := proto.Unmarshal(item.Notif, notif); err != nil {
		return st, notif, &importError{cause: causeUnreadable, err: err}
	}
	run := &importRun{m: m, a: a, cli: cli, ctx: ctx, item: item}
	if !carriesBlob(notif) {
		err := run.flush(true) // nothing to import: only the notification is taken off the queue
		return run.st, notif, err
	}
	blob, err := m.download(ctx, cli, notif)
	if err != nil {
		return st, notif, err
	}
	if err := run.run(blob); err != nil {
		return run.st, notif, err
	}
	m.afterImport(ctx, a, cli, notif, blob)
	return run.st, notif, nil
}

// carriesBlob tells whether the notification has a blob to import. The first one after
// a link has it inside, and the others say where it is on the media server. One that
// has neither (the phone saying that there is no history to send, or what access it
// has to the messages) gives DownloadHistorySync nothing to fetch, and it would fail
// with no address (download.go:223) at each of the tries, while the notifications
// behind it waited for their pauses.
func carriesBlob(n *waE2E.HistorySyncNotification) bool {
	return n.InitialHistBootstrapInlinePayload != nil || n.GetDirectPath() != ""
}

// afterImport is what follows the commit of the last chunk, each step as a thing of
// its own: the notification is off the queue, so no failure of either, a panic of the
// delete (whatsmeow indexes the hosts of the media connection without looking
// whether there are any, upload.go:286) included, is a failed import, or keeps the
// other from being done.
func (m *Manager) afterImport(ctx context.Context, a *account, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification, blob *waHistorySync.HistorySync) {
	m.guarded("history sync clean-up panicked", a, func() { m.deleteHistoryBlob(ctx, a, cli, notif) })
	if len(blob.GetConversations()) > 0 || len(blob.GetPhoneNumberToLidMappings()) > 0 {
		m.guarded("history sync filing of chats panicked", a, func() { m.reconcileChats(a, cli) })
	}
}

// guarded runs f and logs a panic of it, with the stack, and goes on.
func (m *Manager) guarded(what string, a *account, f func()) {
	defer func() {
		if r := recover(); r != nil {
			m.panicked(what, a, r)
		}
	}()
	f()
}

// download is the blob of the notification. The storage flag is true: the call then
// has stored the blob's LID pairs and push names when it returns, which the chats
// are filed by, where false would leave that to a goroutine that may be late
// (message.go:800-823).
func (m *Manager) download(ctx context.Context, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
	ctx, cancel := context.WithTimeout(ctx, m.historyWait)
	defer cancel()
	blob, err := m.net.downloadHistory(cli, ctx, notif, true)
	if err != nil {
		return nil, downloadError(err)
	}
	return blob, nil
}

// deleteHistoryBlob takes the blob off WhatsApp's server. whatsmeow does that after
// its own download (message.go:736) and does not when the download is manual. The
// data is stored by now, so a failure is only logged: the blob stays on the server
// until it expires.
func (m *Manager) deleteHistoryBlob(ctx context.Context, a *account, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification) {
	ctx, cancel := context.WithTimeout(ctx, historySideWait)
	defer cancel()
	if err := m.net.deleteHistoryMedia(cli, ctx, notif); err != nil && ctx.Err() == nil {
		m.log.Warn("delete a history sync from WhatsApp's server; its data is stored", "account", a.nick, "err", errText(err))
	}
}

// histWrite is one write of an import: an operation on a chat, or, with the zero
// Op, the chat's own row (its name), which is what a conversation with nothing to
// archive leaves.
type histWrite struct {
	chat Chat
	op   Op
}

// importRun is the import of one blob. It collects writes and writes them in chunks:
// each chunk is a transaction of its own, so that the live handler gets the writer
// between two of them and is not made to wait for the whole history. A chunk is
// written when the next write finds it full, so that the last one, and with it the
// removal of the notification from the queue, is known to be the last. A crash
// between chunks leaves the notification queued, to be imported again from the start,
// which changes nothing in what the chunks before it wrote (the writes are
// idempotent).
type importRun struct {
	m     *Manager
	a     *account
	cli   *whatsmeow.Client
	ctx   context.Context
	item  archive.QueueItem
	batch []histWrite
	st    importStats
}

func (r *importRun) run(blob *waHistorySync.HistorySync) error {
	lids := lidsOf(r.cli)
	for _, conv := range blob.GetConversations() {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if err := r.conversation(lids, conv); err != nil {
			return err
		}
	}
	return r.flush(true)
}

// conversation adds the conversation's chat row and its messages. A conversation that
// is not a chat of the archive (a status, a broadcast list, a channel, a bot, or one
// whose id is not a JID) is left out with its messages, as the live handler leaves
// out the messages of such places (receivedChat).
func (r *importRun) conversation(lids lidStore, conv *waHistorySync.Conversation) error {
	jid, err := types.ParseJID(conv.GetID())
	if err != nil {
		r.st.skipped++
		return nil
	}
	jid = whatsmeowSpelling(jid)
	if err := learnConversationPair(r.ctx, lids, jid, conv); err != nil {
		return &importError{cause: causeWrite, err: err}
	}
	chat, err := canonical(r.ctx, lids, jid)
	switch {
	case errors.Is(err, errBadChat):
		r.st.skipped++
		return nil
	case err != nil:
		return &importError{cause: causeWrite, err: err} // the LID store could not be read
	}
	r.st.conversations++
	name := conversationName(conv)
	if err := r.add(histWrite{chat: chat, op: Op{ChatName: name}}); err != nil {
		return err
	}
	for _, hm := range oldestFirst(conv.GetMessages()) {
		if err := r.message(chat, jid, name, hm.GetMessage()); err != nil {
			return err
		}
	}
	return nil
}

// whatsmeowSpelling is the JID of a conversation as ParseWebMessage needs it. It
// takes the sender of a message of another person from the chat only when the chat is
// on s.whatsapp.net or lid, and fails for the other spellings of these, which have no
// participant to name (client.go:1037-1044). whatsmeow reads these spellings as the
// two the same way for the messages that come live (message.go:144-147, 173-176), and
// canonical does for the chat.
func whatsmeowSpelling(jid types.JID) types.JID {
	switch jid.Server {
	case types.LegacyUserServer, types.HostedServer:
		jid.Server = types.DefaultUserServer
	case types.HostedLIDServer:
		jid.Server = types.HiddenUserServer
	}
	return jid
}

// learnConversationPair keeps the other address that the phone gives a conversation,
// its phone JID or its LID, in the LID store: the chat is filed by what the store
// knows (canonical), and whatsmeow keeps the pairs that the blob lists apart from its
// conversations and not these (message.go:804-806).
func learnConversationPair(ctx context.Context, lids lidStore, jid types.JID, conv *waHistorySync.Conversation) error {
	for _, s := range []string{conv.GetPnJID(), conv.GetLidJID()} {
		other, err := types.ParseJID(s)
		if s == "" || err != nil {
			continue
		}
		if lid, pn, ok := lidPair(jid, whatsmeowSpelling(other)); ok {
			return lids.PutLIDMapping(ctx, lid, pn)
		}
	}
	return nil
}

// oldestFirst is the messages of a conversation in the order they were sent. Phones
// differ in the order of the list, and the archive reads messages of the same second
// in the order they were written (its row ids), so an album that comes newest first
// would be read backwards; and the name of a chat that the phone gives none is the
// push name of the last message written, which should be the newest.
func oldestFirst(msgs []*waHistorySync.HistorySyncMsg) []*waHistorySync.HistorySyncMsg {
	out := slices.Clone(msgs)
	ts := func(m *waHistorySync.HistorySyncMsg) uint64 { return m.GetMessage().GetMessageTimestamp() }
	if n := len(out); n > 1 && ts(out[0]) > ts(out[n-1]) {
		slices.Reverse(out) // newest first
	}
	slices.SortStableFunc(out, func(a, b *waHistorySync.HistorySyncMsg) int {
		return cmp.Or(cmp.Compare(ts(a), ts(b)), cmp.Compare(a.GetMsgOrderID(), b.GetMsgOrderID()))
	})
	return out
}

// conversationName is what the phone calls the chat: the name it has for the
// contact or the group, which is better than the push name a contact gives itself,
// and beats it (the name of the chat is the last one written).
func conversationName(c *waHistorySync.Conversation) string {
	if n := strings.TrimSpace(c.GetName()); n != "" {
		return n
	}
	return strings.TrimSpace(c.GetDisplayName())
}

// message adds what the message means for the archive, read as a live one is
// (opOf). One that has no content (the stubs for a group's notices, a message
// deleted for everyone), or that is not for the archive, is skipped, and one that
// cannot be read is skipped and counted, not failed: the others of the notification
// are not to be lost for it, and a failing import would be tried again to fail the
// same way.
func (r *importRun) message(chat Chat, jid types.JID, name string, web *waWeb.WebMessageInfo) error {
	if web.GetMessage() == nil {
		r.st.skipped++
		return nil
	}
	if web.GetMessageTimestamp() == 0 { // the archive files a message by its time, and whatsmeow would make it 1970
		r.st.unparsed++
		return nil
	}
	evt, err := r.m.net.parseWebMessage(r.cli, jid, web)
	if err != nil {
		r.st.unparsed++
		return nil
	}
	op := opOf(evt, r.m.now())
	switch op.Kind {
	case OpSkip, OpHistoryNotif: // a notification inside a history is no one's to follow
		r.st.skipped++
		return nil
	}
	if name != "" {
		op.ChatName = name
	}
	r.st.messages++
	return r.add(histWrite{chat: chat, op: op.In(r.a.nick, chat.JID)})
}

// add queues a write, writing the chunk before it if that is full.
func (r *importRun) add(w histWrite) error {
	if len(r.batch) >= max(r.m.historyChunk, 1) {
		if err := r.flush(false); err != nil {
			return err
		}
	}
	r.batch = append(r.batch, w)
	return nil
}

// flush writes the queued writes in one transaction, and with the last chunk takes
// the notification off the queue in that same transaction: a crash leaves the chunk
// and the notification both, which is imported again, or both done.
//
// It writes nothing for an account whose client is not the one the import runs with
// (enterEvent): the account may have been removed and its nick given to another.
func (r *importRun) flush(last bool) error {
	switch state, done := r.m.enterEvent(r.a, r.cli); state {
	case eventClosing:
		return context.Canceled
	case eventStale:
		return errStaleClient
	default:
		defer done()
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.m.writeWait)
	defer cancel()
	err := r.m.db.Tx(ctx, func(t *archive.Tx) error {
		// What the merge of a chat does, it does once for the transaction: the writes
		// that follow file everything under the LID from the start. Done again for each
		// of a chat's messages it costs more than the messages (3x at 500 of them).
		merged := make(map[string]bool)
		for _, w := range r.batch {
			if !merged[w.chat.JID] {
				if err := mergeChat(t, r.a.nick, w.chat); err != nil {
					return err
				}
				merged[w.chat.JID] = true
			}
			if err := writeOp(t, r.a.nick, w.op, w.chat); err != nil {
				return err
			}
		}
		if last {
			return t.QueueDone(r.item.ID)
		}
		return nil
	})
	if err != nil {
		if r.ctx.Err() != nil {
			return r.ctx.Err() // cut off, not failed
		}
		return &importError{cause: causeWrite, err: err}
	}
	r.st.chunks++
	r.m.log.Debug("history chunk written", "account", r.a.nick, "chunk", r.st.chunks, "writes", len(r.batch), "last", last)
	r.batch = r.batch[:0]
	return nil
}
