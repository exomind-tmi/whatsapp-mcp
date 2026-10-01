package wa

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"sync"
	"time"
)

// loginTTL is how long a login link works (plan 5.3). It is a window, not a
// single use: a reload of the page, which carries the nonce, must work.
const loginTTL = 10 * time.Minute

// nonceBytes makes the nonce 128 bits (plan 10).
const nonceBytes = 16

// absentNonce is what a nonce is compared with when the nick has none, so
// that an unknown nick costs the same as a wrong nonce. Its length is that of
// a real one.
var absentNonce = strings.Repeat("-", base64.RawURLEncoding.EncodedLen(nonceBytes))

// pairingGrace is how long a nonce lives on from the moment its page starts
// the pairing: the codes last 160 s, the end takes up to a minute more
// (qrSilence, pairedWait), and the page must still be able to read how it
// ended. Without it a page opened late in the window would die in the middle
// of a scan.
const pairingGrace = 5 * time.Minute

// loginNonce is the capability to open one account's login page. It is
// never logged and never put into an error: whoever has it can scan a QR
// code into the account.
type loginNonce struct {
	value   string
	expires time.Time // the end of the window, as the add tool tells it; never changes
	until   time.Time // when the nonce stops working: expires, or later, see extend; guarded by loginNonces.mu

	// lock is held while the first poll decides and starts the pairing. A
	// channel of one, not a mutex, so that a request can stop waiting for it.
	lock    chan struct{}
	sess    *pairSession // the pairing this nonce's page started; guarded by lock
	settled *LoginState  // the answer when that first call started none: a refusal or a reconnect; guarded by lock
}

// acquire takes lock, or gives up when ctx ends.
func (n *loginNonce) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case n.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *loginNonce) release() { <-n.lock }

// loginNonces holds the nonce of each nick's latest add, in memory only: a
// restarted daemon has a new port, so no link outlives it anyway.
type loginNonces struct {
	now func() time.Time // the clock, replaced by tests

	mu     sync.Mutex
	byNick map[string]*loginNonce
}

func newLoginNonces() *loginNonces {
	return &loginNonces{now: time.Now, byNick: map[string]*loginNonce{}}
}

// issue makes a new nonce for nick; the one before stops working.
func (n *loginNonces) issue(nick string) *loginNonce {
	b := make([]byte, nonceBytes)
	rand.Read(b) // never fails (Go 1.24)
	now := n.now()
	end := now.Add(loginTTL)
	rec := &loginNonce{value: base64.RawURLEncoding.EncodeToString(b), expires: end, until: end, lock: make(chan struct{}, 1)}
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, old := range n.byNick { // the nicks that never came back
		if !now.Before(old.until) {
			delete(n.byNick, k)
		}
	}
	n.byNick[nick] = rec
	return rec
}

// revoke ends the nonce of nick, if any: an add that hands out no link
// replaces the one handed out before just as one that does.
func (n *loginNonces) revoke(nick string) {
	n.mu.Lock()
	delete(n.byNick, nick)
	n.mu.Unlock()
}

// find returns nick's nonce record if got is its nonce and it has not
// expired; nil otherwise, for every reason alike.
func (n *loginNonces) find(nick, got string) *loginNonce {
	n.mu.Lock()
	defer n.mu.Unlock()
	rec := n.byNick[nick]
	want := absentNonce
	if rec != nil {
		want = rec.value
	}
	if !sameNonce(want, got) || rec == nil || !n.now().Before(rec.until) {
		return nil
	}
	return rec
}

// live reports whether rec is still nick's nonce and works: a call that found
// it and then waited for its lock asks again, as an add may have replaced it
// meanwhile.
func (n *loginNonces) live(nick string, rec *loginNonce) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.byNick[nick] == rec && n.now().Before(rec.until)
}

// extend lets rec work until at least until, if it is still nick's.
func (n *loginNonces) extend(nick string, rec *loginNonce, until time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byNick[nick] == rec && until.After(rec.until) {
		rec.until = until
	}
}

// sameNonce compares in constant time, so the time of a refusal does not
// tell how much of a guess was right.
func sameNonce(want, got string) bool {
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
