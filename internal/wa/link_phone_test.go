package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
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

// The number the tests link by, as a user types it, and as WhatsApp has it.
const (
	typedPhone  = "+7 (999) 123-45-67"
	typedDigits = "79991234567"
)

type linkResult struct {
	tk  LinkTicket
	err error
}

// linkAsync is Link in a goroutine, for the calls that wait.
func linkAsync(m *Manager, nick, phone string) <-chan linkResult {
	return linkAsyncCtx(context.Background(), m, nick, phone)
}

func linkAsyncCtx(ctx context.Context, m *Manager, nick, phone string) <-chan linkResult {
	res := make(chan linkResult, 1)
	go func() {
		tk, err := m.Link(ctx, nick, phone)
		res <- linkResult{tk, err}
	}()
	return res
}

// result waits up to 2 s for a call started by linkAsync.
func result(t *testing.T, res <-chan linkResult) linkResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("Link did not return")
		return linkResult{}
	}
}

// phoneCalls are the PairPhone calls so far.
func phoneCalls(fn *fakeNet) []pairCall {
	fn.mu.Lock()
	defer fn.mu.Unlock()
	return slices.Clone(fn.phones)
}

func accountInfo(t *testing.T, m *Manager, nick string) AccountInfo {
	t.Helper()
	for _, a := range m.Accounts(context.Background()) {
		if a.Nick == nick {
			return a
		}
	}
	t.Fatalf("list has no account %q", nick)
	return AccountInfo{}
}

// requireNothingLeft: a pairing that failed or was refused left no device and
// no number, and its session has ended with its client disconnected.
func requireNothingLeft(t *testing.T, f *fixture, m *Manager, fn *fakeNet, nick string, s *pairSession) {
	t.Helper()
	if s != nil {
		if !isDone(s) {
			t.Error("the session is still running")
		}
		if got := s.status(); got.State != pairFailed {
			t.Errorf("session %+v, want failed", got)
		}
		if !slices.Contains(fn.called(false), "disconnect pairing before cancel") {
			t.Errorf("calls %v: the pairing client is still connected", fn.called(true))
		}
		m.mu.Lock()
		a := m.accounts[nick]
		if a.cli != nil || a.pcli != nil {
			t.Error("the failed pairing left a client on the account")
		}
		m.mu.Unlock()
	}
	if got := accountJID(t, f, nick); got != "" {
		t.Errorf("accounts.jid = %q", got)
	}
	if got := numberOf(m, nick); got != "" {
		t.Errorf("the account holds the number %q", got)
	}
	if got := storedDevices(t, m); len(got) != 0 {
		t.Errorf("devices %v", got)
	}
}

// link is Link, bounded: a call that waits for something that never comes
// fails the test instead of hanging it.
func link(t *testing.T, m *Manager, nick, phone string) linkResult {
	t.Helper()
	return result(t, linkAsync(m, nick, phone))
}

// TestLinkPhone: add with a phone number for a new account gives the code
// WhatsApp gave, once the connection is up, and the account is linking until
// the phone takes the code.
func TestLinkPhone(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	f.log = debugLog(&buf)
	// WhatsApp answers the request later than the first code, and the second
	// code comes later still: a window that is taken from the clock at either
	// would not be the first code's.
	fn := &fakeNet{firstCode: "2@secretcode", pairHold: func(context.Context) error { time.Sleep(30 * time.Millisecond); return nil }}
	m := f.start(t, fn.network(readyGlobals()))

	before := time.Now()
	r := link(t, m, "fresh", typedPhone)
	after := time.Now()
	if r.err != nil {
		t.Fatal(r.err)
	}
	tk := r.tk
	s := sessOf(m, "fresh")
	q := fn.qr(t, 0)
	// The ticket is the code as whatsmeow returns it, and the end of the QR
	// window, which started when the first code did.
	if want := (LinkTicket{PairCode: pairCodeOK, ExpiresAt: s.window}); tk != want {
		t.Errorf("ticket %+v, want %+v", tk, want)
	}
	if qrWindow != 160*time.Second || tk.ExpiresAt.Before(before.Add(qrWindow)) || tk.ExpiresAt.After(after.Add(qrWindow)) {
		t.Errorf("the code expires at %v, want the window of %v from the first code, between %v and %v", tk.ExpiresAt, qrWindow, before, after)
	}
	if s.kind != kindPhone || !noNonces(m) {
		t.Errorf("session kind %v, nonces %v: want a pairing by phone number and no link", s.kind, !noNonces(m))
	}
	// The window is the first code's; the later codes, which rotate inside it,
	// do not move it.
	time.Sleep(30 * time.Millisecond)
	q.ch <- code("2@two", 20*time.Second)
	eventually(t, "the next code", func() bool { return s.status().Code == "2@two" })
	if s.window != tk.ExpiresAt {
		t.Errorf("the window moved to %v with the next code, was %v", s.window, tk.ExpiresAt)
	}

	// PairPhone is asked after the connection is up, with the number as digits.
	if got := fn.called(true); !slices.Equal(got, []string{"qr pairing", "connect pairing", "pairphone pairing"}) {
		t.Errorf("calls %v", got)
	}
	want := []pairCall{{cli: q.cli, phone: typedDigits, push: true, typ: whatsmeow.PairClientChrome, name: pairCodeName()}}
	if got := phoneCalls(fn); !slices.Equal(got, want) || !strings.HasPrefix(pairCodeName(), "Chrome (") {
		t.Errorf("PairPhone calls %+v, want %+v", got, want)
	}

	// Meanwhile list shows linking, and nothing is recorded.
	if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking || got.Reason != "" || got.Phone != "" {
		t.Errorf("account %+v, want linking", got)
	}
	if got := accountJID(t, f, "fresh"); got != "" {
		t.Errorf("accounts.jid = %q before the phone took the code", got)
	}

	// The phone takes the code: the same events as a scan (pair.go:132-160).
	jid := types.NewADJID(typedDigits, 0, 13)
	if !scan(t, q, jid) {
		t.Fatal("PrePairCallback refused the number the code was asked for")
	}
	eventually(t, "paired", stateIs(s, pairPaired))
	if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking {
		t.Errorf("account %+v after the scan, before the new device connects", got)
	}
	q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	<-s.done
	if got := accountInfo(t, m, "fresh"); got.Status != StatusConnected || got.Reason != "" || got.Phone != "+"+typedDigits {
		t.Errorf("account %+v, want connected", got)
	}
	if got := accountJID(t, f, "fresh"); got != jid.String() {
		t.Errorf("accounts.jid = %q, want %q", got, jid)
	}

	// Neither the number, nor the code, nor a QR code is logged (plan 10).
	if log := buf.String(); strings.Contains(log, typedDigits) || strings.Contains(log, pairCodeOK) || strings.Contains(log, "secretcode") ||
		!strings.Contains(log, "pairing code issued") {
		t.Errorf("log:\n%s", log)
	}
}

