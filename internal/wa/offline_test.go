package wa

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestOfflineManagerReachesNobody: a Manager with Config.Offline checks no
// version, connects no client and unlinks nothing on a server, whatever the
// accounts it has; it is the one the tests of the daemon and the shim run, and it
// must still list, link and remove.
func TestOfflineManagerReachesNobody(t *testing.T) {
	keepGlobals(t) // the Manager's globals write whatsmeow's, as the daemon's do
	ctx := context.Background()
	f := newFixture(t)
	f.account(t, "personal", personalPhone)
	f.devices(t, map[string]string{personalPhone: "Anton"})
	var buf bytes.Buffer
	m, err := NewManager(ctx, Config{Log: debugLog(&buf), StorePath: f.storePath(), Archive: f.db, BaseURL: "http://127.0.0.1:1", Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	if m.net.globals == liveNetwork.globals {
		t.Fatal("the Manager uses the process's own client globals, whose version check asks WhatsApp")
	}
	// None of the seams that reach the server is the live one, whose functions
	// dial it. deleteDevice is, rightly: store.db is ours.
	live := liveNetwork
	for name, fns := range map[string][2]any{
		"connect":    {m.net.connect, live.connect},
		"disconnect": {m.net.disconnect, live.disconnect},
		"logout":     {m.net.logout, live.logout},
		"qrChannel":  {m.net.qrChannel, live.qrChannel},
		"pairPhone":  {m.net.pairPhone, live.pairPhone},
	} {
		if reflect.ValueOf(fns[0]).Pointer() == reflect.ValueOf(fns[1]).Pointer() {
			t.Errorf("the offline network's %s is the live one", name)
		}
	}
	if err := m.net.globals.wait(ctx); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait() // the connect of the account has returned
	if out := buf.String(); !strings.Contains(out, "WhatsApp Web version check failed") || !strings.Contains(out, errOffline.Error()) {
		t.Errorf("the version check was not the offline one:\n%s", out)
	}
	if got := infoOf(m, "personal"); got.Status != StatusReconnecting || got.Phone != "+"+personalPhone {
		t.Errorf("account %+v, want reconnecting: nothing connects it", got)
	}

	tk, err := m.Link(ctx, LinkRequest{Nick: "fresh"})
	if err != nil || tk.LoginURL == "" {
		t.Fatalf("Link = %+v, %v; want a login link", tk, err)
	}
	// Logout is not possible without a server, so the device goes by the way of
	// the hint: it is for the phone to forget.
	res, err := m.Remove(ctx, "personal")
	if err != nil || res.Hint != removeDeviceHint {
		t.Errorf("Remove = %+v, %v; want the device deleted here and the hint for the phone", res, err)
	}
	if got := storedDevices(t, m); len(got) != 0 {
		t.Errorf("devices %v, want none", got)
	}
}
