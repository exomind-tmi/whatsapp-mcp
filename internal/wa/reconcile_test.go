package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// syncGroupsNow runs what follows a connect, the fetch of the groups and the
// filing of the chats after it, to its end.
func (r *rx) syncGroupsNow() {
	r.m.startGroupSync(r.m.accounts["personal"], r.cli("personal"))
	r.m.wg.Wait()
}

// syncGroupsAndWait is syncGroupsNow for a test that has handlers at work beside
// it, which Add to the Manager's group while Wait would be running.
func (r *rx) syncGroupsAndWait() {
	a := r.m.accounts["personal"]
	r.m.startGroupSync(a, r.cli("personal"))
	for {
		r.m.mu.Lock()
		busy := a.syncingGroups
		r.m.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// groupsTeachingBob is a fetch of the groups that brings Bob's pair into the
// store, as whatsmeow's GetJoinedGroups does with the members it reads.
func groupsTeachingBob(cli *whatsmeow.Client, ctx context.Context) ([]*types.GroupInfo, error) {
	err := cli.Store.LIDs.PutManyLIDMappings(ctx, []store.LIDMapping{{LID: lidJID(bobLID), PN: pnJID(bobPN)}})
	return []*types.GroupInfo{group(teamID, "Family")}, err
}

// requireBobFiledByHisLID is the archive once the chat of Bob has been settled:
// his chat, one, under the LID with his number and his name, and none of his
// messages under the number.
func (r *rx) requireBobFiledByHisLID(t *testing.T) {
	t.Helper()
	bobs := 0
	for _, c := range r.chats(t, "personal") {
		if c.IsGroup {
			continue
		}
		bobs++
		if c.JID != bobLIDChat || c.PN != bobPNChat || c.Name != "Bob" {
			t.Errorf("chat %+v, want Bob's under the LID with his number and name", c)
		}
	}
	if bobs != 1 {
		t.Errorf("%d chats of Bob", bobs)
	}
	if got := r.msgs(t, "personal", bobPNChat); len(got) != 0 {
		t.Errorf("messages left under the number: %+v", got)
	}
}

// TestReconcileFilesQuietChatsAfterTheGroupSync: a pair of a number and a LID that
// the groups' members bring is in the store before any message of the chat, and a
// chat that nobody writes in would stay under the number: what the tools ask for
// by its canonical identity would be empty. The chats are settled once the groups
// are fetched, and again does not change them.
func TestReconcileFilesQuietChatsAfterTheGroupSync(t *testing.T) {
	r := newRx(t)
	r.seedBobByNumber(t, "personal") // three messages under the number, one edited
	r.fn.groups = groupsTeachingBob
	r.syncGroupsNow()
	r.requireBobFiledByHisLID(t)
	if got := ids(r.msgs(t, "personal", bobLIDChat)); !equalIDs(got, []string{"M1", "M2"}) {
		t.Errorf("messages under the LID %v", got)
	}
	if m, err := r.db.Message(context.Background(), "personal", bobLIDChat, "M1"); err != nil || m.Text != "one edited-beta" || m.EditedAt.IsZero() {
		t.Errorf("M1 = %+v, %v; want the edited text and its mark kept", m, err)
	}
	if accs, err := r.db.AccountsForChat(context.Background(), bobPNChat); err != nil || len(accs) != 1 || accs[0] != "personal" {
		t.Errorf("accounts of the chat by its number: %v, %v", accs, err)
	}
	if c := r.chatsByJID(t, "personal")[teamID+"@g.us"]; c.Name != "Family" || !c.IsGroup || c.PN != "" {
		t.Errorf("the group = %+v, want it as the fetch made it", c)
	}

	before := r.size(t, "personal")
	r.syncGroupsNow()
	if after := r.size(t, "personal"); after != before {
		t.Errorf("the second pass changed %v to %v", before, after)
	}
}

// TestReconcileGivesAChatUnderALIDItsNumber: the chat is under the LID from its
// first message, and the number is learnt later: it is recorded, for the chat to be
// found by it.
func TestReconcileGivesAChatUnderALIDItsNumber(t *testing.T) {
	r := newRx(t)
	r.mustDeliver(t, "personal", at(incoming("M1", lidJID(bobLID)), time.Second), text("by lid only"))
	if cs := r.chats(t, "personal"); len(cs) != 1 || cs[0].PN != "" {
		t.Fatalf("chats %+v", cs)
	}
	r.learn(t, "personal")
	r.syncGroupsNow()
	if cs := r.chats(t, "personal"); len(cs) != 1 || cs[0].JID != bobLIDChat || cs[0].PN != bobPNChat {
		t.Errorf("chats %+v, want the LID's, with the number", cs)
	}
	if accs, err := r.db.AccountsForChat(context.Background(), bobPNChat); err != nil || len(accs) != 1 {
		t.Errorf("accounts of the chat by its number: %v, %v", accs, err)
	}
}

// TestReconcileOnAFailedFetch: the fetch of the groups is what brought the pair,
// and it failed on the way: what it has brought is in the store all the same, and
// the chats follow it.
func TestReconcileOnAFailedFetch(t *testing.T) {
	r := newRx(t)
	r.seedBobByNumber(t, "personal")
	r.fn.groups = func(cli *whatsmeow.Client, ctx context.Context) ([]*types.GroupInfo, error) {
		groupsTeachingBob(cli, ctx)
		return nil, errors.New("the server went away")
	}
	r.syncGroupsNow()
	r.requireBobFiledByHisLID(t)
}

// TestReconcileOfARemovedAccountWritesNothing: the chats of a removed account are
// not filed again, which would bring its row back, and the call has nothing to
// tell of it.
func TestReconcileOfARemovedAccountWritesNothing(t *testing.T) {
	r := newRx(t)
	r.seedBobByNumber(t, "personal")
	a, cli := r.m.accounts["personal"], r.cli("personal")
	r.learn(t, "personal")
	if res := remove(t, r.m, "personal"); res.err != nil {
		t.Fatal(res.err)
	}
	want, err := CanonicalChat(context.Background(), cli.Store.LIDs, bobPN)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.m.refile(context.Background(), a, cli, want); err != nil {
		t.Errorf("refile of a removed account: %v", err)
	}
	r.m.reconcileChats(a, cli)
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the removed account has %v again", n)
	}
}

// TestReconcileRacingMessages: the settling of the chats and the messages of the
// chat run together, as they do after a connect. Whatever the order, one chat
// under the LID with every message, none under the number.
func TestReconcileRacingMessages(t *testing.T) {
	for round := range 5 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			r := newRx(t)
			r.seedBobByNumber(t, "personal")
			r.fn.groups = groupsTeachingBob
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				r.syncGroupsAndWait()
			}()
			go func() {
				defer wg.Done()
				for i := range 12 {
					id := fmt.Sprintf("N%d", i)
					if i%2 == 0 {
						r.deliver("personal", at(incoming(id, pnJID(bobPN)), time.Duration(10+i)*time.Second), text("by number"))
					} else {
						r.deliver("personal", byLID(id, time.Duration(10+i)*time.Second), text("by lid"))
					}
				}
			}()
			wg.Wait()
			r.syncGroupsAndWait() // a last pass, as the next connect makes
			r.requireBobFiledByHisLID(t)
			if got := len(r.msgs(t, "personal", bobLIDChat)); got != 2+12 {
				t.Errorf("%d messages under the LID, want the two that were not edits and the twelve", got)
			}
		})
	}
}

