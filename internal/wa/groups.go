package wa

import (
	"context"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// groupsWait bounds the fetch of the groups an account is in, and the filing of
// its chats by LID after it. The write of the names has the wait of any write
// (writeWait): a fetch that took all of this must not leave it none.
const groupsWait = time.Minute

// A message of a group says nothing of the group's name, so the names come from
// the account's list of groups, fetched once the client is connected (the
// message handler makes no call to WhatsApp), and are kept up to date by the
// events of a group that is renamed or joined.

// startGroupSync fetches the names of the account's groups in the background,
// unless a fetch is running: the account has at most one, whatever number of
// times it connects meanwhile. Close and a timeout end it. The members of the
// groups bring pairs of LIDs and phone numbers into the store, so the account's
// chats are filed by what the store knows once the fetch is over, whether it
// worked or not (reconcileChats).
func (m *Manager) startGroupSync(a *account, cli *whatsmeow.Client) {
	m.mu.Lock()
	ok := !a.syncingGroups && m.track()
	if ok {
		a.syncingGroups = true
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			a.syncingGroups = false
			m.mu.Unlock()
		}()
		// Not whatsmeow's goroutine, so no one recovers a panic here but this: it
		// would end the daemon, and again at every connect.
		defer func() {
			if r := recover(); r != nil {
				m.panicked("group sync panicked", a, r)
			}
		}()
		m.syncGroups(a, cli)
		m.reconcileChats(a, cli)
	}()
}

// syncGroups asks WhatsApp for the account's groups and files each under its
// name. A fetch that fails is only logged: the names then come from the events
// of the groups and from the next connect, and a message of a group is in the
// archive without them all the same.
func (m *Manager) syncGroups(a *account, cli *whatsmeow.Client) {
	ctx, cancel := context.WithTimeout(m.ctx, m.groupsWait)
	defer cancel()
	groups, err := m.net.joinedGroups(cli, ctx)
	if err != nil {
		m.log.Warn("fetch the joined groups; their names come later", "account", a.nick, "err", err)
		return
	}
	upds := make([]archive.ChatUpd, 0, len(groups))
	for _, g := range groups {
		if g != nil {
			upds = appendGroup(upds, a.nick, g.JID, g.Name)
		}
	}
	m.groupNames(a, cli, upds)
}

// groupNames writes the groups with the wait of a write, of its own and not what
// is left of the fetch's.
func (m *Manager) groupNames(a *account, cli *whatsmeow.Client, upds []archive.ChatUpd) {
	ctx, cancel := context.WithTimeout(m.ctx, m.writeWait)
	defer cancel()
	m.touchGroups(ctx, a, cli, upds)
}

// groupNamed files a group under the name an event of it brings.
func (m *Manager) groupNamed(a *account, cli *whatsmeow.Client, jid types.JID, name string) {
	upds := appendGroup(nil, a.nick, jid, name)
	if len(upds) == 0 || upds[0].Name == "" {
		return
	}
	m.groupNames(a, cli, upds)
}

// appendGroup adds the group jid under name to upds, if it is a group's JID.
func appendGroup(upds []archive.ChatUpd, nick string, jid types.JID, name string) []archive.ChatUpd {
	if jid.Server != types.GroupServer || jid.User == "" {
		return upds
	}
	return append(upds, archive.ChatUpd{
		Account: nick, JID: types.NewJID(jid.User, types.GroupServer).String(),
		Name: strings.TrimSpace(name), IsGroup: true,
	})
}

// touchGroups writes the groups in one transaction, unless the account is no
// longer the client's: the groups of an account that was removed while they were
// being fetched must not come back as rows of it. A failure is logged and goes
// no further: unlike a message, a name has no acknowledgement to withhold.
func (m *Manager) touchGroups(ctx context.Context, a *account, cli *whatsmeow.Client, upds []archive.ChatUpd) {
	if len(upds) == 0 {
		return
	}
	state, done := m.enterEvent(a, cli)
	if state != eventLive {
		return
	}
	defer done()
	err := m.db.Tx(ctx, func(t *archive.Tx) error {
		for _, u := range upds {
			if err := t.TouchChat(u); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		m.log.Warn("store the names of groups", "account", a.nick, "err", err)
	}
}
