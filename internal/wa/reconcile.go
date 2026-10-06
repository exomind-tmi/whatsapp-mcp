package wa

import (
	"context"
	"math"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// reconcileChats files the account's private chats under the identity the LID
// store gives them now. A message that comes in does that for its own chat, but a
// pair of a phone number and a LID reaches the store in ways no message of that
// chat tells of (the members of the groups just fetched, a sync of the phone), and
// a chat that is quiet would stay under the number, or without it, until it is
// written to: a tool that asks for it by its canonical identity (CanonicalChat)
// would find it empty. It makes no call to WhatsApp, and a failure is only logged:
// the next message of the chat, or the next connect, does it again.
//
// The LID store is shared by the accounts of a store.db, so a pair that another
// account's client has brought is settled for this account at its next connect.
func (m *Manager) reconcileChats(a *account, cli *whatsmeow.Client) {
	ctx, cancel := context.WithTimeout(m.ctx, m.groupsWait)
	defer cancel()
	chats, err := m.db.Chats(ctx, archive.ChatQuery{Accounts: []string{a.nick}, Limit: math.MaxInt32})
	if err != nil {
		if m.ctx.Err() == nil { // a Manager that is closing has no one to tell
			m.log.Warn("list the chats to file by LID", "account", a.nick, "err", err)
		}
		return
	}
	lids := lidsOf(cli)
	for _, c := range chats {
		if ctx.Err() != nil {
			return
		}
		want, ok := settled(ctx, lids, c) // a group is its own identity, and is never one to move
		if !ok {
			continue
		}
		if err := m.refile(ctx, a, cli, want); err != nil {
			m.log.Warn("file a chat by its LID", "account", a.nick, "err", err)
		}
	}
}

// settled is the identity chat c should have, and whether it is not that
// already: it is when the store knows the number of a chat and the row does not
// say so. That is a chat filed under a phone number that has a LID now (the row
// of such a chat has no number of its own: it is the number), and one filed under
// a LID whose number was not known when it was written.
func settled(ctx context.Context, lids lidLookup, c archive.Chat) (Chat, bool) {
	jid, err := types.ParseJID(c.JID)
	if err != nil {
		return Chat{}, false
	}
	want, err := canonical(ctx, lids, jid)
	if err != nil {
		return Chat{}, false
	}
	return want, want.PN != "" && c.PN == ""
}

// refile merges the chat into its LID and records its number, in one
// transaction, unless the account is no longer the client's: the chats of a
// removed account must not get a row back.
func (m *Manager) refile(ctx context.Context, a *account, cli *whatsmeow.Client, want Chat) error {
	state, done := m.enterEvent(a, cli)
	if state != eventLive {
		return nil
	}
	defer done()
	return m.db.Tx(ctx, func(t *archive.Tx) error {
		if pn, lid, ok := want.Merge(); ok {
			if err := t.MergeChat(a.nick, pn, lid); err != nil {
				return err
			}
		}
		return t.TouchChat(archive.ChatUpd{Account: a.nick, JID: want.JID, PN: want.PN})
	})
}
