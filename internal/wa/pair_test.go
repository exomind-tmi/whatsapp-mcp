package wa

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// eventually waits up to 2 s for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func infoOf(m *Manager, nick string) AccountInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accounts[nick].info
}

// forceStatus puts an account in status s, as its events would.
func forceStatus(m *Manager, nick string, s Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.accounts[nick]
	a.info = with(a.info, s, "forced by the test", time.Time{})
}

func stateIs(s *pairSession, st pairState) func() bool {
	return func() bool { return s.status().State == st }
}

func isDone(s *pairSession) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func code(c string, timeout time.Duration) whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: c, Timeout: timeout}
}

// scan does what the phone's scan makes whatsmeow do: PrePairCallback, the
// save of the device (pair.go:204-226), then the final item of the channel,
// "success" or the "error" of a refused device.
func scan(t *testing.T, q fakeQR, jid types.JID) bool {
	t.Helper()
	ok := q.cli.PrePairCallback(jid, "android", "")
	if ok {
		saveDevice(t, q.cli.Store, jid)
		q.ch <- whatsmeow.QRChannelSuccess
	} else {
		q.ch <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: whatsmeow.ErrPairRejectedLocally}
	}
	close(q.ch)
	return ok
}

func accountJID(t *testing.T, f *fixture, nick string) string {
	t.Helper()
	accs, err := f.db.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accs {
		if a.Nick == nick {
			return a.JID
		}
	}
	t.Fatalf("no account %q in archive.db", nick)
	return ""
}

func storedDevices(t *testing.T, m *Manager) []string {
	t.Helper()
	devs, err := m.store.GetAllDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range devs {
		out = append(out, d.ID.String())
	}
	slices.Sort(out)
	return out
}

// pair starts a pairing for nick with add.
func pair(t *testing.T, m *Manager, nick string) *pairSession {
	t.Helper()
	s, err := m.add(context.Background(), nick)
	if err != nil || s == nil {
		t.Fatalf("add(%s) = %v, %v; want a pairing", nick, s, err)
	}
	return s
}

func TestPairingNewAccount(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))

	s := pair(t, m, "fresh")
	if got := accountJID(t, f, "fresh"); got != "" {
		t.Errorf("a new account has jid %q", got)
	}
	if got := infoOf(m, "fresh"); got.Status != StatusLinking {
		t.Errorf("status %s, want linking", got.Status)
	}
	q := fn.qr(t, 0)
	if q.cli.BackgroundEventCtx != m.ctx || !q.cli.ManualHistorySyncDownload || q.cli.PrePairCallback == nil {
		t.Error("the pairing client lacks the settings of a permanent one")
	}

	start := time.Now()
	q.ch <- code("2@one", 60*time.Second)
	eventually(t, "the first code", func() bool { return s.status().Code == "2@one" })
	if st := s.status(); st.State != pairCode || st.Expires.Before(start.Add(60*time.Second)) || st.Expires.After(time.Now().Add(60*time.Second)) {
		t.Errorf("after the first code: %+v", st)
	}
	q.ch <- code("2@two", 20*time.Second)
	eventually(t, "the next code", func() bool { return s.status().Code == "2@two" })
	if st := s.status(); st.Expires.After(time.Now().Add(20 * time.Second)) {
		t.Errorf("the next code expires at %v, want in 20 s", st.Expires)
	}

	jid := types.NewADJID("70000000005", 0, 13)
	if !scan(t, q, jid) {
		t.Fatal("PrePairCallback refused a new account")
	}
	eventually(t, "paired", stateIs(s, pairPaired))
	if got := infoOf(m, "fresh"); got.Status != StatusLinking || got.Phone != "+70000000005" {
		t.Errorf("after the scan, before Connected: %+v; want linking with the phone", got)
	}
	if got := accountJID(t, f, "fresh"); got != jid.String() {
		t.Errorf("accounts.jid = %q, want %q", got, jid)
	}

	// The 515 reconnect brings Connected (connectionevents.go:26-39).
	q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	eventually(t, "done", stateIs(s, pairDone))
	if got := infoOf(m, "fresh"); got.Status != StatusConnected {
		t.Errorf("status %s, want connected", got.Status)
	}
	m.mu.Lock()
	a := m.accounts["fresh"]
	if a.cli != q.cli || a.pcli != nil {
		t.Error("the pairing client did not become the account's")
	}
	m.mu.Unlock()
	if st := s.status(); st.Code != "" || st.Hint != "" {
		t.Errorf("finished session: %+v", st)
	}
	<-s.done
	// The QR channel before the connect, which would otherwise emit the
	// codes to nobody.
	if got := fn.called(true); !slices.Equal(got, []string{"qr pairing", "connect pairing"}) {
		t.Errorf("calls %v", got)
	}
	if p, ok := m.pairing("fresh"); !ok || p.State != pairDone {
		t.Errorf("pairing(fresh) = %+v, %v", p, ok)
	}
	if _, ok := m.pairing("nobody"); ok {
		t.Error("pairing of an unknown account")
	}
	if _, err := m.add(context.Background(), "fresh"); err == nil || !strings.Contains(err.Error(), "already linked and connected") {
		t.Errorf("add of the linked account: %v", err)
	}
}

