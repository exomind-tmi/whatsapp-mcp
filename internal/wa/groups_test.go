package wa

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

func group(user, name string) *types.GroupInfo {
	return &types.GroupInfo{JID: types.NewJID(user, types.GroupServer), GroupName: types.GroupName{Name: name}}
}

const (
	teamID  = groupID
	otherID = "120363000000000002"
)

// chatsByJID are the chats of the account by their JID.
func (r *rx) chatsByJID(t *testing.T, nick string) map[string]archive.Chat {
	t.Helper()
	out := map[string]archive.Chat{}
	for _, c := range r.chats(t, nick) {
		out[c.JID] = c
	}
	return out
}

// connect gives the account's client the event that follows a connect.
func (r *rx) connect(nick string) bool {
	return r.cli(nick).DangerousInternals().DispatchEvent(&events.Connected{})
}

func (r *rx) groupQueries() int {
	r.fn.mu.Lock()
	defer r.fn.mu.Unlock()
	return r.fn.groupsCalls
}

// TestGroupNamesAfterConnect: once the client is connected the account's groups
// are asked for, and each is filed under its name, a group with no message
// included, and without a last message time. What is not a group is not filed.
func TestGroupNamesAfterConnect(t *testing.T) {
	r := newRx(t)
	r.fn.groups = func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error) {
		return []*types.GroupInfo{
			group(teamID, "  Team  "),
			group(otherID, ""),
			{JID: pnJID(bobPN), GroupName: types.GroupName{Name: "Bob"}},
			nil,
		}, nil
	}
	if r.connect("personal") {
		t.Fatal("the handler failed the event")
	}
	eventually(t, "the groups", func() bool { return len(r.chats(t, "personal")) == 2 })
	cs := r.chatsByJID(t, "personal")
	if c := cs[teamID+"@g.us"]; c.Name != "Team" || !c.IsGroup || !c.LastMessageTS.IsZero() {
		t.Errorf("Team = %+v", c)
	}
	if c := cs[otherID+"@g.us"]; c.Name != "" || !c.IsGroup || !c.LastMessageTS.IsZero() {
		t.Errorf("the group with no name = %+v", c)
	}
	if got := infoOf(r.m, "personal").Status; got != StatusConnected {
		t.Errorf("status %s, want connected", got)
	}
	if n := r.size(t, "work"); n != [2]int{} {
		t.Errorf("the other account has %v", n)
	}
}

// TestGroupFetchDoesNotHoldTheHandler: the handler returns at once, whatever the
// fetch does; it is the whatsmeow goroutine that dispatches Connected.
func TestGroupFetchDoesNotHoldTheHandler(t *testing.T) {
	r := newRx(t)
	release := make(chan struct{})
	r.fn.groups = func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error) {
		<-release
		return []*types.GroupInfo{group(teamID, "Team")}, nil
	}
	returned := make(chan struct{})
	go func() { r.connect("personal"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler waits for the groups")
	}
	if n := len(r.chats(t, "personal")); n != 0 {
		t.Errorf("%d chats before the groups came", n)
	}
	close(release)
	eventually(t, "the groups", func() bool { return len(r.chats(t, "personal")) == 1 })
}

// TestGroupFetchOneAtATime: a connect while the fetch of the account is running
// does not start another; the next one after it does.
func TestGroupFetchOneAtATime(t *testing.T) {
	r := newRx(t)
	release := make(chan struct{})
	var mu sync.Mutex
	running := map[*whatsmeow.Client]int{} // by client: the two accounts' fetches run side by side
	r.fn.groups = func(cli *whatsmeow.Client, _ context.Context) ([]*types.GroupInfo, error) {
		mu.Lock()
		running[cli]++
		if running[cli] > 1 {
			t.Error("two fetches of one account run at once")
		}
		mu.Unlock()
		defer func() { mu.Lock(); running[cli]--; mu.Unlock() }()
		<-release
		return nil, nil
	}
	r.connect("personal")
	eventually(t, "the first fetch", func() bool { return r.groupQueries() == 1 })
	r.connect("personal")
	r.connect("personal")
	time.Sleep(20 * time.Millisecond)
	if n := r.groupQueries(); n != 1 {
		t.Errorf("%d fetches while one was running", n)
	}
	// The other account has its own.
	r.connect("work")
	eventually(t, "the other account's fetch", func() bool { return r.groupQueries() == 2 })
	close(release)
	requireEnded(t, r.m)
	r.connect("personal")
	eventually(t, "a fetch after the first", func() bool { return r.groupQueries() == 3 })
	requireEnded(t, r.m)
}

