package wa

import (
	"encoding/base64"
	"testing"
	"time"
)

// clock is a time the test moves.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestLoginNonceLifecycle(t *testing.T) {
	c := &clock{time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	n := newLoginNonces()
	n.now = c.now

	a := n.issue("personal")
	if b, err := base64.RawURLEncoding.DecodeString(a.value); err != nil || len(b) != 16 {
		t.Fatalf("nonce %q is not 16 bytes in base64url: %v", a.value, err)
	}
	if !a.expires.Equal(c.t.Add(10 * time.Minute)) {
		t.Errorf("expires %v, want in 10 minutes", a.expires)
	}

	// Reusable inside the window, for that nick alone.
	for range 3 {
		if got := n.find("personal", a.value); got != a {
			t.Fatal("a valid nonce was not found")
		}
	}
	for _, bad := range []struct{ nick, nonce string }{
		{"personal", ""}, {"personal", "x"}, {"personal", a.value[1:]}, {"personal", a.value + "A"},
		{"personal", flip(a.value)}, {"business", a.value}, {"nobody", ""}, {"nobody", absentNonce}, {"", a.value},
	} {
		if n.find(bad.nick, bad.nonce) != nil {
			t.Errorf("find(%q, %q) accepted", bad.nick, bad.nonce)
		}
	}

	// Another nick's nonce is independent.
	b := n.issue("business")
	if b.value == a.value || n.find("personal", a.value) != a || n.find("business", b.value) != b {
		t.Error("nonces of two nicks interfere")
	}

	// The window is half-open: valid until the instant it ends.
	c.t = a.expires.Add(-time.Nanosecond)
	if n.find("personal", a.value) != a {
		t.Error("the nonce expired early")
	}
	c.t = a.expires
	if n.find("personal", a.value) != nil {
		t.Error("the nonce lives past its window")
	}
	if n.find("business", b.value) != nil { // issued at the same moment
		t.Error("business's nonce lives past its window")
	}
}

// TestLoginNonceReplaced: a new add for the nick replaces its nonce; the old
// one stops working at once, though its window is open.
func TestLoginNonceReplaced(t *testing.T) {
	n := newLoginNonces()
	old := n.issue("personal")
	fresh := n.issue("personal")
	if old.value == fresh.value {
		t.Fatal("two nonces are equal")
	}
	if n.find("personal", old.value) != nil {
		t.Error("the replaced nonce still works")
	}
	if n.find("personal", fresh.value) != fresh {
		t.Error("the new nonce does not work")
	}
}

func TestLoginNonceRevokeAndLive(t *testing.T) {
	n := newLoginNonces()
	a, b := n.issue("personal"), n.issue("business")
	if !n.live("personal", a) || n.live("business", a) || n.live("nobody", a) {
		t.Error("live: a nonce is live for its own nick only")
	}
	n.revoke("personal")
	n.revoke("nobody") // nothing to end
	if n.find("personal", a.value) != nil || n.live("personal", a) {
		t.Error("a revoked nonce works")
	}
	if n.find("business", b.value) != b {
		t.Error("revoking one nick ended another's nonce")
	}
	if c := n.issue("personal"); n.live("personal", a) || !n.live("personal", c) {
		t.Error("a nonce issued after a revoke is not the only live one")
	}
}

func TestLoginNonceExtend(t *testing.T) {
	c := &clock{time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	n := newLoginNonces()
	n.now = c.now
	a := n.issue("personal")
	end := a.expires

	n.extend("personal", a, end.Add(-time.Minute)) // never shortens
	n.extend("business", a, end.Add(time.Hour))    // nor works for another nick
	c.t = end.Add(-time.Second)
	if n.find("personal", a.value) != a {
		t.Error("an extension that ends earlier shortened the window")
	}
	c.t = end
	if n.find("personal", a.value) != nil {
		t.Error("the window moved without an extension")
	}
	c.t = end.Add(-time.Second)
	n.extend("personal", a, end.Add(time.Hour))
	c.t = end.Add(time.Hour - time.Nanosecond)
	if n.find("personal", a.value) != a || !a.expires.Equal(end) {
		t.Error("an extension did not hold, or it moved the window the tool tells")
	}
	c.t = end.Add(time.Hour)
	if n.find("personal", a.value) != nil {
		t.Error("an extension lasts past its end")
	}

	// A replaced nonce is not extended.
	old := n.issue("personal")
	n.issue("personal")
	n.extend("personal", old, c.t.Add(time.Hour))
	if n.live("personal", old) {
		t.Error("a replaced nonce was extended")
	}
}

// TestLoginNonceExpiredAreForgotten: the record of a nick that never came
// back goes with the next issue, so the map does not grow with every nick
// ever added.
func TestLoginNonceExpiredAreForgotten(t *testing.T) {
	c := &clock{time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	n := newLoginNonces()
	n.now = c.now
	n.issue("old")
	c.t = c.t.Add(loginTTL - time.Nanosecond)
	n.issue("recent")
	if len(n.byNick) != 2 {
		t.Fatalf("%d records, want old and recent", len(n.byNick))
	}
	c.t = c.t.Add(time.Nanosecond) // old ends, recent has 10 minutes
	n.issue("newest")
	if _, ok := n.byNick["old"]; ok || len(n.byNick) != 2 {
		t.Errorf("records %v: old should be gone", n.byNick)
	}
}

func TestLoginNonceIsRandom(t *testing.T) {
	n := newLoginNonces()
	seen := map[string]bool{}
	for range 200 {
		v := n.issue("personal").value
		if seen[v] {
			t.Fatalf("nonce %q repeated", v)
		}
		seen[v] = true
	}
}

func TestSameNonce(t *testing.T) {
	for _, tc := range []struct {
		want, got string
		same      bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "ab", false},
		{"abc", "abcd", false},
		{"abc", "", false},
		{"", "", true}, // never reached: find refuses a nick with no record
	} {
		if got := sameNonce(tc.want, tc.got); got != tc.same {
			t.Errorf("sameNonce(%q, %q) = %v", tc.want, tc.got, got)
		}
	}
	if len(absentNonce) != base64.RawURLEncoding.EncodedLen(nonceBytes) {
		t.Error("the stand-in for an absent nonce differs in length from a real one")
	}
}

// flip changes the last character of s.
func flip(s string) string {
	last := s[len(s)-1]
	if last == 'A' {
		return s[:len(s)-1] + "B"
	}
	return s[:len(s)-1] + "A"
}
