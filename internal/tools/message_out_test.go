package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// sizeOf is what a result weighs as the JSON that goes out.
func sizeOf(t *testing.T, out any) int {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// page is a result of n messages with texts of the given lengths in characters (a
// negative length is a message with no text), of the letter given: "ж" is two bytes a
// character, so that a cut by bytes would break one, and "<" is six, as JSON has it.
func page(letter string, lens ...int) MessagesOut {
	out := MessagesOut{Account: "personal", Chat: bobChat, Messages: make([]MessageOut, len(lens))}
	for i, n := range lens {
		out.Messages[i] = MessageOut{Account: "personal", Chat: bobChat, ID: "M" + strconv.Itoa(i), Sender: bobChat, At: "2026-10-07T15:00:00+03:00"}
		if n >= 0 {
			s := strings.Repeat(letter, n)
			out.Messages[i].Text = &s
		}
	}
	return out
}

func runes(m MessageOut) int {
	if m.Text == nil {
		return -1
	}
	return utf8.RuneCountInString(*m.Text)
}

// TestCapTexts: no text is longer than the limit of one, the result is no heavier
// than the limit of a result, a text that is cut says so, and the short ones, which are
// most of them, are not touched.
func TestCapTexts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lens      []int
		want      []int // the length of each after; -1 for no text
		cut       []bool
		cutShort  bool // cut shorter than the limit of a text
		letter    string
		wantExact bool // the lengths are exactly want (else: at most)
	}{
		{"nothing", nil, nil, nil, false, "ж", true},
		{"short", []int{0, 1, 100}, []int{0, 1, 100}, []bool{false, false, false}, false, "ж", true},
		{"no text", []int{-1, 5}, []int{-1, 5}, []bool{false, false}, false, "ж", true},
		{"the longest that is let be", []int{maxText}, []int{maxText}, []bool{false}, false, "ж", true},
		{"one more", []int{maxText + 1}, []int{maxText}, []bool{true}, false, "ж", true},
		{"very long", []int{100000}, []int{maxText}, []bool{true}, false, "ж", true},
		{"fifteen long ones are too much", []int{4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000},
			nil, []bool{true, true, true, true, true, true, true, true, true, true, true, true, true, true, true}, true, "ж", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := page(tc.letter, tc.lens...)
			if got := capMessages(&out, refs(out.Messages)); got != tc.cutShort {
				t.Errorf("cut shorter than a text may be = %v, want %v", got, tc.cutShort)
			}
			if size := sizeOf(t, out); size > maxResultBytes {
				t.Errorf("the result is %d bytes, more than %d", size, maxResultBytes)
			}
			for i, m := range out.Messages {
				n := runes(m)
				switch {
				case tc.wantExact && n != tc.want[i]:
					t.Errorf("text %d is %d characters, want %d", i, n, tc.want[i])
				case n > maxText:
					t.Errorf("text %d is %d characters, more than %d", i, n, maxText)
				}
				if m.Text != nil && !utf8.ValidString(*m.Text) {
					t.Errorf("text %d was cut in the middle of a character", i)
				}
				if m.Truncated != tc.cut[i] {
					t.Errorf("text %d: truncated = %v, want %v", i, m.Truncated, tc.cut[i])
				}
			}
		})
	}
}

// TestCapCountsBytesNotCharacters: what a character weighs in the JSON is what the
// result is limited by, so a page of the characters that JSON escapes (six bytes each)
// is no heavier than one of letters, and every message of it is still there.
func TestCapCountsBytesNotCharacters(t *testing.T) {
	for name, letter := range map[string]string{
		"angle brackets": "<", "NULs": "\x00", "emoji": "\U0001F600", "cyrillic": "ж", "ascii": "a",
	} {
		t.Run(name, func(t *testing.T) {
			lens := make([]int, 200)
			for i := range lens {
				lens[i] = 4000
			}
			out := page(letter, lens...)
			if !capMessages(&out, refs(out.Messages)) {
				t.Error("the cut below the limit of a text is not said")
			}
			if size := sizeOf(t, out); size > maxResultBytes {
				t.Errorf("the result is %d bytes, more than %d", size, maxResultBytes)
			}
			if len(out.Messages) != 200 {
				t.Fatalf("%d messages of 200", len(out.Messages))
			}
			for i, m := range out.Messages {
				if !m.Truncated || runes(m) >= maxText {
					t.Fatalf("message %d: %d characters, truncated=%v", i, runes(m), m.Truncated)
				}
			}
		})
	}
}