// TestPairingWaitsForGlobals: no connect before the client globals are
// final.
func TestPairingWaitsForGlobals(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	g := heldGlobals()
	m := f.start(t, fn.network(g))
	s := pair(t, m, "fresh")
	time.Sleep(20 * time.Millisecond)
	if got := fn.called(true); !slices.Equal(got, []string{"qr pairing"}) {
		t.Errorf("calls before the globals: %v", got)
	}
	close(g.ready)
	eventually(t, "the connect", func() bool { return len(fn.called(false)) == 2 })
	fn.qr(t, 0).ch <- whatsmeow.QRChannelTimeout
	<-s.done
}

func TestPairingFails(t *testing.T) {
	scanned := whatsmeow.QRChannelScannedWithoutMultidevice
	for _, tc := range []struct {
		name   string
		fails  []error // of the connect
		before any     // dispatched through the client before the items
		items  []whatsmeow.QRChannelItem
		close  bool
		status Status
		reason string
		hint   string
	}{
		{name: "QR expired", items: []whatsmeow.QRChannelItem{code("2@a", time.Minute), whatsmeow.QRChannelTimeout},
			status: StatusNeedsLink, reason: "QR expired, call add again"},
		{name: "no QR at all", items: []whatsmeow.QRChannelItem{whatsmeow.QRChannelTimeout},
			status: StatusNeedsLink, reason: "could not get a QR code from WhatsApp; check the network and call add again"},
		{name: "connect fails", fails: []error{errHandshake},
			status: StatusNeedsLink, reason: "could not connect to WhatsApp; check the network and call add again"},
		{name: "pair error", items: []whatsmeow.QRChannelItem{code("2@a", time.Minute), {Event: whatsmeow.QRChannelEventError, Error: errors.New("boom")}},
			status: StatusNeedsLink, reason: "linking failed: boom; call add again"},
		{name: "passkey request", items: []whatsmeow.QRChannelItem{code("2@a", time.Minute), {Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{}}},
			status: StatusNeedsLink, reason: "the phone asked to confirm the link with a passkey, which whatsapp-mcp cannot do; call add again"},
		{name: "passkey confirmation", items: []whatsmeow.QRChannelItem{{Event: whatsmeow.QRChannelEventPasskeyResponse, PasskeyConfirmation: &events.PairPasskeyConfirmation{}}},
			status: StatusNeedsLink, reason: "the phone asked to confirm the link with a passkey, which whatsapp-mcp cannot do; call add again"},
		{name: "unexpected state", items: []whatsmeow.QRChannelItem{whatsmeow.QRChannelErrUnexpectedEvent},
			status: StatusNeedsLink, reason: "WhatsApp answered unexpectedly while linking; call add again"},
		{name: "unexpected ban", before: &events.TemporaryBan{Code: events.TempBanBlockedByUsers, Expire: time.Hour},
			items:  []whatsmeow.QRChannelItem{whatsmeow.QRChannelErrUnexpectedEvent},
			status: StatusNeedsLink, reason: "linking failed: temporarily banned: 102: too many people blocked you"},
		{name: "client outdated", items: []whatsmeow.QRChannelItem{whatsmeow.QRChannelClientOutdated},
			status: StatusClientOutdated, reason: "WhatsApp rejected this client version; whatsapp-mcp needs an update"},
		{name: "channel closed", close: true,
			status: StatusNeedsLink, reason: "linking was interrupted; call add again"},
		{name: "unknown event and a scan without multi-device go on",
			items:  []whatsmeow.QRChannelItem{{Event: "from-the-future"}, code("2@a", time.Minute), scanned, whatsmeow.QRChannelTimeout},
			status: StatusNeedsLink, reason: "QR expired, call add again",
			hint: "the phone scanned the code without multi-device support; update WhatsApp on the phone and scan again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{fails: map[string][]error{"pairing": tc.fails}}
			m := f.start(t, fn.network(readyGlobals()))
			s := pair(t, m, "fresh")
			q := fn.qr(t, 0)
			if tc.before != nil {
				q.cli.DangerousInternals().DispatchEvent(tc.before)
			}
			for _, it := range tc.items {
				q.ch <- it
			}
			if tc.close {
				close(q.ch)
			}
			<-s.done

			want := pairStatus{State: pairFailed, Reason: tc.reason, Hint: tc.hint}
			if got := s.status(); got != want {
				t.Errorf("session %+v, want %+v", got, want)
			}
			got := infoOf(m, "fresh")
			if got.Status != tc.status || got.Reason != tc.reason || !got.ExpiresAt.IsZero() {
				t.Errorf("account %+v, want %s: %s", got, tc.status, tc.reason)
			}
			m.mu.Lock()
			a := m.accounts["fresh"]
			if a.cli != nil || a.pcli != nil {
				t.Error("a failed pairing left a client on the account")
			}
			m.mu.Unlock()
			if calls := fn.called(true); calls[len(calls)-1] != "disconnect pairing before cancel" {
				t.Errorf("calls %v: want the pairing client disconnected", calls)
			}
			if q.ctx.Err() == nil {
				t.Error("the QR channel's context is still alive")
			}
			if accountJID(t, f, "fresh") != "" || len(storedDevices(t, m)) != 0 {
				t.Error("a failed pairing left a device")
			}
		})
	}
}

