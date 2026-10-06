package tools

import (
	"encoding/json"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// TestManageAccountsListHistoryMissing: part of the history that could not be
// imported comes out of list in the fields that are there, reason and next_step: the
// reason is the account's (wa's words), and the next step tells the agent to tell
// the user, as no call can bring the history back. The golden schema stays as it was.
func TestManageAccountsListHistoryMissing(t *testing.T) {
	const reason = "1 part of the history sync could not be imported: some older messages are missing (download or decoding failed)"
	for _, tc := range []struct {
		name string
		accs []wa.AccountInfo
		want string
	}{
		{"connected", []wa.AccountInfo{{Nick: "a", Status: wa.StatusConnected, Reason: reason, HistoryStuck: 1}},
			"tell the user that part of the older messages of a could not be imported (see reason); the daemon log has the details"},
		{"reconnecting", []wa.AccountInfo{{Nick: "a", Status: wa.StatusReconnecting, Reason: reason, HistoryStuck: 1}},
			"tell the user that part of the older messages of a could not be imported (see reason); the daemon log has the details"},
		// An account that has to be linked again is told that, and not the other.
		{"needs_link", []wa.AccountInfo{{Nick: "a", Status: wa.StatusNeedsLink, Reason: "device unlinked", HistoryStuck: 1}},
			"manage-accounts action=add account_id=a — re-link, the message archive is kept"},
		{"nothing missing", []wa.AccountInfo{{Nick: "a", Status: wa.StatusConnected}}, ""},
		{"two accounts", []wa.AccountInfo{
			{Nick: "a", Status: wa.StatusConnected, Reason: reason, HistoryStuck: 1}, {Nick: "b", Status: wa.StatusConnected, Reason: reason, HistoryStuck: 3}},
			"tell the user that part of the older messages of a could not be imported (see reason); the daemon log has the details; " +
				"tell the user that part of the older messages of b could not be imported (see reason); the daemon log has the details"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, text := call(t, connect(t, &toolstest.WA{Accs: tc.accs}), map[string]any{"action": "list"})
			var out ManageOut
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatal(err)
			}
			if out.NextStep != tc.want || len(out.Accounts) != len(tc.accs) {
				t.Errorf("next_step = %q, want %q", out.NextStep, tc.want)
			}
			for i, a := range tc.accs {
				if out.Accounts[i].Reason != a.Reason {
					t.Errorf("reason of %s = %q, want %q", a.Nick, out.Accounts[i].Reason, a.Reason)
				}
			}
		})
	}
}