// TestLinkPhoneOutcomes: add with a phone number applies the same policy as
// without one (plan 6.4). A reconnect ignores the number, and a refusal of
// the policy is told whatever the number.
func TestLinkPhoneOutcomes(t *testing.T) {
	for _, tc := range linkCases() {
		t.Run(tc.name, func(t *testing.T) {
			m, fn, _ := linkFixture(t)
			fn.firstCode = "2@one"
			tc.prep(t, m)
			phone := "+7 999 111 22 33" // not the account's
			if tc.want == wantLink {
				phone = "+7 000 000 00 01"
			}
			before := fn.called(true)
			r := link(t, m, "personal", phone)
			tk, err := r.tk, r.err

			switch tc.want {
			case wantLink:
				if err != nil || tk.PairCode != pairCodeOK || tk.LoginURL != "" || tk.Reconnecting || tk.ExpiresAt.IsZero() {
					t.Fatalf("Link = %+v, %v; want a pairing code", tk, err)
				}
				if got := infoOf(m, "personal"); got.Status != StatusLinking {
					t.Errorf("status %s, want linking", got.Status)
				}
				if got := phoneCalls(fn); len(got) != 1 || got[0].phone != "70000000001" {
					t.Errorf("PairPhone calls %+v", got)
				}
			case wantReconnect:
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
			case wantRefuse:
				if err == nil || !strings.Contains(err.Error(), tc.reason) || (tk != LinkTicket{}) {
					t.Fatalf("Link = %+v, %v; want a refusal with %q", tk, err, tc.reason)
				}
			}
			if tc.want != wantLink && (len(phoneCalls(fn)) != 0 || qrCount(fn) != 0 || !noNonces(m)) {
				t.Errorf("a pairing or a link came out of %s: %v", tc.want, fn.called(true))
			}
			if tc.want == wantRefuse && !slices.Equal(fn.called(true), before) {
				t.Errorf("a refusal started something: %v", fn.called(true))
			}
		})
	}
}

// TestLinkPhoneRefusals: the number is checked before any session or
// connection exists, and a refusal changes nothing: no new account, no number
// recorded, a pairing that is live goes on and so does its link.
func TestLinkPhoneRefusals(t *testing.T) {
	const mine, other = "70000000001", "70000000003"
	for _, tc := range []struct {
		name    string
		accs    map[string]string // nick: accounts.jid's phone
		keys    bool              // the accounts have devices in store.db
		nick    string
		phone   string
		refusal string
	}{
		{name: "relink with another number", accs: map[string]string{"personal": mine}, nick: "personal", phone: "+7 000 000 00 02",
			refusal: differentNumberReason},
		{name: "a number of another account", accs: map[string]string{"work": other}, nick: "fresh", phone: "+7 000 000 00 03",
			refusal: `this number is already linked as account "work"`},
		{name: "a number of another account with keys", accs: map[string]string{"work": other}, keys: true, nick: "fresh", phone: "+7 000 000 00 03",
			refusal: `this number is already linked as account "work"`},
		{name: "an existing account, a number of another", accs: map[string]string{"personal": mine, "work": other}, nick: "personal", phone: "+7 000 000 00 03",
			refusal: differentNumberReason}, // its own number is checked first
		{name: "an existing account without a number, a number of another", accs: map[string]string{"personal": "", "work": other}, nick: "personal", phone: "+7 000 000 00 03",
			refusal: `this number is already linked as account "work"`},
	} {
		_, exists := tc.accs[tc.nick]
		for _, live := range []bool{false, true} {
			name := tc.name
			if live {
				if !exists {
					continue // no page for an account that is not there
				}
				name += ", with a pairing open"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				devs := map[string]string{}
				for nick, p := range tc.accs {
					f.account(t, nick, p)
					if p != "" {
						devs[p] = ""
					}
				}
				if tc.keys {
					f.devices(t, devs)
				}
				fn := &fakeNet{firstCode: "2@one"}
				m := f.start(t, fn.network(readyGlobals()))
				m.wg.Wait()
				// A status the policy pairs: it has no device, or lost it.
				for nick := range tc.accs {
					forceStatus(m, nick, StatusNeedsLink)
				}
				ctx := context.Background()
				var s *pairSession
				var nonce string
				if live { // a QR page shows codes
					nonce = linkNonce(t, m, tc.nick)
					if _, err := m.Login(ctx, tc.nick, nonce); err != nil {
						t.Fatal(err)
					}
					s = sessOf(m, tc.nick)
					eventually(t, "the page's pairing", func() bool { return len(fn.called(true)) == 2 }) // qr, connect
				}
				before := fn.called(true)
				jid := ""
				if exists {
					jid = accountJID(t, f, tc.nick)
				}
				tk, err := m.Link(ctx, tc.nick, tc.phone)

				var r refusal
				if !errors.As(err, &r) || err.Error() != tc.refusal || (tk != LinkTicket{}) {
					t.Fatalf("Link = %+v, %v; want the refusal %q", tk, err, tc.refusal)
				}
				if got := fn.called(true); !slices.Equal(got, before) || len(phoneCalls(fn)) != 0 {
					t.Errorf("a refused add started something: %v", got)
				}
				if exists {
					if got := numberOf(m, tc.nick); got != tc.accs[tc.nick] {
						t.Errorf("number %q, want it as before", got)
					}
					if got := accountJID(t, f, tc.nick); got != jid {
						t.Errorf("accounts.jid = %q, want %q", got, jid)
					}
				} else if accs, _ := f.db.Accounts(ctx); slices.ContainsFunc(accs, func(a archive.Account) bool { return a.Nick == tc.nick }) {
					t.Errorf("a refused add created the account %q", tc.nick)
				}
				m.mu.Lock()
				_, created := m.accounts[tc.nick]
				m.mu.Unlock()
				if created != exists {
					t.Errorf("account %q exists = %v after a refused add", tc.nick, created)
				}
				if live {
					if isDone(s) || fn.qr(t, 0).ctx.Err() != nil || sessOf(m, tc.nick) != s {
						t.Error("a refused add cancelled the pairing that was open")
					}
					if !m.ValidLogin(tc.nick, nonce) {
						t.Error("a refused add ended the link")
					}
				}
			})
		}
	}
}