// TestPairingSilence ends a pairing whose QR channel goes quiet, as it does
// when a keepalive failure drops the socket, unless the phone has scanned.
func TestPairingSilence(t *testing.T) {
	const silence = 20 * time.Millisecond
	for _, tc := range []struct {
		name   string
		items  []whatsmeow.QRChannelItem
		alive  time.Duration // how long the pairing must last at least
		bound  bool
		reason string
	}{
		{name: "no code", reason: "could not get a QR code from WhatsApp; check the network and call add again"},
		{name: "no code after the last", items: []whatsmeow.QRChannelItem{code("2@a", 10*silence)},
			alive: 5 * silence, reason: "QR expired, call add again"},
		{name: "scanned", items: []whatsmeow.QRChannelItem{code("2@a", 0)}, bound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			m.qrSilence = silence
			s := pair(t, m, "fresh")
			q := fn.qr(t, 0)
			for _, it := range tc.items {
				q.ch <- it
			}
			if !tc.bound {
				time.Sleep(tc.alive)
				if isDone(s) {
					t.Fatalf("the pairing ended while its code was valid: %+v", s.status())
				}
				<-s.done
				if got := s.status(); got.State != pairFailed || got.Reason != tc.reason {
					t.Errorf("session %+v, want failed: %s", got, tc.reason)
				}
				return
			}
			eventually(t, "the code", stateIs(s, pairCode))
			jid := types.NewADJID("70000000005", 0, 13)
			if !q.cli.PrePairCallback(jid, "android", "") {
				t.Fatal("PrePairCallback refused")
			}
			time.Sleep(5 * m.qrSilence)
			if isDone(s) {
				t.Fatalf("a scanned pairing ended on silence: %+v", s.status())
			}
			saveDevice(t, q.cli.Store, jid)
			q.ch <- whatsmeow.QRChannelSuccess
			eventually(t, "paired", stateIs(s, pairPaired))
			q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
			<-s.done
		})
	}
}

