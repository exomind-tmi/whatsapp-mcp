package wa

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/exomind-tmi/whatsapp-mcp/internal/qr"
)

// linkQRAsync is add with qr_image in a goroutine, for the calls that wait.
func linkQRAsync(ctx context.Context, m *Manager, nick string) <-chan linkResult {
	return linkReqAsync(ctx, m, LinkRequest{Nick: nick, QRImage: true})
}

// linkQR is add with qr_image, bounded like link.
func linkQR(t *testing.T, m *Manager, nick string) linkResult {
	t.Helper()
	return result(t, linkQRAsync(context.Background(), m, nick))
}

// requireQRImage checks that data is the QR code of content as the login page
// would draw it, and that it is a whole PNG image.
func requireQRImage(t *testing.T, data []byte, content string) {
	t.Helper()
	want, err := qr.PNG(content)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("the image (%d bytes) is not the QR code of %q (%d bytes)", len(data), content, len(want))
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the image is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != b.Dy() || b.Dx() < 256 {
		t.Errorf("the image is %v, want a square of at least 256 px", b)
	}
}

// TestLinkQR: add with qr_image for a new account gives the first QR code
// WhatsApp showed, drawn, once the connection is up; the account is linking
// until the phone scans it, and no link of the page is left.
func TestLinkQR(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	f.log = debugLog(&buf)
	fn := &fakeNet{firstCode: "2@secretcode"}
	m := f.start(t, fn.network(readyGlobals()))
	old := linkNonce(t, m, "fresh") // the link of an add before

	before := time.Now()
	r := linkQR(t, m, "fresh")
	after := time.Now()
	if r.err != nil {
		t.Fatal(r.err)
	}
	tk := r.tk
	s, q := sessOf(m, "fresh"), fn.qr(t, 0)

	// The ticket is the image, and the end of that code's life: 60 s from the
	// first code, and not the end of the QR window, which is for a pairing code.
	requireQRImage(t, tk.QRPNG, "2@secretcode")
	if other, _ := qr.PNG("2@othercode"); bytes.Equal(tk.QRPNG, other) {
		t.Error("the image does not depend on the code")
	}
	if tk.LoginURL != "" || tk.PairCode != "" || tk.Reconnecting {
		t.Errorf("ticket %+v: more than an image", tk)
	}
	if qrFirst != 60*time.Second || tk.ExpiresAt.Before(before.Add(qrFirst)) || tk.ExpiresAt.After(after.Add(qrFirst)) {
		t.Errorf("the code expires at %v, want %v after the first code, between %v and %v", tk.ExpiresAt, qrFirst, before, after)
	}
	if st := s.status(); !tk.ExpiresAt.Equal(st.Expires) || st.Code != "2@secretcode" {
		t.Errorf("session %+v does not match the ticket", st)
	}

	// The pairing is its own, which no page can reach: the link of the add
	// before ended with it.
	if s.kind != kindChat || !noNonces(m) {
		t.Errorf("session kind %v, nonces %v: want a pairing for the chat and no link", s.kind, !noNonces(m))
	}
	if m.ValidLogin("fresh", old) {
		t.Error("the link of the page survived the add with an image")
	}
	if _, err := m.Login(context.Background(), "fresh", old); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Login by the old link = %v, want ErrNoLogin", err)
	}
	if got := fn.called(true); !slices.Equal(got, []string{"qr pairing", "connect pairing"}) || len(phoneCalls(fn)) != 0 {
		t.Errorf("calls %v: want the QR channel and the connect, and no pairing code", got)
	}

	// Meanwhile list shows linking, and nothing is recorded.
	if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking || got.Reason != "" || got.Phone != "" {
		t.Errorf("account %+v, want linking", got)
	}
	if got := accountJID(t, f, "fresh"); got != "" {
		t.Errorf("accounts.jid = %q before the phone scanned", got)
	}

	// The next codes rotate unseen, and the phone scans the one in the chat.
	q.ch <- code("2@two", 20*time.Second)
	eventually(t, "the next code", func() bool { return s.status().Code == "2@two" })
	jid := types.NewADJID("70000000005", 0, 13)
	if !scan(t, q, jid) {
		t.Fatal("PrePairCallback refused a new account")
	}
	eventually(t, "paired", stateIs(s, pairPaired))
	if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking {
		t.Errorf("account %+v after the scan, before the new device connects", got)
	}
	q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
	<-s.done
	if got := accountInfo(t, m, "fresh"); got.Status != StatusConnected || got.Reason != "" || got.Phone != "+70000000005" {
		t.Errorf("account %+v, want connected", got)
	}
	if got := accountJID(t, f, "fresh"); got != jid.String() {
		t.Errorf("accounts.jid = %q, want %q", got, jid)
	}

	// No QR code is logged (plan 10).
	if log := buf.String(); strings.Contains(log, "secretcode") || !strings.Contains(log, "QR code issued") {
		t.Errorf("log:\n%s", log)
	}
}