// TestCapDoesNotDropWhatItCannotCut: the ids and the addresses of the messages are not
// the sender's to make long and are not cut, so a result of many messages that have no
// text stays as heavy as it is, whole, and the agent is not told that texts were cut.
func TestCapDoesNotDropWhatItCannotCut(t *testing.T) {
	lens := make([]int, 200)
	for i := range lens {
		lens[i] = -1 // no text
	}
	out := page("ж", lens...)
	for i := range out.Messages {
		out.Messages[i].ID, out.Messages[i].Sender = strings.Repeat("i", 128), strings.Repeat("9", 100)
	}
	if sizeOf(t, out) <= maxResultBytes {
		t.Fatal("the premise is a result that is over the limit with no text at all")
	}
	if capMessages(&out, refs(out.Messages)) {
		t.Error("the agent is told that texts were cut, and there are none")
	}
	if len(out.Messages) != 200 {
		t.Errorf("%d messages of 200", len(out.Messages))
	}
}

// TestCapKeepsShortTextsWhole: a result that is too heavy is brought down by the long
// texts alone, to one common length, and never by dropping a message or touching a short one.
func TestCapKeepsShortTextsWhole(t *testing.T) {
	lens := make([]int, 160)
	for i := range lens {
		lens[i] = 50
	}
	for i := 0; i < 10; i++ {
		lens[i*16] = maxText
	}
	out := page("ж", lens...)
	if !capMessages(&out, refs(out.Messages)) {
		t.Error("the cut below the limit of a text is not said")
	}
	if len(out.Messages) != 160 {
		t.Fatalf("%d messages left of 160", len(out.Messages))
	}
	long := -1
	for i, m := range out.Messages {
		switch {
		case lens[i] == 50 && (runes(m) != 50 || m.Truncated):
			t.Fatalf("a short text was touched: %d characters, truncated=%v", runes(m), m.Truncated)
		case lens[i] == maxText && !m.Truncated:
			t.Fatalf("a long text of %d characters is not marked truncated", runes(m))
		case lens[i] == maxText && long >= 0 && runes(m) != long:
			t.Fatalf("the long texts are cut to different lengths: %d and %d", long, runes(m))
		case lens[i] == maxText:
			long = runes(m)
		}
	}
	if size := sizeOf(t, out); size > maxResultBytes {
		t.Errorf("the result is %d bytes, more than %d", size, maxResultBytes)
	}
}