// TestLinkPhoneNoCode: no first QR code in time, as offline or behind a
// captive portal, ends the pairing and the call, and the account is left as
// it was, needs_link with the reason.
func TestLinkPhoneNoCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fails  []error
		items  []whatsmeow.QRChannelItem
		reason string
		slow   bool // the call waits out codeWait
	}{
		{name: "silence", reason: couldNotReach, slow: true},
		{name: "the connect fails", fails: []error{errHandshake},
			reason: "could not connect to WhatsApp; check the network and call add again"},
		{name: "the channel times out", items: []whatsmeow.QRChannelItem{whatsmeow.QRChannelTimeout}, reason: couldNotReach},
		{name: "client outdated", items: []whatsmeow.QRChannelItem{whatsmeow.QRChannelClientOutdated},
			reason: "WhatsApp rejected this client version; whatsapp-mcp needs an update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{fails: map[string][]error{"pairing": tc.fails}}
			m := f.start(t, fn.network(readyGlobals()))
			m.codeWait = 50 * time.Millisecond
			res := linkAsync(m, "fresh", typedPhone)
			if len(tc.items) > 0 {
				eventually(t, "the pairing", func() bool { return qrCount(fn) == 1 })
				for _, it := range tc.items {
					fn.qr(t, 0).ch <- it
				}
			}
			start := time.Now()
			r := result(t, res)
			if r.err == nil || r.err.Error() != tc.reason || (r.tk != LinkTicket{}) {
				t.Fatalf("Link = %+v, %v; want the error %q", r.tk, r.err, tc.reason)
			}
			if tc.slow && time.Since(start) < m.codeWait/2 {
				t.Error("Link gave up before codeWait")
			}
			s := sessOf(m, "fresh")
			if got := s.status().Reason; got != tc.reason {
				t.Errorf("session says %q", got)
			}
			wantStatus := StatusNeedsLink
			if tc.name == "client outdated" {
				wantStatus = StatusClientOutdated
			}
			if got := infoOf(m, "fresh"); got.Status != wantStatus || got.Reason != tc.reason {
				t.Errorf("account %+v, want %s: %s", got, wantStatus, tc.reason)
			}
			if len(phoneCalls(fn)) != 0 {
				t.Error("PairPhone was called without a connection")
			}
			if tc.slow {
				if q := fn.qr(t, 0); q.ctx.Err() == nil {
					t.Error("the QR channel's context is still alive")
				}
			}
			requireNothingLeft(t, f, m, fn, "fresh", s)
			// The account exists and the next add pairs again.
			if got := accountInfo(t, m, "fresh"); got.Status != wantStatus {
				t.Errorf("list: %+v", got)
			}
		})
	}
}

// TestLinkPhoneBarrier: the wait for the client globals is not part of the 10
// s for the first code, but the call is bound by its ctx and by Close.
func TestLinkPhoneBarrier(t *testing.T) {
	t.Run("not counted", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		g := heldGlobals()
		m := f.start(t, fn.network(g))
		m.codeWait = 30 * time.Millisecond
		res := linkAsync(m, "fresh", typedPhone)
		time.Sleep(150 * time.Millisecond) // five times codeWait
		select {
		case r := <-res:
			t.Fatalf("Link returned while the globals were not final: %+v, %v", r.tk, r.err)
		default:
		}
		if got := fn.called(true); !slices.Equal(got, []string{"qr pairing"}) {
			t.Errorf("calls before the globals: %v", got)
		}
		close(g.ready)
		if r := result(t, res); r.err != nil || r.tk.PairCode != pairCodeOK {
			t.Errorf("Link = %+v, %v", r.tk, r.err)
		}
	})
	t.Run("ctx", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(heldGlobals()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res := linkAsyncCtx(ctx, m, "fresh", typedPhone)
		s := awaitSession(t, m, "fresh") // the call is in its wait: the pairing is built with the ctx alive
		cancel()
		if r := result(t, res); !errors.Is(r.err, context.Canceled) || (r.tk != LinkTicket{}) {
			t.Fatalf("Link = %+v, %v; want its ctx's error", r.tk, r.err)
		}
		eventually(t, "the session's end with no connect", func() bool { return isDone(s) }) // the globals never come
		if got := fn.called(true); !slices.Equal(got, []string{"qr pairing", "disconnect pairing before cancel"}) {
			t.Errorf("calls %v", got)
		}
		if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != cancelledReason {
			t.Errorf("account %+v", got)
		}
		requireNothingLeft(t, f, m, fn, "fresh", s)
	})
	t.Run("ctx while waiting for the code", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{}
		m := f.start(t, fn.network(readyGlobals()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res := linkAsyncCtx(ctx, m, "fresh", typedPhone)
		eventually(t, "the connect", func() bool { return countCalls(fn, "connect pairing") == 1 })
		cancel()
		if r := result(t, res); !errors.Is(r.err, context.Canceled) || (r.tk != LinkTicket{}) {
			t.Fatalf("Link = %+v, %v; want its ctx's error", r.tk, r.err)
		}
		s := sessOf(m, "fresh")
		eventually(t, "the session's end", func() bool { return isDone(s) })
		if len(phoneCalls(fn)) != 0 {
			t.Error("PairPhone was called")
		}
		requireNothingLeft(t, f, m, fn, "fresh", s)
	})
}

