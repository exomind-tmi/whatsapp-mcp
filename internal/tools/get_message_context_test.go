package tools

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

func (r *rig) around(args map[string]any) ContextOut {
	r.t.Helper()
	var out ContextOut
	r.ok("get-message-context", args, &out)
	return out
}

func contextIDs(out ContextOut) []string {
	ids := make([]string, len(out.Messages))
	for i, x := range out.Messages {
		ids[i] = x.ID
	}
	return ids
}

func targets(out ContextOut) []string {
	var ids []string
	for _, x := range out.Messages {
		if x.Target {
			ids = append(ids, x.ID)
		}
	}
	return ids
}

func TestGetMessageContext(t *testing.T) {
	r := bobsRig(t)
	r.series("personal", bobChat, "M", 1, 20, false)
	for _, tc := range []struct {
		name          string
		before, after any
		id            string
		want          []string
		note          string
	}{
		{"the default is five and five", nil, nil, "M10", []string{"M5", "M6", "M7", "M8", "M9", "M10", "M11", "M12", "M13", "M14", "M15"}, ""},
		{"none on either side", 0, 0, "M10", []string{"M10"}, ""},
		{"only before", 2, 0, "M10", []string{"M8", "M9", "M10"}, ""},
		{"only after", 0, 3, "M10", []string{"M10", "M11", "M12", "M13"}, ""},
		{"the start of the chat", nil, nil, "M2", []string{"M1", "M2", "M3", "M4", "M5", "M6", "M7"}, ""},
		{"the end of the chat", nil, nil, "M19", []string{"M14", "M15", "M16", "M17", "M18", "M19", "M20"}, ""},
		{"exactly the most is not lowered", 20, 0, "M20", prefixed("M", 1, 20), ""},
		{"more than the most is the most", 100, 0, "M20", prefixed("M", 1, 20), "before lowered to the most there is, 20"},
		{"a negative is none", -3, 1, "M10", []string{"M10", "M11"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"chat": bobChat, "message_id": tc.id}
			if tc.before != nil {
				args["before"] = tc.before
			}
			if tc.after != nil {
				args["after"] = tc.after
			}
			out := r.around(args)
			if got := contextIDs(out); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if got := targets(out); !slices.Equal(got, []string{tc.id}) {
				t.Errorf("the target is marked on %v, want %s alone", got, tc.id)
			}
			if tc.note == "" && len(out.Notes) != 0 || tc.note != "" && (len(out.Notes) != 1 || out.Notes[0] != tc.note) {
				t.Errorf("notes = %q, want %q", out.Notes, tc.note)
			}
		})
	}
}

func prefixed(prefix string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, prefix+strconv.Itoa(i))
	}
	return out
}

// TestGetMessageContextOfEqualTimes: the neighbours of a message are those next to
// it in the order of the chat, also when they were all sent in the same second.
func TestGetMessageContextOfEqualTimes(t *testing.T) {
	r := bobsRig(t)
	for _, id := range []string{"A", "B", "C", "D", "E"} {
		r.put(m{account: "personal", chat: bobChat, id: id, sender: bobChat, text: "same second " + id})
	}
	out := r.around(map[string]any{"chat": bobChat, "message_id": "C", "before": 1, "after": 1})
	if got := contextIDs(out); !slices.Equal(got, []string{"B", "C", "D"}) {
		t.Errorf("got %v, want B C D", got)
	}
}

func TestGetMessageContextChatAndAccount(t *testing.T) {
	r := twoAccounts(t)
	r.put(m{account: "personal", chat: bobChat, id: "B2", sender: bobChat, at: time.Second, text: "second"})
	// Bob's chat by his number, and with no account: it is the one that has it.
	out := r.around(map[string]any{"chat": bobPNJID, "message_id": "B1", "before": 0})
	if out.Account != "personal" || out.Chat != bobChat || !slices.Equal(contextIDs(out), []string{"B1", "B2"}) {
		t.Errorf("out = %+v", out)
	}
	if text := r.fails("get-message-context", map[string]any{"chat": "70000000102@s.whatsapp.net", "message_id": "D1"}); !strings.Contains(text, "pass account") {
		t.Errorf("a chat in both accounts: %s", text)
	}
	out = r.around(map[string]any{"chat": "70000000102@s.whatsapp.net", "message_id": "D2", "account": "work"})
	if out.Account != "work" || !slices.Equal(contextIDs(out), []string{"D2"}) {
		t.Errorf("the account asked for: %+v", out)
	}
}

func TestGetMessageContextOfAMessageThatIsNotThere(t *testing.T) {
	r := twoAccounts(t)
	r.put(m{account: "work", chat: bobChat, id: "ELSEWHERE", sender: bobChat, text: "in another account"})
	for name, args := range map[string]map[string]any{
		"an id nobody has":            {"chat": bobChat, "message_id": "NOPE", "account": "personal"},
		"the id of another chat":      {"chat": bobChat, "message_id": "C1", "account": "personal"},
		"the id of another account's": {"chat": bobChat, "message_id": "ELSEWHERE", "account": "personal"},
		"a hostile id":                {"chat": bobChat, "message_id": marker, "account": "personal"},
	} {
		text := r.fails("get-message-context", args)
		if text != errNoSuchMessage.Error() || strings.Contains(text, marker) {
			t.Errorf("%s: %s", name, text)
		}
	}
}

func TestGetMessageContextShowsRevokedAndSenderNames(t *testing.T) {
	r := bobsRig(t)
	r.wa.People = map[string]string{bobChat: "Bob"}
	r.series("personal", bobChat, "M", 1, 3, false)
	if err := r.db.Tx(context.Background(), func(tx *archive.Tx) error {
		return tx.Revoke(archive.Revoke{Account: "personal", Chat: bobChat, ID: "M2", RevokedAt: base.Add(time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	out := r.around(map[string]any{"chat": bobChat, "message_id": "M1"})
	if len(out.Messages) != 3 {
		t.Fatalf("out = %+v", out)
	}
	if x := out.Messages[1]; !x.Revoked || x.RevokedAt != iso(time.Minute) || x.Text == nil || *x.Text != "text M2" || x.Target {
		t.Errorf("the revoked message = %+v", x)
	}
	if x := out.Messages[0]; x.SenderName != "Bob" || !x.Target {
		t.Errorf("the target = %+v", x)
	}
}

// TestGetMessageContextAsksTheArchive: the window asked for reaches the archive
// clamped; the archive's own errors are not the agent's.
func TestGetMessageContextAsksTheArchive(t *testing.T) {
	arc := &toolstest.Archive{}
	w := &toolstest.WA{Accs: []wa.AccountInfo{{Nick: "personal", Status: wa.StatusConnected}}}
	r := &rig{t: t, wa: w}
	r.cs = serve(t, Deps{WA: w, Archive: arc})
	r.around(map[string]any{"chat": bobChat, "message_id": "M1", "before": 50, "after": 0})
	r.around(map[string]any{"chat": bobChat, "message_id": "M1"})
	want := []toolstest.Around{
		{Account: "personal", Chat: bobChat, ID: "M1", Before: 20, After: 0},
		{Account: "personal", Chat: bobChat, ID: "M1", Before: 5, After: 5},
	}
	if got := arc.Queries().Arounds; !slices.Equal(got, want) {
		t.Errorf("the archive was asked %+v, want %+v", got, want)
	}
}