// TestLinkQROutcomes: add with qr_image applies the same policy as the other
// ways (plan 6.4). A reconnect ignores qr_image, and a refusal of the policy is
// told as it is.
func TestLinkQROutcomes(t *testing.T) {
	for _, tc := range linkCases() {
		t.Run(tc.name, func(t *testing.T) {
			m, fn, _ := linkFixture(t)
			fn.firstCode = "2@one"
			tc.prep(t, m)
			before := fn.called(true)
			r := linkQR(t, m, "personal")
			tk, err := r.tk, r.err

			switch tc.want {
			case wantLink:
				if err != nil || tk.LoginURL != "" || tk.PairCode != "" || tk.Reconnecting || tk.ExpiresAt.IsZero() {
					t.Fatalf("Link = %+v, %v; want an image", tk, err)
				}
				requireQRImage(t, tk.QRPNG, "2@one")
				if got := infoOf(m, "personal"); got.Status != StatusLinking {
					t.Errorf("status %s, want linking", got.Status)
				}
				if s := sessOf(m, "personal"); s.kind != kindChat || qrCount(fn) != 1 {
					t.Errorf("want one pairing for the chat, got kind %v and %d QR channels", s.kind, qrCount(fn))
				}
			case wantReconnect:
				if err != nil || !tk.Reconnecting || tk.LoginURL != "" || tk.PairCode != "" || tk.QRPNG != nil {
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
				if err == nil || !strings.Contains(err.Error(), tc.reason) || !noTicket(tk) {
					t.Fatalf("Link = %+v, %v; want a refusal with %q", tk, err, tc.reason)
				}
			}
			if tc.want != wantLink && (qrCount(fn) != 0 || !noNonces(m)) {
				t.Errorf("a pairing or a link came out of %s: %v", tc.want, fn.called(true))
			}
			if tc.want == wantRefuse && !slices.Equal(fn.called(true), before) {
				t.Errorf("a refusal started something: %v", fn.called(true))
			}
		})
	}
}

// TestLinkQRRefusals: what add with qr_image cannot do is refused before any
// account, session or connection exists, and changes nothing: no new account, a
// pairing that is live goes on and so does its link.
func TestLinkQRRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("nothing started", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		for _, nick := range []string{"", "Bad Nick", "../x", strings.Repeat("a", 65)} {
			// Bounded: a call that got through would start a pairing and wait for it.
			if r := result(t, linkReqAsync(ctx, m, LinkRequest{Nick: nick, QRImage: true})); r.err == nil || !noTicket(r.tk) {
				t.Errorf("Link(%q) with qr_image accepted the nick", nick)
			}
		}
		// Both ways to link at once, with a valid and with an invalid number.
		for _, phone := range []string{typedPhone, "abc"} {
			r := result(t, linkReqAsync(ctx, m, LinkRequest{Nick: "fresh", Phone: phone, QRImage: true}))
			if !errors.Is(r.err, ErrQRImageWithPhone) || r.err.Error() != "qr_image and phone are mutually exclusive" || !noTicket(r.tk) {
				t.Errorf("Link with qr_image and the phone %q = %+v, %v; want the refusal that they exclude each other", phone, r.tk, r.err)
			}
		}
		if accs, _ := f.db.Accounts(ctx); len(accs) != 0 || !noNonces(m) || len(fn.called(true)) != 0 {
			t.Errorf("a refused Link left something behind: %v, %v", accs, fn.called(true))
		}
		m.Close()
		if _, err := m.Link(ctx, LinkRequest{Nick: "fresh", QRImage: true}); !errors.Is(err, errClosing) {
			t.Errorf("Link with qr_image after Close: %v", err)
		}
	})
	t.Run("with a pairing open", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		nonce := linkNonce(t, m, "fresh")
		if _, err := m.Login(ctx, "fresh", nonce); err != nil { // a QR page shows codes
			t.Fatal(err)
		}
		s := sessOf(m, "fresh")
		eventually(t, "the page's pairing", func() bool { return len(fn.called(true)) == 2 }) // qr, connect
		before := fn.called(true)

		r := result(t, linkReqAsync(ctx, m, LinkRequest{Nick: "fresh", Phone: typedPhone, QRImage: true}))
		if !errors.Is(r.err, ErrQRImageWithPhone) || !noTicket(r.tk) {
			t.Fatalf("Link = %+v, %v; want the refusal", r.tk, r.err)
		}
		if got := fn.called(true); !slices.Equal(got, before) || isDone(s) || sessOf(m, "fresh") != s || fn.qr(t, 0).ctx.Err() != nil {
			t.Errorf("a refused add touched the pairing that was open: %v", got)
		}
		if !m.ValidLogin("fresh", nonce) {
			t.Error("a refused add ended the link")
		}
	})
}

