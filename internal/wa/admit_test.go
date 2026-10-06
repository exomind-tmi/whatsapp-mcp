package wa

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// within is call(m), failing the test instead of hanging it when it does not return:
// a wait that is not bound is the very thing the tests of this file are for.
func within(t *testing.T, call func(*Manager) error, m *Manager) error {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- call(m) }()
	select {
	case err := <-res:
		return err
	case <-time.After(3 * time.Second): // the tests set abortWait to 50 ms
		t.Fatal("the call did not return: its wait is not bound")
		return nil
	}
}

// TestRestartWaitIsBounded: every call that waits for the pairing it cancels to
// end, add of each kind and remove, waits abortWait and no longer, when the pairing
// hangs in its connect: it answers that it is still finishing (errStillFinishing; a
// remove says errStillCancelling), and starts nothing while the pairing still owns
// the account's client. Once it has ended, the same call goes through.
func TestRestartWaitIsBounded(t *testing.T) {
	ctx := context.Background()
	type call struct {
		name    string
		pageToo bool // a page restarts only a page's pairing
		do      func(m *Manager) error
		still   error // what it answers while the pairing goes on
	}
	link := func(req LinkRequest) func(m *Manager) error {
		return func(m *Manager) error { _, err := m.Link(ctx, req); return err }
	}
	calls := []call{
		{"link", false, link(LinkRequest{Nick: "fresh"}), errStillFinishing},
		{"phone", false, link(LinkRequest{Nick: "fresh", Phone: typedPhone}), errStillFinishing},
		{"chat", false, link(LinkRequest{Nick: "fresh", QRImage: true}), errStillFinishing},
		{"page", true, func(m *Manager) error { _, err := m.add(ctx, "fresh"); return err }, errStillFinishing},
		{"remove", false, func(m *Manager) error { _, err := m.Remove(ctx, "fresh"); return err }, errStillCancelling},
	}
	for _, first := range []string{"page", "phone", "chat"} {
		for _, c := range calls {
			if c.pageToo && first != "page" {
				continue
			}
			t.Run(first+" then "+c.name, func(t *testing.T) {
				f := newFixture(t)
				entered, release := make(chan struct{}), make(chan struct{})
				var connects atomic.Int32
				var once sync.Once
				unhang := func() { once.Do(func() { close(release) }) }
				fn := &fakeNet{firstCode: "2@one", hold: func(*whatsmeow.Client) {
					if connects.Add(1) == 1 { // the first pairing's connect hangs, as in the noise handshake
						close(entered)
						<-release
					}
				}}
				m := f.start(t, fn.network(readyGlobals()))
				t.Cleanup(unhang) // before Close, which would wait for the connect
				m.codeWait, m.abortWait = 50*time.Millisecond, 50*time.Millisecond

				switch first {
				case "page":
					if _, err := m.Login(ctx, "fresh", linkNonce(t, m, "fresh")); err != nil {
						t.Fatal(err)
					}
					<-entered
				case "phone":
					res := linkAsync(m, "fresh", typedPhone)
					<-entered
					if r := result(t, res); r.err == nil || r.err.Error() != couldNotReach {
						t.Fatalf("the first add = %+v, %v", r.tk, r.err)
					}
				case "chat":
					res := linkQRAsync(ctx, m, "fresh")
					<-entered
					if r := result(t, res); r.err == nil || r.err.Error() != couldNotReach {
						t.Fatalf("the first add = %+v, %v", r.tk, r.err)
					}
				}
				s1 := sessOf(m, "fresh")

				start := time.Now()
				if err := within(t, c.do, m); !errors.Is(err, c.still) || time.Since(start) > 2*time.Second {
					t.Fatalf("the call = %v after %v; want %q, soon", err, time.Since(start), c.still)
				}
				m.mu.Lock()
				a := m.accounts["fresh"]
				owner, status, removing := a.pcli, a.info.Status, a.removing
				m.mu.Unlock()
				if sessOf(m, "fresh") != s1 || isDone(s1) || owner != s1.cli || status != StatusLinking || removing ||
					qrCount(fn) != 1 || connects.Load() != 1 {
					t.Errorf("a second pairing started over the first, or the account moved: %d pairings, %d connects, status %s, calls %v",
						qrCount(fn), connects.Load(), status, fn.called(true))
				}

				unhang()
				eventually(t, "the first pairing's end", func() bool { return isDone(s1) })
				if err := within(t, c.do, m); err != nil {
					t.Errorf("the call after the pairing has ended = %v", err)
				}
			})
		}
	}
}