// TestPrePairCallback binds the account to the number of its first device.
func TestPrePairCallback(t *testing.T) {
	const phone = "70000000001"
	for _, tc := range []struct {
		name    string
		bound   string // accounts.jid's phone; "" for a new account
		scanned string
		ok      bool
	}{
		{"new account", "", phone, true},
		{"relink, same number", phone, phone, true},
		{"relink, different number", phone, "70000000002", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.bound != "" {
				f.account(t, "personal", tc.bound) // its device is gone: needs_link
			}
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			before := ""
			if tc.bound != "" {
				before = accountJID(t, f, "personal")
			}
			s := pair(t, m, "personal")
			q := fn.qr(t, 0)
			q.ch <- code("2@a", time.Minute)
			jid := types.NewADJID(tc.scanned, 0, 13)
			if got := scan(t, q, jid); got != tc.ok {
				t.Fatalf("PrePairCallback = %v, want %v", got, tc.ok)
			}
			if tc.ok {
				eventually(t, "paired", stateIs(s, pairPaired))
				if got := accountJID(t, f, "personal"); got != jid.String() {
					t.Errorf("accounts.jid = %q, want %q", got, jid)
				}
				q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
				<-s.done
				return
			}
			<-s.done
			reason := "this is a different phone number; remove the account and add it again"
			if got := s.status(); got.State != pairFailed || got.Reason != reason {
				t.Errorf("session %+v", got)
			}
			if got := infoOf(m, "personal"); got.Status != StatusNeedsLink || got.Reason != reason || got.Phone != "" {
				t.Errorf("account %+v", got)
			}
			if got := accountJID(t, f, "personal"); got != before {
				t.Errorf("accounts.jid = %q, want the old %q", got, before)
			}
		})
	}
}

// TestPrePairCallbackOneNumberOneAccount: a number is linked under one nick
// only; the refusal names the account that has it.
func TestPrePairCallbackOneNumberOneAccount(t *testing.T) {
	const phone, other = "70000000001", "70000000002"
	for _, tc := range []struct {
		name    string
		accs    map[string]string // nick: accounts.jid's phone of the other accounts
		keys    bool              // the other accounts have devices in store.db
		bound   string            // the pairing account's number
		scanned string
		refusal string // "" if accepted
	}{
		{name: "same nick, same number", bound: phone, scanned: phone},
		{name: "other nick, same number", accs: map[string]string{"work": phone}, scanned: phone,
			refusal: `this number is already linked as account "work"`},
		{name: "other nick with keys, same number", accs: map[string]string{"work": phone}, keys: true, scanned: phone,
			refusal: `this number is already linked as account "work"`},
		{name: "other nick, different number", accs: map[string]string{"work": other}, scanned: phone},
		{name: "relink with the number of another", accs: map[string]string{"work": other}, bound: phone, scanned: other,
			refusal: "this is a different phone number; remove the account and add it again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.bound != "" {
				f.account(t, "personal", tc.bound)
			}
			devs := map[string]string{}
			for nick, p := range tc.accs {
				f.account(t, nick, p)
				devs[p] = ""
			}
			if tc.keys {
				f.devices(t, devs)
			}
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			m.wg.Wait()
			before := ""
			if tc.bound != "" {
				before = accountJID(t, f, "personal")
			}
			s := pair(t, m, "personal")
			q := fn.qr(t, 0)
			q.ch <- code("2@a", time.Minute)
			jid := types.NewADJID(tc.scanned, 0, 13)
			if got := scan(t, q, jid); got != (tc.refusal == "") {
				t.Fatalf("PrePairCallback = %v, want %v", got, tc.refusal == "")
			}
			if tc.refusal == "" {
				eventually(t, "paired", stateIs(s, pairPaired))
				if got := accountJID(t, f, "personal"); got != jid.String() {
					t.Errorf("accounts.jid = %q, want %q", got, jid)
				}
				q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
				<-s.done
				return
			}
			<-s.done
			if got := s.status(); got.State != pairFailed || got.Reason != tc.refusal {
				t.Errorf("session %+v, want failed: %s", got, tc.refusal)
			}
			if got := infoOf(m, "personal"); got.Status != StatusNeedsLink || got.Reason != tc.refusal || got.Phone != "" {
				t.Errorf("account %+v", got)
			}
			if got := accountJID(t, f, "personal"); got != before {
				t.Errorf("accounts.jid = %q, want the old %q", got, before)
			}
			m.mu.Lock()
			if got := m.accounts["personal"].phone; got != tc.bound {
				t.Errorf("the refused pairing left the number %q, want %q", got, tc.bound)
			}
			m.mu.Unlock()
		})
	}
}