// TestLinkQRNoCode: no first QR code in time, as offline or behind a captive
// portal, ends the pairing and the call, and the account is left as it was,
// needs_link with the reason, the same for the chat as for a pairing code.
func TestLinkQRNoCode(t *testing.T) {
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
			res := linkQRAsync(context.Background(), m, "fresh")
			if len(tc.items) > 0 {
				eventually(t, "the pairing", func() bool { return qrCount(fn) == 1 })
				for _, it := range tc.items {
					fn.qr(t, 0).ch <- it
				}
			}
			start := time.Now()
			r := result(t, res)
			if r.err == nil || r.err.Error() != tc.reason || !noTicket(r.tk) {
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
			if tc.slow {
				if q := fn.qr(t, 0); q.ctx.Err() == nil {
					t.Error("the QR channel's context is still alive")
				}
			}
			requireNothingLeft(t, f, m, fn, "fresh", s)
		})
	}
}

// TestLinkQRBarrier: the wait for the client globals is not part of the 10 s
// for the first code, but the call is bound by its ctx and by Close, in either
// wait.
func TestLinkQRBarrier(t *testing.T) {
	t.Run("not counted", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		g := heldGlobals()
		m := f.start(t, fn.network(g))
		m.codeWait = 30 * time.Millisecond
		res := linkQRAsync(context.Background(), m, "fresh")
		time.Sleep(150 * time.Millisecond) // five times codeWait
		select {
		case r := <-res:
			t.Fatalf("Link returned while the globals were not final: %+v, %v", r.tk, r.err)
		default:
		}
		close(g.ready)
		r := result(t, res)
		if r.err != nil {
			t.Fatal(r.err)
		}
		requireQRImage(t, r.tk.QRPNG, "2@one")
	})
	t.Run("ctx", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(heldGlobals()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res := linkQRAsync(ctx, m, "fresh")
		s := awaitSession(t, m, "fresh")
		cancel()
		if r := result(t, res); !errors.Is(r.err, context.Canceled) || !noTicket(r.tk) {
			t.Fatalf("Link = %+v, %v; want its ctx's error", r.tk, r.err)
		}
		eventually(t, "the session's end with no connect", func() bool { return isDone(s) })
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
		res := linkQRAsync(ctx, m, "fresh")
		eventually(t, "the connect", func() bool { return countCalls(fn, "connect pairing") == 1 })
		cancel()
		if r := result(t, res); !errors.Is(r.err, context.Canceled) || !noTicket(r.tk) {
			t.Fatalf("Link = %+v, %v; want its ctx's error", r.tk, r.err)
		}
		s := sessOf(m, "fresh")
		eventually(t, "the session's end", func() bool { return isDone(s) })
		if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != cancelledReason {
			t.Errorf("account %+v", got)
		}
		requireNothingLeft(t, f, m, fn, "fresh", s)
	})
}

