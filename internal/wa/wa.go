// Package wa manages WhatsApp accounts. It will be the only package that
// imports whatsmeow (M1); in M0 Manager is a stub with no accounts.
package wa

import (
	"context"
	"errors"
	"regexp"
	"time"
)

type Status string

const (
	StatusLinking        Status = "linking"
	StatusConnected      Status = "connected"
	StatusReconnecting   Status = "reconnecting"
	StatusNeedsLink      Status = "needs_link"
	StatusReplaced       Status = "replaced"
	StatusClientOutdated Status = "client_outdated"
	StatusError          Status = "error"
)

type AccountInfo struct {
	Nick      string
	Status    Status
	Reason    string
	ExpiresAt time.Time // zero unless the status is temporary (e.g. a ban)
	Phone     string
	PushName  string
}

// LinkTicket is what the user needs to finish linking: a local QR page or a
// pairing code to type on the phone.
type LinkTicket struct {
	LoginURL  string
	PairCode  string
	ExpiresAt time.Time
}

type RemoveResult struct {
	Hint string // e.g. "remove the device on the phone manually"
}

var nickRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ValidNick reports whether s is an acceptable account nickname.
func ValidNick(s string) bool { return nickRe.MatchString(s) }

// ErrNotImplemented is returned by operations that arrive in a later milestone.
var ErrNotImplemented = errors.New("not implemented until M1: linking and removing WhatsApp accounts arrives with the WhatsApp client in the next milestone")

// Manager is the M0 stub: no accounts, linking not available yet.
type Manager struct{}

func NewManager() *Manager { return &Manager{} }

func (*Manager) Accounts(context.Context) []AccountInfo { return nil }

func (*Manager) Link(context.Context, string, string) (LinkTicket, error) {
	return LinkTicket{}, ErrNotImplemented
}

func (*Manager) Remove(context.Context, string) (RemoveResult, error) {
	return RemoveResult{}, ErrNotImplemented
}

// Close disconnects all clients.
func (*Manager) Close() error { return nil }
