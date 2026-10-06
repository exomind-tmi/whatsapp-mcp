package archive

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// best is the time of the fastest of n runs of f: the first one reads the
// file into the cache, and what is of interest is what the query costs. A run
// that is under the clock's resolution is timed again as the average of a
// hundred.
func best(n int, f func()) time.Duration {
	var d time.Duration
	for i := range n {
		start := time.Now()
		f()
		e := time.Since(start)
		if e < 2*time.Millisecond {
			start = time.Now()
			for range 100 {
				f()
			}
			e = time.Since(start) / 100
		}
		if i == 0 || e < d {
			d = e
		}
	}
	return d
}

// logTime reports what a query took; the log and the benchmarks are where the
// numbers are read. It does not hold the query to a limit: one that fits a
// laptop failed on a runner that was busy (the same code, several times slower),
// and what a limit could catch, a query that has lost its index or its full-text
// table, the pinned query plans catch on any machine, whatever else it is doing.
func logTime(t *testing.T, name string, d time.Duration) {
	t.Helper()
	t.Logf("%-40s %v", name, d)
}

func TestPlansOnABigArchive(t *testing.T) {
	checkPlans(t, perf.get(t))
}

func TestSearchOnABigArchive(t *testing.T) {
	db := perf.get(t)
	t.Logf("%d messages, built in %v", perfMessages, perf.took)
	w := perf.words
	cases := []struct {
		name string
		q    SearchQuery
	}{
		{"the commonest word (a third of all)", SearchQuery{Query: w[0]}},
		{"a common word", SearchQuery{Query: w[10]}},
		{"an ordinary word", SearchQuery{Query: w[500]}},
		{"a rare word", SearchQuery{Query: w[2900]}},
		{"two common words", SearchQuery{Query: w[0] + " " + w[1]}},
		{"a part of a word", SearchQuery{Query: w[10][2:]}},
		{"the commonest word, one account", SearchQuery{Query: w[0], Accounts: []string{"bob"}}},
		{"the commonest word, one chat", SearchQuery{Query: w[0], Chat: perfChat(5)}},
		{"the commonest word, in the big chat", SearchQuery{Query: w[0], Chat: perfChat(perfBig)}},
		{"a common word, a window of time", SearchQuery{Query: w[10], After: at(1_600_500_000), Before: at(1_601_000_000)}},
	}
	for _, tt := range cases {
		tt.q.Limit = 20
		var hits []Hit
		d := best(3, func() {
			var err error
			if hits, err = db.Search(bg, tt.q); err != nil {
				t.Fatal(err)
			}
		})
		logTime(t, tt.name, d)
		if len(hits) == 0 {
			t.Errorf("%s: no hits", tt.name)
		}
	}
}

// TestSearchAgreesWithAScan checks the answers, not only the speed: what the
// index finds, in the order it is returned, is what a scan of the text finds.
func TestSearchAgreesWithAScan(t *testing.T) {
	db := perf.get(t)
	for _, word := range []string{perf.words[0], perf.words[500], perf.words[2900]} {
		hits, err := db.Search(bg, SearchQuery{Query: word, Limit: 25, Accounts: []string{"alice"}})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.r.QueryContext(bg, `SELECT msg_id FROM messages WHERE account = 'alice' AND instr(text, ?) > 0
		  ORDER BY ts DESC, id DESC LIMIT 25`, word)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			want = append(want, id)
		}
		rows.Close()
		if got := hitIDs(hits); got != strings.Join(want, ",") {
			t.Errorf("%q: hits %s\nwant %v", word, got, want)
		}
	}
}

func TestQueriesOnABigArchive(t *testing.T) {
	db := perf.get(t)
	var midID string
	if err := db.r.QueryRow(`SELECT msg_id FROM messages WHERE account = 'alice' AND chat_jid = ?
	  ORDER BY ts, id LIMIT 1 OFFSET 8000`, perfChat(perfBig)).Scan(&midID); err != nil {
		t.Fatal(err)
	}
	mid, err := db.Message(bg, "alice", perfChat(perfBig), midID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueuePush(bg, "alice", "h1", []byte("n")); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		name string
		f    func() error
	}{
		{"the newest page of the big chat", func() error {
			_, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: perfChat(perfBig), Limit: 50})
			return err
		}},
		{"a page from the middle of its history", func() error {
			p, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: perfChat(perfBig), Before: mid.Cursor(), Limit: 50})
			if err == nil && (len(p.Messages) != 50 || p.Next.IsZero()) {
				err = fmt.Errorf("page = %d messages, next %v", len(p.Messages), p.Next)
			}
			return err
		}},
		{"around a message of the middle", func() error {
			m, err := db.Around(bg, "alice", perfChat(perfBig), midID, 5, 5)
			if err == nil && len(m) != 11 {
				err = fmt.Errorf("%d messages", len(m))
			}
			return err
		}},
		{"a message", func() error { _, err := db.Message(bg, "alice", perfChat(perfBig), midID); return err }},
		{"a message with its raw", func() error { _, err := db.MessageWithRaw(bg, "alice", perfChat(perfBig), midID); return err }},
		{"the accounts of a chat", func() error {
			a, err := db.AccountsForChat(bg, perfChat(7))
			if err == nil && len(a) != 1 {
				err = fmt.Errorf("accounts = %v", a)
			}
			return err
		}},
		{"the list of chats by a name", func() error {
			c, err := db.Chats(bg, ChatQuery{Query: "chat 12", Limit: 50})
			if err == nil && len(c) == 0 {
				err = fmt.Errorf("no chats")
			}
			return err
		}},
		{"the next notification", func() error {
			_, ok, err := db.QueueNext(bg, "alice")
			if err == nil && !ok {
				err = fmt.Errorf("nothing queued")
			}
			return err
		}},
		{"the stuck notifications", func() error { _, err := db.QueueStuck(bg, "alice"); return err }},
		{"the size of the accounts", func() error { _, err := db.Accounts(bg); return err }},
	}
	for _, s := range steps {
		d := best(3, func() {
			if err := s.f(); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
		})
		logTime(t, s.name, d)
	}
}

func BenchmarkSearch(b *testing.B) {
	db := perf.get(b)
	w := perf.words
	for _, bb := range []struct {
		name string
		q    SearchQuery
	}{
		{"commonest word", SearchQuery{Query: w[0]}},
		{"common word", SearchQuery{Query: w[10]}},
		{"ordinary word", SearchQuery{Query: w[500]}},
		{"rare word", SearchQuery{Query: w[2900]}},
		{"two common words", SearchQuery{Query: w[0] + " " + w[1]}},
		{"commonest word in one chat", SearchQuery{Query: w[0], Chat: perfChat(5)}},
	} {
		bb.q.Limit = 20
		b.Run(bb.name, func(b *testing.B) {
			for range b.N {
				if _, err := db.Search(bg, bb.q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMessagesPage(b *testing.B) {
	db := perf.get(b)
	var mid Message
	var id string
	if err := db.r.QueryRow(`SELECT msg_id FROM messages WHERE chat_jid = ? ORDER BY ts, id LIMIT 1 OFFSET 8000`, perfChat(perfBig)).Scan(&id); err != nil {
		b.Fatal(err)
	}
	mid, _ = db.Message(bg, "alice", perfChat(perfBig), id)
	for range b.N {
		if _, err := db.Messages(bg, MsgQuery{Account: "alice", Chat: perfChat(perfBig), Before: mid.Cursor(), Limit: 50}); err != nil {
			b.Fatal(err)
		}
	}
}