// TestLinkQRClose: Close while add waits for the first code ends the call and
// the session.
func TestLinkQRClose(t *testing.T) {
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
			res := linkQRAsync(context.Background(), m, "fresh")
			eventually(t, "the pairing", func() bool { return len(fn.called(true)) == len(tc.calls) })
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			if r := result(t, res); !errors.Is(r.err, errClosing) || !noTicket(r.tk) {
				t.Errorf("Link = %+v, %v; want the closing error", r.tk, r.err)
			}
			if s := sessOf(m, "fresh"); !isDone(s) {
				t.Error("Close left the pairing running")
			}
			if got := fn.called(true); !slices.Equal(got, append(tc.calls, "disconnect pairing")) {
				t.Errorf("calls %v: want the pairing client disconnected after Close", got)
			}
		})
	}
}

// TestLinkQRSilence: the QR channel's silence, which a keepalive failure
// brings, ends the pairing in the words of a QR that nobody scanned, and not in
// those of a pairing code.
func TestLinkQRSilence(t *testing.T) {
	const silence = 20 * time.Millisecond
	t.Run("before the first code", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{}
		m := f.start(t, fn.network(readyGlobals()))
		m.qrSilence = silence // codeWait is longer: it is the channel that ends this
		r := linkQR(t, m, "fresh")
		if r.err == nil || r.err.Error() != couldNotReach || !noTicket(r.tk) {
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
		res := linkQRAsync(context.Background(), m, "fresh")
		eventually(t, "the pairing", func() bool { return qrCount(fn) == 1 })
		fn.qr(t, 0).ch <- code("2@one", 10*time.Millisecond) // the only one, and then nothing
		if r := result(t, res); r.err != nil {
			t.Fatal(r.err)
		}
		s := sessOf(m, "fresh")
		eventually(t, "the silence to end the pairing", func() bool { return isDone(s) })
		const expired = "QR expired, call add again"
		if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != expired {
			t.Errorf("account %+v, want needs_link: %s", got, expired)
		}
	})
}

// TestLinkQRWindow: the ticket ends with the code in the image, which the first
// code's timeout tells: 60 s for the six refs, 20 s for fewer (qrchan.go:92-96).
// The QR window of the pairing is longer, and is not what the user can scan.
func TestLinkQRWindow(t *testing.T) {
	for _, first := range []time.Duration{60 * time.Second, 20 * time.Second} {
		t.Run(first.String(), func(t *testing.T) {
			f := newFixture(t)
			fn := &fakeNet{}
			m := f.start(t, fn.network(readyGlobals()))
			before := time.Now()
			res := linkQRAsync(context.Background(), m, "fresh")
			eventually(t, "the pairing", func() bool { return qrCount(fn) == 1 })
			fn.qr(t, 0).ch <- code("2@one", first)
			r := result(t, res)
			after := time.Now()
			if r.err != nil || r.tk.ExpiresAt.Before(before.Add(first)) || r.tk.ExpiresAt.After(after.Add(first)) {
				t.Fatalf("Link = %+v, %v; want the code to expire %v after the first code, between %v and %v", r.tk, r.err, first, before, after)
			}
			if window := sessOf(m, "fresh").window; first == qrFirst && !r.tk.ExpiresAt.Before(window) {
				t.Errorf("the ticket ends at %v, not before the window %v", r.tk.ExpiresAt, window)
			}
			requireQRImage(t, r.tk.QRPNG, "2@one")
		})
	}
}

