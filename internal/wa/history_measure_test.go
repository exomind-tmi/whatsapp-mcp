package wa

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
)

// TestHistoryChunkCost measures what a transaction of an import costs for a few
// chunk sizes, which is the time the writer is held and a message that comes live
// waits: it is how historyChunk was chosen. It writes into a real archive.db, with the
// full-text index, the raw message of each row and, for a chat filed under a LID, the
// merge that apply makes for each message. It is not a test of anything and takes
// seconds, so it runs only when asked:
//
//	HISTORY_CHUNK_MEASURE=1 go test -run TestHistoryChunkCost -v ./internal/wa/
func TestHistoryChunkCost(t *testing.T) {
	if os.Getenv("HISTORY_CHUNK_MEASURE") == "" {
		t.Skip("a measurement: set HISTORY_CHUNK_MEASURE=1 to run it")
	}
	const rounds = 7
	for _, kind := range []struct {
		name string
		chat Chat
	}{
		{"a chat under its number", Chat{JID: bobChat}},
		{"a chat under its LID, the number known", Chat{JID: bobLID + "@lid", PN: bobChat}},
	} {
		for _, size := range []int{50, 100, 250, 500, 1000, 2000, 5000} {
			x := newHx(t, size)
			x.m.mu.Lock()
			a := x.m.accounts["personal"]
			x.m.mu.Unlock()
			cli := x.cli("personal")
			var took []time.Duration
			seq := 0
			for range rounds {
				run := &importRun{m: x.m, a: a, cli: cli, ctx: x.m.ctx, item: archive.QueueItem{ID: 1}}
				for range size {
					seq++
					id := fmt.Sprintf("M%d", seq)
					msg := text(fmt.Sprintf("a message of the history that is about a hundred characters long, number %d, of no importance", seq))
					info := incoming(id, pnJID(bobPN))
					info.Timestamp = t0.Add(time.Duration(seq) * time.Second)
					op := Classify(info, msg).In("personal", kind.chat.JID)
					run.batch = append(run.batch, histWrite{chat: kind.chat, op: op})
				}
				start := time.Now()
				if err := run.flush(false); err != nil {
					t.Fatal(err)
				}
				took = append(took, time.Since(start))
			}
			slices.Sort(took)
			median := took[len(took)/2]
			t.Logf("%-42s %5d messages in a transaction: median %8v, %6.1f us per message", kind.name, size,
				median.Round(10*time.Microsecond), float64(median.Microseconds())/float64(size))
		}
	}
}
