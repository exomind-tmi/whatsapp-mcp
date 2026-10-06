package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// fakeLIDs is a LID store that answers as the real one does: the device of the
// asked JID is on the answer, and a JID of the wrong server is an error, so that
// a caller that does not strip the device or sends the wrong kind is caught.
type fakeLIDs struct {
	pn2lid map[string]string // phone user -> LID user
	err    error
	asked  []string // the JIDs the lookups were made for
}

func (f *fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	f.asked = append(f.asked, pn.String())
	if f.err != nil {
		return types.EmptyJID, f.err
	}
	if pn.Server != types.DefaultUserServer {
		return types.EmptyJID, fmt.Errorf("invalid GetLIDForPN call with non-PN JID %s", pn)
	}
	if lid, ok := f.pn2lid[pn.User]; ok {
		return types.JID{User: lid, Device: pn.Device, Server: types.HiddenUserServer}, nil
	}
	return types.EmptyJID, nil
}

func (f *fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	f.asked = append(f.asked, lid.String())
	if f.err != nil {
		return types.EmptyJID, f.err
	}
	if lid.Server != types.HiddenUserServer {
		return types.EmptyJID, fmt.Errorf("invalid GetPNForLID call with non-LID JID %s", lid)
	}
	for pn, l := range f.pn2lid {
		if l == lid.User {
			return types.JID{User: pn, Device: lid.Device, Server: types.DefaultUserServer}, nil
		}
	}
	return types.EmptyJID, nil
}

func TestCanonicalChat(t *testing.T) {
	known := map[string]string{bobPN: bobLID}
	for _, tc := range []struct {
		name  string
		chat  string
		known map[string]string
		want  Chat
	}{
		// A phone number, as people write it.
		{"phone with plus and spaces", "+7 999 123-45-67", nil, Chat{JID: "79991234567@s.whatsapp.net"}},
		{"phone in brackets", "+7 (999) 123-45-67", nil, Chat{JID: "79991234567@s.whatsapp.net"}},
		{"phone as digits", "79991234567", nil, Chat{JID: "79991234567@s.whatsapp.net"}},
		{"phone with spaces around", "  79991234567\n", nil, Chat{JID: "79991234567@s.whatsapp.net"}},
		{"phone with a LID known", "+" + bobPN, known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},

		// A phone JID: the LID if the store has one, else itself.
		{"phone JID, LID unknown", bobPN + "@s.whatsapp.net", nil, Chat{JID: bobPN + "@s.whatsapp.net"}},
		{"phone JID, another number's LID known", carolPN + "@s.whatsapp.net", known, Chat{JID: carolPN + "@s.whatsapp.net"}},
		{"phone JID, LID known", bobPN + "@s.whatsapp.net", known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},
		{"phone JID of a device", bobPN + ":12@s.whatsapp.net", known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},
		{"phone JID of an agent's device", bobPN + ".0:12@s.whatsapp.net", known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},
		{"phone JID of a device, LID unknown", bobPN + ":12@s.whatsapp.net", nil, Chat{JID: bobPN + "@s.whatsapp.net"}},
		{"legacy server", bobPN + "@c.us", known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},
		{"hosted phone", bobPN + "@hosted", nil, Chat{JID: bobPN + "@s.whatsapp.net"}},

		// A LID stays, with the phone JID if it is known.
		{"LID, phone unknown", bobLID + "@lid", nil, Chat{JID: bobLID + "@lid"}},
		{"LID, phone known", bobLID + "@lid", known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},
		{"LID of a device", bobLID + ":5@lid", known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},
		{"hosted LID", bobLID + "@hosted.lid", known, Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}},

		// A group is as it is.
		{"group", groupID + "@g.us", known, Chat{JID: groupID + "@g.us", IsGroup: true}},
		{"group of the old kind", "70000000100-1500000000@g.us", known, Chat{JID: "70000000100-1500000000@g.us", IsGroup: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lids := &fakeLIDs{pn2lid: tc.known}
			got, err := CanonicalChat(context.Background(), lids, tc.chat)
			if err != nil || got != tc.want {
				t.Fatalf("CanonicalChat(%q) = %+v, %v; want %+v", tc.chat, got, err, tc.want)
			}
			// What is asked of the store is a JID without a device, and a group is not asked about.
			for _, asked := range lids.asked {
				if strings.Contains(asked, ":") {
					t.Errorf("the store was asked about %q, with a device", asked)
				}
			}
			if tc.want.IsGroup && len(lids.asked) != 0 {
				t.Errorf("the store was asked about a group: %v", lids.asked)
			}
			// It is the identity of a chat: asking again with its own answer gives the same.
			again, err := CanonicalChat(context.Background(), lids, got.JID)
			if err != nil || again != got {
				t.Errorf("CanonicalChat of the canonical %q = %+v, %v; want %+v", got.JID, again, err, got)
			}
		})
	}
}