// TestLinkQRDrawFails: a code that cannot be drawn ends the pairing, as one
// that nobody sees cannot be scanned, with a reason that holds no code.
func TestLinkQRDrawFails(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	f.log = debugLog(&buf)
	fn := &fakeNet{firstCode: strings.Repeat("secretcode", 500)} // more than a QR code holds
	m := f.start(t, fn.network(readyGlobals()))

	r := linkQR(t, m, "fresh")
	if r.err == nil || r.err.Error() != drawFailed || !noTicket(r.tk) {
		t.Fatalf("Link = %+v, %v; want the error %q", r.tk, r.err, drawFailed)
	}
	s := sessOf(m, "fresh")
	if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != drawFailed {
		t.Errorf("account %+v, want needs_link: %s", got, drawFailed)
	}
	requireNothingLeft(t, f, m, fn, "fresh", s)
	if log := buf.String(); strings.Contains(log, "secretcode") || !strings.Contains(log, "draw the QR code") {
		t.Errorf("log:\n%s", log)
	}
}

// TestIssueQRWithoutACode: a pairing that is in no state to show a code when
// the call has waited for it gives no image, and ends.
func TestIssueQRWithoutACode(t *testing.T) {
	f := newFixture(t)
	m := f.start(t, (&fakeNet{}).network(readyGlobals()))
	m.abortWait = 10 * time.Millisecond // the session is not running, so it does not end
	m.mu.Lock()
	a := newAccount("fresh")
	m.accounts["fresh"] = a
	m.mu.Unlock()
	s, err := m.newPairing(a, kindChat)
	if err != nil {
		t.Fatal(err)
	}
	close(s.connecting)
	s.update(func(p *pairStatus) { p.State = pairPaired }) // taken in the moment between the code and the answer
	s.markCoded()

	tk, err := m.issueQR(context.Background(), "fresh", s)
	if err == nil || err.Error() != cancelledReason || !noTicket(tk) {
		t.Errorf("issueQR = %+v, %v; want no image, and %q", tk, err, cancelledReason)
	}
}

// TestQRTicketAfterEnd: a pairing that ends while the call has its code in hand,
// by Close or by the stop of a newer add, gets no image though its state still
// shows the code: the end reaches the state only through the session's own
// goroutine, which is later.
func TestQRTicketAfterEnd(t *testing.T) {
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
			m := f.start(t, (&fakeNet{}).network(readyGlobals()))
			m.abortWait = 10 * time.Millisecond // the session is not running, so it does not end
			m.mu.Lock()
			a := newAccount("fresh")
			m.accounts["fresh"] = a
			m.mu.Unlock()
			s, err := m.newPairing(a, kindChat)
			if err != nil {
				t.Fatal(err)
			}
			s.update(func(p *pairStatus) { p.State, p.Code, p.Expires = pairCode, "2@one", time.Now().Add(qrFirst) })
			tc.end(m, s)
			if st := s.status(); st.State != pairCode {
				t.Fatalf("session %+v: want the code still shown", st)
			}

			tk, err := m.qrTicket(context.Background(), "fresh", s)
			if err == nil || err.Error() != tc.want.Error() || !noTicket(tk) {
				t.Errorf("qrTicket = %+v, %v; want no image, and %q", tk, err, tc.want)
			}
		})
	}
}

