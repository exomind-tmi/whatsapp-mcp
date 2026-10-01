package wa

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// removeDeviceHint is for a remove that could not unlink the device on the phone:
// whatsmeow's Logout was not possible, and the device may still be listed there.
const removeDeviceHint = "remove the device on the phone manually: WhatsApp → Settings → Linked devices"

// removalInterruptedReason is what an account says whose remove has taken its device
// away and has not yet deleted its archive: the call failed, was cancelled or gave up
// waiting. list sends a needs_link account to add, which links it again and keeps the
// archive, and a user who asked to forget the account must be told of the other way
// out. A restart forgets it, and the account is then as if its keys were lost
// (noKeysReason).
const removalInterruptedReason = "the removal was interrupted: the device is unlinked and the message archive is not deleted yet; " +
	"call remove again to finish, or add to link the account again"

// The answers of a remove that has waited abortWait for what it cancelled or found
// going on, and that has erased nothing: the archive and the account are as they were.
const (
	errStillCancelling = refusal("the linking of the account is still being cancelled; repeat the call in a minute")
	errStillRemoving   = refusal("the account is still being removed; repeat the call in a minute")
)

// unlink is the unlinking of an account's devices (runUnlink). Its goroutine may
// outlive the remove that began it, as a connect that hangs holds up the
// client (see awaitEnd), and the remove that follows finds it ended.
type unlink struct {
	done chan struct{} // closed when it has ended
	err  error         // why a device is still there; written before done is closed
}

// Remove forgets the account (Anton's decision of 2026-10-01, plan 6.5): it
// unlinks the account's device, deletes its archive from this computer and drops
// the account, which frees its number for another. Downloaded files stay. The
// steps come in this order, each making the next safe:
//
//  1. The account is taken (beginRemove): a pairing the phone has not scanned is
//     cancelled and awaited, one it has is refused, and from then on add and
//     remove refuse the account.
//  2. The device goes (unlinkDevice). Whatever fails or is still going on here,
//     the archive stays, and so does a device that could not be deleted, with its
//     client connecting again, so a repeated call finds the account as it was.
//  3. The archive goes. The device is gone by now, so a failure leaves the
//     account needs_link with removalInterruptedReason, and a repeated call goes
//     on from here.
//  4. The account is dropped.
//
// Steps 1 and 2 wait for what hangs only abortWait, and answer errStillCancelling
// or errStillRemoving then, having erased nothing; the unlinking goes on by itself,
// and the call repeated finds it done. ctx and Close end the call too.
func (m *Manager) Remove(ctx context.Context, nick string) (RemoveResult, error) {
	if err := ctx.Err(); err != nil {
		return RemoveResult{}, err
	}
	a, err := m.beginRemove(ctx, nick)
	if err != nil {
		return RemoveResult{}, err
	}
	m.log.Info("removing the account", "account", nick)
	hint, err := m.removeTaken(ctx, a)
	if err != nil {
		return RemoveResult{}, err
	}
	m.log.Info("account removed", "account", nick)
	return RemoveResult{Hint: hint}, nil
}

// beginRemove takes the account for a remove (step 1): it is marked removing and
// its login link ends, in one hold of mu. A pairing that shows codes is cancelled
// and awaited first (cancelPairing), as add does; the decision is made again after,
// as the status may have moved.
func (m *Manager) beginRemove(ctx context.Context, nick string) (*account, error) {
	for {
		m.mu.Lock()
		if m.closed || m.ctx.Err() != nil {
			m.mu.Unlock()
			return nil, errClosing
		}
		a := m.accounts[nick]
		if a == nil {
			m.mu.Unlock()
			return nil, refusal(fmt.Sprintf("there is no account %q; see manage-accounts action=list", nick))
		}
		if a.removing {
			m.mu.Unlock()
			return nil, errStillRemoving
		}
		var prev *pairSession
		if a.sess != nil {
			switch a.sess.phase() {
			case pairingBound:
				m.mu.Unlock()
				return nil, refusal(fmt.Sprintf("account %q was just linked and is connecting; check its status with list, then remove it", nick))
			case pairingOpen:
				prev = a.sess
			}
		}
		if prev == nil {
			a.removing = true
			m.nonces.revoke(nick)
			m.mu.Unlock()
			return a, nil
		}
		m.mu.Unlock()
		if err := m.cancelPairing(ctx, nick, prev, errStillCancelling); err != nil {
			return nil, err
		}
	}
}