func TestCanonicalChatRefuses(t *testing.T) {
	for _, chat := range []string{
		"", "   ", "hello", "+", "123456", "0079991234567", "+0 999 123 45 67", "7999123456+7",
		"@s.whatsapp.net", bobPN + "@", "abc@s.whatsapp.net", "+" + bobPN + "@s.whatsapp.net",
		"12-34@s.whatsapp.net", bobPN + ":x@s.whatsapp.net", bobPN + "@s.whatsapp.net@evil", groupID + "@g.us@evil",
		"abc@g.us", "-1@g.us", "status@broadcast", "1699999999@broadcast", "120363111111111111@newsletter",
		"867051314767696@bot", "1234@msgr", "1234@interop", "s.whatsapp.net",
	} {
		lids := &fakeLIDs{}
		got, err := CanonicalChat(context.Background(), lids, chat)
		if err == nil {
			t.Errorf("CanonicalChat(%q) = %+v, want an error", chat, got)
			continue
		}
		if len(lids.asked) != 0 {
			t.Errorf("the store was asked about %q, which is no chat", chat)
		}
		// The error is for the agent: it tells what a chat is, and holds none of what was given.
		if !errors.Is(err, errBadChat) {
			t.Errorf("CanonicalChat(%q) error = %q, want the one that explains what a chat is", chat, err)
		}
	}
}

// TestCanonicalChatStoreFailure: a store that cannot be read is not the same as
// one that does not know the number. An unknown phone JID would be kept as
// itself, and the chat's rows filed under the wrong key.
func TestCanonicalChatStoreFailure(t *testing.T) {
	boom := errors.New("disk I/O error")
	for _, chat := range []string{bobPN + "@s.whatsapp.net", "+" + bobPN, bobLID + "@lid"} {
		_, err := CanonicalChat(context.Background(), &fakeLIDs{err: boom}, chat)
		if !errors.Is(err, boom) {
			t.Errorf("CanonicalChat(%q) error = %v, want the store's", chat, err)
		}
	}
	// A group needs no store.
	if got, err := CanonicalChat(context.Background(), &fakeLIDs{err: boom}, groupID+"@g.us"); err != nil || !got.IsGroup {
		t.Errorf("a group: %+v, %v", got, err)
	}
}

// TestCanonicalChatIgnoresWrongAnswers: the store answers with a JID of the kind
// asked for, or none. Anything else is no pair.
func TestCanonicalChatIgnoresWrongAnswers(t *testing.T) {
	for name, lids := range map[string]lidLookup{
		"kinds swapped":  wrongLIDs{forPN: pnJID("2"), forLID: lidJID("1")},
		"no user":        wrongLIDs{forPN: types.NewJID("", types.HiddenUserServer), forLID: types.NewJID("", types.DefaultUserServer)},
		"another server": wrongLIDs{forPN: types.NewJID("2", types.GroupServer), forLID: types.NewJID("1", types.GroupServer)},
		"nothing at all": wrongLIDs{},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := CanonicalChat(context.Background(), lids, bobPN+"@s.whatsapp.net"); err != nil || got != (Chat{JID: bobPN + "@s.whatsapp.net"}) {
				t.Errorf("phone chat: %+v, %v", got, err)
			}
			if got, err := CanonicalChat(context.Background(), lids, bobLID+"@lid"); err != nil || got != (Chat{JID: bobLID + "@lid"}) {
				t.Errorf("LID chat: %+v, %v", got, err)
			}
		})
	}
}

// wrongLIDs answers every lookup with the same JID, right or wrong.
type wrongLIDs struct{ forPN, forLID types.JID }