// TestLinkQRHungConnect: a connect that hangs, as in the noise handshake with a
// network that takes no answers, ignores the cancel of the pairing; the call
// still answers when the code does not come, as for a pairing code, and not
// only when the connect has returned. The account is linking until then, and
// leaves it with the reason the caller was told.
func TestLinkQRHungConnect(t *testing.T) {
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

	res := linkQRAsync(context.Background(), m, "fresh")
	<-entered
	r := result(t, res)
	if r.err == nil || r.err.Error() != couldNotReach || !noTicket(r.tk) {
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

// TestLinkQRRestarts: an add with qr_image replaces the pairing of the add
// before, whichever kind, and is replaced in its turn.
func TestLinkQRRestarts(t *testing.T) {
	ctx := context.Background()
	t.Run("a QR page by the chat", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		old := linkNonce(t, m, "fresh")
		if _, err := m.Login(ctx, "fresh", old); err != nil {
			t.Fatal(err)
		}
		s1, q1 := sessOf(m, "fresh"), fn.qr(t, 0)
		eventually(t, "the code", stateIs(s1, pairCode))

		fn.firstCode = "2@two"
		r := linkQR(t, m, "fresh")
		if r.err != nil {
			t.Fatal(r.err)
		}
		requireQRImage(t, r.tk.QRPNG, "2@two") // the new pairing's code, not the page's
		if !isDone(s1) || q1.ctx.Err() == nil {
			t.Error("the QR page's pairing was not cancelled")
		}
		if got := s1.status(); got.State != pairFailed || got.Reason != cancelledReason {
			t.Errorf("old session %+v", got)
		}
		if m.ValidLogin("fresh", old) {
			t.Error("the page's link survived the add with an image")
		}
		if _, err := m.Login(ctx, "fresh", old); !errors.Is(err, ErrNoLogin) {
			t.Errorf("Login by the old link = %v, want ErrNoLogin", err)
		}
		if s2 := sessOf(m, "fresh"); s2 == s1 || s2.kind != kindChat || isDone(s2) || qrCount(fn) != 2 {
			t.Errorf("the account does not follow a new pairing for the chat: %d QR channels", qrCount(fn))
		}
		if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking {
			t.Errorf("account %+v", got)
		}
	})
	t.Run("the chat by the chat", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if r := linkQR(t, m, "fresh"); r.err != nil {
			t.Fatal(r.err)
		}
		s1, q1 := sessOf(m, "fresh"), fn.qr(t, 0)

		fn.firstCode = "2@two"
		r := linkQR(t, m, "fresh")
		if r.err != nil {
			t.Fatal(r.err)
		}
		requireQRImage(t, r.tk.QRPNG, "2@two")
		if !isDone(s1) || q1.ctx.Err() == nil {
			t.Error("the first pairing was not cancelled")
		}
		if got := s1.status(); got.State != pairFailed || got.Reason != cancelledReason {
			t.Errorf("old session %+v", got)
		}
		if s2 := sessOf(m, "fresh"); s2 == s1 || s2.kind != kindChat || isDone(s2) || qrCount(fn) != 2 {
			t.Errorf("the second image is not of a new pairing: %d QR channels", qrCount(fn))
		}
		if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking {
			t.Errorf("account %+v", got)
		}
	})
	t.Run("the chat by a code", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if r := linkQR(t, m, "fresh"); r.err != nil {
			t.Fatal(r.err)
		}
		s1 := sessOf(m, "fresh")
		if r := link(t, m, "fresh", typedPhone); r.err != nil || r.tk.PairCode != pairCodeOK {
			t.Fatalf("Link with a phone number = %+v, %v", r.tk, r.err)
		}
		if s2 := sessOf(m, "fresh"); !isDone(s1) || s2 == s1 || s2.kind != kindPhone || isDone(s2) {
			t.Error("the pairing for the chat was not replaced by one for a code")
		}
	})
	t.Run("a code by the chat", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if r := link(t, m, "fresh", typedPhone); r.err != nil {
			t.Fatal(r.err)
		}
		s1 := sessOf(m, "fresh")
		if r := linkQR(t, m, "fresh"); r.err != nil || len(r.tk.QRPNG) == 0 {
			t.Fatalf("Link with qr_image = %+v, %v", r.tk, r.err)
		}
		if s2 := sessOf(m, "fresh"); !isDone(s1) || s2 == s1 || s2.kind != kindChat || isDone(s2) {
			t.Error("the pairing by code was not replaced by one for the chat")
		}
		if got := s1.status(); got.State != pairFailed || got.Reason != cancelledReason {
			t.Errorf("old session %+v", got)
		}
	})
	t.Run("the chat by a link", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if r := linkQR(t, m, "fresh"); r.err != nil {
			t.Fatal(r.err)
		}
		s1 := sessOf(m, "fresh")
		tk, err := m.Link(ctx, LinkRequest{Nick: "fresh"})
		if err != nil {
			t.Fatal(err)
		}
		nonceIn(t, tk, "fresh")
		if !isDone(s1) {
			t.Error("the pairing for the chat was not cancelled")
		}
		if got := infoOf(m, "fresh"); got.Status != StatusNeedsLink || got.Reason != cancelledReason {
			t.Errorf("account %+v", got)
		}
	})
	t.Run("not after the scan", func(t *testing.T) {
		f := newFixture(t)
		fn := &fakeNet{firstCode: "2@one"}
		m := f.start(t, fn.network(readyGlobals()))
		if r := linkQR(t, m, "fresh"); r.err != nil {
			t.Fatal(r.err)
		}
		s, q := sessOf(m, "fresh"), fn.qr(t, 0)
		jid := types.NewADJID("70000000005", 0, 13)
		if !q.cli.PrePairCallback(jid, "android", "") {
			t.Fatal("PrePairCallback refused")
		}
		if err := linkQR(t, m, "fresh").err; err == nil || !strings.Contains(err.Error(), "just linked") {
			t.Errorf("Link with qr_image after the phone scanned: %v", err)
		}
		if q.ctx.Err() != nil || qrCount(fn) != 1 {
			t.Error("an add cancelled a pairing whose device is taken")
		}
		saveDevice(t, q.cli.Store, jid)
		q.ch <- whatsmeow.QRChannelSuccess
		eventually(t, "paired", stateIs(s, pairPaired))
		q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
		<-s.done
	})
}