// TestCapIsTheLongestLengthThatFits: for any mix of long texts, short ones and names,
// the common length is such that the result fits, and one character more would not (so
// that nothing is cut that need not be).
func TestCapIsTheLongestLengthThatFits(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for round := range 40 {
		letter := []string{"ж", "<", "a", "😀"}[round%4]
		texts, names := make([]string, 1+rng.IntN(200)), make([]string, 0)
		long := strings.Repeat(letter, 5000) // over the limit of a text, so that a cut is to the common length
		for i := range texts {
			texts[i] = strings.Repeat("s", rng.IntN(60))
			if i == 0 || rng.IntN(3) == 0 {
				texts[i] = long
			}
			names = append(names, strings.Repeat("n", rng.IntN(300)))
		}
		// build is the result with each string cut to limit (or to its own limit if
		// that is less): what the cut should make of it. A negative limit cuts nothing.
		build := func(limit int) MessagesOut {
			out := page(letter, make([]int, len(texts))...)
			for i := range texts {
				x := &out.Messages[i]
				text, name := texts[i], names[i]
				if limit >= 0 {
					text, name = cutRunes(text, min(limit, maxText)), cutRunes(name, min(limit, maxName))
				}
				x.Text, x.Truncated, x.SenderName = &text, text != texts[i], name
			}
			return out
		}
		out := build(-1)
		capMessages(&out, refs(out.Messages))
		if size := sizeOf(t, out); size > maxResultBytes {
			t.Fatalf("round %d: the result is %d bytes", round, size)
		}
		limit := runes(out.Messages[0]) // the first text is a long one: it is what the others were cut to
		if limit == maxText {
			continue // the result fit, with each string at its own limit
		}
		if got, want := sizeOf(t, out), sizeOf(t, build(limit)); got != want {
			t.Fatalf("round %d: the result is %d bytes, and what a cut to %d characters makes is %d", round, got, limit, want)
		}
		if size := sizeOf(t, build(limit+1)); size <= maxResultBytes {
			t.Fatalf("round %d: %d characters is the common length, but %d fit as well (%d bytes)", round, limit, limit+1, size)
		}
	}
}