// TestGroupFetchFailure: it is only logged, and the next connect tries again.
func TestGroupFetchFailure(t *testing.T) {
	r := newRx(t)
	var fail atomic.Bool
	fail.Store(true)
	r.fn.groups = func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error) {
		if fail.Load() {
			return nil, errors.New("info query timed out")
		}
		return []*types.GroupInfo{group(teamID, "Team")}, nil
	}
	if r.connect("personal") {
		t.Error("the handler failed the event")
	}
	requireEnded(t, r.m)
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v", n)
	}
	if log := r.logs.String(); !strings.Contains(log, "fetch the joined groups") || !strings.Contains(log, "level=WARN") {
		t.Errorf("the failure is not in the log:\n%s", log)
	}
	if got := infoOf(r.m, "personal").Status; got != StatusConnected {
		t.Errorf("status %s: a failed fetch is not the connection's", got)
	}
	fail.Store(false)
	r.connect("personal")
	eventually(t, "the groups", func() bool { return len(r.chats(t, "personal")) == 1 })
}

// TestGroupFetchStopsAtClose: Close ends a fetch that is under way, and does not
// wait for the time it was given.
func TestGroupFetchStopsAtClose(t *testing.T) {
	r := newRx(t)
	started, cancelled := make(chan struct{}), make(chan struct{})
	r.fn.groups = func(_ *whatsmeow.Client, ctx context.Context) ([]*types.GroupInfo, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}
	r.connect("personal")
	<-started
	begin := time.Now()
	if err := r.m.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > time.Second {
		t.Errorf("Close took %v", d)
	}
	select {
	case <-cancelled:
	default:
		t.Error("the fetch was not cancelled")
	}
	requireEnded(t, r.m)
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v", n)
	}
	// What follows the fetch has nothing to say of a Manager that has closed.
	if log := r.logs.String(); strings.Contains(log, "file by LID") {
		t.Errorf("the settling of the chats complained of the close:\n%s", log)
	}
}

// TestGroupFetchHasATimeout: a fetch that never ends is ended by its time, and
// the account can fetch again.
func TestGroupFetchHasATimeout(t *testing.T) {
	r := newRx(t)
	r.m.groupsWait = 50 * time.Millisecond
	r.fn.groups = func(_ *whatsmeow.Client, ctx context.Context) ([]*types.GroupInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r.connect("personal")
	requireEnded(t, r.m)
	if got := r.groupQueries(); got != 1 {
		t.Errorf("%d fetches", got)
	}
	if !strings.Contains(r.logs.String(), "fetch the joined groups") {
		t.Errorf("the timeout is not in the log:\n%s", r.logs)
	}
	r.connect("personal")
	eventually(t, "a second fetch", func() bool { return r.groupQueries() == 2 })
	requireEnded(t, r.m)
}

// TestGroupNamesFromEvents: a group that is joined or renamed is filed under the
// name the event brings; an event with none, or about what is not a group, files
// nothing. A message of the group does not take the name away.
func TestGroupNamesFromEvents(t *testing.T) {
	r := newRx(t)
	cli := r.cli("personal")
	dispatch := func(evt any) {
		t.Helper()
		if cli.DangerousInternals().DispatchEvent(evt) {
			t.Fatalf("the handler failed %T", evt)
		}
	}
	dispatch(&events.JoinedGroup{GroupInfo: *group(teamID, "Team")})
	if c := r.chatsByJID(t, "personal")[teamID+"@g.us"]; c.Name != "Team" || !c.IsGroup {
		t.Fatalf("after joining: %+v", c)
	}
	dispatch(&events.GroupInfo{JID: types.NewJID(teamID, types.GroupServer), Name: &types.GroupName{Name: " Team 2 "}})
	if c := r.chatsByJID(t, "personal")[teamID+"@g.us"]; c.Name != "Team 2" {
		t.Errorf("after the rename: %+v", c)
	}

	// Nothing about the name: no row for a group that is not archived yet, no change to one that is.
	dispatch(&events.GroupInfo{JID: types.NewJID(otherID, types.GroupServer), Topic: &types.GroupTopic{Topic: "a topic"}})
	dispatch(&events.JoinedGroup{GroupInfo: *group(otherID, "")})
	dispatch(&events.GroupInfo{JID: types.NewJID(teamID, types.GroupServer), Announce: &types.GroupAnnounce{IsAnnounce: true}})
	dispatch(&events.GroupInfo{JID: pnJID(bobPN), Name: &types.GroupName{Name: "Bob"}})
	dispatch(&events.GroupInfo{Name: &types.GroupName{Name: "no JID"}})
	cs := r.chatsByJID(t, "personal")
	if len(cs) != 1 || cs[teamID+"@g.us"].Name != "Team 2" {
		t.Errorf("chats %+v, want Team 2 alone", cs)
	}

	r.mustDeliver(t, "personal", at(inGroup("G1", pnJID(bobPN)), time.Second), text("hi all"))
	if c := r.chatsByJID(t, "personal")[teamID+"@g.us"]; c.Name != "Team 2" || c.LastMessageTS.IsZero() {
		t.Errorf("after a message: %+v, want the name kept and the time set", c)
	}
}

// TestGroupNamedAfterItsFirstMessage: a group that a message made, with no name,
// is named when the list comes.
func TestGroupNamedAfterItsFirstMessage(t *testing.T) {
	r := newRx(t)
	r.mustDeliver(t, "personal", at(inGroup("G1", pnJID(bobPN)), time.Second), text("hi all"))
	if c := r.chatsByJID(t, "personal")[teamID+"@g.us"]; c.Name != "" {
		t.Fatalf("named before it was asked: %+v", c)
	}
	r.fn.groups = func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error) {
		return []*types.GroupInfo{group(teamID, "Team")}, nil
	}
	r.connect("personal")
	eventually(t, "the name", func() bool { return r.chatsByJID(t, "personal")[teamID+"@g.us"].Name == "Team" })
	if c := r.chatsByJID(t, "personal")[teamID+"@g.us"]; c.LastMessageTS.IsZero() {
		t.Errorf("the group lost its time: %+v", c)
	}
}

