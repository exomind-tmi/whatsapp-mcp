package wa

import (
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// next returns a with the Status, Reason and ExpiresAt that the whatsmeow
// event evt leads to; now dates the end of a ban. whatsmeow
// dispatches events as pointers. Events that say nothing about the
// connection leave a unchanged, and so does StreamError: when the server
// then closes the socket, Disconnected follows (client.go:600-602).
//
// whatsmeow dispatches many events in goroutines of their own, so they may
// arrive out of order, and a late one must not hide a terminal status:
//   - Disconnected and the keepalive events are about a live connection, so
//     they move only connected and reconnecting: during linking the pairing
//     session decides the outcome, and after LoggedOut, StreamReplaced or a
//     failure whatsmeow does not reconnect (connectionevents.go:40-51,
//     108-111).
//   - Connected is sent from the goroutine that ends a connect, which a
//     stream error can cut short while LoggedOut or StreamReplaced is still
//     on its way (connectionevents.go:21, 43, 51, 200-204). So it moves only
//     linking, the live statuses and error, which our own Connect leaves
//     once a ban ends, whatsmeow not reconnecting after one; whoever
//     reconnects a replaced account sets reconnecting first.
func next(a AccountInfo, evt any, now time.Time) AccountInfo {
	live := a.Status == StatusConnected || a.Status == StatusReconnecting
	switch e := evt.(type) {
	case *events.Connected:
		if live || a.Status == StatusLinking || a.Status == StatusError {
			return with(a, StatusConnected, "", time.Time{})
		}
	case *events.KeepAliveRestored:
		if live {
			return with(a, StatusConnected, "", time.Time{})
		}
	case *events.Disconnected, *events.KeepAliveTimeout:
		if live {
			return with(a, StatusReconnecting, "", time.Time{})
		}
	case *events.LoggedOut:
		return with(a, StatusNeedsLink, "device unlinked: "+e.Reason.String(), time.Time{})
	case *events.StreamReplaced:
		return with(a, StatusReplaced, "another client connected with this device's keys", time.Time{})
	case *events.ClientOutdated:
		return with(a, StatusClientOutdated, "WhatsApp rejected this client version; whatsapp-mcp needs an update", time.Time{})
	case *events.TemporaryBan:
		return with(a, StatusError, "temporarily banned: "+e.Code.String(), banEnd(now, e.Expire))
	case *events.ConnectFailure:
		// Never a logout: whatsmeow reports those as LoggedOut and sends
		// ConnectFailure only for the reasons it has no event for
		// (connectionevents.go:126-154).
		return with(a, StatusError, e.PermanentDisconnectDescription(), time.Time{})
	case *events.CATRefreshError:
		return with(a, StatusError, e.PermanentDisconnectDescription(), time.Time{})
	}
	return a
}

func with(a AccountInfo, s Status, reason string, expires time.Time) AccountInfo {
	a.Status, a.Reason, a.ExpiresAt = s, reason, expires
	return a
}

// banEnd is when a ban of length d imposed at now ends; zero when WhatsApp
// did not say.
func banEnd(now time.Time, d time.Duration) time.Time {
	if d <= 0 {
		return time.Time{}
	}
	return now.Add(d)
}
