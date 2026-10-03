package wa

import (
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestNext(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	banned := AccountInfo{Status: StatusError, Reason: "temporarily banned: 101", ExpiresAt: now.Add(time.Hour)}
	st := func(s Status) AccountInfo { return AccountInfo{Status: s} }

	for _, tc := range []struct {
		name string
		cur  AccountInfo
		evt  any
		want AccountInfo
	}{
		{"linked", st(StatusLinking), &events.Connected{}, st(StatusConnected)},
		{"reconnected", st(StatusReconnecting), &events.Connected{}, st(StatusConnected)},
		{"reconnected after a ban", banned, &events.Connected{}, st(StatusConnected)},
		{"late Connected after logout", st(StatusNeedsLink), &events.Connected{}, st(StatusNeedsLink)},
		{"late Connected after replace", st(StatusReplaced), &events.Connected{}, st(StatusReplaced)},
		{"late Connected when outdated", st(StatusClientOutdated), &events.Connected{}, st(StatusClientOutdated)},
		{"pings back", st(StatusReconnecting), &events.KeepAliveRestored{}, st(StatusConnected)},
		{"pings back while linking", st(StatusLinking), &events.KeepAliveRestored{}, st(StatusLinking)},
		{"dropped", st(StatusConnected), &events.Disconnected{}, st(StatusReconnecting)},
		{"pings lost", st(StatusConnected), &events.KeepAliveTimeout{ErrorCount: 1}, st(StatusReconnecting)},
		{"still down", st(StatusReconnecting), &events.Disconnected{}, st(StatusReconnecting)},
		{"dropped while linking", st(StatusLinking), &events.Disconnected{}, st(StatusLinking)},
		{"late drop after logout", st(StatusNeedsLink), &events.Disconnected{}, st(StatusNeedsLink)},
		{"late drop after replace", st(StatusReplaced), &events.KeepAliveTimeout{}, st(StatusReplaced)},
		{"late drop after a ban", banned, &events.Disconnected{}, banned},
		{"unlinked on the phone", st(StatusConnected),
			&events.LoggedOut{OnConnect: false, Reason: events.ConnectFailureLoggedOut},
			AccountInfo{Status: StatusNeedsLink, Reason: "device unlinked: 401: logged out from another device"}},
		{"logged out on connect", st(StatusReconnecting),
			&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureMainDeviceGone},
			AccountInfo{Status: StatusNeedsLink, Reason: "device unlinked: 403: primary device was logged out"}},
		{"replaced", st(StatusConnected), &events.StreamReplaced{},
			AccountInfo{Status: StatusReplaced, Reason: "another client connected with this device's keys"}},
		{"outdated", st(StatusReconnecting), &events.ClientOutdated{},
			AccountInfo{Status: StatusClientOutdated, Reason: "WhatsApp rejected this client version; whatsapp-mcp needs an update"}},
		{"banned", st(StatusReconnecting),
			&events.TemporaryBan{Code: events.TempBanBlockedByUsers, Expire: 2 * time.Hour},
			AccountInfo{Status: StatusError, Reason: "temporarily banned: 102: too many people blocked you", ExpiresAt: now.Add(2 * time.Hour)}},
		{"banned, no end given", st(StatusReconnecting),
			&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople},
			AccountInfo{Status: StatusError, Reason: "temporarily banned: 101: you sent too many messages to people who don't have you in their address books"}},
		{"connect failure", st(StatusReconnecting),
			&events.ConnectFailure{Reason: events.ConnectFailureGeneric, Message: "x"},
			AccountInfo{Status: StatusError, Reason: "connect failure: 400: unknown error"}},
		{"CAT refresh", st(StatusReconnecting), &events.CATRefreshError{Error: errors.New("x")},
			AccountInfo{Status: StatusError, Reason: "CAT refresh failed"}},
		{"stream error", st(StatusConnected), &events.StreamError{Code: "999"}, st(StatusConnected)},
		{"unrelated event", st(StatusConnected), &events.PushNameSetting{}, st(StatusConnected)},
		{"event by value", st(StatusReconnecting), events.Connected{}, st(StatusReconnecting)},
		{"nil", st(StatusConnected), nil, st(StatusConnected)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur, want := tc.cur, tc.want
			cur.Nick, cur.Phone, cur.PushName = "personal", "+70000000000", "Anton"
			want.Nick, want.Phone, want.PushName = cur.Nick, cur.Phone, cur.PushName
			if got := next(cur, tc.evt, now); got != want {
				t.Errorf("next(%+v, %T) =\n%+v, want\n%+v", tc.cur, tc.evt, got, want)
			}
		})
	}
}
