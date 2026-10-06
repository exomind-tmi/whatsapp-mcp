package wa

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// writeWait bounds how long the handler of a message waits for archive.db. It
// runs on whatsmeow's goroutine, which handles one stanza at a time and keeps
// its handler lock while it does (client.go:980-990), so a writer held for good
// (a VACUUM of another account's removal, a stuck disk) would otherwise stop the
// client, a pairing's registration of its QR handler among the rest.
const writeWait = 30 * time.Second

// eventState is what the state of the Manager lets an event of a client do.
type eventState int

const (
	// eventLive: the client is the account's, and the Manager runs.
	eventLive eventState = iota
	// eventStale: the account has been removed, or the client is not its own any
	// more (the one a relink has replaced): what it says is no longer the account's.
	eventStale
	// eventClosing: the Manager is closing. The archive closes after it, so
	// nothing may be written from here on.
	eventClosing
)

// enterEvent tells what the event of cli may do for the account a, and on eventLive
// counts the caller among the goroutines Close waits for (done ends that), so
// that archive.db is not closed under a write. It holds mu for that and for no
// longer: the caller's archive and whatsmeow calls are made without it.
//
// Close waits for them for closeWait at most, as it does for the clients. A
// handler still at work after that finds the archive closed, and fails its
// message, which is not acknowledged then.
func (m *Manager) enterEvent(a *account, cli *whatsmeow.Client) (state eventState, done func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return eventClosing, nil
	}
	if m.accounts[a.nick] != a || cli != a.owner() {
		return eventStale, nil
	}
	// Close ends the context before it marks the Manager closed, both of them
	// before it waits, so a context that is alive under mu is a Manager that
	// still takes goroutines to wait for (see track).
	m.wg.Add(1)
	return eventLive, m.wg.Done
}

// receive is the handler of a message that comes in live. It decides what the
// message is (Classify), puts it, and the chat it belongs to, in the archive in
// one transaction, and says whether whatsmeow may acknowledge it. It makes no
// call to WhatsApp, only to the databases.
//
// It returns false for a message it could not store, and for all that arrive
// once Close has begun: whatsmeow then neither acknowledges the stanza nor
// clears the plaintext it kept of it (EnableDecryptedEventBuffer;
// message.go:459-487), so WhatsApp sends the message again, and what is sent
// again is read from that buffer and not decrypted a second time, as a
// consumed key could not be (message.go:518-566). That is why returning false is
// of use at all, and why the buffer is on. It comes again after the next
// connect, not at once, and one whose sender's LID becomes known meanwhile does
// not come again at all (see newClient).
//
// Everything else is true, a message that is not for the archive included: it
// is acknowledged and forgotten. So is one of a client that is no longer the
// account's (see enterEvent), which has nobody to deliver it to again.
func (m *Manager) receive(a *account, cli *whatsmeow.Client, e *events.Message) (ok bool) {
	state, done := m.enterEvent(a, cli)
	switch state {
	case eventClosing:
		return false
	case eventStale:
		return true
	}
	defer done()
	// whatsmeow recovers a panic of a handler and counts it a success
	// (client.go:983-988), which would acknowledge a message that was not stored.
	// Recovered here, the handler returns its result as it stands, false.
	defer func() {
		if r := recover(); r != nil {
			m.panicked("message handler panicked; the message is not acknowledged", a, r)
		}
	}()

	op := opOf(e, m.now())
	switch op.Kind {
	case OpSkip:
		return true
	case OpHistoryNotif:
		return m.queueHistory(a, cli, e.Info.ID, op.Notif)
	}
	if err := m.record(a, cli, e.Info, op); err != nil {
		// What failed, not what was said: no text, no JID.
		m.log.Warn(refusedMessage(e), "account", a.nick, "op", op.Kind, "err", err)
		return false
	}
	return true
}

