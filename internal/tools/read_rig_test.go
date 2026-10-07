package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/exomind-tmi/whatsapp-mcp/internal/archive"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
	"github.com/exomind-tmi/whatsapp-mcp/internal/tools/toolstest"
	"github.com/exomind-tmi/whatsapp-mcp/internal/wa"
)

// The people of the tests of the read tools.
const (
	bobPN    = "70000000100"
	bobLID   = "200000000100"
	bobChat  = bobLID + "@lid"
	bobPNJID = bobPN + "@s.whatsapp.net"
	carolPN  = "70000000101"
	carolJID = carolPN + "@s.whatsapp.net"
	ownJID   = "70000000001@s.whatsapp.net"
	groupJID = "120363000000000001@g.us"
)

// base is the time of the first message of the tests: 2026-10-07 12:00:00 UTC,
// which is 15:00 in the zone of TestMain.
var base = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// iso is what a tool shows of base plus d.
func iso(d time.Duration) string { return base.Add(d).In(time.Local).Format(time.RFC3339) }

// syncLog is a log the server may write while the test reads it.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rig is the tools over a real archive and a fake WA, and a client of them.
type rig struct {
	t   *testing.T
	db  *archive.DB
	wa  *toolstest.WA
	cs  *mcp.ClientSession
	log *syncLog
}

// newRig makes a rig with the accounts, connected unless a test says otherwise.
func newRig(t *testing.T, nicks ...string) *rig {
	t.Helper()
	db, err := archive.Open(filepath.Join(testutil.TempDir(t), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	w := &toolstest.WA{}
	for _, n := range nicks {
		if err := db.AddAccount(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		w.Accs = append(w.Accs, wa.AccountInfo{Nick: n, Status: wa.StatusConnected})
	}
	r := &rig{t: t, db: db, wa: w, log: &syncLog{}}
	r.cs = serve(t, Deps{WA: w, Archive: db, Log: slog.New(slog.NewTextHandler(r.log, nil))})
	return r
}

// serve connects a client to a server of the tools over d.
func serve(t *testing.T, d Deps) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := NewServer("v0.0.1", d).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// status sets the status of an account.
func (r *rig) status(nick string, s wa.Status) {
	for i := range r.wa.Accs {
		if r.wa.Accs[i].Nick == nick {
			r.wa.Accs[i].Status = s
		}
	}
}

// m is a message to put in the archive.
type m struct {
	account, chat, id, sender string
	fromMe                    bool
	at                        time.Duration // after base
	text                      string
	media                     string // the type, for a media message
	name                      string // its file name
	quoted                    string
}

// put stores the messages and makes their chats known, as the receiver does.
func (r *rig) put(msgs ...m) {
	r.t.Helper()
	for _, x := range msgs {
		err := r.db.Tx(context.Background(), func(tx *archive.Tx) error {
			row := archive.Row{
				Account: x.account, Chat: x.chat, ID: x.id, Sender: x.sender, FromMe: x.fromMe,
				TS: base.Add(x.at), Text: x.text, MediaType: x.media, MediaName: x.name, QuotedID: x.quoted,
				Raw: []byte("raw of " + x.id),
			}
			if x.media != "" {
				row.MediaMime, row.MediaSize = "application/pdf", 99
			}
			if err := tx.Upsert(row); err != nil {
				return err
			}
			return tx.TouchChat(archive.ChatUpd{Account: x.account, JID: x.chat, LastMessageTS: base.Add(x.at)})
		})
		if err != nil {
			r.t.Fatal(err)
		}
	}
}

// chat names a chat, with its number and kind.
func (r *rig) chat(account, jid, pn, name string, group bool) {
	r.t.Helper()
	err := r.db.Tx(context.Background(), func(tx *archive.Tx) error {
		return tx.TouchChat(archive.ChatUpd{Account: account, JID: jid, PN: pn, Name: name, IsGroup: group})
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// call calls the tool; the result and its text.
func (r *rig) call(name string, args map[string]any) (*mcp.CallToolResult, string) {
	r.t.Helper()
	res, err := r.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		r.t.Fatal(err)
	}
	return res, ResultText(res)
}

// ok calls the tool, which must succeed, and decodes what it said into out.
func (r *rig) ok(name string, args map[string]any, out any) {
	r.t.Helper()
	res, text := r.call(name, args)
	if res.IsError {
		r.t.Fatalf("%s %v: error: %s", name, args, text)
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		r.t.Fatalf("%s: %v\n%s", name, err, text)
	}
}

// fails calls the tool, which must fail, and returns what it said.
func (r *rig) fails(name string, args map[string]any) string {
	r.t.Helper()
	res, text := r.call(name, args)
	if !res.IsError {
		r.t.Fatalf("%s %v succeeded: %s", name, args, text)
	}
	return text
}

func (r *rig) messages(args map[string]any) MessagesOut {
	r.t.Helper()
	var out MessagesOut
	r.ok("get-messages", args, &out)
	return out
}

func (r *rig) search(args map[string]any) SearchOut {
	r.t.Helper()
	var out SearchOut
	r.ok("search-messages", args, &out)
	return out
}

// ids are the ids of the messages of a page.
func ids(ms []MessageOut) []string {
	out := make([]string, len(ms))
	for i, x := range ms {
		out[i] = x.ID
	}
	return out
}

// seconds puts n messages in a chat, one a second from `from`: id <prefix><i>.
func (r *rig) series(account, chat, prefix string, from, n int, fromMe bool) {
	r.t.Helper()
	msgs := make([]m, n)
	for i := range msgs {
		msgs[i] = m{account: account, chat: chat, id: prefix + strconv.Itoa(from+i), sender: bobChat,
			fromMe: fromMe, at: time.Duration(from+i) * time.Second, text: "text " + prefix + strconv.Itoa(from+i)}
	}
	r.put(msgs...)
}
