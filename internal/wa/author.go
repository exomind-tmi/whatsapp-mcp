package wa

import (
	"context"
	"errors"
	"slices"

	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// mayChange tells whether an edit or a revoke may be applied to the message it
// names. Only the one who wrote a message edits it, and in a private chat only
// the one who wrote it deletes it, as WhatsApp's own apps hold; anything else
// would let whoever can message the account, or sit in a group with it, rewrite
// what the account or another member said: a message is keyed by its chat and
// its id, the id is known to everyone who read the message, and the agent that
// reads the archive takes what it finds for what was said.
//
// The sender is the stanza's (info), and not the key's inside the message, which
// is written by whoever sent it. A message the archive does not have yet has
// nothing to be compared with, and gets the stub the operation makes (the edit
// of a message may come before the message). In a group an admin may delete what
// another member wrote, and the admins are not kept, so a delete there is let
// through.
//
// Only what comes live is held to this: the history of the phone is the phone's
// own account of who wrote what, and has no stanza to check it by.
func (m *Manager) mayChange(ctx context.Context, lids lidLookup, nick string, chat Chat, info types.MessageInfo, op Op) (bool, error) {
	if op.Kind != OpEdit && op.Kind != OpRevoke {
		return true, nil
	}
	id := op.Edit.ID
	if op.Kind == OpRevoke {
		id = op.Revoke.ID
	}
	was, found, err := m.archived(ctx, nick, chat, id)
	if err != nil || !found {
		return true, err
	}
	switch {
	case op.Kind == OpRevoke && chat.IsGroup:
		return true, nil
	case was.FromMe != info.IsFromMe:
		return false, nil
	case chat.IsGroup && !info.IsFromMe:
		return sameSender(ctx, lids, was.Sender, info)
	}
	return true, nil
}

// archived is the message of the chat with that id, wherever the chat keeps it:
// under its LID, or still under the phone number if the merge of the two is yet
// to come.
func (m *Manager) archived(ctx context.Context, nick string, chat Chat, id string) (archive.Message, bool, error) {
	for _, key := range []string{chat.JID, chat.PN} {
		if key == "" {
			continue
		}
		msg, err := m.db.Message(ctx, nick, key, id)
		if err == nil {
			return msg, true, nil
		}
		if !errors.Is(err, archive.ErrNoMessage) {
			return archive.Message{}, false, err
		}
	}
	return archive.Message{}, false, nil
}

// sameSender tells whether the sender of the stanza is the one a message was
// filed under (as received, so by number or by LID): by either of the two
// addresses the stanza gives for them, or the one the LID store pairs with them.
func sameSender(ctx context.Context, lids lidLookup, filed string, info types.MessageInfo) (bool, error) {
	was, err := types.ParseJID(filed)
	if err != nil {
		return false, nil
	}
	for _, j := range []types.JID{info.Sender, info.SenderAlt} {
		if j.IsEmpty() {
			continue
		}
		forms, err := addressesOf(ctx, lids, j.ToNonAD())
		if err != nil {
			return false, err
		}
		if slices.Contains(forms, was.ToNonAD()) {
			return true, nil
		}
	}
	return false, nil
}

// addressesOf is jid and, if the LID store knows it, the other address of the
// same person.
func addressesOf(ctx context.Context, lids lidLookup, jid types.JID) ([]types.JID, error) {
	var other types.JID
	var err error
	switch jid.Server {
	case types.DefaultUserServer:
		other, err = lids.GetLIDForPN(ctx, jid)
	case types.HiddenUserServer:
		other, err = lids.GetPNForLID(ctx, jid)
	}
	if err != nil {
		return nil, err
	}
	if other.IsEmpty() {
		return []types.JID{jid}, nil
	}
	return []types.JID{jid, other.ToNonAD()}, nil
}