// TestLinkPhoneFails: whatever WhatsApp or the connection does when the code
// is asked for, the caller is told a reason that holds neither the server's
// words nor the number, the account has the same reason, and nothing is left.
// Only an answer of WhatsApp's tells that the request is to blame; the rest,
// and a server that failed, are for the connection or for another try.
func TestLinkPhoneFails(t *testing.T) {
	iq := func(code int, text string) error { return &whatsmeow.IQError{Code: code, Text: text} }
	for _, tc := range []struct {
		name   string
		err    error
		hold   func(context.Context) error
		reason string
	}{
		{name: "refused", err: iq(400, "bad-request SERVER-WORDS"), reason: rejectedReason},
		{name: "refused, wrapped", err: fmt.Errorf("request: %w", iq(404, "item-not-found")), reason: rejectedReason},
		{name: "refused, not a number WhatsApp knows", err: whatsmeow.ErrIQNotAcceptable, reason: rejectedReason},
		{name: "an error without a code", err: &whatsmeow.IQError{}, reason: rejectedReason},
		{name: "an answer without the ref", err: &whatsmeow.ElementMissingError{Tag: "link_code_pairing_ref", In: "SERVER-WORDS"}, reason: rejectedReason},
		{name: "too many codes", err: iq(429, "rate-overlimit"), reason: rateLimitedReason},
		{name: "server error", err: iq(500, "internal-server-error"), reason: couldNotReach},
		{name: "service unavailable", err: whatsmeow.ErrIQServiceUnavailable, reason: couldNotReach},
		{name: "IQ timeout", err: whatsmeow.ErrIQTimedOut, reason: couldNotReach},
		{name: "not connected", err: whatsmeow.ErrNotConnected, reason: couldNotReach},
		{name: "the socket dropped", err: fmt.Errorf("request: %w", whatsmeow.ErrIQDisconnected), reason: couldNotReach},
		{name: "something else", err: errors.New("SERVER-WORDS"), reason: couldNotReach},
		{name: "no answer", hold: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, reason: couldNotReach},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var buf bytes.Buffer
			f.log = debugLog(&buf)
			fn := &fakeNet{firstCode: "2@one", pairErr: tc.err, pairHold: tc.hold}
			m := f.start(t, fn.network(readyGlobals()))
			m.phoneWait = 50 * time.Millisecond
			r := link(t, m, "fresh", typedPhone)
			if r.err == nil || r.err.Error() != tc.reason || (r.tk != LinkTicket{}) {
				t.Fatalf("Link = %+v, %v; want the error %q", r.tk, r.err, tc.reason)
			}
			if strings.Contains(r.err.Error(), typedDigits) || strings.Contains(r.err.Error(), "SERVER-WORDS") {
				t.Errorf("the error tells too much: %v", r.err)
			}
			s := sessOf(m, "fresh")
			if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != tc.reason {
				t.Errorf("account %+v, want needs_link: %s", got, tc.reason)
			}
			if got := s.status(); got.State != pairFailed || got.Reason != tc.reason {
				t.Errorf("session %+v", got)
			}
			if len(phoneCalls(fn)) != 1 {
				t.Errorf("%d PairPhone calls", len(phoneCalls(fn)))
			}
			requireNothingLeft(t, f, m, fn, "fresh", s)
			// The server's words are for the log, the user's number is not (plan 10).
			if log := buf.String(); strings.Contains(log, typedDigits) {
				t.Errorf("the number is in the log:\n%s", log)
			}
		})
	}
}

// TestLinkPhoneClose: Close while add waits for the first code ends the call
// and the session, and asks WhatsApp for nothing.
func TestLinkPhoneClose(t *testing.T) {
	for _, tc := range []struct {
		name    string
		globals *waGlobals
		calls   []string // what the session did before Close
	}{
		{"waiting for the code", readyGlobals(), []string{"qr pairing", "connect pairing"}},
		{"waiting for the globals", heldGlobals(), []string{"qr pairing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{}
			m := f.start(t, fn.network(tc.globals))
			res := linkAsync(m, "fresh", typedPhone)
			eventually(t, "the pairing", func() bool { return len(fn.called(true)) == len(tc.calls) })
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			if r := result(t, res); !errors.Is(r.err, errClosing) || (r.tk != LinkTicket{}) {
				t.Errorf("Link = %+v, %v; want the closing error", r.tk, r.err)
			}
			s := sessOf(m, "fresh")
			if !isDone(s) {
				t.Error("Close left the pairing running")
			}
			if got := fn.called(true); !slices.Equal(got, append(tc.calls, "disconnect pairing")) {
				t.Errorf("calls %v: want the pairing client disconnected after Close and no PairPhone", got)
			}
			if _, err := m.Link(context.Background(), "other", typedPhone); !errors.Is(err, errClosing) {
				t.Errorf("Link after Close: %v", err)
			}
		})
	}
}

