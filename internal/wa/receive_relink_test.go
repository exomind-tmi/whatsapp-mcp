package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// TestReceiveAcrossARelink: the account follows the events of one client, as its
// status does (owner): the one that links the new device while it does, the new
// one after, and never the old one, which is on its way out and whose messages
// the account has nobody to answer for. The new device's messages are the
// account's from the moment it is linked, its status being the old one's.
func TestReceiveAcrossARelink(t *testing.T) {
	r := newRx(t)
	old := r.cli("personal")
	bob := pnJID(bobPN)
	r.mustDeliver(t, "personal", at(incoming("M1", bob), 1*time.Second), text("by the old device"))

	forceStatus(r.m, "personal", StatusNeedsLink)
	s := pair(t, r.m, "personal")
	q := r.fn.qr(t, 0)
	q.ch <- code("2@a", time.Minute)
	scan(t, q, types.NewADJID(ownPN, 0, 13))
	eventually(t, "paired", stateIs(s, pairPaired))

	// The new device is linked and connecting; the account still shows its status as the pairing's.
	if !send(old, at(incoming("M2", bob), 2*time.Second), text("by the old device, while pairing")) {
		t.Error("the old client's message was failed")
	}
	if !send(q.cli, at(incoming("M3", bob), 3*time.Second), text("by the new device, while pairing")) {
		t.Error("the new client's message was failed")
	}
	q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	<-s.done

	if !send(old, at(incoming("M4", bob), 4*time.Second), text("by the old device, after")) {
		t.Error("the old client's message was failed")
	}
	if !send(q.cli, at(incoming("M5", bob), 5*time.Second), text("by the new device, after")) {
		t.Error("the new client's message was failed")
	}
	if got := ids(r.msgs(t, "personal", bobPNChat)); !equalIDs(got, []string{"M1", "M3", "M5"}) {
		t.Errorf("messages %v, want those of the new device and of the old before the pairing", got)
	}
	if n := r.size(t, "personal"); n != [2]int{1, 3} {
		t.Errorf("the archive has %v chats and messages", n)
	}
	requireEnded(t, r.m)
}