// TestLoginTellsTheWaitIsOn: the page that cannot restart its pairing yet says
// to repeat, in the words of the refusal, and not in those of an internal error.
func TestLoginTellsTheWaitIsOn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unhang := func() { once.Do(func() { close(release) }) }
	var connects atomic.Int32
	fn := &fakeNet{hold: func(*whatsmeow.Client) {
		if connects.Add(1) == 1 {
			close(entered)
			<-release
		}
	}}
	m := f.start(t, fn.network(readyGlobals()))
	t.Cleanup(unhang)
	m.abortWait = 50 * time.Millisecond
	if _, err := m.Login(ctx, "fresh", linkNonce(t, m, "fresh")); err != nil {
		t.Fatal(err)
	}
	<-entered

	n := m.nonces.issue("fresh") // a second page, as a reload with a new link would be
	st, err := m.Login(ctx, "fresh", n.value)
	if err != nil || st.State != "failed" || st.Reason != string(errStillFinishing) {
		t.Errorf("Login = %+v, %v; want failed with %q", st, err, errStillFinishing)
	}
}

// TestAdmitDecidesTheLinkWithTheAccount: the link of the add before ends, or is
// replaced, in the hold of mu that decides, so that nothing an add does after it
// has decided can undo a later add's link (the race of a revoke that came after
// the next add's issue).
func TestAdmitDecidesTheLinkWithTheAccount(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.account(t, "personal", personalPhone)
	f.devices(t, map[string]string{personalPhone: ""})
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	m.wg.Wait()
	admit := func(nick string, kind pairKind, user string) admitted {
		t.Helper()
		adm, err := m.admit(ctx, nick, kind, user)
		if err != nil {
			t.Fatal(err)
		}
		return adm
	}
	works := func(nick string, adm admitted) bool { return m.ValidLogin(nick, adm.link.value) }

	t.Run("a pairing", func(t *testing.T) {
		first := admit("fresh", kindNone, "")
		if first.link == nil || !works("fresh", first) {
			t.Fatal("admit issued no link")
		}
		chat := admit("fresh", kindChat, "")
		if chat.sess == nil || works("fresh", first) {
			t.Fatal("a pairing for the chat was started and the link before it still works: the revoke is left to the caller")
		}
		later := admit("fresh", kindNone, "") // cancels the pairing for the chat, and issues
		if later.link == nil || !isDone(chat.sess) || !works("fresh", later) {
			t.Fatal("the later link does not work")
		}
		// What is left of the add that began that pairing, the image, ends in
		// an error, and takes no link with it.
		if _, err := m.issueQR(ctx, "fresh", chat.sess); err == nil {
			t.Error("an image for a pairing that was cancelled")
		}
		if !works("fresh", later) {
			t.Error("the tail of an earlier add ended the link of a later one")
		}
		phone := admit("fresh", kindPhone, typedDigits)
		if phone.sess == nil || works("fresh", later) {
			t.Error("a pairing by code was started and the link before it still works")
		}
	})
	t.Run("a reconnect", func(t *testing.T) {
		forceStatus(m, "personal", StatusNeedsLink)
		first := admit("personal", kindNone, "")
		if !works("personal", first) {
			t.Fatal("admit issued no link")
		}
		forceStatus(m, "personal", StatusReplaced)
		if re := admit("personal", kindNone, ""); re.outcome != outcomeReconnect || works("personal", first) {
			t.Errorf("a reconnect left the link before it: %+v", re)
		}
		eventually(t, "the reconnect", func() bool { return countCalls(fn, "connect "+personalPhone) == 2 })

		// The page's own decision leaves the links alone: the one it holds is its own.
		forceStatus(m, "personal", StatusNeedsLink)
		held := admit("personal", kindNone, "")
		forceStatus(m, "personal", StatusReplaced)
		if s, err := m.add(ctx, "personal"); s != nil || err != nil || !works("personal", held) {
			t.Errorf("the page's reconnect = %v, %v; its link works: %v", s, err, works("personal", held))
		}
	})
}

// TestAdmitIssuesTheLinkInTheHoldOfTheDecision: a link is issued while mu is held
// that decided on it, so an add that decides after it, and ends links, comes after
// the issue and ends this link too; issued outside the hold, the link would come
// out of the add that ended links, as one that works. The test starts that add from
// the clock that issue reads.
func TestAdmitIssuesTheLinkInTheHoldOfTheDecision(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	var once sync.Once
	chat, done := make(chan admitted, 1), make(chan struct{})
	m.nonces.now = func() time.Time {
		once.Do(func() {
			go func() {
				adm, err := m.admit(ctx, "fresh", kindChat, "")
				if err != nil {
					t.Error(err)
				}
				chat <- adm
				close(done)
			}()
			select { // with mu held it cannot come this far
			case <-done:
			case <-time.After(100 * time.Millisecond):
			}
		})
		return time.Now()
	}

	adm, err := m.admit(ctx, "fresh", kindNone, "")
	if err != nil || adm.link == nil {
		t.Fatalf("admit = %+v, %v", adm, err)
	}
	if got := <-chat; got.sess == nil {
		t.Fatal("the add for the chat started no pairing")
	}
	if m.ValidLogin("fresh", adm.link.value) {
		t.Error("a link that was issued before a pairing was started works after it")
	}
}