// TestPrePairCallbackConcurrent: two accounts whose pairings scan the same
// phone at once; exactly one gets it, whichever callback comes first.
func TestPrePairCallbackConcurrent(t *testing.T) {
	jid := types.NewADJID("70000000001", 0, 13)
	for round := range 20 {
		f := newFixture(t)
		fn := &fakeNet{}
		m := f.start(t, fn.network(readyGlobals()))
		sa, sb := pair(t, m, "a"), pair(t, m, "b")
		var qs [2]fakeQR
		for i := range qs {
			qs[i] = fn.qr(t, i)
			qs[i].ch <- code("2@a", time.Minute)
		}
		var accepted [2]bool
		var ready, wg sync.WaitGroup
		start := make(chan struct{})
		for i := range qs {
			ready.Add(1)
			wg.Go(func() {
				ready.Done()
				<-start
				accepted[i] = qs[i].cli.PrePairCallback(jid, "android", "")
			})
		}
		ready.Wait()
		close(start)
		wg.Wait()
		if accepted[0] == accepted[1] {
			t.Fatalf("round %d: accepted %v, want exactly one", round, accepted)
		}
		// End both pairings: the winner's device is saved, the loser's is refused.
		winner, loser := 0, 1
		if accepted[1] {
			winner, loser = 1, 0
		}
		saveDevice(t, qs[winner].cli.Store, jid)
		qs[winner].ch <- whatsmeow.QRChannelSuccess
		qs[loser].ch <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: whatsmeow.ErrPairRejectedLocally}
		sessions := [2]*pairSession{sa, sb}
		eventually(t, "paired", stateIs(sessions[winner], pairPaired))
		qs[winner].cli.DangerousInternals().DispatchEvent(&events.Connected{})
		<-sessions[winner].done
		<-sessions[loser].done
		nicks := [2]string{"a", "b"}
		if got := accountJID(t, f, nicks[winner]); got != jid.String() {
			t.Errorf("round %d: winner's accounts.jid = %q", round, got)
		}
		if got := accountJID(t, f, nicks[loser]); got != "" {
			t.Errorf("round %d: loser's accounts.jid = %q", round, got)
		}
		want := `this number is already linked as account "` + nicks[winner] + `"`
		if got := sessions[loser].status(); got.Reason != want {
			t.Errorf("round %d: loser's session %+v, want %s", round, got, want)
		}
	}
}

// differentNumber is the refusal of a relink with another number.
const differentNumber = "this is a different phone number; remove the account and add it again"

// numberOf is the number the manager holds for nick.
func numberOf(m *Manager, nick string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accounts[nick].phone
}

// requireRelinkKeepsNumber: the account nick, which had the number bound before
// a failed pairing, still has it, so the pairing of another number is refused.
func requireRelinkKeepsNumber(t *testing.T, m *Manager, fn *fakeNet, nick, bound string) {
	t.Helper()
	if got := numberOf(m, nick); got != bound {
		t.Fatalf("number %q after the failed pairing, want %q", got, bound)
	}
	s := pair(t, m, nick)
	q := fn.qr(t, 1)
	q.ch <- code("2@a", time.Minute)
	if scan(t, q, types.NewADJID("70000000009", 0, 13)) {
		t.Fatal("a relink with another number was taken")
	}
	<-s.done
	if got := s.status(); got.Reason != differentNumber {
		t.Errorf("session %+v, want failed: %s", got, differentNumber)
	}
}

// TestPrePairCallbackCancelledReleasesNumber: a pairing refused as cancelled
// does not keep the number it tried to take, and gives an account its old one
// back.
func TestPrePairCallbackCancelledReleasesNumber(t *testing.T) {
	const phone = "70000000001"
	jid := types.NewADJID(phone, 0, 13)
	t.Run("new account", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{}
		m := f.start(t, fn.network(readyGlobals()))
		sa := pair(t, m, "a")
		if !sa.stop() {
			t.Fatal("stop of an open pairing returned false")
		}
		<-sa.done
		if fn.qr(t, 0).cli.PrePairCallback(jid, "android", "") {
			t.Fatal("a cancelled pairing took a device")
		}
		sb := pair(t, m, "b")
		q := fn.qr(t, 1)
		q.ch <- code("2@a", time.Minute)
		if !scan(t, q, jid) {
			t.Fatalf("the number is still held by the cancelled pairing: %q", sb.refused())
		}
		eventually(t, "paired", stateIs(sb, pairPaired))
		q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
		<-sb.done
	})
	t.Run("relink", func(t *testing.T) {
		f := newFixture(t)
		f.account(t, "a", phone)
		fn := &fakeNet{}
		m := f.start(t, fn.network(readyGlobals()))
		sa := pair(t, m, "a")
		if !sa.stop() {
			t.Fatal("stop of an open pairing returned false")
		}
		<-sa.done
		if fn.qr(t, 0).cli.PrePairCallback(jid, "android", "") {
			t.Fatal("a cancelled pairing took a device")
		}
		requireRelinkKeepsNumber(t, m, fn, "a", phone)
	})
}

