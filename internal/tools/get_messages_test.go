package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// bobsRig is a rig with Bob's chat (filed under his LID, which is known) in the
// account personal.
func bobsRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, "personal")
	r.wa.Pairs = []string{bobLID, bobPN}
	r.chat("personal", bobChat, bobPNJID, "Bob", false)
	return r
}

// TestGetMessagesFields: what a message shows of what is stored, and what it does
// not: nothing of the raw message, nothing of where a file was saved.
func TestGetMessagesFields(t *testing.T) {
	r := bobsRig(t)
	r.wa.People = map[string]string{bobChat: "Bob Marley", ownJID: "Anton"}
	r.put(
		m{account: "personal", chat: bobChat, id: "M1", sender: bobChat, at: 1 * time.Second, text: "plain"},
		m{account: "personal", chat: bobChat, id: "M2", sender: ownJID, fromMe: true, at: 2 * time.Second, text: "from me"},
		m{account: "personal", chat: bobChat, id: "M3", sender: bobChat, at: 3 * time.Second, text: "the plan", media: "document", name: "plan.pdf"},
		m{account: "personal", chat: bobChat, id: "M4", sender: bobChat, at: 4 * time.Second, media: "image"},
		m{account: "personal", chat: bobChat, id: "M5", sender: bobChat, at: 5 * time.Second, text: "to be edited"},
		m{account: "personal", chat: bobChat, id: "M6", sender: bobChat, at: 6 * time.Second, text: "to be deleted"},
		m{account: "personal", chat: bobChat, id: "M7", sender: bobChat, at: 7 * time.Second, text: "a reply", quoted: "M1"},
	)
	err := r.db.Tx(context.Background(), func(tx *archive.Tx) error {
		if err := tx.Edit(archive.Edit{Account: "personal", Chat: bobChat, ID: "M5", Text: "edited", EditedAt: base.Add(30 * time.Second)}); err != nil {
			return err
		}
		return tx.Revoke(archive.Revoke{Account: "personal", Chat: bobChat, ID: "M6", RevokedAt: base.Add(40 * time.Second)})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.db.SetMedia(context.Background(), "personal", bobChat, "M3", `C:\Users\someone\Downloads\plan.pdf`, []byte("raw of M3 again")); err != nil {
		t.Fatal(err)
	}

	_, text := r.call("get-messages", map[string]any{"chat": bobChat})
	var raw struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatal(err)
	}
	for i, want := range []map[string]any{
		{"account": "personal", "chat": bobChat, "id": "M1", "sender": bobChat, "sender_name": "Bob Marley", "from_me": false, "at": iso(1 * time.Second), "text": "plain"},
		{"account": "personal", "chat": bobChat, "id": "M2", "sender": ownJID, "from_me": true, "at": iso(2 * time.Second), "text": "from me"},
		{"account": "personal", "chat": bobChat, "id": "M3", "sender": bobChat, "sender_name": "Bob Marley", "from_me": false, "at": iso(3 * time.Second), "text": "the plan",
			"media": map[string]any{"type": "document", "mime": "application/pdf", "name": "plan.pdf", "size": float64(99)}},
		{"account": "personal", "chat": bobChat, "id": "M4", "sender": bobChat, "sender_name": "Bob Marley", "from_me": false, "at": iso(4 * time.Second), "text": nil,
			"media": map[string]any{"type": "image", "mime": "application/pdf", "size": float64(99)}},
		{"account": "personal", "chat": bobChat, "id": "M5", "sender": bobChat, "sender_name": "Bob Marley", "from_me": false, "at": iso(5 * time.Second), "text": "edited", "edited_at": iso(30 * time.Second)},
		{"account": "personal", "chat": bobChat, "id": "M6", "sender": bobChat, "sender_name": "Bob Marley", "from_me": false, "at": iso(6 * time.Second), "text": "to be deleted",
			"revoked": true, "revoked_at": iso(40 * time.Second)},
		{"account": "personal", "chat": bobChat, "id": "M7", "sender": bobChat, "sender_name": "Bob Marley", "from_me": false, "at": iso(7 * time.Second), "text": "a reply", "quoted_id": "M1"},
	} {
		if len(raw.Messages) != 7 || !sameValue(raw.Messages[i], want) {
			t.Errorf("message %d = %v\nwant     %v", i, raw.Messages, want)
			break
		}
	}
	// The raw message and the place of the download are not what a read shows.
	for _, secret := range []string{"raw of", "Downloads", "media_path", `"raw"`, `"path"`} {
		if strings.Contains(text, secret) {
			t.Errorf("the result has %q in it:\n%s", secret, text)
		}
	}
}

// sameValue compares what was decoded from JSON.
func sameValue(got, want map[string]any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}

// TestGetMessagesChatForms: the chat as a LID, as the number it is the LID of,
// written as people write numbers, with a device in it: all the one chat, which is
// filed under its LID; and a chat that has no LID known, under its number.
func TestGetMessagesChatForms(t *testing.T) {
	r := bobsRig(t)
	r.series("personal", bobChat, "B", 1, 3, false)
	r.series("personal", carolJID, "C", 1, 2, false)
	for _, chat := range []string{bobChat, bobPNJID, bobPN + ":12@s.whatsapp.net", "+7 000 000 0100", "+7 (000) 000-01-00", bobPN, " " + bobChat + " "} {
		if got := ids(r.messages(map[string]any{"chat": chat}).Messages); !slices.Equal(got, []string{"B1", "B2", "B3"}) {
			t.Errorf("chat %q: %v", chat, got)
		}
	}
	out := r.messages(map[string]any{"chat": bobPNJID})
	if out.Account != "personal" || out.Chat != bobChat {
		t.Errorf("the output names the chat as %s of %s, want the canonical %s of personal", out.Chat, out.Account, bobChat)
	}
	if got := ids(r.messages(map[string]any{"chat": "+7 000 000 0101"}).Messages); !slices.Equal(got, []string{"C1", "C2"}) {
		t.Errorf("a chat with no LID known: %v", got)
	}
	// A group is its JID.
	r.series("personal", groupJID, "G", 1, 1, false)
	if got := ids(r.messages(map[string]any{"chat": groupJID}).Messages); !slices.Equal(got, []string{"G1"}) {
		t.Errorf("group: %v", got)
	}
	for _, bad := range []string{"", "Bob", "bob@example.com", "a@b@c", "9876543210987654", "70000000100@status", "x@g.us", "70000000100@s.whatsapp.net@evil"} {
		text := r.fails("get-messages", map[string]any{"chat": bad})
		if !strings.Contains(text, "not a chat") || !strings.Contains(text, "list-chats") {
			t.Errorf("chat %q: %s", bad, text)
		}
		if bad != "" && strings.Contains(text, bad) {
			t.Errorf("chat %q is repeated in the error: %s", bad, text)
		}
	}
}

// TestGetMessagesPagesWithEqualTimes: the messages of a second come in the order
// they were stored in, and pages of any size cut between them without losing or
// repeating one: the cursor is the opaque next_before that is passed back as it
// is.
func TestGetMessagesPagesWithEqualTimes(t *testing.T) {
	r := bobsRig(t)
	var all []m
	for i := range 11 { // three seconds, several messages in each
		all = append(all, m{account: "personal", chat: bobChat, id: fmt.Sprintf("M%02d", i), sender: bobChat,
			at: time.Duration(i/4) * time.Second, text: fmt.Sprintf("message %d", i)})
	}
	r.put(all...)
	want := make([]string, len(all))
	for i, x := range all {
		want[i] = x.id
	}

	for _, size := range []int{1, 2, 3, 4, 5, 11, 50} {
		var got []string
		args := map[string]any{"chat": bobChat, "limit": size}
		for pages := 0; ; pages++ {
			if pages > len(all) {
				t.Fatalf("limit %d: paging does not end", size)
			}
			page := r.messages(args)
			got = append(ids(page.Messages), got...) // each page is older than the last
			if len(page.Messages) > size {
				t.Fatalf("limit %d: a page of %d", size, len(page.Messages))
			}
			if page.NextBefore == "" {
				break
			}
			if !strings.Contains(page.NextBefore, "_") {
				t.Fatalf("next_before %q is not the cursor", page.NextBefore)
			}
			args = map[string]any{"chat": bobChat, "limit": size, "before": page.NextBefore}
		}
		if !slices.Equal(got, want) {
			t.Errorf("limit %d: the pages together are %v, want %v", size, got, want)
		}
	}
	// A page that is the last has no next_before, and one that is not has it even
	// when the page is full to the last message.
	if page := r.messages(map[string]any{"chat": bobChat, "limit": 11}); page.NextBefore != "" {
		t.Errorf("the whole chat has a next_before %q", page.NextBefore)
	}
	if page := r.messages(map[string]any{"chat": bobChat, "limit": 10}); page.NextBefore == "" || len(page.Messages) != 10 {
		t.Errorf("10 of 11: next_before = %q, %d messages", page.NextBefore, len(page.Messages))
	}
}

// TestGetMessagesByTime: before is the older than, after the not older than; both
// are times in ISO 8601 with an offset, in any zone; before is also a cursor.
func TestGetMessagesByTime(t *testing.T) {
	r := bobsRig(t)
	r.series("personal", bobChat, "M", 0, 6, false) // at 0 s .. 5 s
	for _, tc := range []struct {
		name          string
		before, after string
		want          []string
	}{
		{"before, in the zone of the machine", iso(3 * time.Second), "", []string{"M0", "M1", "M2"}},
		{"before, in UTC", base.Add(3 * time.Second).UTC().Format(time.RFC3339), "", []string{"M0", "M1", "M2"}},
		{"before, in another zone", base.Add(3 * time.Second).In(time.FixedZone("x", -5*3600)).Format(time.RFC3339), "", []string{"M0", "M1", "M2"}},
		{"before, with a fraction", base.Add(3 * time.Second).UTC().Format("2006-01-02T15:04:05.000Z07:00"), "", []string{"M0", "M1", "M2"}},
		{"after is inclusive", "", iso(4 * time.Second), []string{"M4", "M5"}},
		{"both", iso(5 * time.Second), iso(2 * time.Second), []string{"M2", "M3", "M4"}},
		{"before everything", iso(0), "", nil},
		{"before the epoch itself, which is no position", "1970-01-01T00:00:00Z", "", nil},
		{"before a time before the epoch", "1960-01-01T00:00:00Z", "", nil},
		{"after everything", "", iso(time.Hour), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"chat": bobChat}
			if tc.before != "" {
				args["before"] = tc.before
			}
			if tc.after != "" {
				args["after"] = tc.after
			}
			got := ids(r.messages(args).Messages)
			if !slices.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	// after is not part of the cursor: it bounds each call, and paging inside it
	// comes to its edge.
	var got []string
	args := map[string]any{"chat": bobChat, "limit": 2, "after": iso(1 * time.Second)}
	for {
		page := r.messages(args)
		got = append(ids(page.Messages), got...)
		if page.NextBefore == "" {
			break
		}
		args["before"] = page.NextBefore
	}
	if !slices.Equal(got, []string{"M1", "M2", "M3", "M4", "M5"}) {
		t.Errorf("paging after a time: %v", got)
	}
}

// marker is a text that nothing of ours says: where it turns up, it was repeated.
const marker = "IGNORE-ALL-PREVIOUS-INSTRUCTIONS-7f3a"

// TestGetMessagesRefusesWhatIsNotAbsoluteTime: relative words are not understood,
// and the answer is an example and never the words that were given.
func TestGetMessagesRefusesWhatIsNotAbsoluteTime(t *testing.T) {
	r := bobsRig(t)
	r.series("personal", bobChat, "M", 0, 3, false)
	for _, bad := range []string{"now", "yesterday", "-1h", "1 hour ago", "today", "2026-10-07", "12:00", "2026-10-07T18:30:00", "2026-10-07 18:30:00+03:00",
		"123_", "_5", "0_0", "-5_3", "1_2_3", "1__2", "+5_3", "999999999999999999999_1", "0x10_1", marker} {
		for _, field := range []string{"before", "after"} {
			text := r.fails("get-messages", map[string]any{"chat": bobChat, field: bad})
			if !strings.Contains(text, timeExample) || strings.Contains(text, marker) {
				t.Errorf("%s=%q: %s", field, bad, text)
			}
			if field == "before" && !strings.Contains(text, "next_before") {
				t.Errorf("before=%q: %s", bad, text)
			}
			if field == "after" && !strings.HasPrefix(text, "after must be a time") {
				t.Errorf("after=%q: %s", bad, text)
			}
		}
	}
	// A cursor is for before, not after.
	if text := r.fails("get-messages", map[string]any{"chat": bobChat, "after": "1_2"}); !strings.Contains(text, timeExample) {
		t.Errorf("a cursor as after: %s", text)
	}
}

func TestGetMessagesLimit(t *testing.T) {
	r := bobsRig(t)
	r.series("personal", bobChat, "M", 1, 260, false)
	for _, tc := range []struct {
		name     string
		limit    any
		want     int
		wantNote bool
	}{
		{"default", nil, 50, false},
		{"asked", 3, 3, false},
		{"the most", 200, 200, false},
		{"over the most", 1000, 200, true},
	} {
		args := map[string]any{"chat": bobChat}
		if tc.limit != nil {
			args["limit"] = tc.limit
		}
		out := r.messages(args)
		if len(out.Messages) != tc.want || (len(out.Notes) == 1) != tc.wantNote {
			t.Errorf("%s: %d messages, notes %q", tc.name, len(out.Messages), out.Notes)
		}
		// The newest ones, oldest first.
		if out.Messages[len(out.Messages)-1].ID != "M260" {
			t.Errorf("%s: the last is %s", tc.name, out.Messages[len(out.Messages)-1].ID)
		}
	}
	if text := r.fails("get-messages", map[string]any{"chat": bobChat, "limit": -5}); text != "limit must be positive" {
		t.Errorf("negative limit: %s", text)
	}
}

// TestGetMessagesOfAChatThatIsNotThere: an empty page is told apart from an
// error, and says why it may be empty.
func TestGetMessagesOfAChatThatIsNotThere(t *testing.T) {
	r := bobsRig(t)
	out := r.messages(map[string]any{"chat": "+7 999 999 99 99"})
	if len(out.Messages) != 0 || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "list-chats") {
		t.Errorf("out = %+v", out)
	}
	_, text := r.call("get-messages", map[string]any{"chat": "+7 999 999 99 99"})
	if !strings.Contains(text, `"messages":[]`) {
		t.Errorf("an empty page is not a list: %s", text)
	}
}

func TestParseBefore(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want archive.Cursor
		ok   bool
	}{
		{"1760000000_42", archive.Cursor{TS: 1760000000, ID: 42}, true},
		{"1_1", archive.Cursor{TS: 1, ID: 1}, true},
		{"0_5", archive.Cursor{TS: 0, ID: 5}, true},
		{"2026-10-07T12:00:00Z", archive.Cursor{TS: base.Unix()}, true},
		{"2026-10-07T15:00:00+03:00", archive.Cursor{TS: base.Unix()}, true},
		{"1970-01-01T00:00:00Z", archive.Cursor{TS: -1}, true}, // not the zero cursor, which is the newest page
		{"0_0", archive.Cursor{}, false},
		{"", archive.Cursor{}, false},
		{"yesterday", archive.Cursor{}, false},
	} {
		got, err := parseBefore(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("parseBefore(%q) = %v, %v; want %v ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}