// opOf is what a message event means for the archive, for the live handler and for
// the history worker alike (which parses the phone's history into the same events,
// ParseWebMessage), so that a message is read the same way whichever way it came.
//
// The message is as it was sent, wrappers on: whatsmeow has taken them off
// e.Message (UnwrapRaw) and kept only a flag of each, the view-once one among
// them, which Classify reads off the wrapper itself. For a message of a resend from
// the phone (ParseWebMessage) an edit comes as the edit's content under the
// original's id, and only the raw message still says that it is one: ParseWebMessage
// keeps it in RawMessage (client.go:1053-1062), and that is what Classify reads, so
// the edit is not mistaken for the original sent again. A message that was never
// unwrapped has only Message.
func opOf(e *events.Message, now time.Time) Op {
	msg := e.RawMessage
	if msg == nil {
		msg = e.Message
	}
	return Classify(e.Info, msg).NoLaterThan(now)
}

// refusedMessage is what the log says of a message the archive did not take. A
// message that came as the phone's resend of one that was missing is not given
// again whatever the handler returns: whatsmeow takes the answer for that of the
// protocol message that carried it, and not for its own (message.go:863-880,
// 1172-1176).
func refusedMessage(e *events.Message) string {
	if e.SourceWebMsg != nil || e.UnavailableRequestID != "" {
		return "archive write failed; the message came as a resend from the phone, which is not delivered again: it is lost"
	}
	return "archive write failed; the message is not acknowledged and will be delivered again"
}