func TestLargestFit(t *testing.T) {
	for _, tc := range []struct {
		name string
		most int
		fits func(int) bool
		want int
	}{
		{"all fit", 4000, func(int) bool { return true }, 4000},
		{"none fits", 4000, func(int) bool { return false }, 0},
		{"zero fits", 4000, func(n int) bool { return n == 0 }, 0},
		{"one fits", 4000, func(n int) bool { return n <= 1 }, 1},
		{"all but the last", 4000, func(n int) bool { return n < 4000 }, 3999},
		{"in the middle", 4000, func(n int) bool { return n <= 1234 }, 1234},
		{"no range", 0, func(int) bool { return true }, 0},
	} {
		if got := largestFit(tc.most, tc.fits); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCutRunes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 3, "hel"}, {"hello", 5, "hello"}, {"hello", 9, "hello"}, {"hello", 0, ""}, {"", 3, ""},
		{"приве́т", 5, "приве"}, {"日本語です", 2, "日本"}, {"a😀b", 2, "a😀"}, {"a‮b", 2, "a‮"},
	} {
		if got := cutRunes(tc.in, tc.n); got != tc.want {
			t.Errorf("cutRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// TestLongTextsAreCut: through the tools. A text over 4000 characters is cut and
// marked, one of exactly 4000 is not, the cut is at a character, and a result of
// many long messages keeps every message and fits.
func TestLongTextsAreCut(t *testing.T) {
	r := bobsRig(t)
	r.put(
		m{account: "personal", chat: bobChat, id: "L1", sender: bobChat, at: 1 * time.Second, text: strings.Repeat("я", 5000)},
		m{account: "personal", chat: bobChat, id: "L2", sender: bobChat, at: 2 * time.Second, text: strings.Repeat("я", 4000)},
		m{account: "personal", chat: bobChat, id: "L3", sender: bobChat, at: 3 * time.Second, text: "short"},
	)
	out := r.messages(map[string]any{"chat": bobChat})
	if len(out.Messages) != 3 {
		t.Fatalf("%d messages", len(out.Messages))
	}
	if x := out.Messages[0]; !x.Truncated || utf8.RuneCountInString(*x.Text) != 4000 || !utf8.ValidString(*x.Text) {
		t.Errorf("L1: truncated=%v, %d characters", x.Truncated, utf8.RuneCountInString(*x.Text))
	}
	if x := out.Messages[1]; x.Truncated || utf8.RuneCountInString(*x.Text) != 4000 {
		t.Errorf("L2: truncated=%v, %d characters", x.Truncated, utf8.RuneCountInString(*x.Text))
	}
	if x := out.Messages[2]; x.Truncated || *x.Text != "short" {
		t.Errorf("L3 = %+v", x)
	}
	// Three texts of that length are well inside what a result may be: nothing is cut
	// shorter than a text may be, and the agent is not told that something was.
	if len(out.Notes) != 0 {
		t.Errorf("notes %q", out.Notes)
	}
	// The same message, alone, is what get-message-context gives: up to 4000.
	if ctx := r.around(map[string]any{"chat": bobChat, "message_id": "L1", "before": 0, "after": 0}); len(ctx.Messages) != 1 ||
		!ctx.Messages[0].Truncated || utf8.RuneCountInString(*ctx.Messages[0].Text) != 4000 {
		t.Errorf("context: %+v", ctx.Messages)
	}
	if hits := r.search(map[string]any{"query": strings.Repeat("я", 10)}); len(hits.Results) != 2 || !hits.Results[1].Truncated {
		t.Errorf("search: %d results", len(hits.Results))
	}
}

// notesAllowance is what the notes of a result may weigh on top of the limit of the
// strings: the notes come after the cut and are not in its measure.
const notesAllowance = 2000

func TestResultsThatAreTooBigAreCutNotDropped(t *testing.T) {
	r := bobsRig(t)
	msgs := make([]m, 100)
	for i := range msgs {
		msgs[i] = m{account: "personal", chat: bobChat, id: fmt.Sprintf("M%03d", i), sender: bobChat, at: time.Duration(i) * time.Second,
			text: "needle " + strings.Repeat("слово ", 600)} // 3600 characters
	}
	r.put(msgs...)
	check := func(name string, n int, truncated []bool, notes []string, body string) {
		t.Helper()
		if len(truncated) != n {
			t.Fatalf("%s: %d messages, want %d", name, len(truncated), n)
		}
		for i, cut := range truncated {
			if !cut {
				t.Errorf("%s: message %d is not marked truncated", name, i)
			}
		}
		if len(body) > maxResultBytes+notesAllowance {
			t.Errorf("%s: %d bytes, more than %d", name, len(body), maxResultBytes)
		}
		if len(notes) != 1 || notes[0] != cutAdvice {
			t.Errorf("%s: notes %q, want the advice to ask for fewer messages", name, notes)
		}
	}
	flags := func(ms []MessageOut) []bool {
		out := make([]bool, len(ms))
		for i, x := range ms {
			out[i] = x.Truncated
		}
		return out
	}

	_, body := r.call("get-messages", map[string]any{"chat": bobChat, "limit": 100})
	var got MessagesOut
	mustDecode(t, body, &got)
	check("get-messages", 100, flags(got.Messages), got.Notes, body)

	_, body = r.call("search-messages", map[string]any{"query": "needle", "limit": 100})
	var found SearchOut
	mustDecode(t, body, &found)
	hits := make([]MessageOut, len(found.Results))
	for i, h := range found.Results {
		hits[i] = h.MessageOut
	}
	check("search-messages", 100, flags(hits), found.Notes, body)

	_, body = r.call("get-message-context", map[string]any{"chat": bobChat, "message_id": "M050", "before": 20, "after": 20})
	var ctx ContextOut
	mustDecode(t, body, &ctx)
	if len(ctx.Messages) != 41 {
		t.Fatalf("context has %d messages", len(ctx.Messages))
	}
	shown := make([]MessageOut, len(ctx.Messages))
	for i, c := range ctx.Messages {
		shown[i] = c.MessageOut
	}
	check("get-message-context", 41, flags(shown), ctx.Notes, body)
}

func mustDecode(t *testing.T, text string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), out); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
}

// TestNamesAreCut: a name, which other people write too, is at most maxName
// characters, in every field that has one, and is not marked: only a text is.
func TestNamesAreCut(t *testing.T) {
	long := strings.Repeat("ж", 1000)
	whole := strings.Repeat("ж", 200) // the limit of a name, as the tools' own words have it
	r := bobsRig(t)
	r.wa.People = map[string]string{bobChat: long}
	r.chat("personal", bobChat, bobPNJID, long, false)
	r.chat("personal", groupJID, "", long, true)
	r.put(
		m{account: "personal", chat: bobChat, id: "L1", sender: bobChat, at: time.Second, text: "needle", media: "document", name: long, quoted: long},
		m{account: "personal", chat: groupJID, id: "L2", sender: bobChat, at: 2 * time.Second, text: "needle"},
	)
	for _, x := range r.messages(map[string]any{"chat": bobChat}).Messages {
		if x.SenderName != whole || x.Media.Name != whole || x.QuotedID != whole || x.Truncated {
			t.Errorf("get-messages: %d characters of sender_name, %d of the file name, %d of quoted_id, truncated=%v",
				utf8.RuneCountInString(x.SenderName), utf8.RuneCountInString(x.Media.Name), utf8.RuneCountInString(x.QuotedID), x.Truncated)
		}
	}
	for _, h := range r.search(map[string]any{"query": "needle"}).Results {
		if h.ChatName != whole || h.SenderName != whole {
			t.Errorf("search: %d characters of chat_name, %d of sender_name", utf8.RuneCountInString(h.ChatName), utf8.RuneCountInString(h.SenderName))
		}
	}
	var chats ChatsOut
	r.ok("list-chats", nil, &chats)
	for _, c := range chats.Chats {
		if c.Name != whole {
			t.Errorf("list-chats: %d characters of the name of %s", utf8.RuneCountInString(c.Name), c.Chat)
		}
	}
}

// TestALongNameCostsTheTextsOnlyWhatANameMayBe: the names of a message are cut to the
// limit of a name before the result is measured, so a long one does not take from the
// texts more than that, and they are not cut while the result fits.
func TestALongNameCostsTheTextsOnlyWhatANameMayBe(t *testing.T) {
	text, name := strings.Repeat("я", 3000), strings.Repeat("ж", 1000)
	out := page("ж", make([]int, 8)...)
	for i := range out.Messages {
		s := text
		out.Messages[i].Text, out.Messages[i].SenderName, out.Messages[i].QuotedID = &s, name, name
		out.Messages[i].Media = &MediaOut{Type: "document", Name: name, Mime: name}
	}
	if capMessages(&out, refs(out.Messages)) {
		t.Error("something was cut below the limit of a text")
	}
	for i, x := range out.Messages {
		if x.Truncated || runes(x) != 3000 {
			t.Fatalf("message %d: text of %d characters, truncated=%v: cut for the names, which are cut to %d", i, runes(x), x.Truncated, maxName)
		}
		for _, s := range []string{x.SenderName, x.QuotedID, x.Media.Name, x.Media.Mime} {
			if got := utf8.RuneCountInString(s); got != maxName {
				t.Errorf("message %d: a name of %d characters", i, got)
			}
		}
	}
}

// TestResultSizeIsBounded: what other people write cannot make a result heavier
// than the limit, whichever characters it is made of, in every tool that shows it.
func TestResultSizeIsBounded(t *testing.T) {
	bodies := map[string]string{
		"angle brackets": strings.Repeat("<", 4000), // 6 bytes each, escaped
		"NULs":           strings.Repeat("\x00", 4000),
		"emoji":          strings.Repeat("\U0001F600", 4000),
		"cyrillic":       strings.Repeat("ж", 4000),
		"cyrillic 150":   strings.Repeat("ж", 150), // what a chat of long messages looks like
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "personal")
			msgs := make([]m, 200)
			for i := range msgs {
				msgs[i] = m{account: "personal", chat: bobChat, id: "M" + strconv.Itoa(i), sender: bobChat, at: time.Duration(i) * time.Second, text: "needle " + body}
			}
			r.put(msgs...)
			for _, c := range []struct {
				tool string
				args map[string]any
			}{
				{"get-messages", map[string]any{"chat": bobChat, "limit": 200}},
				{"search-messages", map[string]any{"query": "needle", "limit": 100}},
				{"get-message-context", map[string]any{"chat": bobChat, "message_id": "M100", "before": 20, "after": 20}},
			} {
				if res, text := r.call(c.tool, c.args); res.IsError || len(text) > maxResultBytes+notesAllowance {
					t.Errorf("%s: isError=%v, a result of %d bytes", c.tool, res.IsError, len(text))
				}
			}
		})
	}
	t.Run("names of chats", func(t *testing.T) {
		r := newRig(t, "personal")
		for i := 0; i < 200; i++ {
			r.chat("personal", strconv.Itoa(200000000000+i)+"@lid", "", strings.Repeat("<", 200), false)
		}
		var chats ChatsOut
		res, text := r.call("list-chats", map[string]any{"limit": 200})
		if res.IsError || len(text) > maxResultBytes+notesAllowance {
			t.Fatalf("list-chats: isError=%v, a result of %d bytes", res.IsError, len(text))
		}
		mustDecode(t, text, &chats)
		if len(chats.Chats) != 200 || len(chats.Notes) != 1 || chats.Notes[0] != namesCutAdvice {
			t.Errorf("%d chats, notes %q: the names were cut and the agent is told to ask for fewer chats", len(chats.Chats), chats.Notes)
		}
	})
}