// TestAwaitFirstCodeAfterEnd: a code that arrives together with the end of the
// pairing, Close or a newer add's stop, is not used: select picks among what is
// ready, and the call must not ask WhatsApp for a pairing that is over,
// whichever it picks.
func TestAwaitFirstCodeAfterEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*Manager, *pairSession)
		want error
	}{
		{"Close", func(m *Manager, _ *pairSession) { m.Close() }, errClosing},
		{"stop", func(_ *Manager, s *pairSession) { s.stop() }, errors.New(cancelledReason)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			m.abortWait = 10 * time.Millisecond // the session is not running, so it does not end
			m.mu.Lock()
			a := newAccount("fresh")
			m.accounts["fresh"] = a
			m.mu.Unlock()
			s, err := m.newPairing(a, kindPhone) // not started, so not ended: only the code is ready
			if err != nil {
				t.Fatal(err)
			}
			close(s.connecting)
			s.markCoded()
			tc.end(m, s)
			if _, err := m.awaitFirstCode(context.Background(), "fresh", s); err == nil || err.Error() != tc.want.Error() {
				t.Errorf("awaitFirstCode = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestLinkPhoneAnswerEnds: Close, or the caller giving up, while WhatsApp is
// being asked for the code ends the pairing too, with the error that tells
// which, and not with a reason about WhatsApp.
func TestLinkPhoneAnswerEnds(t *testing.T) {
	for _, tc := range []string{"Close", "the caller"} {
		t.Run(tc, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{firstCode: "2@one"}
			m := f.start(t, fn.network(readyGlobals()))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fn.pairHold = func(pctx context.Context) error {
				if tc == "Close" {
					<-m.ctx.Done()
					return whatsmeow.ErrNotConnected // what Disconnect does to a request in flight
				}
				<-pctx.Done()
				return pctx.Err()
			}
			res := linkAsyncCtx(ctx, m, "fresh", typedPhone)
			eventually(t, "PairPhone", func() bool { return len(phoneCalls(fn)) == 1 })
			want := error(errClosing)
			if tc == "Close" {
				m.Close()
			} else {
				cancel()
				want = context.Canceled
			}
			if r := result(t, res); !errors.Is(r.err, want) || (r.tk != LinkTicket{}) {
				t.Errorf("Link = %+v, %v; want %v", r.tk, r.err, want)
			}
			s := sessOf(m, "fresh")
			eventually(t, "the session's end", func() bool { return isDone(s) })
			if tc != "Close" {
				if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != cancelledReason {
					t.Errorf("account %+v", got)
				}
				requireNothingLeft(t, f, m, fn, "fresh", s)
			}
		})
	}
}

// TestLinkPhoneEndsInFlight: a pairing that ends while WhatsApp is being asked
// for the code, as the codes running out or the socket dropping do, gives the
// caller no code, whatever the answer then is, and the reason it ended with,
// which is the account's.
func TestLinkPhoneEndsInFlight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error // of the request, once the pairing has ended
	}{
		{"the request fails with it", whatsmeow.ErrIQDisconnected},
		{"the request is answered", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{firstCode: "2@one"}
			entered, release := make(chan struct{}), make(chan struct{})
			fn.pairHold = func(context.Context) error {
				close(entered)
				<-release
				return tc.answer
			}
			m := f.start(t, fn.network(readyGlobals()))
			res := linkAsync(m, "fresh", typedPhone)
			<-entered
			s := sessOf(m, "fresh")
			fn.qr(t, 0).ch <- whatsmeow.QRChannelTimeout // the codes run out, or the socket drops (qrchan.go:196-197)
			eventually(t, "the session's end", func() bool { return isDone(s) })
			close(release)

			r := result(t, res)
			if r.err == nil || r.err.Error() != codeExpired || (r.tk != LinkTicket{}) {
				t.Errorf("Link = %+v, %v; want no code, and %q", r.tk, r.err, codeExpired)
			}
			if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != codeExpired {
				t.Errorf("account %+v", got)
			}
		})
	}
}

// TestLinkPhoneOverlap: an add with a phone number while the one before is
// waiting for its code, as an agent's retry is, cancels that one's pairing: the
// first call is told so, and not what its request made of it, and only the
// second gives a code.
func TestLinkPhoneOverlap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error // of the first request, once it is answered
	}{
		{"the first request fails", whatsmeow.ErrIQDisconnected},
		{"the first request is answered", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{firstCode: "2@one"}
			entered, release := make(chan struct{}), make(chan struct{})
			var asked atomic.Int32
			fn.pairHold = func(context.Context) error {
				if asked.Add(1) > 1 {
					return nil
				}
				close(entered)
				<-release
				return tc.answer
			}
			m := f.start(t, fn.network(readyGlobals()))
			first := linkAsync(m, "fresh", typedPhone)
			<-entered
			s1 := sessOf(m, "fresh")

			fn.pairCode = "9Z8Y-XW7V"
			if r := link(t, m, "fresh", typedPhone); r.err != nil || r.tk.PairCode != "9Z8Y-XW7V" {
				t.Fatalf("the second add = %+v, %v; want its code", r.tk, r.err)
			}
			close(release)
			if r := result(t, first); r.err == nil || r.err.Error() != cancelledReason || (r.tk != LinkTicket{}) {
				t.Errorf("the first add = %+v, %v; want no code, and %q", r.tk, r.err, cancelledReason)
			}
			calls := phoneCalls(fn)
			s2 := sessOf(m, "fresh")
			if s2 == s1 || !isDone(s1) || isDone(s2) || len(calls) != 2 || calls[0].cli == calls[1].cli {
				t.Errorf("want the second pairing alone alive: %+v", calls)
			}
			if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking || got.Reason != "" {
				t.Errorf("account %+v, want linking", got)
			}
			if got := s1.status(); got.Reason != cancelledReason {
				t.Errorf("the first pairing says %q", got.Reason)
			}
		})
	}
}