// panicked logs a panic that a goroutine of the Manager has recovered: with its
// stack, which names the code and holds no message, for a panic that comes with
// every delivery of its message to be found by.
func (m *Manager) panicked(what string, a *account, r any) {
	m.log.Error(what, "account", a.nick, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
}

// queueHistory keeps the announcement of a history sync for the worker that
// downloads it. whatsmeow acknowledges the message once this returns true and
// throws away what it kept of it, so an announcement that is not kept here is
// gone, and the history it names with it: the first one after a link carries
// the whole bootstrap inline (message.go:786-787), the announcement is the
// history itself. Queued twice, it is one row.
//
// Once it is kept the phone is told so (ackHistory) and the worker that imports it
// is woken (startHistory). The calls to WhatsApp that these make are not made on
// whatsmeow's goroutine, which this is on: each has a goroutine of its own, as
// whatsmeow's own receipt has (message.go:899-905).
func (m *Manager) queueHistory(a *account, cli *whatsmeow.Client, msgID string, n *waE2E.HistorySyncNotification) bool {
	if msgID == "" {
		return true // WhatsApp gives every message an id; there is nothing to key the row by
	}
	raw, err := proto.Marshal(n)
	if err == nil {
		ctx, cancel := context.WithTimeout(m.ctx, m.writeWait)
		defer cancel()
		err = m.db.QueuePush(ctx, a.nick, msgID, raw)
	}
	if err != nil {
		m.log.Warn("queue a history sync; the message is not acknowledged and will be delivered again", "account", a.nick, "err", err)
		return false
	}
	m.log.Debug("history sync queued", "account", a.nick, "type", n.GetSyncType().String(), "chunk", n.GetChunkOrder())
	m.ackHistory(a, cli, msgID)
	m.startHistory(a)
	return true
}

// record applies op to the archive in one transaction: the chat's earlier rows
// brought over to its LID if there is one, the operation itself, and the chat's
// own row with the name and the time. All of it is written or none, so a failed
// message leaves no chat behind and no merged rows, and comes again whole.
func (m *Manager) record(a *account, cli *whatsmeow.Client, info types.MessageInfo, op Op) error {
	ctx, cancel := context.WithTimeout(m.ctx, m.writeWait)
	defer cancel()
	lids := lidsOf(cli)
	m.learnLIDs(ctx, lids, info, a.nick)
	chat, err := canonical(ctx, lids, op.Chat)
	if err != nil {
		return err
	}
	op = op.In(a.nick, chat.JID)
	ok, err := m.mayChange(ctx, lids, a.nick, chat, info, op)
	if err != nil {
		return err
	}
	if !ok {
		// Acknowledged, and dropped: an edit or a delete that is not of the author's
		// is not the archive's to apply, and sending it again would change nothing.
		m.log.Warn("an edit or a delete of a message by someone who did not write it is ignored", "account", a.nick, "op", op.Kind)
		return nil
	}
	return m.db.Tx(ctx, func(t *archive.Tx) error { return apply(t, a.nick, op, chat) })
}

// apply is the writes of one message. The merge goes first: what the operation
// writes is then in the chat's final place from the start, and not moved after
// it, and an edit or a revoke meets the message it is about there.
func apply(t *archive.Tx, account string, op Op, chat Chat) error {
	if err := mergeChat(t, account, chat); err != nil {
		return err
	}
	return writeOp(t, account, op, chat)
}

// mergeChat brings what the chat has under its phone JID over to its LID, if it has
// both. The history worker, which writes many messages of a chat in one transaction,
// does it once for them and then only writes.
func mergeChat(t *archive.Tx, account string, chat Chat) error {
	if pn, lid, ok := chat.Merge(); ok {
		return t.MergeChat(account, pn, lid)
	}
	return nil
}

// writeOp is apply without the merge: the operation, and the chat's own row.
func writeOp(t *archive.Tx, account string, op Op, chat Chat) error {
	var err error
	upd := archive.ChatUpd{Account: account, JID: chat.JID, PN: chat.PN, Name: op.ChatName, IsGroup: chat.IsGroup}
	switch op.Kind {
	case OpUpsert:
		err = t.Upsert(op.Row)
		upd.LastMessageTS = op.Row.TS // an edit or a revoke is not a new message in the chat
	case OpEdit:
		err = t.Edit(op.Edit)
	case OpRevoke:
		err = t.Revoke(op.Revoke)
	}
	if err != nil {
		return err
	}
	return t.TouchChat(upd)
}

// lidStore is what the handler uses of the client's LID store (store.LIDStore):
// the lookups of a chat's identity and the mappings a message brings.
type lidStore interface {
	lidLookup
	PutLIDMapping(ctx context.Context, lid, pn types.JID) error
}

// noLIDs is the LID store of a client that has none, which is a device that was
// never saved: it knows no pair and keeps none.
type noLIDs struct{}

func (noLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) { return types.EmptyJID, nil }
func (noLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) { return types.EmptyJID, nil }
func (noLIDs) PutLIDMapping(context.Context, types.JID, types.JID) error { return nil }

func lidsOf(cli *whatsmeow.Client) lidStore {
	if cli.Store == nil || cli.Store.LIDs == nil {
		return noLIDs{}
	}
	return cli.Store.LIDs
}

// learnLIDs keeps the pair of a LID and a phone JID that the message brings: the
// sender and the other address of the sender, and for a message of our own the
// chat and the other address of the recipient. whatsmeow keeps the same pairs
// before it decrypts the message (message.go:51-55), so this is no news for a
// message that comes live; it is for the chat's identity to depend on nothing
// but what this handler has seen, and to survive a write of whatsmeow's that
// failed, which it only logs. A store that cannot keep a pair does not keep the
// message from the archive: the chat is then filed under the phone JID, and
// what is under it comes over when a pair is known.
func (m *Manager) learnLIDs(ctx context.Context, lids lidStore, info types.MessageInfo, nick string) {
	for _, p := range [][2]types.JID{{info.Sender, info.SenderAlt}, {info.Chat, info.RecipientAlt}} {
		lid, pn, ok := lidPair(p[0], p[1])
		if !ok {
			continue
		}
		if err := lids.PutLIDMapping(ctx, lid, pn); err != nil {
			m.log.Warn("keep a LID mapping of a message", "account", nick, "err", err)
		}
	}
}

// lidPair tells which of two JIDs is the LID and which the phone JID, in either
// order, as whatsmeow's StoreLIDPNMapping does (client.go:1066-1075), without
// the device: it is of no use in the pair.
func lidPair(x, y types.JID) (lid, pn types.JID, ok bool) {
	if x.Server == types.DefaultUserServer && y.Server == types.HiddenUserServer {
		x, y = y, x
	}
	if x.Server != types.HiddenUserServer || y.Server != types.DefaultUserServer || x.User == "" || y.User == "" {
		return types.EmptyJID, types.EmptyJID, false
	}
	return x.ToNonAD(), y.ToNonAD(), true
}
