package wa

import (
	"strings"
	"testing"
)

func TestValidNick(t *testing.T) {
	for _, tc := range []struct {
		nick string
		ok   bool
	}{
		{"personal", true},
		{"business_2", true},
		{"a-b", true},
		{"0", true},
		{strings.Repeat("a", 64), true},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"Personal", false},
		{"my nick", false},
		{"личный", false},
		{"../etc", false},
		{"a.b", false},
		{"nick\n", false},
	} {
		if got := ValidNick(tc.nick); got != tc.ok {
			t.Errorf("ValidNick(%q) = %v, want %v", tc.nick, got, tc.ok)
		}
	}
}
