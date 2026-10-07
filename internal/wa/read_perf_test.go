package wa_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// The big archive of the performance test: 100 000 messages of two accounts, in 300
// private chats (a third of the messages in one chat of personal's, a long history to
// page through) and 20 groups of up to 200 people, with Russian-looking words of a
// skewed vocabulary, so that a search can be rare, ordinary or hit a very common word.
// The Manager is the real one, with the LID pairs and the contacts of the people.
const (
	perfMessages = 100_000
	perfPrivate  = 300
	perfGroups   = 20
	perfPeople   = 200
	perfVocab    = 3000
)

// perfBudget is the most that a call may take, the median of several, on a machine that
// is slower than the one the numbers in the log are from, and busy with other tests.
const perfBudget = 250 * time.Millisecond

func perfPrivateChat(c int) string { return fmt.Sprintf("2000%08d@lid", c) }
func perfGroup(g int) string       { return fmt.Sprintf("1203630000%08d@g.us", g) }
func perfPerson(p int) string      { return fmt.Sprintf("3000%08d@lid", p) }

func perfAccount(i int) string {
	if i%10 < 7 {
		return "personal"
	}
	return "work"
}

// fillPerf puts the messages in the archive, and the people in the stores of the
// account's client; it returns the vocabulary, the commonest word first, and the id
// of a message from the middle of the long history.
func fillPerf(t *testing.T, g *wa.Rig) (words []string, middle string) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(1, 2))
	syll := strings.Fields("ка ма ре ло ни ту по са ви де зо бе лу ра шу ко ги не фа ми то ру ди")
	words = make([]string, perfVocab)
	for i := range words {
		var w strings.Builder
		for range 2 + rng.IntN(3) {
			w.WriteString(syll[rng.IntN(len(syll))])
		}
		words[i] = w.String()
	}

	// The people: each has a LID, a number and a name in the contacts.
	cli := g.Client("personal")
	var pairs []store.LIDMapping
	var contacts []store.ContactEntry
	for p := range perfPeople {
		pn := types.NewJID(fmt.Sprintf("7001%07d", p), types.DefaultUserServer)
		pairs = append(pairs, store.LIDMapping{LID: types.NewJID(strings.TrimSuffix(perfPerson(p), "@lid"), types.HiddenUserServer), PN: pn})
		contacts = append(contacts, store.ContactEntry{JID: pn, FirstName: fmt.Sprintf("Имя%d", p), FullName: fmt.Sprintf("Имя%d Фамилия%d", p, p)})
	}
	if err := cli.Store.LIDs.PutManyLIDMappings(ctx, pairs); err != nil {
		t.Fatal(err)
	}
	if err := cli.Store.Contacts.PutAllContactNames(ctx, contacts); err != nil {
		t.Fatal(err)
	}

	err := g.Archive().Tx(ctx, func(tx *archive.Tx) error {
		for c := range perfPrivate {
			jid := perfPrivateChat(c)
			if err := tx.TouchChat(archive.ChatUpd{Account: perfAccount(c), JID: jid, PN: fmt.Sprintf("7000%07d@s.whatsapp.net", c),
				Name: fmt.Sprintf("Чат %d", c), LastMessageTS: time.Unix(1_700_000_000+int64(c), 0)}); err != nil {
				return err
			}
		}
		for p := range 10 { // some of the people have a chat of their own, which the archive knows by their push name
			if err := tx.TouchChat(archive.ChatUpd{Account: "personal", JID: perfPerson(p), PN: fmt.Sprintf("7001%07d@s.whatsapp.net", p),
				Name: fmt.Sprintf("Push %d", p), LastMessageTS: time.Unix(1_700_000_000+int64(p), 0)}); err != nil {
				return err
			}
		}
		for gr := range perfGroups {
			if err := tx.TouchChat(archive.ChatUpd{Account: perfAccount(gr), JID: perfGroup(gr), Name: fmt.Sprintf("Группа %d", gr), IsGroup: true,
				LastMessageTS: time.Unix(1_700_000_000+int64(gr), 0)}); err != nil {
				return err
			}
		}
		inHistory := 0
		for i := range perfMessages {
			row := archive.Row{ID: fmt.Sprintf("id%d", i), FromMe: i%5 == 0, TS: time.Unix(1_600_000_000+int64(i)*30+int64(rng.IntN(60)), 0), Raw: make([]byte, 150)}
			switch {
			case rng.IntN(3) == 0: // the long history
				row.Account, row.Chat, row.Sender = "personal", perfPrivateChat(0), perfPrivateChat(0)
				if inHistory++; inHistory == perfMessages/6 {
					middle = row.ID
				}
			case rng.IntN(5) == 0:
				gr := rng.IntN(perfGroups)
				row.Account, row.Chat, row.Sender = perfAccount(gr), perfGroup(gr), perfPerson(rng.IntN(perfPeople))
			default:
				c := 1 + rng.IntN(perfPrivate-1)
				row.Account, row.Chat, row.Sender = perfAccount(c), perfPrivateChat(c), perfPrivateChat(c)
			}
			var text []string
			for range 3 + rng.IntN(8) {
				u := rng.Float64()
				text = append(text, words[int(u*u*u*perfVocab)])
			}
			row.Text = strings.Join(text, " ")
			if err := tx.Upsert(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return words, middle
}

// timed runs f n times and logs the shortest, the median and the longest; it
// returns the median.
func timed(t *testing.T, name string, n int, f func()) time.Duration {
	t.Helper()
	ds := make([]time.Duration, n)
	for i := range ds {
		start := time.Now()
		f()
		ds[i] = time.Since(start)
	}
	slices.Sort(ds)
	t.Logf("%-62s min %-9v median %-9v max %v", name, ds[0].Round(10*time.Microsecond), ds[n/2].Round(10*time.Microsecond), ds[n-1].Round(10*time.Microsecond))
	return ds[n/2]
}

// TestReadToolsOnABigArchive asks the four tools, as an agent does, of 100 000
// messages, with the real Manager and the real stores behind them, and logs what
// each call takes. It holds each to a quarter of a second, which a lost index or a
// query per message would not keep (the numbers are in the log: they are meant to be a
// few milliseconds to a few tens, the commonest word of all being the slowest, at
// about 70). The fixture is skipped under -short and under the race detector, which is
// many times slower.
func TestReadToolsOnABigArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("the 100 000 message archive takes seconds to build")
	}
	if raceBuild {
		t.Skip("the race detector is too slow for the big archive")
	}
	g := wa.NewRig(t)
	g.Connect("personal")
	g.Connect("work")
	start := time.Now()
	words, middle := fillPerf(t, g)
	t.Logf("%d messages, built in %v", perfMessages, time.Since(start).Round(time.Millisecond))
	a := newAgent(t, g)

	type call struct {
		name, tool string
		args       map[string]any
	}
	person := "Имя7 Фамилия7"
	calls := []call{
		{"list-chats", "list-chats", nil},
		{"list-chats, limit 200", "list-chats", map[string]any{"limit": 200}},
		{"list-chats, a name of the archive", "list-chats", map[string]any{"query": "Чат 17"}},
		{"list-chats, a name that only the contacts have", "list-chats", map[string]any{"query": "Фамилия7"}},
		{"list-chats, a number", "list-chats", map[string]any{"query": "+7 000 000 0042"}},
		{"get-messages, the newest 50 of the chat with a third of the messages", "get-messages", map[string]any{"chat": perfPrivateChat(0), "account": "personal"}},
		{"get-messages, 200 of it", "get-messages", map[string]any{"chat": perfPrivateChat(0), "account": "personal", "limit": 200}},
		{"get-messages, 50 from the middle of it, by a time", "get-messages", map[string]any{"chat": perfPrivateChat(0), "before": "2020-09-30T00:00:00+03:00"}},
		{"get-messages, a group of 200 people, 200 messages", "get-messages", map[string]any{"chat": perfGroup(0), "limit": 200}},
		{"get-messages, a small chat, the account found by the chat", "get-messages", map[string]any{"chat": perfPrivateChat(42)}},
		{"get-message-context, the middle of the big chat", "get-message-context", map[string]any{"chat": perfPrivateChat(0), "account": "personal", "message_id": middle, "before": 20, "after": 20}},
		{"search, the commonest word (a third of all)", "search-messages", map[string]any{"query": words[0]}},
		{"search, a common word", "search-messages", map[string]any{"query": words[10], "limit": 100}},
		{"search, an ordinary word", "search-messages", map[string]any{"query": words[500]}},
		{"search, a rare word", "search-messages", map[string]any{"query": words[2900]}},
		{"search, two common words", "search-messages", map[string]any{"query": words[0] + " " + words[1]}},
		{"search, the commonest word, one account", "search-messages", map[string]any{"query": words[0], "account": "work"}},
		{"search, the commonest word, one chat", "search-messages", map[string]any{"query": words[0], "chat": perfPrivateChat(5)}},
		{"search, the commonest word, a person by the name in the contacts", "search-messages", map[string]any{"query": words[0], "sender": person}},
		{"search, the commonest word, a person by the number", "search-messages", map[string]any{"query": words[0], "sender": "+7 001 000 0007"}},
		{"search, the commonest word, a window of time", "search-messages", map[string]any{"query": words[0], "after": "2020-09-14T00:00:00+03:00", "before": "2020-10-14T00:00:00+03:00"}},
	}
	// Every call starts with the roster; the list of the accounts counts their archives, which it does not.
	timed(t, "the roster, which every call asks for first", 7, func() { g.Manager().Roster(context.Background()) })
	timed(t, "(the list of accounts, with the size of each archive)", 7, func() { g.Manager().Accounts(context.Background()) })
	for _, c := range calls {
		var size int
		d := timed(t, c.name, 7, func() {
			text, isErr := a.call(c.tool, c.args)
			if isErr {
				t.Fatalf("%s: %s", c.name, text)
			}
			size = len(text)
		})
		t.Logf("%-62s %d bytes", "", size)
		if d > perfBudget {
			t.Errorf("%s took %v, more than %v", c.name, d, perfBudget)
		}
	}
}
