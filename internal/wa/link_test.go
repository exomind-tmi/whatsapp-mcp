package wa

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// nonceIn checks that the ticket is a login URL of nick on the Manager's
// base URL and returns its nonce.
func nonceIn(t *testing.T, tk LinkTicket, nick string) string {
	t.Helper()
	u, err := url.Parse(tk.LoginURL)
	if err != nil || tk.LoginURL == "" {
		t.Fatalf("ticket %+v has no login URL: %v", tk, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "http://127.0.0.1:1/login/"+nick {
		t.Errorf("login URL %q, want one for %q", tk.LoginURL, nick)
	}
	n := u.Query().Get("t")
	if len(n) != 22 || strings.Trim(n, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		t.Errorf("nonce %q is not 128 bits in base64url", n)
	}
	if tk.PairCode != "" || tk.Reconnecting {
		t.Errorf("ticket %+v: more than a login URL", tk)
	}
	return n
}

func noNonces(m *Manager) bool {
	m.nonces.mu.Lock()
	defer m.nonces.mu.Unlock()
	return len(m.nonces.byNick) == 0
}

func sessOf(m *Manager, nick string) *pairSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accounts[nick].sess
}

func qrCount(fn *fakeNet) int {
	fn.mu.Lock()
	defer fn.mu.Unlock()
	return len(fn.qrs)
}

func countCalls(fn *fakeNet, call string) int {
	return len(slices.DeleteFunc(fn.called(false), func(c string) bool { return c != call }))
}

// linkFixture is a Manager with the account "personal", which has a device,
// and a clock the test moves.
func linkFixture(t *testing.T) (*Manager, *fakeNet, *fixture) {
	t.Helper()
	f := newFixture(t)
	f.account(t, "personal", "70000000001")
	f.devices(t, map[string]string{"70000000001": ""})
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	m.wg.Wait()
	return m, fn, f
}

// TestLinkOutcomes: Link applies the add policy of plan 6.4 to each status
// and starts no pairing: a link, a reconnect or a refusal comes out.
func TestLinkOutcomes(t *testing.T) {
	const (
		link      = "link"
		reconnect = "reconnect"
		refuse    = "refuse"
	)
	banned := func(t *testing.T, m *Manager) {
		m.accounts["personal"].cli.DangerousInternals().DispatchEvent(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour})
	}
	for _, tc := range []struct {
		name   string
		prep   func(t *testing.T, m *Manager)
		want   string
		reason string // a part of the refusal
	}{
		{"needs_link", func(t *testing.T, m *Manager) { forceStatus(m, "personal", StatusNeedsLink) }, link, ""},
		{"connected", func(t *testing.T, m *Manager) { forceStatus(m, "personal", StatusConnected) }, refuse, "already linked and connected"},
		{"reconnecting", func(*testing.T, *Manager) {}, refuse, "reconnects by itself"},
		{"replaced", func(t *testing.T, m *Manager) { forceStatus(m, "personal", StatusReplaced) }, reconnect, ""},
		{"error", func(t *testing.T, m *Manager) { forceStatus(m, "personal", StatusError) }, reconnect, ""},
		{"banned", banned, refuse, "banned by WhatsApp"},
		{"ban over", func(t *testing.T, m *Manager) {
			banned(t, m)
			m.mu.Lock()
			m.accounts["personal"].info.ExpiresAt = time.Now().Add(-time.Second)
			m.mu.Unlock()
		}, reconnect, ""},
		{"client_outdated", func(t *testing.T, m *Manager) { forceStatus(m, "personal", StatusClientOutdated) }, refuse, "update whatsapp-mcp"},
		{"no device", func(t *testing.T, m *Manager) {
			m.mu.Lock()
			m.accounts["personal"].cli = nil
			m.accounts["personal"].info.Status = StatusError
			m.mu.Unlock()
		}, link, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fn, _ := linkFixture(t)
			tc.prep(t, m)
			before := fn.called(true)
			tk, err := m.Link(context.Background(), "personal", "")

			switch tc.want {
			case link:
				if err != nil {
					t.Fatal(err)
				}
				nonceIn(t, tk, "personal")
				if !tk.ExpiresAt.Equal(m.nonces.byNick["personal"].expires) || time.Until(tk.ExpiresAt) < 9*time.Minute || time.Until(tk.ExpiresAt) > loginTTL {
					t.Errorf("link expires at %v, want in 10 minutes", tk.ExpiresAt)
				}
			case reconnect:
				if err != nil || !tk.Reconnecting || tk.LoginURL != "" || tk.PairCode != "" {
					t.Fatalf("Link = %+v, %v; want a reconnect", tk, err)
				}
				if got := infoOf(m, "personal"); got.Status != StatusReconnecting || got.Reason != "" {
					t.Errorf("status after the reconnect began: %+v", got)
				}
				m.wg.Wait()
				if n := countCalls(fn, "connect 70000000001"); n != 2 {
					t.Errorf("%d connects, want the first and the reconnect: %v", n, fn.called(true))
				}
			case refuse:
				if err == nil || !strings.Contains(err.Error(), tc.reason) || (tk != LinkTicket{}) {
					t.Fatalf("Link = %+v, %v; want a refusal with %q", tk, err, tc.reason)
				}
			}
			if tc.want != link && !noNonces(m) {
				t.Error("a nonce was issued without a link")
			}
			if tc.want != reconnect && !slices.Equal(fn.called(true), before) {
				t.Errorf("Link started something: %v", fn.called(true))
			}
		})
	}
}