// TestACutByTheSizeOfTheResultIsSaid: when texts are cut shorter than a text may be
// only because the result is big, the agent is told so and what to do. When only the
// limit of a text cut one, which the text's `truncated` says, there is no note.
func TestACutByTheSizeOfTheResultIsSaid(t *testing.T) {
	r := newRig(t, "personal")
	msgs := make([]m, 200)
	for i := range msgs {
		msgs[i] = m{account: "personal", chat: bobChat, id: "M" + strconv.Itoa(i), sender: bobChat, at: time.Duration(i) * time.Second, text: strings.Repeat("ж", 4000)}
	}
	r.put(msgs...)
	page := r.messages(map[string]any{"chat": bobChat, "limit": 200})
	cutShort := 0
	for _, x := range page.Messages {
		if x.Truncated && utf8.RuneCountInString(*x.Text) < maxText {
			cutShort++
		}
	}
	if cutShort == 0 {
		t.Fatal("the premise is texts cut shorter than their own limit")
	}
	if len(page.Notes) != 1 || page.Notes[0] != cutAdvice {
		t.Errorf("%d texts are cut shorter than %d characters, and the notes are %q", cutShort, maxText, page.Notes)
	}
	// What the agent is told to do about it is what can be done: fewer messages, or one at a time.
	for _, want := range []string{"ask for fewer messages (limit)", "get-message-context before=0 after=0"} {
		if !strings.Contains(cutAdvice, want) {
			t.Errorf("the advice lacks %q: %s", want, cutAdvice)
		}
	}
	if !strings.Contains(namesCutAdvice, "ask for fewer chats (limit)") {
		t.Errorf("the advice of the names of chats: %s", namesCutAdvice)
	}
	// The same texts in a page that fits: cut to 4000 or not, but not for the size.
	small := r.messages(map[string]any{"chat": bobChat, "limit": 3})
	if len(small.Notes) != 0 {
		t.Errorf("a page of 3 has the notes %q", small.Notes)
	}
}

