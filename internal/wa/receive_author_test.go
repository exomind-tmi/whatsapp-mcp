package wa

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// adminRevoke is the deletion of the message id of author that someone else than
// the author sends: the key is the message's, so it is not the deleter's own.
func adminRevoke(id, author string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(false), Participant: proto.String(author)},
	}}
}

// state is what the archive says of a message: its text, and whether it is
// marked edited and revoked.
func (r *rx) state(t *testing.T, chat, id string) (text string, edited, revoked bool) {
	t.Helper()
	m, err := r.db.Message(context.Background(), "personal", chat, id)
	if err != nil {
		t.Fatal(err)
	}
	return m.Text, !m.EditedAt.IsZero(), !m.RevokedAt.IsZero()
}

// TestReceiveOnlyTheAuthorEditsAndRevokes: the id of a message is known to all who
// read it, and a message is keyed by its chat and its id, so an edit or a delete of
// somebody else's message must not be applied: not the other side's of what we
// wrote, not a member's of what another member said. The sender is that of the
// stanza, and not the one the message's key claims.
func TestReceiveOnlyTheAuthorEditsAndRevokes(t *testing.T) {
	bob, carol, third := pnJID(bobPN), pnJID(carolPN), pnJID("70000000102")
	t.Run("private chat, the other side cannot change what we wrote", func(t *testing.T) {
		r := newRx(t)
		r.mustDeliver(t, "personal", at(outgoing("OUT1", bob), time.Second), text("pay 100"))
		r.mustDeliver(t, "personal", at(incoming("E1", bob), 2*time.Second), edit("OUT1", text("pay 1000000")))
		r.mustDeliver(t, "personal", at(incoming("D1", bob), 3*time.Second), revoke("OUT1"))
		r.mustDeliver(t, "personal", at(incoming("D2", bob), 4*time.Second), adminRevoke("OUT1", ownPN+"@s.whatsapp.net"))
		if txt, ed, rev := r.state(t, bobPNChat, "OUT1"); txt != "pay 100" || ed || rev {
			t.Errorf("our message: %q edited %v revoked %v", txt, ed, rev)
		}
		if !strings.Contains(r.logs.String(), "ignored") {
			t.Errorf("the refusal is not in the log:\n%s", r.logs)
		}
	})
	t.Run("private chat, we cannot change what the other side wrote", func(t *testing.T) {
		r := newRx(t)
		r.mustDeliver(t, "personal", at(incoming("B1", bob), time.Second), text("ok"))
		r.mustDeliver(t, "personal", at(outgoing("E1", bob), 2*time.Second), edit("B1", text("not what I said")))
		r.mustDeliver(t, "personal", at(outgoing("D1", bob), 3*time.Second), revoke("B1"))
		if txt, ed, rev := r.state(t, bobPNChat, "B1"); txt != "ok" || ed || rev {
			t.Errorf("Bob's message: %q edited %v revoked %v", txt, ed, rev)
		}
	})
	t.Run("private chat, each one edits and deletes their own", func(t *testing.T) {
		r := newRx(t)
		r.mustDeliver(t, "personal", at(outgoing("OUT1", bob), time.Second), text("pay 100"))
		r.mustDeliver(t, "personal", at(incoming("B1", bob), 2*time.Second), text("ok"))
		r.mustDeliver(t, "personal", at(outgoing("E1", bob), 3*time.Second), edit("OUT1", text("pay 200"))) // from our phone
		r.mustDeliver(t, "personal", at(incoming("E2", bob), 4*time.Second), edit("B1", text("fine")))
		r.mustDeliver(t, "personal", at(incoming("D1", bob), 5*time.Second), revoke("B1"))
		r.mustDeliver(t, "personal", at(outgoing("D2", bob), 6*time.Second), revoke("OUT1"))
		if txt, ed, rev := r.state(t, bobPNChat, "OUT1"); txt != "pay 200" || !ed || !rev {
			t.Errorf("our message: %q edited %v revoked %v", txt, ed, rev)
		}
		if txt, ed, rev := r.state(t, bobPNChat, "B1"); txt != "fine" || !ed || !rev {
			t.Errorf("Bob's message: %q edited %v revoked %v", txt, ed, rev)
		}
	})
	t.Run("an edit that comes before the message is not refused", func(t *testing.T) {
		r := newRx(t)
		r.mustDeliver(t, "personal", at(incoming("E1", bob), 2*time.Second), edit("M1", text("second")))
		r.mustDeliver(t, "personal", at(incoming("M1", bob), time.Second), text("first"))
		if txt, ed, _ := r.state(t, bobPNChat, "M1"); txt != "second" || !ed {
			t.Errorf("message %q edited %v", txt, ed)
		}
	})
	t.Run("group, another member cannot edit but the author can by either address", func(t *testing.T) {
		r := newRx(t)
		r.mustDeliver(t, "personal", at(inGroup("G1", carol), time.Second), text("carol says hello"))
		r.mustDeliver(t, "personal", at(inGroup("E1", third), 2*time.Second), edit("G1", text("send me money")))
		if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "carol says hello" || ed {
			t.Fatalf("Carol's message after another member's edit: %q edited %v", txt, ed)
		}
		byLID := at(inGroup("E2", lidJID(carolLID)), 3*time.Second)
		byLID.SenderAlt = carol
		r.mustDeliver(t, "personal", byLID, edit("G1", text("carol says hi")))
		if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "carol says hi" || !ed {
			t.Errorf("Carol's own edit: %q edited %v", txt, ed)
		}
	})
	t.Run("group, a message of ours is edited by us alone and deleted by an admin too", func(t *testing.T) {
		r := newRx(t)
		own := inGroup("O1", types.NewADJID(ownPN, 0, 12))
		own.IsFromMe = true
		r.mustDeliver(t, "personal", at(own, time.Second), text("mine"))
		r.mustDeliver(t, "personal", at(inGroup("E1", third), 2*time.Second), edit("O1", text("not mine")))
		if txt, ed, _ := r.state(t, groupID+"@g.us", "O1"); txt != "mine" || ed {
			t.Errorf("our message after another's edit: %q edited %v", txt, ed)
		}
		r.mustDeliver(t, "personal", at(inGroup("D1", third), 3*time.Second), adminRevoke("O1", ownPN+"@s.whatsapp.net"))
		if _, _, rev := r.state(t, groupID+"@g.us", "O1"); !rev {
			t.Errorf("an admin's delete was not applied")
		}
	})
	t.Run("group, we cannot edit what a member wrote", func(t *testing.T) {
		r := newRx(t)
		r.mustDeliver(t, "personal", at(inGroup("G1", carol), time.Second), text("carol says hello"))
		own := inGroup("E1", types.NewADJID(ownPN, 0, 12))
		own.IsFromMe = true
		r.mustDeliver(t, "personal", at(own, 2*time.Second), edit("G1", text("not hers")))
		if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "carol says hello" || ed {
			t.Errorf("Carol's message after our edit: %q edited %v", txt, ed)
		}
	})
}