// TestLinkNewAccount: a link for a new nick creates the account, "not linked
// yet", and starts nothing; the pairing waits for the page.
func TestLinkNewAccount(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))

	tk, err := m.Link(context.Background(), "fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	nonceIn(t, tk, "fresh")
	if got := accountJID(t, f, "fresh"); got != "" { // the row exists, with no device
		t.Errorf("jid %q", got)
	}
	if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != "not linked yet" {
		t.Errorf("account %+v, want needs_link, not linked yet", got)
	}
	if got := fn.called(true); len(got) != 0 {
		t.Errorf("Link started a pairing: %v", got)
	}
	// A restart shows the same account.
	m.Close()
	m2 := f.start(t, (&fakeNet{}).network(readyGlobals()))
	if got := infoOf(m2, "fresh"); got.Status != StatusNeedsLink {
		t.Errorf("after a restart: %+v", got)
	}
}

func TestLinkRefusesWhatItCannotDo(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	ctx := context.Background()

	for _, nick := range []string{"", "Bad Nick", "../x", strings.Repeat("a", 65)} {
		if _, err := m.Link(ctx, nick, ""); err == nil {
			t.Errorf("Link(%q) accepted the nick", nick)
		}
	}
	if _, err := m.Link(ctx, "fresh", "+7 999 000 00 01"); !errors.Is(err, errPhoneNotImplemented) {
		t.Errorf("Link with a phone: %v, want the not implemented error", err)
	}
	if accs, _ := f.db.Accounts(ctx); len(accs) != 0 || !noNonces(m) || len(fn.called(true)) != 0 {
		t.Errorf("a refused Link left something behind: %v, %v", accs, fn.called(true))
	}
	m.Close()
	if _, err := m.Link(ctx, "fresh", ""); !errors.Is(err, errClosing) {
		t.Errorf("Link after Close: %v", err)
	}
}