// TestLinkPhoneHungConnect: a connect that hangs, as in the noise handshake
// with a network that takes no answers, ignores the cancel of the pairing; the
// call still answers when the code does not come, and not only when the
// connect has returned. The account is linking until then, and leaves it with
// the reason the caller was told.
func TestLinkPhoneHungConnect(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fn := &fakeNet{
		fails: map[string][]error{"pairing": {errHandshake}},
		hold:  func(*whatsmeow.Client) { close(entered); <-release },
	}
	m := f.start(t, fn.network(readyGlobals()))
	var once sync.Once
	unhang := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unhang) // before Close, which would wait for the connect
	m.codeWait, m.abortWait = 50*time.Millisecond, 50*time.Millisecond

	res := linkAsync(m, "fresh", typedPhone)
	<-entered
	r := result(t, res)
	if r.err == nil || r.err.Error() != couldNotReach || (r.tk != LinkTicket{}) {
		t.Fatalf("Link = %+v, %v; want the error %q", r.tk, r.err, couldNotReach)
	}
	s := sessOf(m, "fresh")
	if isDone(s) {
		t.Fatal("the session ended with its connect hung")
	}
	if got := infoOf(m, "fresh"); got.Status != StatusLinking {
		t.Errorf("account %+v while the connect hangs", got)
	}

	unhang() // the connect returns its error: the account has the reason the caller got
	eventually(t, "the session's end", func() bool { return isDone(s) })
	if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != couldNotReach {
		t.Errorf("account %+v, want needs_link: %s", got, couldNotReach)
	}
	requireNothingLeft(t, f, m, fn, "fresh", s)
}

// TestLinkPhoneSilence: the QR channel's silence, which a keepalive failure
// brings, ends a pairing by code in its own words: that no code came, before
// the first, and that the code is spent after it.
func TestLinkPhoneSilence(t *testing.T) {
	const silence = 20 * time.Millisecond
	t.Run("before the first code", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{}
		m := f.start(t, fn.network(readyGlobals()))
		m.qrSilence = silence // codeWait is longer: it is the channel that ends this
		r := link(t, m, "fresh", typedPhone)
		if r.err == nil || r.err.Error() != couldNotReach || (r.tk != LinkTicket{}) {
			t.Errorf("Link = %+v, %v; want the error %q", r.tk, r.err, couldNotReach)
		}
		if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != couldNotReach {
			t.Errorf("account %+v", got)
		}
	})
	t.Run("after the code", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{}
		m := f.start(t, fn.network(readyGlobals()))
		m.qrSilence = silence
		res := linkAsync(m, "fresh", typedPhone)
		eventually(t, "the pairing", func() bool { return qrCount(fn) == 1 })
		fn.qr(t, 0).ch <- code("2@one", 10*time.Millisecond) // the only one, and then nothing
		if r := result(t, res); r.err != nil || r.tk.PairCode != pairCodeOK {
			t.Fatalf("Link = %+v, %v", r.tk, r.err)
		}
		s := sessOf(m, "fresh")
		eventually(t, "the silence to end the pairing", func() bool { return isDone(s) })
		if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != codeExpired {
			t.Errorf("account %+v, want needs_link: %s", got, codeExpired)
		}
	})
}

// TestLinkPhoneWindow: the ticket ends when the connection does, which the
// first code's timeout tells: six refs make it 60 s and the window 160 s, and
// of fewer only that timeout is known (qrchan.go:92-96).
func TestLinkPhoneWindow(t *testing.T) {
	for _, tc := range []struct {
		first time.Duration
		want  time.Duration
	}{
		{60 * time.Second, qrWindow},
		{20 * time.Second, 20 * time.Second},
	} {
		t.Run(tc.first.String(), func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			before := time.Now()
			res := linkAsync(m, "fresh", typedPhone)
			eventually(t, "the pairing", func() bool { return qrCount(fn) == 1 })
			fn.qr(t, 0).ch <- code("2@one", tc.first)
			r := result(t, res)
			after := time.Now()
			if r.err != nil || r.tk.ExpiresAt.Before(before.Add(tc.want)) || r.tk.ExpiresAt.After(after.Add(tc.want)) {
				t.Errorf("Link = %+v, %v; want the code to expire %v after the first code, between %v and %v", r.tk, r.err, tc.want, before, after)
			}
		})
	}
}

