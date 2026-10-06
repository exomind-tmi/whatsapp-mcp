package wa

import (
	"context"
	"errors"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

var errOffline = errors.New("offline: this Manager never reaches WhatsApp")

// offlineNetwork is the network of a Manager with Config.Offline: one that
// reaches nobody, for the tests of the packages above, which run the daemon
// in-process. The version check fails at once, which leaves the version built
// into whatsmeow, and a connect does nothing and returns, so that an account
// with a device stays reconnecting. A pairing, which needs the server for its QR
// code, gets none and ends on its own silence.
func offlineNetwork() network {
	live := liveNetwork
	return network{
		globals:    newGlobals(func(context.Context) (*store.WAVersionContainer, error) { return nil, errOffline }),
		connect:    func(*whatsmeow.Client) error { return nil },
		disconnect: func(*whatsmeow.Client) {},
		logout:     func(*whatsmeow.Client, context.Context) error { return whatsmeow.ErrNotConnected },
		qrChannel: func(*whatsmeow.Client, context.Context) (<-chan whatsmeow.QRChannelItem, error) {
			return make(chan whatsmeow.QRChannelItem), nil
		},
		pairPhone: func(*whatsmeow.Client, context.Context, string, bool, whatsmeow.PairClientType, string) (string, error) {
			return "", errOffline
		},
		retryStep:    live.retryStep,
		deleteDevice: live.deleteDevice, // store.db is ours
	}
}