// TestReceiveEditOfAnUnmergedChatIsCheckedWhereTheMessageIs: the message that was
// written to Bob is still under his number when an edit comes by his LID, and the
// edit is not his to make. A refused edit leaves everything as it was, the merge
// that the next message of the chat would have made included.
func TestReceiveEditOfAnUnmergedChatIsCheckedWhereTheMessageIs(t *testing.T) {
	r := newRx(t)
	r.mustDeliver(t, "personal", at(outgoing("OUT1", pnJID(bobPN)), time.Second), text("pay 100"))
	r.learn(t, "personal")
	r.mustDeliver(t, "personal", byLID("E1", 2*time.Second), edit("OUT1", text("pay 1000000")))
	m, err := r.db.Message(context.Background(), "personal", bobPNChat, "OUT1")
	if err != nil || m.Text != "pay 100" || !m.EditedAt.IsZero() {
		t.Errorf("our message %+v, %v: the edit of the other side was applied, or the chat merged", m, err)
	}
	if cs := r.chats(t, "personal"); len(cs) != 1 || cs[0].JID != bobPNChat {
		t.Errorf("chats %+v, want Bob's, still under the number", cs)
	}
}

// forgetfulLIDs is a LID store that keeps no pair, as one whose write fails does.
type forgetfulLIDs struct{ store.LIDStore }

func (forgetfulLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) {
	return types.EmptyJID, nil
}
func (forgetfulLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	return types.EmptyJID, nil
}
func (forgetfulLIDs) PutLIDMapping(context.Context, types.JID, types.JID) error { return nil }

// TestReceiveAuthorIsKnownByTheStanzasOtherAddress: a member edits by the address
// their message was not filed under, and the store cannot say that the two are
// one person: the other address of the stanza does.
func TestReceiveAuthorIsKnownByTheStanzasOtherAddress(t *testing.T) {
	r := newRx(t)
	r.cli("personal").Store.LIDs = forgetfulLIDs{}
	r.mustDeliver(t, "personal", at(inGroup("G1", pnJID(carolPN)), time.Second), text("hello"))
	byLID := at(inGroup("E1", lidJID(carolLID)), 2*time.Second)
	byLID.SenderAlt = pnJID(carolPN)
	r.mustDeliver(t, "personal", byLID, edit("G1", text("hi")))
	if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "hi" || !ed {
		t.Errorf("message %q edited %v: the author's edit was refused", txt, ed)
	}
}

