// Package wa manages WhatsApp accounts. It is the only package that imports
// whatsmeow.
package wa

import (
	"errors"
	"regexp"
	"strings"
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
	Chats     int // archived chats and messages of the account
	Messages  int
}

// LinkTicket is what the user needs to finish linking: a local QR page or a
// pairing code to type on the phone, or nothing but the news that the
// account reconnects with the keys it has.
type LinkTicket struct {
	LoginURL     string
	PairCode     string
	ExpiresAt    time.Time
	Reconnecting bool // no URL and no code: the existing device connects again
}

type RemoveResult struct {
	Hint string // e.g. "remove the device on the phone manually"
}

var nickRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ValidNick reports whether s is an acceptable account nickname.
func ValidNick(s string) bool { return nickRe.MatchString(s) }

// phoneRe admits a number as people write it: an optional leading +,
// digits, spaces, dashes and parentheses.
var phoneRe = regexp.MustCompile(`^\+?[0-9 ()-]+$`)

// ValidPhone reports whether s is a number PairPhone accepts. PairPhone
// drops every non-digit and wants more than 6 digits not starting with 0,
// i.e. in international form (pair-code.go:97-102); letters are refused
// here rather than dropped, and E.164 caps a number at 15 digits.
func ValidPhone(s string) bool {
	if !phoneRe.MatchString(s) {
		return false
	}
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	return len(d) > 6 && len(d) <= 15 && d[0] != '0'
}

// ErrNotImplemented is returned by operations that arrive in a later step.
var ErrNotImplemented = errors.New("not implemented yet: removing WhatsApp accounts arrives in a later step of the accounts milestone")
