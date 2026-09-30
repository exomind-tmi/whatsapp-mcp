// Package proc holds the two OS-specific process primitives: an exclusive
// file lock and a detached spawn.
package proc

import (
	"errors"
	"os"
)

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("lock is held by another process")

// Lock is an exclusive, non-blocking lock on a file. The OS releases it when
// the holder dies, so a killed daemon never leaves a stale lock.
type Lock struct{ f *os.File }

// TryLock takes the lock or returns ErrLocked immediately.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	uerr := unlockFile(l.f)
	return errors.Join(uerr, l.f.Close())
}
