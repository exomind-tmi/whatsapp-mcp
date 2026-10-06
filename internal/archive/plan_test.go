package archive

import (
	"strings"
	"testing"
)

// explain is the EXPLAIN QUERY PLAN of a query, one string for its steps.
func explain(t *testing.T, db *DB, query string, args []any) string {
	t.Helper()
	rows, err := db.r.QueryContext(bg, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(steps, "; ")
}

// planCase is a query whose plan is a promise: what is slow at a million
// messages is a plan, not a query, and a plan changes with a SQLite upgrade
// or an edit of an index without a test failing. Each must contain every one
// of has and none of not.
type planCase struct {
	name  string
	query string
	args  []any
	has   []string
	not   []string
}

func planCases() []planCase {
	chatPages := []struct {
		name string
		page chatPage
	}{
		{"the newest page", chatPage{account: "alice", chat: pnChat, limit: 5}},
		{"older than a cursor", chatPage{account: "alice", chat: pnChat, cursor: Cursor{5, 6}, limit: 5}},
		{"older than a cursor and not before a time", chatPage{account: "alice", chat: pnChat, cursor: Cursor{5, 6}, since: at(1), limit: 5}},
		{"newer than a cursor (Around)", chatPage{account: "alice", chat: pnChat, cursor: Cursor{5, 6}, newer: true, limit: 5}},
	}
	var cases []planCase
	for _, p := range chatPages {
		q, args := p.page.sql()
		cases = append(cases, planCase{
			name: "messages of a chat, " + p.name, query: q, args: args,
			// The order (ts, id) is the index's own: no sort, no scan of the table.
			has: []string{"SEARCH m USING INDEX messages_chat_ts (account=? AND chat_jid=?"},
			not: []string{"TEMP B-TREE", "SCAN"},
		})
	}
	search, sargs := searchSQL(SearchQuery{Accounts: []string{"alice", "bob"}, Chat: pnChat, Senders: []string{"a"}, After: at(1), Before: at(9), Limit: 5}, `"abc"`)
	return append(cases,
		planCase{"a message by its key", messageSQL, []any{"alice", pnChat, "10"},
			[]string{"SEARCH m USING INDEX sqlite_autoindex_messages_1 (account=? AND chat_jid=? AND msg_id=?)"}, []string{"SCAN"}},
		planCase{"a message with its raw", messageRawSQL, []any{"alice", pnChat, "10"},
			[]string{"SEARCH m USING INDEX sqlite_autoindex_messages_1"}, []string{"SCAN"}},
		planCase{"a download recorded", setMediaSQL, []any{"/f", nil, "alice", pnChat, "10"},
			[]string{"SEARCH messages USING INDEX sqlite_autoindex_messages_1 (account=? AND chat_jid=? AND msg_id=?)"}, []string{"SCAN"}},
		planCase{"a search: the index finds, the table is read by rowid, the chat's name by its key", search, sargs,
			[]string{"SCAN f VIRTUAL TABLE INDEX", "SEARCH m USING INTEGER PRIMARY KEY (rowid=?)", "SEARCH c USING PRIMARY KEY (account=? AND jid=?)"},
			[]string{"SCAN messages", "SCAN m USING"}},
		// The queue and the accounts of a chat are read in every call of a
		// tool, so they must never grow with the messages: the queue is
		// looked up by its account, and a chat by its own table.
		planCase{"the next notification", queueNextSQL, []any{"alice", 5},
			[]string{"SEARCH history_queue USING INDEX sqlite_autoindex_history_queue_1 (account=?)"}, []string{"messages", "chats"}},
		planCase{"the stuck notifications", queueStuckSQL, []any{"alice", 5},
			[]string{"SEARCH history_queue USING INDEX sqlite_autoindex_history_queue_1 (account=?)"}, []string{"messages", "chats"}},
		planCase{"the accounts of a chat", accountsForChatSQL, []any{pnChat},
			[]string{"chats"}, []string{"messages"}},
		planCase{"the merge reads the chat's messages by their index", mergeMessagesSQL, []any{"alice", pnChat, lidChat},
			[]string{"SEARCH messages USING INDEX messages_chat_ts (account=? AND chat_jid=?)"}, []string{"SCAN messages", "TEMP B-TREE"}},
		planCase{"and deletes them by it", deleteChatMessagesSQL, []any{"alice", pnChat},
			[]string{"SEARCH messages USING COVERING INDEX messages_chat_ts (account=? AND chat_jid=?)"}, []string{"SCAN"}},
		planCase{"the merge of the chat row", mergeChatSQL, []any{"alice", pnChat, lidChat},
			[]string{"SEARCH chats USING PRIMARY KEY (account=? AND jid=?)"}, []string{"SCAN"}},
	)
}

// checkPlans fails for every case whose plan lacks what it promises.
func checkPlans(t *testing.T, db *DB) {
	t.Helper()
	for _, c := range planCases() {
		got := explain(t, db, c.query, c.args)
		for _, want := range c.has {
			if !strings.Contains(got, want) {
				t.Errorf("%s: plan %q lacks %q", c.name, got, want)
			}
		}
		for _, bad := range c.not {
			if strings.Contains(got, bad) {
				t.Errorf("%s: plan %q has %q", c.name, got, bad)
			}
		}
	}
}

func TestPlans(t *testing.T) {
	checkPlans(t, searchFixture(t))
}