// TestPrePairCallbackRecordFails: a device whose accounts.jid cannot be
// written is refused, and the pairing keeps neither the number nor the bind;
// a relinked account keeps its old number.
func TestPrePairCallbackRecordFails(t *testing.T) {
	const phone = "70000000001"
	for _, bound := range []string{"", phone} {
		t.Run("bound "+bound, func(t *testing.T) {
			f := newFixture(t)
			if bound != "" {
				f.account(t, "fresh", bound)
			}
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			s := pair(t, m, "fresh")
			q := fn.qr(t, 0)
			q.ch <- code("2@a", time.Minute)
			eventually(t, "the code", stateIs(s, pairCode))
			if err := f.db.Close(); err != nil {
				t.Fatal(err)
			}
			if q.cli.PrePairCallback(types.NewADJID(phone, 0, 13), "android", "") {
				t.Fatal("PrePairCallback took a device it could not record")
			}
			if got := s.refused(); !strings.Contains(got, "could not record") {
				t.Errorf("refusal %q", got)
			}
			if got := numberOf(m, "fresh"); got != bound || s.phase() != pairingOpen {
				t.Errorf("number %q, phase %v; want %q and open", got, s.phase(), bound)
			}
			q.ch <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: whatsmeow.ErrPairRejectedLocally}
			<-s.done
		})
	}
}

// TestPairSessionStop: stop cancels a session that has taken no device and
// never one that has (addPolicy refuses a bound session before stop, so
// only this test holds the guard).
func TestPairSessionStop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bind      bool
		stopped   bool
		cancelled bool
	}{
		{"open", false, true, true},
		{"bound", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &pairSession{ctx: ctx, cancel: cancel}
			if tc.bind && !s.bind() {
				t.Fatal("bind of an open session failed")
			}
			if got := s.stop(); got != tc.stopped {
				t.Errorf("stop() = %v, want %v", got, tc.stopped)
			}
			if got := ctx.Err() != nil; got != tc.cancelled {
				t.Errorf("session ctx cancelled = %v, want %v", got, tc.cancelled)
			}
			if got := s.bind(); got == tc.cancelled {
				t.Errorf("bind after stop() = %v, want %v", got, !tc.cancelled)
			}
		})
	}
}