// TestAMediaTypeOfAnyLengthIsBounded: the source bounds the media type of a message,
// but a row that is in the archive already, or one that got past, must not make a
// result as big as its sender likes.
func TestAMediaTypeOfAnyLengthIsBounded(t *testing.T) {
	r := newRig(t, "personal")
	r.putRow(archive.Row{Account: "personal", Chat: bobChat, ID: "M1", Sender: bobChat, TS: base, Text: "look",
		MediaType: "image", MediaMime: strings.Repeat("a", 87000), MediaSize: 5})
	res, text := r.call("get-messages", map[string]any{"chat": bobChat})
	if res.IsError || len(text) > 8000 {
		t.Errorf("isError=%v, %d bytes for one message", res.IsError, len(text))
	}
	var out MessagesOut
	mustDecode(t, text, &out)
	if got := utf8.RuneCountInString(out.Messages[0].Media.Mime); got != maxName {
		t.Errorf("the media type is %d characters, want it cut to %d", got, maxName)
	}
}

// putRow stores a message as the row says, with the strings as they are.
func (r *rig) putRow(row archive.Row) {
	r.t.Helper()
	row.Raw = []byte("raw")
	err := r.db.Tx(context.Background(), func(tx *archive.Tx) error {
		if err := tx.Upsert(row); err != nil {
			return err
		}
		return tx.TouchChat(archive.ChatUpd{Account: row.Account, JID: row.Chat, LastMessageTS: row.TS})
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// TestTagCharactersAreNotShown: the characters U+E0000 to U+E007F spell ASCII and show
// as nothing, so a message may say something to a model that the user, who reads the
// same message in WhatsApp, cannot see. They are taken out of every string that other
// people wrote.
func TestTagCharactersAreNotShown(t *testing.T) {
	r := newRig(t, "personal")
	hidden := string([]rune{0xE0063, 0xE0061, 0xE006C, 0xE006C})
	r.wa.People = map[string]string{bobChat: "Bob" + hidden}
	r.put(m{account: "personal", chat: bobChat, id: "M1", sender: bobChat, text: "hello " + hidden, media: "document", name: "report" + hidden + ".pdf", quoted: "Q" + hidden})
	r.chat("personal", bobChat, "", "Bob"+hidden, false)
	r.chat("personal", groupJID, "", "Group"+hidden, true)
	r.put(m{account: "personal", chat: groupJID, id: "G1", sender: bobChat, at: time.Second, text: "hello group"})
	// Carol has no name in the contacts, so her chat is called by the name the archive has.
	r.chat("personal", carolJID, "", "Carol"+hidden, false)
	r.putRow(archive.Row{Account: "personal", Chat: carolJID, ID: "C1", Sender: carolJID, TS: base.Add(2 * time.Second), Text: "hello carol",
		MediaType: "image", MediaMime: "image/" + hidden, MediaSize: 5})
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"get-messages", map[string]any{"chat": bobChat}},
		{"get-messages", map[string]any{"chat": groupJID}},
		{"get-messages", map[string]any{"chat": carolJID}},
		{"search-messages", map[string]any{"query": "hello"}},
		{"get-message-context", map[string]any{"chat": bobChat, "message_id": "M1"}},
		{"list-chats", nil},
	} {
		res, text := r.call(c.tool, c.args)
		if res.IsError {
			t.Fatalf("%s: %s", c.tool, text)
		}
		for _, ch := range text {
			if ch >= 0xE0000 && ch <= 0xE007F {
				t.Errorf("%s: the answer has the tag character U+%X", c.tool, ch)
				break
			}
		}
	}
	// What is left of a text that had them is the text that the user sees.
	if out := r.messages(map[string]any{"chat": bobChat}); *out.Messages[0].Text != "hello " || out.Messages[0].SenderName != "Bob" ||
		out.Messages[0].Media.Name != "report.pdf" || out.Messages[0].QuotedID != "Q" {
		t.Errorf("%+v", out.Messages[0])
	}
	if out := r.messages(map[string]any{"chat": carolJID}); out.Messages[0].Media.Mime != "image/" {
		t.Errorf("the media type: %q", out.Messages[0].Media.Mime)
	}
	var chats ChatsOut
	r.ok("list-chats", nil, &chats)
	for _, c := range chats.Chats {
		if c.Chat == carolJID && c.Name != "Carol" || c.Chat == groupJID && c.Name != "Group" {
			t.Errorf("the name of %s is %q", c.Chat, c.Name)
		}
	}
}

func TestPlain(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"plain text", "plain text"}, {"Привет, мир", "Привет, мир"},
		{"a\U000E0041b", "ab"}, {"\U000E0000\U000E007F", ""}, {"\U000DFFFF\U000E0080", "\U000DFFFF\U000E0080"}, // the edges of the range
		{"👍🏽 ‮rtl​zw", "👍🏽 ‮rtl​zw"}, // what is seen, or is a mark of the text, stays
	} {
		if got := plain(tc.in); got != tc.want {
			t.Errorf("plain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