// TestLinkQROverlap: an add with qr_image while the one before waits for its
// first code, as an agent's retry is, cancels that one's pairing: the first call
// is told so, and only the second gives an image.
func TestLinkQROverlap(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	first := linkQRAsync(context.Background(), m, "fresh")
	eventually(t, "the connect", func() bool { return countCalls(fn, "connect pairing") == 1 })
	s1 := sessOf(m, "fresh")

	second := linkQRAsync(context.Background(), m, "fresh")
	if r := result(t, first); r.err == nil || r.err.Error() != cancelledReason || !noTicket(r.tk) {
		t.Errorf("the first add = %+v, %v; want no image, and %q", r.tk, r.err, cancelledReason)
	}
	eventually(t, "the second pairing", func() bool { return qrCount(fn) == 2 })
	fn.qr(t, 1).ch <- code("2@two", 60*time.Second)
	r := result(t, second)
	if r.err != nil {
		t.Fatal(r.err)
	}
	requireQRImage(t, r.tk.QRPNG, "2@two")
	if s2 := sessOf(m, "fresh"); s2 == s1 || !isDone(s1) || isDone(s2) {
		t.Error("want the second pairing alone alive")
	}
	if got := accountInfo(t, m, "fresh"); got.Status != StatusLinking || got.Reason != "" {
		t.Errorf("account %+v, want linking", got)
	}
}