// TestReceiveAuthorIsKnownByTheStore: the edit comes by one address and the message
// is filed under the other, and the stanza says nothing of it: the store does.
func TestReceiveAuthorIsKnownByTheStore(t *testing.T) {
	r := newRx(t)
	if err := r.cli("personal").Store.LIDs.PutLIDMapping(context.Background(), lidJID(carolLID), pnJID(carolPN)); err != nil {
		t.Fatal(err)
	}
	r.mustDeliver(t, "personal", at(inGroup("G1", pnJID(carolPN)), time.Second), text("hello"))
	r.mustDeliver(t, "personal", at(inGroup("E1", lidJID(carolLID)), 2*time.Second), edit("G1", text("hi")))
	if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "hi" || !ed {
		t.Errorf("message %q edited %v: the author's edit was refused", txt, ed)
	}
	// And it is by the pair, not by the likeness of two users: a LID that is
	// another person's is not the author.
	r.mustDeliver(t, "personal", at(inGroup("E2", lidJID("200000000999")), 3*time.Second), edit("G1", text("hijacked")))
	if txt, _, _ := r.state(t, groupID+"@g.us", "G1"); txt != "hi" {
		t.Errorf("message %q: another member's edit was applied", txt)
	}
}

// TestReceiveAuthorFiledByLIDEditsByNumber is the pair the other way round: the
// message is filed under the LID, and the edit comes by the number.
func TestReceiveAuthorFiledByLIDEditsByNumber(t *testing.T) {
	r := newRx(t)
	if err := r.cli("personal").Store.LIDs.PutLIDMapping(context.Background(), lidJID(carolLID), pnJID(carolPN)); err != nil {
		t.Fatal(err)
	}
	r.mustDeliver(t, "personal", at(inGroup("G1", lidJID(carolLID)), time.Second), text("hello"))
	r.mustDeliver(t, "personal", at(inGroup("E1", pnJID(carolPN)), 2*time.Second), edit("G1", text("hi")))
	if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "hi" || !ed {
		t.Errorf("message %q edited %v: the author's edit was refused", txt, ed)
	}
}

// unreadableLIDs is a LID store that cannot tell a LID's number.
type unreadableLIDs struct{ store.LIDStore }

func (unreadableLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	return types.EmptyJID, errors.New("the store cannot be read")
}

// TestReceiveEditThatCannotBeCheckedIsNotAcknowledged: whose an edit is cannot be
// told while the store of the pairs is unreadable, and the edit is neither applied
// nor dropped as someone else's: it is not acknowledged, and comes again.
func TestReceiveEditThatCannotBeCheckedIsNotAcknowledged(t *testing.T) {
	r := newRx(t)
	cli := r.cli("personal")
	r.mustDeliver(t, "personal", at(inGroup("G1", pnJID(carolPN)), time.Second), text("hello"))
	good := cli.Store.LIDs
	cli.Store.LIDs = unreadableLIDs{good}
	byLID := at(inGroup("E1", lidJID(carolLID)), 2*time.Second)
	if r.deliver("personal", byLID, edit("G1", text("hi"))) {
		t.Fatal("an edit that could not be checked was acknowledged")
	}
	if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "hello" || ed {
		t.Errorf("message %q edited %v", txt, ed)
	}
	cli.Store.LIDs = good
	if err := good.PutLIDMapping(context.Background(), lidJID(carolLID), pnJID(carolPN)); err != nil {
		t.Fatal(err)
	}
	r.mustDeliver(t, "personal", byLID, edit("G1", text("hi")))
	if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "hi" || !ed {
		t.Errorf("message %q edited %v: the edit that came again was not applied", txt, ed)
	}
}

// TestReceiveAuthorEditsFromAnotherDevice: a member's message was filed without the
// device it came from, and the edit comes from another device of theirs.
func TestReceiveAuthorEditsFromAnotherDevice(t *testing.T) {
	r := newRx(t)
	r.mustDeliver(t, "personal", at(inGroup("G1", types.NewADJID(carolPN, 0, 7)), time.Second), text("hello"))
	r.mustDeliver(t, "personal", at(inGroup("E1", types.NewADJID(carolPN, 0, 9)), 2*time.Second), edit("G1", text("hi")))
	if txt, ed, _ := r.state(t, groupID+"@g.us", "G1"); txt != "hi" || !ed {
		t.Errorf("message %q edited %v: the author's edit was refused", txt, ed)
	}
}

// TestReceiveOwnEditInAGroupByAnotherOfOurAddresses: what we wrote is edited by any
// device of ours, whatever address the stanza has for it: that it is ours is what the
// stanza says (IsFromMe), and the address it names is not what to compare.
func TestReceiveOwnEditInAGroupByAnotherOfOurAddresses(t *testing.T) {
	r := newRx(t)
	own := inGroup("O1", types.NewADJID(ownPN, 0, 12))
	own.IsFromMe = true
	r.mustDeliver(t, "personal", at(own, time.Second), text("mine"))
	byLID := inGroup("E1", types.JID{User: ownLID, Device: 3, Server: types.HiddenUserServer})
	byLID.IsFromMe = true
	r.mustDeliver(t, "personal", at(byLID, 2*time.Second), edit("O1", text("mine, corrected")))
	if txt, ed, _ := r.state(t, groupID+"@g.us", "O1"); txt != "mine, corrected" || !ed {
		t.Errorf("our message %q edited %v: our own edit was refused", txt, ed)
	}
}