// TestLinkPhoneRestarts: an add with a phone number replaces the pairing of
// the add before, whichever kind, and is replaced in its turn.
func TestLinkPhoneRestarts(t *testing.T) {
	ctx := context.Background()
	t.Run("a QR page by a code", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		old := linkNonce(t, m, "fresh")
		if _, err := m.Login(ctx, "fresh", old); err != nil {
			t.Fatal(err)
		}
		s1, q1 := sessOf(m, "fresh"), fn.qr(t, 0)
		eventually(t, "the code", stateIs(s1, pairCode))

		r := link(t, m, "fresh", typedPhone)
		tk, err := r.tk, r.err
		if err != nil || tk.PairCode != pairCodeOK {
			t.Fatalf("Link = %+v, %v", tk, err)
		}
		if !isDone(s1) || q1.ctx.Err() == nil {
			t.Error("the QR pairing was not cancelled")
		}
		if got := s1.status(); got.State != pairFailed || got.Reason != cancelledReason {
			t.Errorf("old session %+v", got)
		}
		if m.ValidLogin("fresh", old) {
			t.Error("the page's link survived the add with a code")
		}
		if _, err := m.Login(ctx, "fresh", old); !errors.Is(err, ErrNoLogin) {
			t.Errorf("Login by the old link = %v, want ErrNoLogin", err)
		}
		s2 := sessOf(m, "fresh")
		if s2 == s1 || s2.kind != kindPhone || isDone(s2) || qrCount(fn) != 2 {
			t.Errorf("the account does not follow a new pairing by code: %d QR channels", qrCount(fn))
		}
		if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking {
			t.Errorf("account %+v", got)
		}
	})
	t.Run("a code by a code", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if err := link(t, m, "fresh", typedPhone).err; err != nil {
			t.Fatal(err)
		}
		s1, q1 := sessOf(m, "fresh"), fn.qr(t, 0)

		fn.pairCode = "9Z8Y-XW7V"
		r := link(t, m, "fresh", "+7 999 123 45 67")
		tk, err := r.tk, r.err
		if err != nil || tk.PairCode != "9Z8Y-XW7V" {
			t.Fatalf("Link = %+v, %v; want the new code", tk, err)
		}
		if !isDone(s1) || q1.ctx.Err() == nil {
			t.Error("the first pairing by code was not cancelled")
		}
		if got := s1.status(); got.State != pairFailed || got.Reason != cancelledReason {
			t.Errorf("old session %+v", got)
		}
		s2, q2 := sessOf(m, "fresh"), fn.qr(t, 1)
		calls := phoneCalls(fn)
		if s2 == s1 || isDone(s2) || len(calls) != 2 || calls[0].cli != q1.cli || calls[1].cli != q2.cli {
			t.Errorf("the second code was not asked of the second pairing: %+v", calls)
		}
		if got := tk.ExpiresAt; got != s2.window {
			t.Errorf("the ticket ends at %v, not with the new window %v", got, s2.window)
		}
		if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking {
			t.Errorf("account %+v", got)
		}
	})
	t.Run("a code by a link", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if err := link(t, m, "fresh", typedPhone).err; err != nil {
			t.Fatal(err)
		}
		s1 := sessOf(m, "fresh")
		tk, err := m.Link(ctx, "fresh", "")
		if err != nil {
			t.Fatal(err)
		}
		nonceIn(t, tk, "fresh")
		if !isDone(s1) {
			t.Error("the pairing by code was not cancelled")
		}
		if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != cancelledReason {
			t.Errorf("account %+v", got)
		}
	})
	t.Run("not after the scan", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if err := link(t, m, "fresh", typedPhone).err; err != nil {
			t.Fatal(err)
		}
		s, q := sessOf(m, "fresh"), fn.qr(t, 0)
		jid := types.NewADJID(typedDigits, 0, 13)
		if !q.cli.PrePairCallback(jid, "android", "") {
			t.Fatal("PrePairCallback refused")
		}
		if err := link(t, m, "fresh", typedPhone).err; err == nil || !strings.Contains(err.Error(), "just linked") {
			t.Errorf("Link after the phone took the code: %v", err)
		}
		if q.ctx.Err() != nil || len(phoneCalls(fn)) != 1 {
			t.Error("an add cancelled a pairing whose device is taken")
		}
		saveDevice(t, q.cli.Store, jid)
		q.ch <- whatsmeow.QRChannelSuccess
		eventually(t, "paired", stateIs(s, pairPaired))
		q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
		<-s.done
	})
}

// TestLoginLeavesACodePairingAlone: a pairing that waits for a code typed on
// the phone is never taken over by a login page. Its link ended with the add
// that began it; and a poll that was already under way then is refused too.
func TestLoginLeavesACodePairingAlone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	old := linkNonce(t, m, "fresh")
	if err := link(t, m, "fresh", typedPhone).err; err != nil {
		t.Fatal(err)
	}
	s, q := sessOf(m, "fresh"), fn.qr(t, 0)
	calls := fn.called(true)

	untouched := func(when string) {
		t.Helper()
		if isDone(s) || q.ctx.Err() != nil || sessOf(m, "fresh") != s || qrCount(fn) != 1 || !slices.Equal(fn.called(true), calls) {
			t.Errorf("%s: the pairing by code was touched: %v", when, fn.called(true))
		}
		if got := infoOf(m, "fresh"); got.Status != StatusLinking {
			t.Errorf("%s: account %+v", when, got)
		}
	}

	// The link of the page that was open before the add.
	if _, err := m.Login(ctx, "fresh", old); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Login by the old link = %v, want ErrNoLogin", err)
	}
	untouched("old link")

	// The start of a page, whatever nonce it carried, as one that passed the
	// nonce check before the add would make it.
	if s2, err := m.add(ctx, "fresh"); s2 != nil || !errors.Is(err, errCodeSession) {
		t.Errorf("add as the page = %v, %v; want it refused", s2, err)
	}
	untouched("page start")

	// A poll with a nonce that works though no add issued it after the code
	// (the race the check in admit is for): refused as a missing page, and
	// nothing is settled for the nonce.
	n := m.nonces.issue("fresh")
	if st, err := m.Login(ctx, "fresh", n.value); !errors.Is(err, ErrNoLogin) || st != (LoginState{}) {
		t.Errorf("Login = %+v, %v; want ErrNoLogin", st, err)
	}
	untouched("poll")
	if n.sess != nil || n.settled != nil {
		t.Error("the refused poll left an answer on the nonce")
	}
	if !m.ValidLogin("fresh", n.value) {
		t.Error("the poll ended the nonce, which is the next add's to do")
	}
}