// TestLoginLeavesAChatPairingAlone: a pairing whose QR code is an image in the
// chat is never taken over by a login page. Its link ended with the add that
// began it; and a poll that was already under way then is refused too.
func TestLoginLeavesAChatPairingAlone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	old := linkNonce(t, m, "fresh")
	if r := linkQR(t, m, "fresh"); r.err != nil {
		t.Fatal(r.err)
	}
	s, q := sessOf(m, "fresh"), fn.qr(t, 0)
	calls := fn.called(true)

	untouched := func(when string) {
		t.Helper()
		if isDone(s) || q.ctx.Err() != nil || sessOf(m, "fresh") != s || qrCount(fn) != 1 || !slices.Equal(fn.called(true), calls) {
			t.Errorf("%s: the pairing for the chat was touched: %v", when, fn.called(true))
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
	if s2, err := m.add(ctx, "fresh"); s2 != nil || !errors.Is(err, errNotPageSession) {
		t.Errorf("add as the page = %v, %v; want it refused", s2, err)
	}
	untouched("page start")

	// A poll with a nonce that works though no add issued it after the image
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

// TestLinkQRPairingEnds: the QR channel's automaton serves a pairing for the
// chat as it does one for a page, and the account's status, as list shows it,
// follows: linking, then connected, or needs_link with the reason.
func TestLinkQRPairingEnds(t *testing.T) {
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
		name   string
		end    func(*testing.T, fakeQR)
		status Status
		reason string
	}{
		{name: "the phone scans the QR", end: scanned(mine), status: StatusConnected},
		{name: "the codes run out", end: items(whatsmeow.QRChannelTimeout),
			status: StatusNeedsLink, reason: "QR expired, call add again"},
		{name: "pair error", end: items(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("boom")}),
			status: StatusNeedsLink, reason: "linking failed: boom; call add again"},
		{name: "another number scanned", end: scanned("70000000002"),
			status: StatusNeedsLink, reason: differentNumberReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.account(t, "personal", mine)
			fn := &fakeNet{firstCode: "2@one"}
			m := f.start(t, fn.network(readyGlobals()))
			m.wg.Wait()
			forceStatus(m, "personal", StatusNeedsLink)
			jid := accountJID(t, f, "personal")

			r := linkQR(t, m, "personal")
			if r.err != nil || len(r.tk.QRPNG) == 0 {
				t.Fatalf("Link = %+v, %v", r.tk, r.err)
			}
			s, q := sessOf(m, "personal"), fn.qr(t, 0)
			if got := accountInfo(t, m, "personal"); got.Status != StatusLinking || got.Reason != "" {
				t.Fatalf("while the image is out: %+v, want linking", got)
			}
			tc.end(t, q)

			if tc.status == StatusConnected {
				eventually(t, "paired", stateIs(s, pairPaired))
				q.cli.DangerousInternals().DispatchEvent(&events.Connected{})
				<-s.done
				if got := accountInfo(t, m, "personal"); got.Status != StatusConnected || got.Reason != "" || got.Phone != "+"+mine {
					t.Errorf("account %+v, want connected", got)
				}
				return
			}
			<-s.done
			if got := accountInfo(t, m, "personal"); got.Status != tc.status || got.Reason != tc.reason || !got.ExpiresAt.IsZero() {
				t.Errorf("account %+v, want %s: %s", got, tc.status, tc.reason)
			}
			if got := accountJID(t, f, "personal"); got != jid {
				t.Errorf("accounts.jid = %q, want the old %q", got, jid)
			}
			if got := numberOf(m, "personal"); got != mine {
				t.Errorf("number %q, want %q", got, mine)
			}
		})
	}
}

// TestLinkQRConcurrent: adds with qr_image that overlap leave one pairing alive
// and no call hanging.
func TestLinkQRConcurrent(t *testing.T) {
	f := newFixture(t)
	fn := &fakeNet{firstCode: "2@one"}
	m := f.start(t, fn.network(readyGlobals()))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			// A call is answered with an image, or told that a newer add cancelled it.
			r := linkQR(t, m, "fresh")
			if r.err != nil && r.err.Error() != cancelledReason {
				t.Errorf("Link = %+v, %v", r.tk, r.err)
			}
		})
	}
	wg.Wait()
	alive := 0
	for i := range qrCount(fn) {
		if fn.qr(t, i).ctx.Err() == nil {
			alive++
		}
	}
	if alive != 1 {
		t.Errorf("%d pairings alive, want 1", alive)
	}
}
