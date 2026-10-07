package wa

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// What the read tools ask of the Manager: the identity of a chat and of a person,
// and the names of people. All of it comes from the databases; nothing here goes
// to WhatsApp, so a read works whatever the status of the account is.

// maxPeople bounds the addresses that a name may stand for, which end up in the
// IN list of a search: a name that fits hundreds of people is no filter.
const maxPeople = 100

// The two ways a sender can fail to name people. A caller may tell the agent either
// as it is: what they say holds nothing of what was asked for.
var (
	ErrNotAPerson    = errors.New("sender is a group, not a person: give a phone number, a JID of a person or a name")
	ErrTooManyPeople = errors.New("sender names too many people: give a phone number or a longer name")
)

// CanonicalChat is the chat that a JID or a phone number names, under the
// identity the archive files it by (see the function of that name). The LID store
// is the container's, shared by all accounts, so the answer does not depend on
// the account, and an account without a device, one that has to be linked again,
// reads its archive by the same keys as any other.
func (m *Manager) CanonicalChat(ctx context.Context, chat string) (Chat, error) {
	return CanonicalChat(ctx, m.store.LIDMap, chat)
}

// SenderJIDs is the addresses, by number and by LID, of the people that sender
// names in the archive of the account: a JID or a phone number is the person of
// it (both forms, if the LID store knows the pair), and anything else is a name,
// which the account's private chats and its contacts are searched for, ignoring
// case and ё/е. The result is sorted and is empty if the name fits no one.
func (m *Manager) SenderJIDs(ctx context.Context, nick, sender string) ([]string, error) {
	sender = strings.TrimSpace(sender)
	if jid, err := ParseChat(sender); err == nil {
		return m.addressesOfPerson(ctx, jid)
	}
	return m.peopleNamed(ctx, nick, sender)
}

func (m *Manager) addressesOfPerson(ctx context.Context, jid types.JID) ([]string, error) {
	c, err := canonical(ctx, m.store.LIDMap, jid)
	if err != nil {
		return nil, err
	}
	if c.IsGroup {
		return nil, ErrNotAPerson
	}
	return addresses(c), nil
}

// addresses is the JIDs a private chat is known by: the key it is filed under
// and its number, which is another one when the key is a LID.
func addresses(c Chat) []string {
	if c.PN == "" {
		return []string{c.JID}
	}
	return []string{c.JID, c.PN}
}

// peopleNamed is the addresses of the people whose name has name in it, from the
// names of the account's private chats and from its contacts.
func (m *Manager) peopleNamed(ctx context.Context, nick, name string) ([]string, error) {
	found := map[string]bool{}
	chats, err := m.db.Chats(ctx, archive.ChatQuery{Accounts: []string{nick}, Query: name, Limit: math.MaxInt32})
	if err != nil {
		return nil, fmt.Errorf("search the chats for a name: %w", err)
	}
	for _, c := range chats {
		if !c.IsGroup {
			found[c.JID] = true
			if c.PN != "" {
				found[c.PN] = true
			}
		}
	}
	if err := m.contactsNamed(ctx, nick, name, found); err != nil {
		return nil, err
	}
	if len(found) > maxPeople {
		return nil, ErrTooManyPeople
	}
	return slices.Sorted(maps.Keys(found)), nil
}

// contactsNamed adds to found the contacts of the account that have name in one
// of their names, and the other address of each.
func (m *Manager) contactsNamed(ctx context.Context, nick, name string, found map[string]bool) error {
	cli := m.clientOf(nick)
	if cli == nil || cli.Store == nil || cli.Store.Contacts == nil {
		return nil // no device: its contacts went with it
	}
	contacts, err := cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return fmt.Errorf("read the contacts: %w", err)
	}
	want := foldName(name)
	for jid, info := range contacts {
		if !namedLike(info, want) {
			continue
		}
		forms, err := addressesOf(ctx, m.store.LIDMap, jid.ToNonAD())
		if err != nil {
			return fmt.Errorf("look up the other address of a contact: %w", err)
		}
		for _, f := range forms {
			found[f.String()] = true
		}
	}
	return nil
}

// Namer gives the name a person is known by to an account, from their JID in
// either form, or "" when nothing is known: a contact's name, or what they call
// themselves.
type Namer func(jid string) string

// Names is the Namer of the account. An account without a device has none of
// the names, which went with the device, and its Namer says "" to all.
func (m *Manager) Names(ctx context.Context, nick string) Namer {
	cli := m.clientOf(nick)
	if cli == nil || cli.Store == nil || cli.Store.Contacts == nil {
		return func(string) string { return "" }
	}
	return func(jid string) string { return m.personName(ctx, cli, jid) }
}

func (m *Manager) personName(ctx context.Context, cli *whatsmeow.Client, jid string) string {
	j, err := types.ParseJID(jid)
	if err != nil {
		return ""
	}
	forms, err := addressesOf(ctx, m.store.LIDMap, j.ToNonAD())
	if err != nil {
		forms = []types.JID{j.ToNonAD()} // the name by the address as it is
	}
	for _, f := range forms {
		info, err := cli.Store.Contacts.GetContact(ctx, f)
		if err != nil {
			m.log.Warn("read a contact", "err", err)
			continue
		}
		if n := displayName(info); n != "" {
			return n
		}
	}
	return ""
}

// displayName is the name of a contact as WhatsApp shows it: the name in the
// address book, then what they call themselves, then a business's name.
func displayName(c types.ContactInfo) string {
	for _, n := range []string{c.FullName, c.FirstName, c.PushName, c.BusinessName} {
		if n != "" {
			return n
		}
	}
	return ""
}

// namedLike tells whether any name of the contact has want (already folded) in it.
func namedLike(c types.ContactInfo, want string) bool {
	for _, n := range []string{c.FullName, c.FirstName, c.PushName, c.BusinessName} {
		if n != "" && strings.Contains(foldName(n), want) {
			return true
		}
	}
	return false
}

// yoFold folds ё into е, as the archive does for names and for the text of
// messages, so that a person who types either finds both.
var yoFold = strings.NewReplacer("ё", "е", "Ё", "Е")

func foldName(s string) string { return strings.ToLower(yoFold.Replace(s)) }

// clientOf is the client of the account, or nil for an account that is unknown
// or has no device.
func (m *Manager) clientOf(nick string) *whatsmeow.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.accounts[nick]; a != nil {
		return a.cli
	}
	return nil
}
