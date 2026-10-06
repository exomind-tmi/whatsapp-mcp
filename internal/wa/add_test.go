package wa

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

func TestAddPolicy(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	st := func(s Status) AccountInfo { return AccountInfo{Nick: "personal", Status: s} }
	ban := func(expires time.Time) AccountInfo {
		return AccountInfo{Nick: "personal", Status: StatusError, Reason: "temporarily banned: 101", ExpiresAt: expires}
	}

	for _, tc := range []struct {
		name    string
		info    AccountInfo
		device  bool
		pairing pairingPhase
		want    addAction
		reason  string // a part of the refusal
	}{
		{"new account", st(StatusNeedsLink), false, noPairing, addPair, ""},
		{"needs link", st(StatusNeedsLink), true, noPairing, addPair, ""},
		{"connected", st(StatusConnected), true, noPairing, addRefuse, "already linked and connected"},
		{"reconnecting", st(StatusReconnecting), true, noPairing, addRefuse, "reconnects by itself"},
		{"pairing shows codes", st(StatusLinking), false, pairingOpen, addRestart, ""},
		{"pairing scanned", st(StatusLinking), false, pairingBound, addRefuse, "was just linked and is connecting"},
		{"pairing scanned, connected", st(StatusConnected), true, pairingBound, addRefuse, "was just linked and is connecting"},
		{"linking, its pairing ended", st(StatusLinking), false, noPairing, addPair, ""},
		{"replaced", st(StatusReplaced), true, noPairing, addReconnect, ""},
		{"replaced with no device", st(StatusReplaced), false, noPairing, addPair, ""},
		{"error", AccountInfo{Nick: "personal", Status: StatusError, Reason: "connect failure: 400: unknown error"}, true, noPairing, addReconnect, ""},
		{"error with no device", st(StatusError), false, noPairing, addPair, ""},
		{"banned", ban(now.Add(time.Hour)), true, noPairing, addRefuse, "banned by WhatsApp until " + now.Add(time.Hour).Local().Format(time.RFC3339) + " (temporarily banned: 101)"},
		{"banned with no device", ban(now.Add(time.Hour)), false, noPairing, addRefuse, "banned by WhatsApp"},
		{"ban ends now", ban(now), true, noPairing, addReconnect, ""},
		{"ban over", ban(now.Add(-time.Second)), true, noPairing, addReconnect, ""},
		{"ban with no end", ban(time.Time{}), true, noPairing, addReconnect, ""},
		{"client outdated", st(StatusClientOutdated), true, noPairing, addRefuse, "update whatsapp-mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := addPolicy(tc.info, tc.device, tc.pairing, now)
			if got.action != tc.want || (tc.want == addRefuse) != (got.reason != "") || !strings.Contains(got.reason, tc.reason) {
				t.Errorf("addPolicy = %+v, want action %d with %q", got, tc.want, tc.reason)
			}
			if tc.want == addRefuse && !strings.Contains(got.reason, `"personal"`) {
				t.Errorf("refusal %q does not name the account", got.reason)
			}
		})
	}
}