func (w wrongLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) { return w.forPN, nil }
func (w wrongLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) { return w.forLID, nil }

// TestCanonicalOfReceivedJIDs: the JID of a received message is trusted to be
// well formed, but not to be a kind the archive keeps; it fails for nothing else.
func TestCanonicalOfReceivedJIDs(t *testing.T) {
	lids := &fakeLIDs{pn2lid: map[string]string{bobPN: bobLID}}
	ctx := context.Background()
	got, err := canonical(ctx, lids, types.JID{User: bobPN, Device: 3, RawAgent: 1, Integrator: 9, Server: types.DefaultUserServer})
	if err != nil || got != (Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}) {
		t.Errorf("a phone JID with all its parts = %+v, %v", got, err)
	}
	for _, bad := range []types.JID{
		types.EmptyJID, types.NewJID("", types.DefaultUserServer), types.NewJID("", types.GroupServer),
		types.StatusBroadcastJID, types.NewJID("1", types.NewsletterServer), types.NewJID("1", types.BotServer),
	} {
		if got, err := canonical(ctx, lids, bad); err == nil {
			t.Errorf("canonical(%v) = %+v, want an error", bad, got)
		}
	}
}

func TestChatMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		chat Chat
		pn   string
		lid  string
		ok   bool
	}{
		{"a LID chat whose number is known", Chat{JID: "2@lid", PN: "1@s.whatsapp.net"}, "1@s.whatsapp.net", "2@lid", true},
		{"a LID chat whose number is not", Chat{JID: "2@lid"}, "", "2@lid", false},
		{"a phone chat", Chat{JID: "1@s.whatsapp.net"}, "", "1@s.whatsapp.net", false},
		{"a group", Chat{JID: "3@g.us", IsGroup: true}, "", "3@g.us", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pn, lid, ok := tc.chat.Merge()
			if ok != tc.ok || (ok && (pn != tc.pn || lid != tc.lid)) {
				t.Errorf("Merge() = %q, %q, %v; want %q, %q, %v", pn, lid, ok, tc.pn, tc.lid, tc.ok)
			}
		})
	}
}

// TestCanonicalChatOnTheRealStore: with whatsmeow's own LID store, which
// answers from a cache that a Put fills, keeps a pair by the users alone and
// adds the asked JID's device to its answer.
func TestCanonicalChatOnTheRealStore(t *testing.T) {
	f := newFixture(t)
	f.devices(t, map[string]string{ownPN: ""})
	c, err := openStore(context.Background(), f.storePath(), newWALog(quiet, "Database"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	devs, err := c.GetAllDevices(context.Background())
	if err != nil || len(devs) != 1 {
		t.Fatalf("devices: %v, %v", devs, err)
	}
	lids := devs[0].LIDs
	ctx := context.Background()
	chat := func(s string) Chat {
		t.Helper()
		got, err := CanonicalChat(ctx, lids, s)
		if err != nil {
			t.Fatalf("CanonicalChat(%q): %v", s, err)
		}
		return got
	}

	if got := chat(bobPN + ":12@s.whatsapp.net"); got != (Chat{JID: bobPN + "@s.whatsapp.net"}) {
		t.Errorf("before the pair is known: %+v", got)
	}
	// The pair comes with the devices of the message, as it does from whatsmeow.
	if err := lids.PutLIDMapping(ctx, types.JID{User: bobLID, Device: 3, Server: types.HiddenUserServer}, types.JID{User: bobPN, Device: 3, Server: types.DefaultUserServer}); err != nil {
		t.Fatal(err)
	}
	want := Chat{JID: bobLID + "@lid", PN: bobPN + "@s.whatsapp.net"}
	for _, s := range []string{bobPN + ":12@s.whatsapp.net", "+" + bobPN, bobLID + ":4@lid", bobLID + "@lid"} {
		if got := chat(s); got != want {
			t.Errorf("CanonicalChat(%q) = %+v, want %+v", s, got, want)
		}
	}
	if got := chat(carolPN + "@s.whatsapp.net"); got != (Chat{JID: carolPN + "@s.whatsapp.net"}) {
		t.Errorf("another number: %+v", got)
	}
	if got := chat(bobPN + "@c.us"); got != want {
		t.Errorf("the legacy server: %+v, want the number's %+v", got, want)
	}
}
