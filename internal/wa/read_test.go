package wa

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// putContact gives the account's contact store a name for a person, as the sync of
// the address book and the push names of messages do.
func (r *rx) putContact(t *testing.T, nick string, jid types.JID, full, first, push string) {
	t.Helper()
	ctx := context.Background()
	c := r.cli(nick).Store.Contacts
	if full != "" || first != "" {
		// The store takes the first name first, whatever the names of its interface say.
		if err := c.PutContactName(ctx, jid, first, full); err != nil {
			t.Fatal(err)
		}
	}
	if push != "" {
		if _, _, err := c.PutPushName(ctx, jid, push); err != nil {
			t.Fatal(err)
		}
	}
}

// TestManagerCanonicalChat: the chat that a JID or a number names is the one the
// archive files it under, whichever account asks, and no call goes to WhatsApp.
func TestManagerCanonicalChat(t *testing.T) {
	r := newRx(t)
	ctx := context.Background()
	r.learn(t, "personal")
	for _, tc := range []struct {
		name, chat string
		want       Chat
	}{
		{"a number whose LID is known", "+7 000 000 0100", Chat{JID: bobLIDChat, PN: bobPNChat}},
		{"its JID", bobPNChat, Chat{JID: bobLIDChat, PN: bobPNChat}},
		{"the LID", bobLIDChat, Chat{JID: bobLIDChat, PN: bobPNChat}},
		{"a number with no LID known", carolPN, Chat{JID: carolChat}},
		{"a group", groupID + "@g.us", Chat{JID: groupID + "@g.us", IsGroup: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.m.CanonicalChat(ctx, tc.chat)
			if err != nil || got != tc.want {
				t.Errorf("CanonicalChat(%q) = %+v, %v; want %+v", tc.chat, got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "Bob", "70000000100@status", "a@b@c"} {
		if got, err := r.m.CanonicalChat(ctx, bad); err == nil {
			t.Errorf("CanonicalChat(%q) = %+v, want an error", bad, got)
		}
	}
}

// TestManagerSenderJIDs: a JID or a number is the person of it, in both of their
// addresses if the LID store knows them; a name is searched in the chats of the
// account and in its contacts; a group is no person.
func TestManagerSenderJIDs(t *testing.T) {
	r := newRx(t)
	ctx := context.Background()
	r.mustDeliver(t, "personal", at(incoming("M1", pnJID(bobPN)), time.Second), text("hello"))
	r.mustDeliver(t, "personal", at(inGroup("G1", pnJID(bobPN)), 2*time.Second), text("hi all"))
	if err := r.db.Tx(ctx, func(tx *archive.Tx) error {
		return tx.TouchChat(archive.ChatUpd{Account: "personal", JID: groupID + "@g.us", Name: "Team of all", IsGroup: true})
	}); err != nil {
		t.Fatal(err)
	}
	r.putContact(t, "personal", pnJID(carolPN), "Фёдор Карлов", "Фёдор", "")
	// Dave is a contact that has no chat, and a name of the other account.
	r.putContact(t, "personal", pnJID("70000000102"), "Dave Davis", "Dave", "dd")
	r.putContact(t, "work", pnJID("70000000103"), "Eve Evans", "Eve", "")

	ask := func(nick, sender string) ([]string, error) { return r.m.SenderJIDs(ctx, nick, sender) }
	for _, tc := range []struct {
		name, sender string
		want         []string
	}{
		{"a number, whose LID is not known", "+7 000 000 0100", []string{bobPNChat}},
		{"a JID, whose LID is not known", bobPNChat, []string{bobPNChat}},
		{"a number with a device", bobPN + ":12@s.whatsapp.net", []string{bobPNChat}},
		{"the name of a chat", "bob", []string{bobPNChat}},
		{"the name of a chat, in capitals", "BOB", []string{bobPNChat}},
		{"a part of the name of a chat", "ob", []string{bobPNChat}},
		{"the name of a contact", "карлов", []string{carolChat}},
		{"spaces round a name are not part of it", "  карлов ", []string{carolChat}},
		{"ё and е are the same", "федор", []string{carolChat}},
		{"ё and е are the same, in capitals", "ФЁДОР", []string{carolChat}},
		{"the first name of a contact", "dave", []string{"70000000102@s.whatsapp.net"}},
		{"the push name of a contact", "dd", []string{"70000000102@s.whatsapp.net"}},
		{"what is the name of no one", "mallory", nil},
		{"what is the name of a contact of another account", "evans", nil},
		{"the name of a group is no person's", "all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ask("personal", tc.sender)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("SenderJIDs(%q) = %v, %v; want %v", tc.sender, got, err, tc.want)
			}
		})
	}
	if got, err := ask("work", "evans"); err != nil || !slices.Equal(got, []string{"70000000103@s.whatsapp.net"}) {
		t.Errorf("the contact of the other account is found in it: %v, %v", got, err)
	}

	// Once the LID is known, both addresses are the person's, by whichever of them they are asked for.
	r.learn(t, "personal")
	for _, sender := range []string{"+7 000 000 0100", bobPNChat, bobLIDChat} {
		if got, err := ask("personal", sender); err != nil || !slices.Equal(got, []string{bobLIDChat, bobPNChat}) {
			t.Errorf("SenderJIDs(%q) = %v, %v; want both addresses", sender, got, err)
		}
	}
	// A contact stored under the number is found by the name, and has both addresses too.
	r.putContact(t, "personal", pnJID(bobPN), "Robert Marley", "Robert", "")
	if got, err := ask("personal", "marley"); err != nil || !slices.Equal(got, []string{bobLIDChat, bobPNChat}) {
		t.Errorf("a contact whose LID is known: %v, %v; want both addresses", got, err)
	}
	if got, err := ask("personal", groupID+"@g.us"); !errors.Is(err, ErrNotAPerson) || got != nil {
		t.Errorf("SenderJIDs of a group = %v, %v; want ErrNotAPerson", got, err)
	}
}

// TestManagerSenderJIDsOfTooManyPeople: a name that fits more than a hundred people
// is refused, one that fits a hundred is not.
func TestManagerSenderJIDsOfTooManyPeople(t *testing.T) {
	r := newRx(t)
	for i := range 100 {
		r.putContact(t, "personal", pnJID(fmt.Sprintf("7000020%04d", i)), "", "", "Same Name")
	}
	if got, err := r.m.SenderJIDs(context.Background(), "personal", "same name"); err != nil || len(got) != 100 {
		t.Fatalf("100 people: %d addresses, %v", len(got), err)
	}
	r.putContact(t, "personal", pnJID("70000209999"), "", "", "Same Name")
	if got, err := r.m.SenderJIDs(context.Background(), "personal", "same name"); !errors.Is(err, ErrTooManyPeople) || got != nil {
		t.Errorf("101 people: %v, %v; want ErrTooManyPeople", got, err)
	}
}

// TestManagerNames: the name of a person is the one of the contacts, then what
// they call themselves, found by either address of them; nothing is "".
func TestManagerNames(t *testing.T) {
	r := newRx(t)
	ctx := context.Background()
	names := r.m.Names(ctx, "personal")
	if got := names(bobPNChat); got != "" {
		t.Errorf("a stranger is called %q", got)
	}
	r.putContact(t, "personal", pnJID(bobPN), "", "", "Bobby")
	if got := names(bobPNChat); got != "Bobby" {
		t.Errorf("by the push name: %q", got)
	}
	r.putContact(t, "personal", pnJID(bobPN), "", "Robert", "")
	if got := names(bobPNChat); got != "Robert" {
		t.Errorf("the first name comes before the push name: %q", got)
	}
	r.putContact(t, "personal", pnJID(bobPN), "Robert Marley", "Robert", "")
	if got := names(bobPNChat); got != "Robert Marley" {
		t.Errorf("the full name comes first: %q", got)
	}
	// By the other address of the person, which the LID store joins.
	if got := names(bobLIDChat); got != "" {
		t.Errorf("the LID of a person that is not known to be theirs: %q", got)
	}
	r.learn(t, "personal")
	if got := names(bobLIDChat); got != "Robert Marley" {
		t.Errorf("by the LID: %q", got)
	}
	if got := names(bobPN + ":12@s.whatsapp.net"); got != "Robert Marley" {
		t.Errorf("by an address with a device: %q", got)
	}
	r.putContact(t, "personal", lidJID(carolLID), "", "", "Carol")
	if got := names(carolChat); got != "" {
		t.Errorf("the number of Carol, whose LID the store does not tie to it: %q", got)
	}
	for _, bad := range []string{"", "not a jid", groupID + "@g.us"} {
		if got := names(bad); got != "" {
			t.Errorf("names(%q) = %q", bad, got)
		}
	}
	// Another account has its own contacts.
	if got := r.m.Names(ctx, "work")(bobPNChat); got != "" {
		t.Errorf("the other account knows Bob as %q", got)
	}
}

// TestReadsOfAnAccountWithNoDevice: an account that has to be linked again has no
// contacts, which went with its device, but its archive is read by the same keys,
// and a name is still found in its chats.
func TestReadsOfAnAccountWithNoDevice(t *testing.T) {
	f := newFixture(t)
	f.account(t, "personal", ownPN)
	f.devices(t, map[string]string{ownPN: "Anton"})
	f.account(t, "ghost", "")
	m := f.start(t, (&fakeNet{}).network(readyGlobals()))
	m.wg.Wait()
	if err := f.db.Tx(context.Background(), func(tx *archive.Tx) error {
		return tx.TouchChat(archive.ChatUpd{Account: "ghost", JID: bobPNChat, Name: "Bob"})
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := m.Names(ctx, "ghost")(bobPNChat); got != "" {
		t.Errorf("an account with no device names %q", got)
	}
	if got, err := m.SenderJIDs(ctx, "ghost", "bob"); err != nil || !slices.Equal(got, []string{bobPNChat}) {
		t.Errorf("the name of a chat of an account with no device: %v, %v", got, err)
	}
	if got, err := m.SenderJIDs(ctx, "ghost", "nobody"); err != nil || got != nil {
		t.Errorf("a name that is no one's: %v, %v", got, err)
	}
	if got, err := m.CanonicalChat(ctx, "+7 000 000 0100"); err != nil || got.JID != bobPNChat {
		t.Errorf("CanonicalChat = %+v, %v", got, err)
	}
	if got := m.Names(ctx, "nobody")(bobPNChat); got != "" {
		t.Errorf("an account that does not exist names %q", got)
	}
}

// TestManagerRoster: the accounts and their statuses as Accounts has them, without the
// sizes of their archives.
func TestManagerRoster(t *testing.T) {
	r := newRx(t)
	ctx := context.Background()
	r.mustDeliver(t, "personal", at(incoming("M1", pnJID(bobPN)), time.Second), text("hello"))
	full, roster := r.m.Accounts(ctx), r.m.Roster(ctx)
	if len(full) != 2 || len(roster) != 2 || full[0].Messages != 1 || full[0].Chats != 1 {
		t.Fatalf("accounts %+v, roster %+v", full, roster)
	}
	for i := range full {
		want := full[i]
		want.Chats, want.Messages = 0, 0
		if roster[i] != want {
			t.Errorf("roster[%d] = %+v, want %+v", i, roster[i], want)
		}
	}
	if roster[0].Nick != "personal" || roster[1].Nick != "work" {
		t.Errorf("roster %+v is not by nick", roster)
	}
}
