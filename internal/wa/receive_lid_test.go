package wa

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// learn makes the account's client know that LID and number are one person, as
// whatsmeow does from a group's participants, a sync or another message.
func (r *rx) learn(t *testing.T, nick string) {
	t.Helper()
	if err := r.cli(nick).Store.LIDs.PutLIDMapping(context.Background(), lidJID(bobLID), pnJID(bobPN)); err != nil {
		t.Fatal(err)
	}
}

// byLID is a message from Bob as WhatsApp now addresses one: by his LID, with
// his number as the other address.
func byLID(id string, d time.Duration) types.MessageInfo {
	i := at(incoming(id, lidJID(bobLID)), d)
	i.SenderAlt = pnJID(bobPN)
	return i
}

// requireOneChatUnderTheLID is the archive after Bob's chat was merged: his
// messages, and nothing else, under the LID; one chat, with the number for
// display and the name; nothing under the number; and the index finds each
// message once, by the text it has now.
func (r *rx) requireOneChatUnderTheLID(t *testing.T, nick string, wantIDs ...string) {
	t.Helper()
	cs := r.chats(t, nick)
	if len(cs) != 1 || cs[0].JID != bobLIDChat || cs[0].PN != bobPNChat || cs[0].Name != "Bob" || cs[0].IsGroup {
		t.Fatalf("chats %+v, want Bob's, one, under the LID with his number and name", cs)
	}
	if got := ids(r.msgs(t, nick, bobLIDChat)); !equalIDs(got, wantIDs) {
		t.Errorf("messages under the LID %v, want %v", got, wantIDs)
	}
	if got := r.msgs(t, nick, bobPNChat); len(got) != 0 {
		t.Errorf("messages are left under the number: %+v", got)
	}
	if n := r.size(t, nick); n != [2]int{1, len(wantIDs)} {
		t.Errorf("the archive has %v chats and messages", n)
	}
	accs, err := r.db.AccountsForChat(context.Background(), bobPNChat)
	if err != nil || !slices.Contains(accs, nick) {
		t.Errorf("accounts of the chat by its number: %v, %v; want %s among them", accs, err, nick)
	}
	hits, err := r.db.Search(context.Background(), archive.SearchQuery{Accounts: []string{nick}, Query: "edited-beta", Limit: 10})
	if err != nil || len(hits) != 1 || hits[0].Chat != bobLIDChat || hits[0].ID != "M1" {
		t.Errorf("search for the text of the edit: %+v, %v; want M1 under the LID", hits, err)
	}
	if hits, err := r.db.Search(context.Background(), archive.SearchQuery{Accounts: []string{nick}, Query: "unique-alpha", Limit: 10}); err != nil || len(hits) != 0 {
		t.Errorf("search for the text before the edit: %+v, %v; want none", hits, err)
	}
	if m, err := r.db.Message(context.Background(), nick, bobLIDChat, "M1"); err != nil || m.Text != "one edited-beta" || m.EditedAt.IsZero() {
		t.Errorf("M1 = %+v, %v; want the edited text and its mark kept", m, err)
	}
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// seedBobByNumber is what the archive has of Bob before his LID is known: three
// messages under his number, one of them edited.
func (r *rx) seedBobByNumber(t *testing.T, nick string) {
	t.Helper()
	bob := pnJID(bobPN)
	r.mustDeliver(t, nick, at(incoming("M1", bob), 1*time.Second), text("one unique-alpha"))
	r.mustDeliver(t, nick, at(incoming("M2", bob), 2*time.Second), text("two"))
	r.mustDeliver(t, nick, at(incoming("E1", bob), 3*time.Second), edit("M1", text("one edited-beta")))
	if got := r.chats(t, nick); len(got) != 1 || got[0].JID != bobPNChat || got[0].PN != "" {
		t.Fatalf("before the LID is known: chats %+v, want one under the number", got)
	}
}

// TestReceiveLIDFlip: Bob's messages are filed under his number until WhatsApp
// tells which LID that is, and from then on under the LID. What was filed under
// the number goes over once, whole, and is not doubled when it comes again.
func TestReceiveLIDFlip(t *testing.T) {
	t.Run("the pair comes with a message addressed by LID", func(t *testing.T) {
		r := newRx(t)
		r.seedBobByNumber(t, "personal")
		r.mustDeliver(t, "personal", byLID("M3", 4*time.Second), text("three"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2", "M3")

		// The phone sends again what it did not see acknowledged, still by the number.
		r.mustDeliver(t, "personal", at(incoming("M1", pnJID(bobPN)), 1*time.Second), text("one unique-alpha"))
		r.mustDeliver(t, "personal", at(incoming("M2", pnJID(bobPN)), 2*time.Second), text("two"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2", "M3")
		if cs := r.chats(t, "personal"); !cs[0].LastMessageTS.Equal(t0.Add(4 * time.Second)) {
			t.Errorf("the chat's last message is %v, not M3's", cs[0].LastMessageTS)
		}
	})

	t.Run("the pair is known before a message by the number comes", func(t *testing.T) {
		r := newRx(t)
		r.seedBobByNumber(t, "personal")
		r.learn(t, "personal")
		r.mustDeliver(t, "personal", at(incoming("M3", pnJID(bobPN)), 4*time.Second), text("three"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2", "M3")
	})

	t.Run("a message of ours to the number", func(t *testing.T) {
		r := newRx(t)
		r.seedBobByNumber(t, "personal")
		own := at(outgoing("M3", pnJID(bobPN)), 4*time.Second)
		own.RecipientAlt = lidJID(bobLID) // whatsmeow's peer_recipient_lid
		r.mustDeliver(t, "personal", own, text("three"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2", "M3")
		if got := r.msgs(t, "personal", bobLIDChat)[2]; !got.FromMe {
			t.Errorf("the message of ours = %+v", got)
		}
	})

	t.Run("an edit or a revoke of an earlier message, by LID", func(t *testing.T) {
		r := newRx(t)
		r.seedBobByNumber(t, "personal")
		e := byLID("D1", 4*time.Second)
		r.mustDeliver(t, "personal", e, revoke("M2"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2")
		if m, err := r.db.Message(context.Background(), "personal", bobLIDChat, "M2"); err != nil || m.RevokedAt.IsZero() {
			t.Errorf("M2 = %+v, %v; want it marked revoked where it now lives", m, err)
		}
	})

	// The edit came by LID before the number was known, so it lies under the LID as
	// a stub; the original lies under the number. When the pair is known both are
	// one message, with the original's header and raw message and the edit's text.
	t.Run("a stub under the LID and the original under the number", func(t *testing.T) {
		r := newRx(t)
		bob := pnJID(bobPN)
		r.mustDeliver(t, "personal", at(incoming("M1", bob), 1*time.Second), text("one unique-alpha"))
		noAlt := at(incoming("E1", lidJID(bobLID)), 3*time.Second)
		r.mustDeliver(t, "personal", noAlt, edit("M1", text("one edited-beta")))
		if got := r.chats(t, "personal"); len(got) != 2 {
			t.Fatalf("chats %+v, want Bob's under the number and under the LID, apart until the pair is known", got)
		}
		r.learn(t, "personal")
		r.mustDeliver(t, "personal", byLID("M2", 4*time.Second), text("two"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2")
		m, err := r.db.MessageWithRaw(context.Background(), "personal", bobLIDChat, "M1")
		if err != nil || !m.TS.Equal(t0.Add(time.Second)) || m.Sender != bobPNChat || len(m.Raw) == 0 {
			t.Errorf("M1 = %+v, %v; want the original's header and raw message", m, err)
		}
	})

	// The same message by the number and by the LID, which came apart because the
	// pair was not known: one message after the merge, with what each had.
	t.Run("the same message under the number and under the LID", func(t *testing.T) {
		r := newRx(t)
		r.mustDeliver(t, "personal", at(incoming("M1", pnJID(bobPN)), 1*time.Second), text("one unique-alpha"))
		r.mustDeliver(t, "personal", at(incoming("M1", lidJID(bobLID)), 1*time.Second), text("one unique-alpha"))
		r.mustDeliver(t, "personal", at(incoming("E1", lidJID(bobLID)), 3*time.Second), edit("M1", text("one edited-beta")))
		r.learn(t, "personal")
		r.mustDeliver(t, "personal", byLID("M2", 4*time.Second), text("two"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2")
	})

	// The pair is the number's, and the accounts share the store of them; the
	// archive is not shared: the other account's chat with Bob moves when it gets
	// a message of its own.
	t.Run("the other account's chat is its own", func(t *testing.T) {
		r := newRx(t)
		r.seedBobByNumber(t, "personal")
		r.seedBobByNumber(t, "work")
		r.mustDeliver(t, "personal", byLID("M3", 4*time.Second), text("three"))
		r.requireOneChatUnderTheLID(t, "personal", "M1", "M2", "M3")
		if got := ids(r.msgs(t, "work", bobPNChat)); !equalIDs(got, []string{"M1", "M2"}) {
			t.Errorf("work's messages under the number: %v, want them where they were", got)
		}
		r.mustDeliver(t, "work", at(incoming("M3", pnJID(bobPN)), 4*time.Second), text("three"))
		r.requireOneChatUnderTheLID(t, "work", "M1", "M2", "M3")
	})

	// A chat that is new under its LID has nothing to merge, and has its number all
	// the same: from the message, or from what the store knows.
	t.Run("a chat first seen by its LID has the number", func(t *testing.T) {
		for name, tc := range map[string]struct {
			info  types.MessageInfo
			learn bool
		}{
			"from the other address of the message": {byLID("M1", time.Second), false},
			"from the store":                        {at(incoming("M1", lidJID(bobLID)), time.Second), true},
		} {
			t.Run(name, func(t *testing.T) {
				r := newRx(t)
				if tc.learn {
					r.learn(t, "personal")
				}
				r.mustDeliver(t, "personal", tc.info, text("hello"))
				cs := r.chats(t, "personal")
				if len(cs) != 1 || cs[0].JID != bobLIDChat || cs[0].PN != bobPNChat || cs[0].Name != "Bob" {
					t.Errorf("chats %+v, want one under the LID, with the number and the name", cs)
				}
			})
		}
	})

	t.Run("groups are never merged", func(t *testing.T) {
		r := newRx(t)
		r.learn(t, "personal")
		r.mustDeliver(t, "personal", at(inGroup("G1", pnJID(bobPN)), time.Second), text("hi all"))
		r.mustDeliver(t, "personal", at(inGroup("G2", lidJID(bobLID)), 2*time.Second), text("hi again"))
		cs := r.chats(t, "personal")
		if len(cs) != 1 || cs[0].JID != groupID+"@g.us" || cs[0].PN != "" || !cs[0].IsGroup {
			t.Errorf("chats %+v, want the group alone", cs)
		}
		if got := r.msgs(t, "personal", groupID+"@g.us"); len(got) != 2 || got[0].Sender != bobPNChat || got[1].Sender != bobLIDChat {
			t.Errorf("group messages %+v: the sender stays as it came", got)
		}
	})
}

// recLIDs is a LID store that only writes down the pairs it is given.
type recLIDs struct{ puts [][2]types.JID }

func (r *recLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) {
	return types.EmptyJID, nil
}
func (r *recLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	return types.EmptyJID, nil
}
func (r *recLIDs) PutLIDMapping(_ context.Context, lid, pn types.JID) error {
	r.puts = append(r.puts, [2]types.JID{lid, pn})
	return nil
}

// TestLearnLIDsKeepsOnlyAPairOfALIDAndANumber: what a message gives to the store is
// a LID and a phone number, in either order and without the device; two of one
// kind, a group or nothing is no pair, and a wrong one would make two people one
// chat.
func TestLearnLIDsKeepsOnlyAPairOfALIDAndANumber(t *testing.T) {
	r := newRx(t)
	for name, tc := range map[string]struct{ x, y types.JID }{
		"two LIDs":                        {lidJID(bobLID), lidJID(carolLID)},
		"two numbers":                     {pnJID(bobPN), pnJID(carolPN)},
		"a LID and a group":               {lidJID(bobLID), groupJID()},
		"a LID and nothing":               {lidJID(bobLID), types.EmptyJID},
		"a number and a LID with no user": {pnJID(bobPN), types.NewJID("", types.HiddenUserServer)},
		"a number with no user and a LID": {types.NewJID("", types.DefaultUserServer), lidJID(bobLID)},
	} {
		rec := &recLIDs{}
		info := types.MessageInfo{MessageSource: types.MessageSource{Sender: tc.x, SenderAlt: tc.y}}
		r.m.learnLIDs(context.Background(), rec, info, "personal")
		if len(rec.puts) != 0 {
			t.Errorf("%s: kept as a pair: %v", name, rec.puts)
		}
	}

	want := [2]types.JID{lidJID(bobLID), pnJID(bobPN)}
	for name, src := range map[string]types.MessageSource{
		"the sender, a number first":    {Sender: types.NewADJID(bobPN, 0, 5), SenderAlt: types.JID{User: bobLID, Device: 5, Server: types.HiddenUserServer}},
		"the sender, a LID first":       {Sender: lidJID(bobLID), SenderAlt: pnJID(bobPN)},
		"the recipient of our message":  {Chat: pnJID(bobPN), RecipientAlt: lidJID(bobLID)},
		"the recipient, a LID chat":     {Chat: lidJID(bobLID), RecipientAlt: pnJID(bobPN)},
		"both, which are the same pair": {Sender: lidJID(bobLID), SenderAlt: pnJID(bobPN), Chat: pnJID(bobPN), RecipientAlt: lidJID(bobLID)},
	} {
		rec := &recLIDs{}
		r.m.learnLIDs(context.Background(), rec, types.MessageInfo{MessageSource: src}, "personal")
		if len(rec.puts) == 0 || rec.puts[0] != want {
			t.Errorf("%s: kept %v, want %v", name, rec.puts, want)
		}
	}
}
