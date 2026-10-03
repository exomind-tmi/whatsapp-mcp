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

func TestValidPhone(t *testing.T) {
	for _, tc := range []struct {
		phone string
		ok    bool
	}{
		{"79161234567", true},
		{"+79161234567", true},
		{"+7 (916) 123-45-67", true},
		{"+1-202-555-0123", true},
		{"1234567", true},         // 7 digits: PairPhone's minimum
		{"123456789012345", true}, // 15: the E.164 maximum
		{"", false},
		{"+", false},
		{"123456", false},           // too short for PairPhone
		{"1234567890123456", false}, // past E.164
		{"89161234567", true},       // a national prefix, but PairPhone cannot tell
		{"0079161234567", false},    // 00 for + starts with 0
		{"+0 916 123 45 67", false},
		{"7916123456+7", false},
		{"++79161234567", false},
		{"+7 916 ABC 45 67", false},
		{"+7.916.123.45.67", false},
		{"+7916123456７", false}, // a full-width digit is not 0-9
		{"+79161234567\n", false},
	} {
		if got := ValidPhone(tc.phone); got != tc.ok {
			t.Errorf("ValidPhone(%q) = %v, want %v", tc.phone, got, tc.ok)
		}
	}
}
