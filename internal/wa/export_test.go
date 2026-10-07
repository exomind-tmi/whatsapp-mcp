package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// What this package's own fixtures give to the tests of the packages above it
// (the external test package of this directory): a real Manager over a real
// archive, with WhatsApp's servers and the phone's history faked, and the means
// to make the events a client of it would be given.

// Rig is a Manager with two accounts, personal and work, each with a device, over
// an empty archive.
type Rig struct {
	t *testing.T
	x *hx
}

// NewRig makes a Rig whose history worker writes chunks of three messages.
func NewRig(t *testing.T) *Rig { return &Rig{t: t, x: newHx(t, 3)} }

func (g *Rig) Manager() *Manager { return g.x.m }

func (g *Rig) Archive() *archive.DB { return g.x.db }

// Client is the whatsmeow client of the account: its stores hold the contacts and
// the LIDs that the Manager answers from.
func (g *Rig) Client(nick string) *whatsmeow.Client { return g.x.cli(nick) }

// Deliver gives the message to the handler of the account's client, as whatsmeow
// does with one it decrypted, and fails the test unless the handler acknowledges it.
func (g *Rig) Deliver(nick string, info types.MessageInfo, msg *waE2E.Message) {
	g.t.Helper()
	g.x.mustDeliver(g.t, nick, info, msg)
}

// SetOwnLID gives the account's device the LID that whatsmeow files our own
// messages of a LID chat under, which a device that is linked has.
func (g *Rig) SetOwnLID(nick string) { g.x.cli(nick).Store.LID = lidJID(ownLID) }

// Connect has the client of the account say that it is connected.
func (g *Rig) Connect(nick string) { g.x.connect(nick) }

// Dispatch gives the account's client an event of its connection.
func (g *Rig) Dispatch(nick string, evt any) { g.x.cli(nick).DangerousInternals().DispatchEvent(evt) }

// ImportHistory has the phone send the blob to the account, and waits until the
// worker has imported it.
func (g *Rig) ImportHistory(nick, id string, blob *waHistorySync.HistorySync) {
	g.t.Helper()
	path := "/h/" + id
	g.x.h.put(path, blob)
	g.x.connect(nick)
	g.x.announce(g.t, nick, id, path)
	g.x.imported(g.t, nick)
}

// The people of the tests, and the means to make a message of them.
const (
	BobPN    = bobPN
	BobLID   = bobLID
	CarolPN  = carolPN
	CarolLID = carolLID
	OwnPN    = ownPN
	OwnLID   = ownLID
	GroupID  = groupID
)

// T0 is the time of a message sent at no offset, in the helpers below.
var T0 = t0

var (
	PNJID   = pnJID
	LIDJID  = lidJID
	GroupOf = groupJID
	Text    = text
	Edit    = edit
	Revoke  = revoke

	Incoming = incoming
	Outgoing = outgoing
	InGroup  = inGroup
	ByLID    = byLID
	At       = func(info types.MessageInfo, d time.Duration) types.MessageInfo { return at(info, d) }

	HistoryConversation = hconv
	HistoryMessage      = hmsg
	HistoryIn           = hin
	HistoryOut          = hout
	HistoryPushed       = pushed
	HistoryBlob         = hblob
	HistoryPairs        = withPairs
)
