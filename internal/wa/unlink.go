package wa

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow"
)

// logoutWait bounds the Logout of a device that is taken away: a relinked
// account's old one, a removed account's.
const logoutWait = 10 * time.Second

// deviceGone tells whether cli's device is deleted from store.db, or on its way:
// whatsmeow's Delete drops the ID before it sets Deleted (store/store.go:286-298),
// and a client with no ID is on its way there. It reads fields that whatsmeow's own
// goroutines write, on LoggedOut and on a stream error that removes the device
// (connectionevents.go:40-47), without a lock to take; the worst a stale read does
// is send the caller to a Delete that whatsmeow's has just done, which the callers
// check for (unlinkClient).
func deviceGone(cli *whatsmeow.Client) bool { return cli.Store.Deleted || cli.Store.ID == nil }

// unlinkClient takes cli's device away, the one way for a relink's old device
// and for a removed account's: Logout, which unlinks it on the phone too. Logout
// needs a live connection and fails at once on a client with no socket
// (client.go:962-963) or no device ID. If its request to the server fails it
// neither disconnects nor deletes (client.go:723-761); if the request goes through
// and the deletion in the store then fails, the phone has unlinked the device and
// the client is disconnected, which this treats like the first failure, so the
// hint that follows may be one too many. A device Logout cannot unlink goes by
// Disconnect and Store.Delete, which also stops its use (store/store.go:286-298),
// and the phone may still list it: listed. A connect that came up in between, from
// the retries of m.connect, which Disconnect does not stop, is closed by the second
// Disconnect: from the Delete on, every connect fails. On an error the keys are
// still in store.db, and the client is disconnected.
//
// It may hang on the socket lock that a connect in its handshake holds (see
// awaitEnd), so a call that must answer runs it on a goroutine of its own.
func (m *Manager) unlinkClient(nick string, cli *whatsmeow.Client) (listed bool, err error) {
	ctx, cancel := context.WithTimeout(m.ctx, m.logoutWait)
	defer cancel()
	lerr := m.net.logout(cli, ctx)
	if lerr == nil {
		m.log.Info("device unlinked", "account", nick)
		return false, nil
	}
	m.net.disconnect(cli)
	// Read after Disconnect, which waits up to 5 s for the client's handler
	// queue (client.go:713-720): a LoggedOut handled later than that may still be
	// deleting the device.
	if deviceGone(cli) {
		return false, nil // on LoggedOut: the phone dropped it first
	}
	m.log.Warn("unlink the device", "account", nick, "err", lerr)
	if err := cli.Store.Delete(m.ctx); err != nil {
		// whatsmeow's own Delete of a device the server has removed (a stream error,
		// connectionevents.go:40-47) may be running beside ours, and ours then fails
		// on the ID it has just dropped: the device is gone, and so is the question.
		if deviceGone(cli) {
			return false, nil
		}
		return true, err
	}
	m.net.disconnect(cli)
	return true, nil
}