// removeTaken is steps 2 to 4 for the account a that beginRemove has taken. It
// returns the hint for the user.
func (m *Manager) removeTaken(ctx context.Context, a *account) (string, error) {
	defer func() {
		m.mu.Lock()
		a.removing = false // of the account that is dropped, or of the one the call leaves
		m.mu.Unlock()
	}()
	if err := m.unlinkDevice(ctx, a); err != nil {
		return "", err
	}
	// An account the archive does not have, a repeat after a delete that was
	// committed but not told, is as good as deleted; one that is deleted and may
	// have left copies in the files is deleted all the same.
	err := m.db.DeleteAccount(ctx, a.nick)
	switch {
	case err == nil || errors.Is(err, archive.ErrNoAccount):
	case errors.Is(err, archive.ErrNotScrubbed):
		m.log.Warn("the archive of the removed account may have left copies in archive.db", "account", a.nick, "err", err)
	default:
		m.log.Warn("delete the archive of the removed account", "account", a.nick, "err", err)
		return "", fmt.Errorf("could not delete the archive of account %q, which stays as it was; repeat the call: %w", a.nick, err)
	}
	m.mu.Lock()
	hint := a.removeHint
	m.dropAccount(a.nick)
	m.mu.Unlock()
	return hint, nil
}

// dropAccount forgets the account of nick, which frees its number and its nick. mu
// must be held. admit compares the count to learn that the row it has seen in
// archive.db may be gone.
func (m *Manager) dropAccount(nick string) {
	delete(m.accounts, nick)
	m.removed++
}

// devices are the clients of a that may hold a device in store.db, those the
// account leaves behind first: the latest pairing's client, if the pairing saved a
// device and then failed, so it never became the account's, and the account's own.
func (a *account) devices() []*whatsmeow.Client {
	var out []*whatsmeow.Client
	if p := a.sess; p != nil && p.cli != a.cli && !deviceGone(p.cli) {
		out = append(out, p.cli)
	}
	if a.cli != nil {
		out = append(out, a.cli)
	}
	return out
}

// unlinkDevice is step 2: it takes the devices of a away, on a goroutine of its
// own that it waits for, or finds there already, abortWait at most, and leaves the
// account with none. An account with none is done with at once.
func (m *Manager) unlinkDevice(ctx context.Context, a *account) error {
	m.mu.Lock()
	u := a.unlinking
	if u == nil {
		clients := a.devices()
		if len(clients) == 0 {
			m.mu.Unlock()
			return nil
		}
		if !m.track() {
			m.mu.Unlock()
			return errClosing
		}
		u = &unlink{done: make(chan struct{})}
		a.unlinking = u
		go m.runUnlink(a, u, clients)
	}
	m.mu.Unlock()
	if err := m.awaitEnd(ctx, u.done, errStillRemoving); err != nil {
		return err
	}
	return u.err
}

// runUnlink takes clients' devices away and records the outcome on the account.
// With the account's own device gone, whatever became of the others, it is ready
// for the archive's deletion: no device, needs_link, and the hint for the user kept
// for a remove that has to be repeated. If its device stays, the account is as
// before, its client connecting again: the unlinking has disconnected it.
func (m *Manager) runUnlink(a *account, u *unlink, clients []*whatsmeow.Client) {
	defer m.wg.Done()
	hint, err := m.dropDevices(a.nick, clients)
	m.mu.Lock()
	a.unlinking = nil
	if hint != "" { // of the devices that are gone, though another is not
		a.removeHint = hint
	}
	var again *whatsmeow.Client
	switch ownGone := a.cli != nil && deviceGone(a.cli); {
	case err == nil || ownGone:
		a.cli = nil
		a.info = AccountInfo{Nick: a.nick, Status: StatusNeedsLink, Reason: removalInterruptedReason}
	case a.cli != nil && !m.closed: // a client with a device that is still there; else the account is as it was
		a.info = with(a.info, StatusReconnecting, "", time.Time{})
		again = a.cli
	}
	m.mu.Unlock()
	if again != nil {
		m.connect(a, again)
	}
	u.err = err
	close(u.done)
}

// dropDevices unlinks each client's device (unlinkClient), all of them whatever
// happens to one, and then cleans up store.db (deviceStore.forget), whatever came
// of them: a hint comes back if the phone may still list any of the devices that are
// gone. The clean-up is best effort, as the archive's: the deletes are committed.
func (m *Manager) dropDevices(nick string, clients []*whatsmeow.Client) (hint string, err error) {
	var errs []error
	for _, cli := range clients {
		listed, err := m.unlinkClient(nick, cli)
		switch {
		case err != nil: // its device is still there, and the repeat decides again
			errs = append(errs, err)
		case listed:
			hint = removeDeviceHint
		}
	}
	if err := m.store.forget(m.ctx); err != nil {
		m.log.Warn("clean up store.db after the removed device; copies of its keys may stay in store.db or its WAL until the next checkpoint", "err", err)
	}
	if err := errors.Join(errs...); err != nil {
		return hint, fmt.Errorf("could not delete the keys of the device from this computer: %w", err)
	}
	return hint, nil
}
