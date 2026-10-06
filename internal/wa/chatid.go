package wa

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// lidLookup is what the identity of a chat needs of whatsmeow's LID store
// (store.LIDStore has it). Both answer from the store, with no call to
// WhatsApp, and a JID the store does not know is the empty JID, not an error.
type lidLookup interface {
	GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error)
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
}

// Chat is the identity under which the archive keeps a chat, the one a message
// is filed under when it comes and a tool looks it up by.
type Chat struct {
	// JID is the archive's key of the chat: a group's, the LID of a private chat
	// when it is known, or else the phone number's JID.
	JID string
	// PN is the phone JID of a private chat kept under its LID, when it is known:
	// for display and for search by number. It is empty for a chat kept under the
	// phone JID (that is JID) and for a group.
	PN      string
	IsGroup bool
}

// Merge reports the phone JID and the LID of a private chat whose number the
// store has a LID for. Whatever was filed under the phone JID before the LID was
// known belongs under the LID (archive.Tx.MergeChat); it is the caller's to do,
// each time and not only when the chat has just turned from a phone JID into a
// LID: WhatsApp now addresses a chat by its LID from the start, and the phone
// JID is then only the alternative address on the message, so the rows that an
// older message or a history sync put under the phone JID would never be
// brought over.
func (c Chat) Merge() (pn, lid string, ok bool) {
	return c.PN, c.JID, c.PN != ""
}

// archivableServer tells whether the archive has chats on that JID server:
// private ones, by phone number or by LID, and groups. The rest of what a JID
// can name (statuses, broadcast lists, channels, bots) is not a conversation.
func archivableServer(server string) bool {
	switch server {
	case types.DefaultUserServer, types.LegacyUserServer, types.HostedServer,
		types.HiddenUserServer, types.HostedLIDServer, types.GroupServer:
		return true
	}
	return false
}

var (
	digitsRe  = regexp.MustCompile(`^[0-9]+$`)
	groupIDRe = regexp.MustCompile(`^[0-9]+(-[0-9]+)*$`)
)

// errBadChat is the answer for what is neither a JID nor a phone number. It is
// for the agent that gave the chat, and holds nothing of it.
var errBadChat = errors.New(`not a chat: give a JID (79991234567@s.whatsapp.net, 1234567890@lid, 120363025246125486@g.us) or a phone number such as "+7 999 123-45-67"`)

// ParseChat reads a chat as a person or an agent writes it: a JID, or a phone
// number with the + and the spaces, dashes and brackets people put in it, which
// is the JID of that number. Whatever is behind the colon or the dot of a JID
// (the device, the agent) is dropped. It does not look at the LID store
// (CanonicalChat does).
func ParseChat(s string) (types.JID, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "@") {
		if !ValidPhone(s) {
			return types.EmptyJID, errBadChat
		}
		return types.NewJID(phoneDigits(s), types.DefaultUserServer), nil
	}
	// ParseJID would read "a@b@c" as a@b and drop the rest.
	if strings.Count(s, "@") != 1 {
		return types.EmptyJID, errBadChat
	}
	jid, err := types.ParseJID(s)
	if err != nil || !archivableServer(jid.Server) {
		return types.EmptyJID, errBadChat
	}
	valid := digitsRe
	if jid.Server == types.GroupServer {
		valid = groupIDRe
	}
	if !valid.MatchString(jid.User) {
		return types.EmptyJID, errBadChat
	}
	return jid.ToNonAD(), nil
}

// CanonicalChat is the Chat that the text chat, a JID or a phone number, names:
// the identity both the messages that come in and the tools that read the
// archive use, so that they find each other. A group is as it is. A phone
// number or its JID is the LID of that number if the store has one, and
// otherwise the phone JID as it came; a LID stays. Devices and agents are
// dropped. The store is asked and nothing else, so no call goes to WhatsApp.
func CanonicalChat(ctx context.Context, lids lidLookup, chat string) (Chat, error) {
	jid, err := ParseChat(chat)
	if err != nil {
		return Chat{}, err
	}
	return canonical(ctx, lids, jid)
}

// canonical is CanonicalChat of a JID that is already parsed, as the one of a
// received message is. It fails only for a chat the archive has no kind for and
// for a store that cannot be read.
func canonical(ctx context.Context, lids lidLookup, jid types.JID) (Chat, error) {
	if jid.User == "" || !archivableServer(jid.Server) {
		return Chat{}, errBadChat
	}
	switch jid.Server {
	case types.GroupServer:
		return Chat{JID: types.NewJID(jid.User, types.GroupServer).String(), IsGroup: true}, nil
	case types.HiddenUserServer, types.HostedLIDServer:
		return canonicalLID(ctx, lids, types.NewJID(jid.User, types.HiddenUserServer))
	}
	// Whatsmeow itself reads a hosted JID as the number's own (message.go:144-147, 173-176).
	return canonicalPN(ctx, lids, types.NewJID(jid.User, types.DefaultUserServer))
}

func canonicalPN(ctx context.Context, lids lidLookup, pn types.JID) (Chat, error) {
	lid, err := lids.GetLIDForPN(ctx, pn)
	if err != nil {
		return Chat{}, fmt.Errorf("look up the LID of a phone number: %w", err)
	}
	if lid.Server != types.HiddenUserServer || lid.User == "" {
		return Chat{JID: pn.String()}, nil
	}
	return Chat{JID: types.NewJID(lid.User, types.HiddenUserServer).String(), PN: pn.String()}, nil
}

func canonicalLID(ctx context.Context, lids lidLookup, lid types.JID) (Chat, error) {
	pn, err := lids.GetPNForLID(ctx, lid)
	if err != nil {
		return Chat{}, fmt.Errorf("look up the phone number of a LID: %w", err)
	}
	c := Chat{JID: lid.String()}
	if pn.Server == types.DefaultUserServer && pn.User != "" {
		c.PN = types.NewJID(pn.User, types.DefaultUserServer).String()
	}
	return c, nil
}