// TestLinkPhonePairingEnds: the QR channel's automaton serves a pairing by code
// as it does one by QR, and the account's status, as list shows it, follows:
// linking, then connected, or needs_link with the reason.
func TestLinkPhonePairingEnds(t *testing.T) {
	const mine = "70000000001" // the number of "personal", which is needs_link
	scanned := func(user string) func(*testing.T, fakeQR) {
		return func(t *testing.T, q fakeQR) { scan(t, q, types.NewADJID(user, 0, 13)) }
	}
	items := func(its ...whatsmeow.QRChannelItem) func(*testing.T, fakeQR) {
		return func(t *testing.T, q fakeQR) {
			for _, it := range its {
				q.ch <- it
			}
		}
	}
	for _, tc := range []struct {
		name      string
		claimedBy string // another account that has the number by the time the phone takes the code
		end       func(*testing.T, fakeQR)
		status    Status
		reason    string
	}{
		{name: "the phone takes the code", end: scanned(mine), status: StatusConnected},
		{name: "the codes run out", end: items(whatsmeow.QRChannelTimeout),
			status: StatusNeedsLink, reason: codeExpired},
		{name: "pair error", end: items(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("boom")}),
			status: StatusNeedsLink, reason: "linking failed: boom; call add again"},
		{name: "passkey", end: items(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{}}),
			status: StatusNeedsLink, reason: "the phone asked to confirm the link with a passkey, which whatsapp-mcp cannot do; call add again"},
		{name: "unexpected state", end: items(whatsmeow.QRChannelErrUnexpectedEvent),
			status: StatusNeedsLink, reason: "WhatsApp answered unexpectedly while linking; call add again"},
		{name: "client outdated", end: items(whatsmeow.QRChannelClientOutdated),
			status: StatusClientOutdated, reason: "WhatsApp rejected this client version; whatsapp-mcp needs an update"},
		{name: "channel closed", end: func(t *testing.T, q fakeQR) { close(q.ch) },
			status: StatusNeedsLink, reason: "linking was interrupted; call add again"},
		{name: "another number scanned", end: scanned("70000000002"),
			status: StatusNeedsLink, reason: differentNumberReason},
		{name: "the number was taken meanwhile", claimedBy: "work", end: scanned(mine),
			status: StatusNeedsLink, reason: `this number is already linked as account "work"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.account(t, "personal", mine)
			if tc.claimedBy != "" {
				f.account(t, tc.claimedBy, "")
			}
			fn := &fakeNet{firstCode: "2@one"}
			m := f.start(t, fn.network(readyGlobals()))
			m.wg.Wait()
			forceStatus(m, "personal", StatusNeedsLink)
			jid := accountJID(t, f, "personal")

			r := link(t, m, "personal", "+"+mine)
			tk, err := r.tk, r.err
			if err != nil || tk.PairCode == "" {
				t.Fatalf("Link = %+v, %v", tk, err)
			}
			s, q := sessOf(m, "personal"), fn.qr(t, 0)
			if got := accountInfo(t, m, "personal"); got.Status != StatusLinking || got.Reason != "" {
				t.Fatalf("while the code is out: %+v, want linking", got)
			}
			if tc.claimedBy != "" {
				m.mu.Lock()
				m.accounts[tc.claimedBy].phone = mine
				m.mu.Unlock()
			}
			tc.end(t, q)

			if tc.status == StatusConnected {
				eventually(t, "paired", stateIs(s, pairPaired))
				if got := accountInfo(t, m, "personal"); got.Status != StatusLinking {
					t.Errorf("after the scan, before the device connects: %+v", got)
				}
				q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
				<-s.done
				if got := accountInfo(t, m, "personal"); got.Status != StatusConnected || got.Reason != "" || got.Phone != "+"+mine {
					t.Errorf("account %+v, want connected", got)
				}
				if got := accountJID(t, f, "personal"); got != types.NewADJID(mine, 0, 13).String() {
					t.Errorf("accounts.jid = %q", got)
				}
				return
			}
			<-s.done
			if got := accountInfo(t, m, "personal"); got.Status != tc.status || got.Reason != tc.reason || !got.ExpiresAt.IsZero() {
				t.Errorf("account %+v, want %s: %s", got, tc.status, tc.reason)
			}
			if got := s.status(); got.State != pairFailed || got.Reason != tc.reason {
				t.Errorf("session %+v", got)
			}
			if got := accountJID(t, f, "personal"); got != jid {
				t.Errorf("accounts.jid = %q, want the old %q", got, jid)
			}
			if got := numberOf(m, "personal"); got != mine {
				t.Errorf("number %q, want %q", got, mine)
			}
			if !slices.Contains(fn.called(false), "disconnect pairing before cancel") {
				t.Errorf("calls %v: the pairing client is still connected", fn.called(true))
			}
		})
	}
}

// TestFinishPairedAfterClose: the pairing's end reaches its lock once Close
// has marked the Manager closed, which a concurrent Close does when it wins the
// race for the lock that the end of the wait (ctx.Done) lets finishPaired
// take. Close has listed the account's clients by then and does not know the
// new one: finishPaired must disconnect it, and leave the account, and its old
// device, which the next start sorts out, as they are.
func TestFinishPairedAfterClose(t *testing.T) {
	const phone = "70000000001"
	for _, old := range []bool{false, true} {
		name := "new account"
		if old {
			name = "relink"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			nick := "fresh"
			if old {
				nick = "personal"
				f.account(t, nick, phone)
				f.devices(t, map[string]string{phone: ""})
			}
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			m.wg.Wait()
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			a := m.accounts[nick]
			if a == nil {
				a = newAccount(nick)
				m.accounts[nick] = a
			}
			m.mu.Unlock()
			s, err := m.newPairing(a, kindPage)
			if err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			a.sess, a.pcli = s, s.cli
			before := a.cli
			m.mu.Unlock()
			calls := fn.called(true)

			m.finishPaired(a, s)

			m.mu.Lock()
			swapped := a.cli != before
			m.mu.Unlock()
			if swapped {
				t.Error("the new client became the account's after Close")
			}
			got := fn.called(true)[len(calls):]
			if !slices.Equal(got, []string{"disconnect pairing"}) {
				t.Errorf("calls after Close: %v, want the new client disconnected and nothing else (no logout, no connect)", got)
			}
			if st := s.status(); st.State != pairDone {
				t.Errorf("session %+v", st)
			}
		})
	}
}