// accountRemovedUnderAdmit is a Manager whose first session built is built while a
// remove ends: the account "fresh" and its row are gone by the time the build is
// done, and with recreated another add has made both again.
func accountRemovedUnderAdmit(t *testing.T, recreated bool) (*Manager, *fixture, *fakeNet) {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.account(t, "fresh", "")
	fn := &fakeNet{firstCode: "2@one"}
	net := fn.network(readyGlobals())
	var m *Manager
	var once sync.Once
	qrChannel := net.qrChannel
	net.qrChannel = func(cli *whatsmeow.Client, c context.Context) (<-chan whatsmeow.QRChannelItem, error) {
		once.Do(func() { // while the first session is built, outside mu
			if err := f.db.DeleteAccount(ctx, "fresh"); err != nil {
				t.Error(err)
			}
			m.mu.Lock()
			m.dropAccount("fresh")
			m.mu.Unlock()
			if recreated {
				if err := f.db.AddAccount(ctx, "fresh"); err != nil {
					t.Error(err)
				}
				m.mu.Lock()
				m.accounts["fresh"] = newAccount("fresh")
				m.mu.Unlock()
			}
		})
		return qrChannel(cli, c)
	}
	m = f.start(t, net)
	m.wg.Wait()
	return m, f, fn
}

// TestAdmitSurvivesAnAccountRemovedUnderIt: a remove that ends while admit holds
// no lock, between its rounds, takes the account and its row, and another add may
// make the account again before admit looks. admit then makes what is missing, the
// row, and pairs the account that is there with a session built for it, not with
// the one it had built for the account that is gone, whose client reports to that.
// The login page, which makes no account, is answered ErrNoLogin instead.
func TestAdmitSurvivesAnAccountRemovedUnderIt(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		name := "gone"
		if recreated {
			name = "made again by another add"
		}
		t.Run(name, func(t *testing.T) {
			m, f, fn := accountRemovedUnderAdmit(t, recreated)
			if r := linkQR(t, m, "fresh"); r.err != nil {
				t.Fatal(r.err)
			}
			if _, there := archiveOf(t, f)["fresh"]; !there {
				t.Error("the account has no row in archive.db")
			}
			m.mu.Lock()
			a := m.accounts["fresh"]
			m.mu.Unlock()
			if a == nil || a.sess == nil || qrCount(fn) != 2 || a.pcli != a.sess.cli || a.sess.cli != fn.qr(t, 1).cli {
				t.Errorf("the account is not paired with a session built for it: %d sessions", qrCount(fn))
			}
		})
	}
	t.Run("the login page", func(t *testing.T) {
		m, f, fn := accountRemovedUnderAdmit(t, false)
		if s, err := m.add(context.Background(), "fresh"); s != nil || !errors.Is(err, ErrNoLogin) {
			t.Errorf("add by the page = %v, %v; want ErrNoLogin", s, err)
		}
		if _, there := archiveOf(t, f)["fresh"]; there || hasAccount(m, "fresh") || qrCount(fn) != 1 {
			t.Errorf("the page brought the account back, or paired it: %d sessions", qrCount(fn))
		}
	})
}

// TestLinkLatestDecisionKeepsItsLink: two adds of a nick at the same time,
// one that hands out a pairing by code and one that hands out a link; whichever
// order they come in, the link works exactly when the link's add came last, which
// is when it cancelled the pairing.
func TestLinkLatestDecisionKeepsItsLink(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	for round := range 60 {
		nick, phone := fmt.Sprintf("n%d", round), fmt.Sprintf("+7 999 000 %02d %02d", round/100, round%100)
		var byCode, byLink linkResult
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			tk, err := m.Link(ctx, LinkRequest{Nick: nick, Phone: phone})
			byCode = linkResult{tk, err}
		})
		wg.Go(func() {
			<-start
			tk, err := m.Link(ctx, LinkRequest{Nick: nick})
			byLink = linkResult{tk, err}
		})
		close(start)
		wg.Wait()
		if byLink.err != nil {
			t.Fatalf("round %d: the link was refused: %v", round, byLink.err)
		}
		nonce := nonceIn(t, byLink.tk, nick)
		s := sessOf(m, nick)
		if s == nil {
			t.Fatalf("round %d: no pairing by code: %+v, %v", round, byCode.tk, byCode.err)
		}
		s.mu.Lock()
		cancelled := s.cancelled
		s.mu.Unlock()
		if works := m.ValidLogin(nick, nonce); works != cancelled {
			t.Fatalf("round %d: the link works: %v, the pairing it came after was cancelled: %v", round, works, cancelled)
		}
	}
}