// TestAdd applies the policy to live accounts: refusals leave them alone,
// a reconnect connects the device they have, a pairing starts for the
// others.
func TestAdd(t *testing.T) {
	f := newFixture(t)
	f.account(t, "personal", "70000000001")
	f.account(t, "fresh", "")
	f.devices(t, map[string]string{"70000000001": ""})
	fn := &fakeNet{}
	m := f.start(t, fn.network(readyGlobals()))
	m.wg.Wait()
	ctx := context.Background()
	dispatch := m.accounts["personal"].cli.DangerousInternals().DispatchEvent
	connects := func() int {
		return len(slices.DeleteFunc(fn.called(false), func(c string) bool { return c != "connect 70000000001" }))
	}

	if s, err := m.add(ctx, "personal"); s != nil || err == nil || !strings.Contains(err.Error(), "reconnects by itself") {
		t.Errorf("add while reconnecting = %v, %v; want a refusal", s, err)
	}
	for i, nick := range []string{"fresh", "second"} {
		if nick == "second" { // the add tool makes the account, and the login page comes after
			if _, err := m.Link(ctx, LinkRequest{Nick: nick}); err != nil {
				t.Fatal(err)
			}
		}
		s, err := m.add(ctx, nick)
		if s == nil || err != nil {
			t.Fatalf("add(%s) = %v, %v; want a pairing", nick, s, err)
		}
		if got := infoOf(m, nick); got.Status != StatusLinking {
			t.Errorf("%s: status %s, want linking", nick, got.Status)
		}
		accountJID(t, f, nick) // in archive.db
		fn.qr(t, i).ch <- whatsmeow.QRChannelTimeout
		<-s.done
	}
	// The login page makes no account: a poll that finds none, as one that a remove
	// has overtaken does, is answered ErrNoLogin and makes neither the row nor the
	// account, nor a pairing.
	if s, err := m.add(ctx, "nobody"); s != nil || !errors.Is(err, ErrNoLogin) || hasAccount(m, "nobody") || qrCount(fn) != 2 {
		t.Errorf("add(nobody) = %v, %v; want ErrNoLogin and no account", s, err)
	}
	if _, there := archiveOf(t, f)["nobody"]; there {
		t.Error("the login page made a row in archive.db")
	}

	dispatch(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour})
	if s, err := m.add(ctx, "personal"); s != nil || err == nil || !strings.Contains(err.Error(), "banned") {
		t.Errorf("add while banned = %v, %v; want a refusal", s, err)
	}
	m.mu.Lock()
	m.accounts["personal"].info.ExpiresAt = time.Now().Add(-time.Second) // the ban is over
	m.mu.Unlock()
	if s, err := m.add(ctx, "personal"); s != nil || err != nil || infoOf(m, "personal").Status != StatusReconnecting {
		t.Errorf("add after the ban = %v, %v, status %s; want a reconnect", s, err, infoOf(m, "personal").Status)
	}
	m.wg.Wait()
	dispatch(&events.Connected{})

	dispatch(&events.StreamReplaced{})
	if s, err := m.add(ctx, "personal"); s != nil || err != nil {
		t.Errorf("add when replaced = %v, %v; want a reconnect", s, err)
	}
	if got := infoOf(m, "personal"); got.Status != StatusReconnecting || got.Reason != "" {
		t.Errorf("status after the reconnect began: %+v", got)
	}
	m.wg.Wait()
	dispatch(&events.Connected{}) // moves only because add set reconnecting first
	if got := infoOf(m, "personal").Status; got != StatusConnected {
		t.Errorf("status after Connected: %s", got)
	}
	if n := connects(); n != 3 {
		t.Errorf("%d connects, want 3: %v", n, fn.called(true))
	}

	// After Close nothing starts.
	m.Close()
	m.mu.Lock()
	m.accounts["personal"].info.Status = StatusReplaced
	m.mu.Unlock()
	for _, nick := range []string{"personal", "fresh", "later"} {
		if _, err := m.add(ctx, nick); !errors.Is(err, errClosing) {
			t.Errorf("add(%s) after Close: %v", nick, err)
		}
	}
	if _, err := m.Link(ctx, LinkRequest{Nick: "later"}); !errors.Is(err, errClosing) { // the add tool makes accounts
		t.Errorf("Link(later) after Close: %v", err)
	}
	m.wg.Wait()
	if _, there := archiveOf(t, f)["later"]; there || hasAccount(m, "later") {
		t.Error("an add after Close made an account")
	}
	if n := connects(); n != 3 || strings.Count(strings.Join(fn.called(false), "\n"), "qr pairing") != 2 {
		t.Errorf("a connect or a pairing started after Close: %v", fn.called(true))
	}
}