// TestPairingReplacesOldDevice relinks an account that has a device: the
// old one goes only once the new one has connected. add pairs only an
// account in needs_link, whose old device whatsmeow has deleted on
// LoggedOut; the others force the status to cover every path of dropOld.
func TestPairingReplacesOldDevice(t *testing.T) {
	const phone = "70000000001"
	oldJID, newJID := adJID(phone), types.NewADJID(phone, 0, 13)
	for _, tc := range []struct {
		name      string
		connected bool  // the old client's socket is up
		logoutErr error // what its Logout returns then
		unlinked  bool  // whatsmeow already deleted the old device on LoggedOut
		hint      string
	}{
		{name: "logout", connected: true},
		{name: "logout fails", connected: true, logoutErr: errors.New("timed out"), hint: oldDeviceHint},
		{name: "offline", hint: oldDeviceHint},
		{name: "unlinked on the phone", unlinked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.account(t, "personal", phone)
			f.devices(t, map[string]string{phone: "Anton"})
			fn := &fakeNet{connected: map[string]bool{phone: tc.connected}, logoutErr: tc.logoutErr}
			m := f.start(t, fn.network(readyGlobals()))
			m.wg.Wait()
			old := m.accounts["personal"].cli
			if tc.unlinked {
				old.DangerousInternals().DispatchEvent(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
				if err := old.Store.Delete(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				forceStatus(m, "personal", StatusNeedsLink)
			}

			s := pair(t, m, "personal")
			q := fn.qr(t, 0)
			q.ch <- code("2@a", time.Minute)
			scan(t, q, newJID)
			eventually(t, "paired", stateIs(s, pairPaired))
			if got := storedDevices(t, m); !tc.unlinked && !slices.Contains(got, oldJID.String()) {
				t.Errorf("the old device went before the new one connected: %v", got)
			}
			q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
			<-s.done

			if got := s.status(); got.State != pairDone || got.Hint != tc.hint {
				t.Errorf("session %+v, want done with hint %q", got, tc.hint)
			}
			if got := storedDevices(t, m); !slices.Equal(got, []string{newJID.String()}) {
				t.Errorf("devices %v, want only the new one", got)
			}
			name := phone
			if tc.unlinked {
				name = "pairing" // phoneOf a device deleted
			}
			calls := fn.called(true)
			if !slices.Contains(calls, "logout "+name) {
				t.Errorf("calls %v: want a Logout of the old client", calls)
			}
			disconnected := slices.Contains(calls, "disconnect "+name+" before cancel")
			if loggedOut := tc.connected && tc.logoutErr == nil; disconnected == loggedOut {
				t.Errorf("calls %v: the old client must be disconnected unless Logout did it", calls)
			}

			// The old client's late events no longer move the status.
			old.DangerousInternals().DispatchEvent(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
			if got := infoOf(m, "personal"); got.Status != StatusConnected || got.Phone != "+"+phone {
				t.Errorf("account %+v, want connected", got)
			}
		})
	}
}

// TestPairedConnectLate covers a 515 reconnect that fails: whatsmeow does
// not retry it, so the account's own connect loop does.
func TestPairedConnectLate(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	m.pairedWait = 10 * time.Millisecond
	s := pair(t, m, "fresh")
	q := fn.qr(t, 0)
	q.ch <- code("2@a", time.Minute)
	scan(t, q, types.NewADJID("70000000005", 0, 13))
	<-s.done
	m.wg.Wait()
	if got := infoOf(m, "fresh"); got.Status != StatusReconnecting {
		t.Errorf("status %s, want reconnecting", got.Status)
	}
	if got := fn.called(true); !slices.Equal(got, []string{"qr pairing", "connect pairing", "connect 70000000005"}) {
		t.Errorf("calls %v", got)
	}
	q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	if got := infoOf(m, "fresh"); got.Status != StatusConnected {
		t.Errorf("status %s, want connected", got.Status)
	}
}

// TestPairingRestart: the next add cancels a pairing and starts its own
// once the old one has ended, but not after the phone scanned.
func TestPairingRestart(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	// The first connect hangs, as in a noise handshake.
	entered, release := make(chan struct{}), make(chan struct{})
	var connects atomic.Int32
	fn.hold = func(*whatsmeow.Client) {
		if connects.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	m := f.start(t, fn.network(readyGlobals()))
	s1 := pair(t, m, "fresh")
	q1 := fn.qr(t, 0)
	<-entered

	// add waits for the old pairing to end, up to its ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.add(ctx, "fresh"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("add while the old pairing connects: %v, want its ctx's error", err)
	}
	if q1.ctx.Err() == nil || isDone(s1) {
		t.Error("the old pairing is not cancelled, or ended inside its connect")
	}
	type result struct {
		s   *pairSession
		err error
	}
	res := make(chan result)
	go func() {
		s, err := m.add(context.Background(), "fresh")
		res <- result{s, err}
	}()
	select {
	case r := <-res:
		t.Fatalf("the new pairing started before the old one ended: %v, %v", r.s, r.err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	s2 := r.s
	if !isDone(s1) {
		t.Fatal("the new pairing started before the old one ended")
	}
	if !slices.Contains(fn.called(false), "disconnect pairing before cancel") {
		t.Errorf("calls %v: the old pairing client was not disconnected", fn.called(true))
	}
	if got := s1.status(); got.State != pairFailed || got.Reason != cancelledReason {
		t.Errorf("old session %+v", got)
	}
	if q1.cli.PrePairCallback(types.NewADJID("70000000005", 0, 13), "", "") || s1.refused() != cancelledReason {
		t.Errorf("the cancelled pairing still accepts a device, or says why not wrongly: %q", s1.refused())
	}
	q2 := fn.qr(t, 1) // the add that timed out built none
	m.mu.Lock()
	a := m.accounts["fresh"]
	if a.sess != s2 || a.pcli != q2.cli || a.info.Status != StatusLinking {
		t.Errorf("the account does not follow the new pairing: %+v", a.info)
	}
	m.mu.Unlock()

	// Scanned: PrePairCallback took the device and accounts.jid names it,
	// before the channel says "success". It is not cancelled any more.
	q2.ch <- code("2@a", time.Minute)
	eventually(t, "the code", stateIs(s2, pairCode))
	jid := types.NewADJID("70000000005", 0, 13)
	if !q2.cli.PrePairCallback(jid, "android", "") {
		t.Fatal("PrePairCallback refused")
	}
	if _, err := m.add(context.Background(), "fresh"); err == nil || !strings.Contains(err.Error(), "just linked") {
		t.Errorf("add after the scan: %v", err)
	}
	if q2.ctx.Err() != nil {
		t.Error("add cancelled a scanned pairing")
	}
	saveDevice(t, q2.cli.Store, jid)
	q2.ch <- whatsmeow.QRChannelSuccess
	eventually(t, "paired", stateIs(s2, pairPaired))
	if _, err := m.add(context.Background(), "fresh"); err == nil || !strings.Contains(err.Error(), "just linked") {
		t.Errorf("add after success: %v", err)
	}
	q2.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	<-s2.done
}

func TestPairingClose(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	s := pair(t, m, "fresh")
	fn.qr(t, 0).ch <- code("2@a", time.Minute)
	eventually(t, "the code", stateIs(s, pairCode))
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if !isDone(s) {
		t.Fatal("Close left the pairing running")
	}
	if !slices.Contains(fn.called(false), "disconnect pairing") {
		t.Errorf("calls %v: the pairing client is still connected", fn.called(true))
	}
	if _, err := m.add(context.Background(), "other"); !errors.Is(err, errClosing) {
		t.Errorf("add after Close: %v", err)
	}
}

// TestPairingCloseAfterScan: Close before the new device connects
// disconnects both clients, and the next start connects the new device and
// drops the old one.
func TestPairingCloseAfterScan(t *testing.T) {
	const phone = "70000000001"
	f := newFixture(t)
	f.account(t, "personal", phone)
	f.devices(t, map[string]string{phone: ""})
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	m.wg.Wait()
	forceStatus(m, "personal", StatusNeedsLink)
	s := pair(t, m, "personal")
	q := fn.qr(t, 0)
	q.ch <- code("2@a", time.Minute)
	newJID := types.NewADJID(phone, 0, 13)
	scan(t, q, newJID)
	eventually(t, "paired", stateIs(s, pairPaired))
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.status(); got.State != pairDone {
		t.Errorf("session %+v", got)
	}
	// The old device and the new one share the number.
	n := 0
	for _, c := range fn.called(false) {
		if strings.HasPrefix(c, "disconnect "+phone) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("calls %v: want both clients disconnected", fn.called(true))
	}

	fn2 := &fakeNet{}
	m2 := f.start(t, fn2.network(readyGlobals()))
	m2.wg.Wait()
	if got := storedDevices(t, m2); !slices.Equal(got, []string{newJID.String()}) {
		t.Errorf("devices %v, want the new one", got)
	}
	if got := fn2.called(false); !slices.Equal(got, []string{"connect " + phone}) {
		t.Errorf("calls %v", got)
	}
}

// TestRelinkCrashLeavesOrphan: a crash between the scan and dropOld leaves
// the old device in store.db with accounts.jid on the new one; the next
// start deletes it as an orphan.
func TestRelinkCrashLeavesOrphan(t *testing.T) {
	const phone = "70000000001"
	f := newFixture(t)
	newJID := types.NewADJID(phone, 0, 13)
	f.account(t, "personal", newJID.String())
	f.devices(t, map[string]string{phone: ""}) // the old one, device 12
	c, err := openStore(context.Background(), f.storePath(), newWALog(quiet, "Database"))
	if err != nil {
		t.Fatal(err)
	}
	saveDevice(t, c.NewDevice(), newJID)
	c.Close()

	m := f.start(t, (&fakeNet{}).network(readyGlobals()))
	if got := storedDevices(t, m); !slices.Equal(got, []string{newJID.String()}) {
		t.Errorf("devices %v, want the old one swept", got)
	}
	if got := m.accounts["personal"].cli.Store.ID; *got != newJID {
		t.Errorf("the account uses %v, want %v", got, newJID)
	}
}