// TestGroupFetchOfARemovedAccount: a fetch that ends after its account was
// removed writes nothing, and the removed account's client's connect starts none.
func TestGroupFetchOfARemovedAccount(t *testing.T) {
	r := newRx(t)
	old := r.cli("personal")
	release := make(chan struct{})
	r.fn.groups = func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error) {
		<-release
		return []*types.GroupInfo{group(teamID, "Team")}, nil
	}
	r.connect("personal")
	eventually(t, "the fetch", func() bool { return r.groupQueries() == 1 })
	if res := remove(t, r.m, "personal"); res.err != nil {
		t.Fatal(res.err)
	}
	close(release)
	requireEnded(t, r.m)
	if n := r.rowsOf(t, "chats", "personal"); n != 0 {
		t.Errorf("%d chats came back for the removed account", n)
	}
	if strings.Contains(r.logs.String(), "store the names of groups") {
		t.Errorf("the write was tried, and refused:\n%s", r.logs)
	}
	// Nor does the removed account's client start a fetch of its own.
	old.DangerousInternals().DispatchEvent(&events.Connected{})
	time.Sleep(20 * time.Millisecond)
	if n := r.groupQueries(); n != 1 {
		t.Errorf("%d fetches", n)
	}
}

// TestGroupFetchPanicIsNotTheDaemons: the fetch runs on a goroutine of the
// Manager's own, where nobody recovers a panic but the Manager, and one that
// escaped would end the process, at each connect. It is logged with its stack, and
// the account can fetch again after it.
func TestGroupFetchPanicIsNotTheDaemons(t *testing.T) {
	r := newRx(t)
	var calls atomic.Int32
	r.fn.groups = func(*whatsmeow.Client, context.Context) ([]*types.GroupInfo, error) {
		if calls.Add(1) == 1 {
			panic("the fetch broke")
		}
		return []*types.GroupInfo{group(teamID, "Team")}, nil
	}
	if r.connect("personal") {
		t.Error("the handler failed the event")
	}
	requireEnded(t, r.m)
	if log := r.logs.String(); !strings.Contains(log, "group sync panicked") || !strings.Contains(log, "the fetch broke") || !strings.Contains(log, "goroutine") {
		t.Errorf("the panic is not in the log, with its stack:\n%s", log)
	}
	r.connect("personal")
	eventually(t, "the groups of the second fetch", func() bool { return len(r.chats(t, "personal")) == 1 })
}

// TestGroupNamesAreWrittenWithTheWaitOfAWrite: a fetch that takes all the time it
// was given leaves its write the wait of any write, and the names are not lost for
// the fetch's clock having run out.
func TestGroupNamesAreWrittenWithTheWaitOfAWrite(t *testing.T) {
	r := newRx(t)
	r.m.groupsWait = 50 * time.Millisecond
	r.fn.groups = func(_ *whatsmeow.Client, ctx context.Context) ([]*types.GroupInfo, error) {
		<-ctx.Done() // the fetch returns what it has, at the end of its time
		return []*types.GroupInfo{group(teamID, "Team")}, nil
	}
	r.connect("personal")
	requireEnded(t, r.m)
	if c := r.chatsByJID(t, "personal")[teamID+"@g.us"]; c.Name != "Team" {
		t.Errorf("Team = %+v, want it named", c)
	}
}
