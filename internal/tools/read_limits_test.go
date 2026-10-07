package tools

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestAListThatEndsAtTheLimitSaysNothingMore: a list that has as many as the limit
// and no more is not said to have more, and one that has more says where.
func TestAListThatEndsAtTheLimitSaysNothingMore(t *testing.T) {
	r := newRig(t, "personal")
	for i := range 8 {
		jid := fmt.Sprintf("2000000001%02d@lid", i)
		r.put(m{account: "personal", chat: jid, id: fmt.Sprintf("M%d", i), sender: jid, at: time.Duration(i) * time.Second, text: "needle"})
	}
	for _, tc := range []struct {
		limit int
		more  bool
	}{{8, false}, {9, false}, {7, true}, {1, true}} {
		var chats ChatsOut
		r.ok("list-chats", map[string]any{"limit": tc.limit}, &chats)
		hits := r.search(map[string]any{"query": "needle", "limit": tc.limit})
		for name, notes := range map[string][]string{"list-chats": chats.Notes, "search-messages": hits.Notes} {
			switch said := len(notes) != 0; {
			case said != tc.more:
				t.Errorf("%s limit %d: notes %q, want a note about more: %v", name, tc.limit, notes, tc.more)
			case said && !strings.Contains(notes[0], fmt.Sprintf("than the %d shown", tc.limit)):
				t.Errorf("%s limit %d: the note does not say how many are shown: %q", name, tc.limit, notes)
			}
		}
		if want := min(tc.limit, 8); len(chats.Chats) != want || len(hits.Results) != want {
			t.Errorf("limit %d: %d chats and %d hits, want %d", tc.limit, len(chats.Chats), len(hits.Results), want)
		}
	}
}

// TestFractionsOfASecondAreRoundedUp: the archive keeps whole seconds, so a message of
// the second 03 is older than 03.5 and one of the second 02 is not from 02.5 on: a time
// with a fraction is read at the second above it, in get-messages and in search.
func TestFractionsOfASecondAreRoundedUp(t *testing.T) {
	r := newRig(t, "personal")
	r.series("personal", bobChat, "M", 0, 6, false)
	at := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"older than 03.5", map[string]any{"before": at(3500 * time.Millisecond)}, "M0,M1,M2,M3"},
		{"older than 03.0", map[string]any{"before": at(3 * time.Second)}, "M0,M1,M2"},
		{"older than 03.000000001", map[string]any{"before": at(3*time.Second + 1)}, "M0,M1,M2,M3"},
		{"from 02.5", map[string]any{"after": at(2500 * time.Millisecond)}, "M3,M4,M5"},
		{"from 02.0", map[string]any{"after": at(2 * time.Second)}, "M2,M3,M4,M5"},
	} {
		got := strings.Join(ids(r.messages(mergeArgs(map[string]any{"chat": bobChat}, tc.args)).Messages), ",")
		if got != tc.want {
			t.Errorf("get-messages %s: %s, want %s", tc.name, got, tc.want)
		}
		hits := hitIDs(r.search(mergeArgs(map[string]any{"query": "text"}, tc.args)))
		slices.Reverse(hits) // newest first
		if strings.ReplaceAll(strings.Join(hits, ","), "personal:", "") != tc.want {
			t.Errorf("search-messages %s: %v, want %s", tc.name, hits, tc.want)
		}
	}
}

func mergeArgs(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range []map[string]any{a, b} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