// TestReconcileWritesNothingWhenThereIsNothingToSettle: chats that are where the store
// puts them are left alone at every connect, and not written to again. Writes to
// chats are refused by the archive here, so that one would fail and be logged.
func TestReconcileWritesNothingWhenThereIsNothingToSettle(t *testing.T) {
	r := newRx(t, refuseSQL)
	r.seedBobByNumber(t, "personal")
	r.fn.groups = groupsTeachingBob
	r.syncGroupsNow()
	r.requireBobFiledByHisLID(t)

	r.refuse(t, refuseChats)
	r.fn.groups = func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error) { return nil, nil }
	r.syncGroupsNow()
	if log := r.logs.String(); strings.Contains(log, "file a chat by its LID") {
		t.Errorf("a chat that was settled was written to again:\n%s", log)
	}
}

// TestReconcileReadFailureIsLogged: an archive that cannot list its chats leaves them
// as they are, and says so: the next connect, or the next message of a chat, settles
// them.
func TestReconcileReadFailureIsLogged(t *testing.T) {
	r := newRx(t)
	r.db.Close()
	r.syncGroupsNow()
	if log := r.logs.String(); !strings.Contains(log, "list the chats to file by LID") || !strings.Contains(log, "level=WARN") {
		t.Errorf("the failure is not in the log:\n%s", log)
	}
}
