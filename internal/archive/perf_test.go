package archive

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixture is a big archive that the performance tests share: 100 000
// messages in 300 chats of two accounts (a third of them in one chat of
// alice's, a long history to page through), with Russian-looking words of a
// skewed vocabulary (the commonest is in a third of the messages, most are in
// a handful), so that a search can be rare, ordinary or hit a very common word.
const (
	perfChats = 300
	perfVocab = 3000
	perfBig   = 0 // the chat that has a third of the messages
)

const perfMessages = 100_000

// perfAccount is the account of chat c.
func perfAccount(c int) string {
	if c%4 == 3 {
		return "bob"
	}
	return "alice"
}

type perfFixture struct {
	once  sync.Once
	dir   string
	db    *DB
	err   error
	took  time.Duration
	words []string // the vocabulary, the commonest first
}

var perf perfFixture

// get builds the fixture on the first call.
func (p *perfFixture) get(t testing.TB) *DB {
	t.Helper()
	switch {
	case testing.Short():
		t.Skip("the 100 000 message fixture takes seconds to build")
	case raceBuild:
		// Several tens of times slower: building it would take minutes, and
		// the timings would mean nothing. The same queries run in the other
		// tests, on small archives, under the detector.
		t.Skip("the race detector is too slow for the big fixture")
	}
	p.once.Do(p.build)
	if p.err != nil {
		t.Fatal(p.err)
	}
	return p.db
}

func (p *perfFixture) close() {
	if p.db != nil {
		p.db.Close()
	}
	if p.dir != "" {
		removeAll(p.dir)
	}
}

func (p *perfFixture) build() {
	start := time.Now()
	var err error
	if p.dir, err = os.MkdirTemp("", "archive-perf"); err != nil {
		p.err = err
		return
	}
	if p.db, err = Open(filepath.Join(p.dir, "archive.db")); err != nil {
		p.err = err
		return
	}
	p.err = p.fill()
	p.took = time.Since(start)
}

func (p *perfFixture) fill() error {
	rng := rand.New(rand.NewPCG(1, 2))
	syll := strings.Fields("ка ма ре ло ни ту по са ви де зо бе лу ра шу ко ги не фа ми то ру ди")
	for i := 0; i < perfVocab; i++ {
		var w strings.Builder
		for range 2 + rng.IntN(3) {
			w.WriteString(syll[rng.IntN(len(syll))])
		}
		p.words = append(p.words, w.String())
	}
	for _, nick := range []string{"alice", "bob"} {
		if err := p.db.AddAccount(bg, nick); err != nil {
			return err
		}
	}
	return p.db.Tx(bg, func(tx *Tx) error {
		for c := range perfChats {
			if _, err := tx.tx.ExecContext(bg, `INSERT INTO chats(account, jid, name, last_message_ts) VALUES (?, ?, ?, ?)`,
				perfAccount(c), perfChat(c), "Chat "+fmt.Sprint(c), 1_700_000_000+c); err != nil {
				return err
			}
		}
		stmt, err := tx.tx.PrepareContext(bg, `INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text, raw)
		  VALUES (?, ?, ?, ?, ?, ?, ?, randomblob(150))`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range perfMessages {
			c := rng.IntN(perfChats)
			if rng.IntN(3) == 0 {
				c = perfBig
			}
			var text []string
			for range 3 + rng.IntN(8) {
				u := rng.Float64()
				text = append(text, p.words[int(u*u*u*perfVocab)])
			}
			ts := 1_600_000_000 + i*30 + rng.IntN(60) // mostly forward, a little shuffled
			if _, err := stmt.ExecContext(bg, perfAccount(c), perfChat(c), fmt.Sprintf("id%d", i), perfChat(c), i%5 == 0, ts,
				strings.Join(text, " ")); err != nil {
				return err
			}
		}
		return nil
	})
}

func perfChat(c int) string { return fmt.Sprintf("%d@lid", 100000+c) }