// TestLinkNeverLogsTheLink: neither the nonce nor the URL reaches the log,
// through Link, Login or the refusals (plan 10).
func TestLinkNeverLogsTheLink(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	f.log = debugLog(&buf)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	ctx := context.Background()

	tk, err := m.Link(ctx, "fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	nonce := nonceIn(t, tk, "fresh")
	if _, err := m.Login(ctx, "fresh", nonce); err != nil {
		t.Fatal(err)
	}
	fn.qr(t, 0).ch <- code("2@secretcode", time.Minute)
	eventually(t, "the code", func() bool { s, _ := m.pairing("fresh"); return s.Code != "" })
	_, wrong := m.Login(ctx, "fresh", "wrong")
	m.Link(ctx, "fresh", "") // restarts the pairing
	m.Close()

	if log := buf.String(); strings.Contains(log, nonce) || strings.Contains(log, "/login/") || strings.Contains(log, "secretcode") ||
		!strings.Contains(log, "login link issued") {
		t.Errorf("log:\n%s", log)
	}
	if wrong == nil || strings.Contains(wrong.Error(), nonce) {
		t.Errorf("error %v", wrong)
	}
}

// TestLinkWhilePairing: a new add for a nick whose pairing shows codes
// cancels it and replaces the nonce; once the phone has scanned, it refuses
// and the nonce stays.
func TestLinkWhilePairing(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	ctx := context.Background()

	tk1, _ := m.Link(ctx, "fresh", "")
	n1 := nonceIn(t, tk1, "fresh")
	if _, err := m.Login(ctx, "fresh", n1); err != nil {
		t.Fatal(err)
	}
	q1 := fn.qr(t, 0)
	q1.ch <- code("2@a", time.Minute)
	s1 := sessOf(m, "fresh")
	eventually(t, "the code", stateIs(s1, pairCode))

	tk2, err := m.Link(ctx, "fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	n2 := nonceIn(t, tk2, "fresh")
	if n1 == n2 {
		t.Error("a new add kept the nonce")
	}
	if !isDone(s1) || q1.ctx.Err() == nil {
		t.Error("the previous pairing was not cancelled")
	}
	if _, err := m.Login(ctx, "fresh", n1); !errors.Is(err, ErrNoLogin) {
		t.Errorf("the old nonce: %v, want ErrNoLogin", err)
	}

	// The second page starts its own pairing, and the phone scans it.
	if st, err := m.Login(ctx, "fresh", n2); err != nil || st.State != "starting" {
		t.Fatalf("Login = %+v, %v", st, err)
	}
	q2 := fn.qr(t, 1)
	q2.ch <- code("2@b", time.Minute)
	s2 := sessOf(m, "fresh")
	eventually(t, "the code", stateIs(s2, pairCode))
	if !q2.cli.PrePairCallback(types.NewADJID("70000000005", 0, 13), "android", "") {
		t.Fatal("PrePairCallback refused")
	}
	if _, err := m.Link(ctx, "fresh", ""); err == nil || !strings.Contains(err.Error(), "just linked") {
		t.Errorf("Link after the scan: %v", err)
	}
	if _, err := m.Login(ctx, "fresh", n2); err != nil {
		t.Errorf("the nonce of a scanned pairing stopped working: %v", err)
	}
	saveDevice(t, q2.cli.Store, types.NewADJID("70000000005", 0, 13))
	q2.ch <- whatsmeow.QRChannelSuccess
	eventually(t, "paired", stateIs(s2, pairPaired))
	q2.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	<-s2.done
}

// TestLogin: the first poll of the page starts the pairing, once; a reload
// and every poll after it read it, whatever became of it.
func TestLogin(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	ctx := context.Background()
	tk, _ := m.Link(ctx, "fresh", "")
	nonce := nonceIn(t, tk, "fresh")

	for _, bad := range []struct{ nick, nonce string }{{"fresh", ""}, {"fresh", "x"}, {"fresh", nonce + "x"}, {"personal", nonce}, {"nobody", nonce}} {
		if st, err := m.Login(ctx, bad.nick, bad.nonce); !errors.Is(err, ErrNoLogin) || st != (LoginState{}) {
			t.Errorf("Login(%q, %q) = %+v, %v; want ErrNoLogin", bad.nick, bad.nonce, st, err)
		}
	}
	if got := fn.called(true); len(got) != 0 {
		t.Fatalf("a bad nonce started something: %v", got)
	}

	st, err := m.Login(ctx, "fresh", nonce)
	if err != nil || st.State != "starting" || st.Code != "" {
		t.Fatalf("Login = %+v, %v; want starting", st, err)
	}
	if got := infoOf(m, "fresh"); got.Status != StatusLinking {
		t.Errorf("status %s, want linking", got.Status)
	}
	q := fn.qr(t, 0)
	q.ch <- code("2@one", time.Minute)
	eventually(t, "the code", func() bool { st, _ := m.Login(ctx, "fresh", nonce); return st.Code == "2@one" })
	st, _ = m.Login(ctx, "fresh", nonce) // the reload
	if st.State != "code" || st.Reason != "" {
		t.Errorf("Login = %+v, want the code", st)
	}
	q.ch <- code("2@two", time.Minute)
	eventually(t, "the next code", func() bool { st, _ := m.Login(ctx, "fresh", nonce); return st.Code == "2@two" })
	if qrCount(fn) != 1 {
		t.Errorf("%d pairings, want the one the page started", qrCount(fn))
	}

	// The QR timeout ends it; reloading shows that, and starts no new one.
	q.ch <- whatsmeow.QRChannelTimeout
	<-sessOf(m, "fresh").done
	st, err = m.Login(ctx, "fresh", nonce)
	if err != nil || st.State != "failed" || st.Reason != "QR expired, call add again" || st.Code != "" {
		t.Errorf("Login after the timeout = %+v, %v", st, err)
	}
	if qrCount(fn) != 1 {
		t.Errorf("a reload started another pairing")
	}
}

// TestLoginStartsOnce: pages opened at the same moment share one pairing.
func TestLoginStartsOnce(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	ctx := context.Background()
	tk, _ := m.Link(ctx, "fresh", "")
	nonce := nonceIn(t, tk, "fresh")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := m.Login(ctx, "fresh", nonce); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	fn.mu.Lock()
	n := len(fn.qrs)
	fn.mu.Unlock()
	if n != 1 {
		t.Errorf("%d pairings started, want 1", n)
	}
}

// linkNonce adds nick and returns the nonce of its link.
func linkNonce(t *testing.T, m *Manager, nick string) string {
	t.Helper()
	tk, err := m.Link(context.Background(), nick, "")
	if err != nil {
		t.Fatal(err)
	}
	return nonceIn(t, tk, nick)
}

// TestLoginWhenTheAccountChanged: an account linked since add is refused, in
// the policy's words and as a state, not an error; and the answer stays.
func TestLoginWhenTheAccountChanged(t *testing.T) {
	ctx := context.Background()
	m, fn, _ := linkFixture(t)
	forceStatus(m, "personal", StatusNeedsLink)
	nonce := linkNonce(t, m, "personal")
	forceStatus(m, "personal", StatusConnected)
	for range 2 {
		st, err := m.Login(ctx, "personal", nonce)
		if err != nil || st.State != "failed" || !strings.Contains(st.Reason, "already linked and connected") {
			t.Errorf("Login of a linked account = %+v, %v", st, err)
		}
	}
	if n := qrCount(fn); n != 0 {
		t.Errorf("%d pairings started", n)
	}
}

// TestLoginWhenTheDeviceIsBack: an account that turned reconnectable since
// add reconnects, with nothing to scan; the poll after the first says the
// same, not the refusal that deciding again would give.
func TestLoginWhenTheDeviceIsBack(t *testing.T) {
	ctx := context.Background()
	m, fn, _ := linkFixture(t)
	forceStatus(m, "personal", StatusNeedsLink)
	nonce := linkNonce(t, m, "personal")
	forceStatus(m, "personal", StatusReplaced)
	for range 3 {
		st, err := m.Login(ctx, "personal", nonce)
		if err != nil || st.State != "done" || st.Hint != reconnectingHint || st.Reason != "" {
			t.Fatalf("Login of a replaced account = %+v, %v", st, err)
		}
	}
	m.wg.Wait()
	if got := infoOf(m, "personal").Status; got != StatusReconnecting {
		t.Errorf("status %s, want reconnecting", got)
	}
	if n := countCalls(fn, "connect 70000000001"); n != 2 { // the first, and one reconnect
		t.Errorf("%d connects: %v", n, fn.called(true))
	}
	if n := qrCount(fn); n != 0 {
		t.Errorf("%d pairings started", n)
	}
}

// TestLoginAfterClose: a Manager that is closing answers with a state; a
// request that ended answers with its error, and the next poll decides.
func TestLoginAfterClose(t *testing.T) {
	ctx := context.Background()
	m, _, _ := linkFixture(t)
	forceStatus(m, "personal", StatusNeedsLink)
	nonce := linkNonce(t, m, "personal")
	m.Close()

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Login(cctx, "personal", nonce); !errors.Is(err, context.Canceled) {
		t.Errorf("Login with a cancelled ctx: %v", err)
	}
	if st, err := m.Login(ctx, "personal", nonce); err != nil || st.State != "failed" || !strings.Contains(st.Reason, "shutting down") {
		t.Errorf("Login after Close = %+v, %v", st, err)
	}
}

// TestLoginKeepsThePairingWhenTheRequestEnds: the page's request may end the
// moment the pairing has started; the pairing runs, and the reload finds it
// instead of starting a second one over a code that may be scanned already.
func TestLoginKeepsThePairingWhenTheRequestEnds(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	net := fn.network(readyGlobals())
	qrChannel := net.qrChannel
	net.qrChannel = func(cli *whatsmeow.Client, sctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
		defer cancel() // the browser gives up while the pairing is built
		return qrChannel(cli, sctx)
	}
	m := f.start(t, net)
	nonce := linkNonce(t, m, "fresh")

	m.Login(ctx, "fresh", nonce)
	s := sessOf(m, "fresh")
	if s == nil || isDone(s) {
		t.Fatal("the pairing did not start")
	}
	st, err := m.Login(context.Background(), "fresh", nonce)
	if err != nil || st.State == "failed" {
		t.Errorf("the reload = %+v, %v", st, err)
	}
	if n := qrCount(fn); n != 1 || sessOf(m, "fresh") != s {
		t.Errorf("%d pairings: the reload restarted it", n)
	}
}

// TestLoginIgnoresAReplacedNonce: a poll that has found its nonce and waits
// for the lock does not start a pairing once a newer add has replaced it.
func TestLoginIgnoresAReplacedNonce(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	old := linkNonce(t, m, "fresh")
	n := m.nonces.find("fresh", old)
	n.lock <- struct{}{} // the poll that holds it is deciding

	res := make(chan error, 1)
	go func() { _, err := m.Login(context.Background(), "fresh", old); res <- err }()
	time.Sleep(50 * time.Millisecond) // for it to reach the lock
	linkNonce(t, m, "fresh")
	n.release()

	if err := <-res; !errors.Is(err, ErrNoLogin) {
		t.Errorf("Login with a replaced nonce = %v, want ErrNoLogin", err)
	}
	if got := qrCount(fn); got != 0 {
		t.Errorf("a replaced nonce started %d pairings", got)
	}
}

// TestLoginWaitsForTheLockOnlyAsLongAsTheRequest: a poll behind another does
// not hang past its own request.
func TestLoginWaitsForTheLockOnlyAsLongAsTheRequest(t *testing.T) {
	f := newFixture(t)
	m := f.start(t, (&fakeNet{}).network(readyGlobals()))
	nonce := linkNonce(t, m, "fresh")
	n := m.nonces.find("fresh", nonce)
	n.lock <- struct{}{}
	defer n.release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.Login(ctx, "fresh", nonce); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Login behind another = %v, want the request's deadline", err)
	}
}

// TestLinkReconnectEndsTheOldLink: an add that reconnects hands out no link,
// and the link of the add before stops working (plan 6.3).
func TestLinkReconnectEndsTheOldLink(t *testing.T) {
	ctx := context.Background()
	m, fn, _ := linkFixture(t)
	forceStatus(m, "personal", StatusNeedsLink)
	old := linkNonce(t, m, "personal")
	forceStatus(m, "personal", StatusReplaced)
	if tk, err := m.Link(ctx, "personal", ""); err != nil || !tk.Reconnecting {
		t.Fatalf("Link = %+v, %v; want a reconnect", tk, err)
	}
	m.wg.Wait()
	if m.ValidLogin("personal", old) {
		t.Error("the old link is valid")
	}
	if _, err := m.Login(ctx, "personal", old); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Login by the old link = %v, want ErrNoLogin", err)
	}
	// And a refusal changes nothing: nothing new was handed out or done.
	forceStatus(m, "personal", StatusNeedsLink)
	fresh := linkNonce(t, m, "personal")
	forceStatus(m, "personal", StatusConnected)
	if _, err := m.Link(ctx, "personal", ""); err == nil {
		t.Fatal("a connected account was linked again")
	}
	if !m.ValidLogin("personal", fresh) {
		t.Error("a refused add ended the link before it")
	}
	if n := qrCount(fn); n != 0 {
		t.Errorf("%d pairings started", n)
	}
}

// TestValidLoginStartsNothing: fetching the page, as a link preview does,
// checks the nonce and leaves the link for the user's browser.
func TestValidLoginStartsNothing(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	nonce := linkNonce(t, m, "fresh")
	for range 3 {
		if !m.ValidLogin("fresh", nonce) {
			t.Fatal("a valid nonce is refused")
		}
	}
	if m.ValidLogin("fresh", nonce+"x") || m.ValidLogin("nobody", nonce) || m.ValidLogin("fresh", "") {
		t.Error("a wrong nonce is valid")
	}
	if got := fn.called(true); len(got) != 0 {
		t.Errorf("checking the nonce started something: %v", got)
	}
	if st, err := m.Login(context.Background(), "fresh", nonce); err != nil || st.State != "starting" {
		t.Errorf("the first poll after it = %+v, %v; want the pairing to start", st, err)
	}
}

// TestLoginOutlivesTheWindowOnceStarted: the window of 10 minutes is for
// opening the page; a pairing started at its end is followed to its end.
func TestLoginOutlivesTheWindowOnceStarted(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	c := &clock{time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m.nonces.now = c.now
	tk, err := m.Link(ctx, "fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	nonce := nonceIn(t, tk, "fresh")

	c.t = tk.ExpiresAt.Add(-10 * time.Second)
	if st, err := m.Login(ctx, "fresh", nonce); err != nil || st.State != "starting" {
		t.Fatalf("Login = %+v, %v", st, err)
	}
	c.t = tk.ExpiresAt.Add(10 * time.Second) // past the window, the code still scans
	fn.qr(t, 0).ch <- code("2@a", time.Minute)
	eventually(t, "the code", func() bool { st, err := m.Login(ctx, "fresh", nonce); return err == nil && st.Code == "2@a" })
	if !tk.ExpiresAt.Equal(m.nonces.byNick["fresh"].expires) {
		t.Error("the window the tool told has moved")
	}

	c.t = tk.ExpiresAt.Add(-10*time.Second + pairingGrace - time.Nanosecond)
	if _, err := m.Login(ctx, "fresh", nonce); err != nil {
		t.Errorf("the nonce of a live pairing expired early: %v", err)
	}
	c.t = c.t.Add(time.Nanosecond)
	if _, err := m.Login(ctx, "fresh", nonce); !errors.Is(err, ErrNoLogin) {
		t.Errorf("the nonce lives past the pairing's grace: %v", err)
	}

	// A page never opened ends with the window, as before.
	tk2, _ := m.Link(ctx, "other", "")
	n2 := nonceIn(t, tk2, "other")
	c.t = tk2.ExpiresAt
	if m.ValidLogin("other", n2) {
		t.Error("an unopened link lives past its window")
	}
}

// TestLoginHidesInternalErrors: the page is told why add refuses, in the
// policy's words, but not what went wrong inside; that is for the log.
func TestLoginHidesInternalErrors(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	f.log = debugLog(&buf)
	net := (&fakeNet{}).network(readyGlobals())
	net.qrChannel = func(*whatsmeow.Client, context.Context) (<-chan whatsmeow.QRChannelItem, error) {
		return nil, errors.New("sqlite: C:/secret/store.db is locked")
	}
	m := f.start(t, net)
	nonce := linkNonce(t, m, "fresh")

	for range 2 {
		st, err := m.Login(context.Background(), "fresh", nonce)
		if err != nil || st.State != "failed" || st.Reason != "could not start linking; call add again" {
			t.Errorf("Login = %+v, %v", st, err)
		}
	}
	if log := buf.String(); !strings.Contains(log, "secret/store.db") || strings.Contains(log, nonce) {
		t.Errorf("the detail is not logged, or the nonce is:\n%s", log)
	}
}
