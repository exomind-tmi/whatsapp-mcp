package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
	"github.com/exomind-tmi/whatsapp-mcp/internal/testutil"
)

// openAt opens archive.db at p and closes it when the test ends; the
// cleanup runs before testutil.TempDir's, which was registered earlier.
func openAt(t *testing.T, p string) *DB {
	t.Helper()
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	return openAt(t, filepath.Join(testutil.TempDir(t), "archive.db"))
}

func userVersion(t *testing.T, db *DB) int {
	t.Helper()
	var v int
	if err := db.w.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenCreatesSchema(t *testing.T) {
	db := openTemp(t)
	if v := userVersion(t, db); v != SchemaVersion || SchemaVersion != 1 {
		t.Fatalf("user_version = %d, SchemaVersion = %d; want both 1", v, SchemaVersion)
	}
	for _, name := range []string{
		"accounts", "chats", "messages", "messages_chat_ts", "messages_fts",
		"messages_ai", "messages_au", "messages_ad", "history_queue",
	} {
		var n int
		if err := db.w.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = ?", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s: %d schema objects, want 1", name, n)
		}
	}
	for _, name := range []string{"accounts", "chats", "messages", "history_queue"} {
		var strict bool
		if err := db.w.QueryRow("SELECT strict FROM pragma_table_list WHERE name = ?", name).Scan(&strict); err != nil {
			t.Fatal(err)
		}
		if !strict {
			t.Errorf("%s is not STRICT", name)
		}
	}
}

// TestPools: one writer connection, so that every write is serialised and a
// transaction cannot wait for a second connection of its own; several readers,
// read-only; and a reopened archive finds its readers working at once.
func TestPools(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db := openAt(t, p)
	if n := db.w.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("the writer has %d connections, want 1", n)
	}
	if n := db.r.Stats().MaxOpenConnections; n != readers || readers < 2 {
		t.Errorf("the reader pool has %d connections, want %d (and more than one)", n, readers)
	}
	var queryOnly int
	if err := db.r.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Errorf("reader query_only = %d, %v; want 1", queryOnly, err)
	}
	if err := db.w.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 0 {
		t.Errorf("writer query_only = %d, %v; want 0", queryOnly, err)
	}

	if err := db.AddAccount(bg, "alice"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = openAt(t, p)
	if accs, err := db.Accounts(bg); err != nil || len(accs) != 1 {
		t.Errorf("after reopening the reader sees %v, %v", accs, err)
	}
}

// TestTxTakesTheWriteLockAtBegin: a transaction takes the write lock when it
// begins, not at its first write (sqlitedb.Pragmas has _txlock=immediate, a
// parameter of the DSN that no PRAGMA shows). One that read first and wrote
// later would fail with SQLITE_BUSY at once, which the busy timeout does not
// retry, if another connection had committed in between. That a second
// connection cannot write while the transaction is still only open is the
// visible side of it.
func TestTxTakesTheWriteLockAtBegin(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db := openAt(t, p)
	other, err := sqlitedb.Open(p, "_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	err = db.Tx(bg, func(tx *Tx) error {
		if _, err := other.Exec(`INSERT INTO accounts(nick, created_at) VALUES ('x', 1)`); err == nil {
			t.Error("another connection wrote while a transaction that had not written yet was open")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsSchemaAndData(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "archive.db")
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAccount(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db = openAt(t, p)
	if v := userVersion(t, db); v != SchemaVersion {
		t.Fatalf("user_version = %d after reopen, want %d", v, SchemaVersion)
	}
	got, err := db.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Nick != "alice" {
		t.Fatalf("accounts after reopen = %+v, want alice", got)
	}
}

// TestOpenEscapesPath only checks that Open goes through sqlitedb; the
// escaping itself is tested there.
func TestOpenEscapesPath(t *testing.T) {
	dir := filepath.Join(testutil.TempDir(t), "a#b")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(dir, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(filepath.Join(dir, "archive.db")); err != nil {
		t.Fatalf("archive.db not created in place: %v", err)
	}
}

// rawAt opens the file at p without Open's migration, to set up or inspect
// it, and closes it when the test ends.
func rawAt(t *testing.T, p string) *sql.DB {
	t.Helper()
	raw, err := sqlitedb.Open(p, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return raw
}

// rawState is what Open must leave as it was when it refuses a file.
func rawState(t *testing.T, raw *sql.DB) (version, objects int) {
	t.Helper()
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&objects); err != nil {
		t.Fatal(err)
	}
	return version, objects
}

// mustNotOpen fails the test if Open succeeds, closing what it opened first:
// a database left open on Windows would fail TempDir's cleanup and hide the
// real failure.
func mustNotOpen(t *testing.T, p string) error {
	t.Helper()
	db, err := Open(p)
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded, want an error")
	}
	return err
}

func TestOpenRejectsUnknownSchema(t *testing.T) {
	tests := []struct {
		version int
		newer   bool // ErrNewerSchema; otherwise another error, not a panic
	}{
		{SchemaVersion + 1, true},
		{99, true},
		{-1, false}, // user_version is signed
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("v%d", tt.version), func(t *testing.T) {
			p := filepath.Join(testutil.TempDir(t), "archive.db")
			raw := rawAt(t, p)
			if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d; CREATE TABLE future(x)", tt.version)); err != nil {
				t.Fatal(err)
			}

			if err := mustNotOpen(t, p); errors.Is(err, ErrNewerSchema) != tt.newer {
				t.Errorf("err = %v, want ErrNewerSchema: %v", err, tt.newer)
			}
			if v, n := rawState(t, raw); v != tt.version || n != 1 {
				t.Errorf("after Open: user_version = %d, %d schema objects; want %d and 1, untouched", v, n, tt.version)
			}
		})
	}
}

// TestMigrate swaps the v1 step for variants, so it checks the mechanism
// that a real v2 will rely on.
func TestMigrate(t *testing.T) {
	tests := []struct {
		name    string
		step    string
		wantErr bool // and then nothing of the step may remain
		check   string
		want    int
	}{
		{
			name:    "a failing step leaves no half schema",
			step:    schemaV1 + `CREATE TABLE ???;`,
			wantErr: true,
		},
		{
			name: "a step breaking a reference is rolled back",
			// The checks are off while migrating, so only foreign_key_check
			// can catch it.
			step:    schemaV1 + `INSERT INTO chats(account, jid) VALUES ('ghost', 'c@s.whatsapp.net');`,
			wantErr: true,
		},
		{
			// SQLite's way to alter a table; with foreign_keys on, the DROP
			// would delete every chat of every account.
			name: "rebuilding a referenced table keeps its children",
			step: schemaV1 + `
INSERT INTO accounts(nick, created_at) VALUES ('alice', 1);
INSERT INTO chats(account, jid) VALUES ('alice', 'c@s.whatsapp.net');
CREATE TABLE accounts_new (nick TEXT PRIMARY KEY, jid TEXT, created_at INTEGER NOT NULL) STRICT, WITHOUT ROWID;
INSERT INTO accounts_new SELECT * FROM accounts;
DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;`,
			check: `SELECT count(*) FROM chats`,
			want:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := migrations[0]
			migrations[0] = tt.step
			t.Cleanup(func() { migrations[0] = saved })
			p := filepath.Join(testutil.TempDir(t), "archive.db")

			if tt.wantErr {
				mustNotOpen(t, p)
				if v, n := rawState(t, rawAt(t, p)); v != 0 || n != 0 {
					t.Fatalf("after a failed migration: user_version = %d, %d schema objects; want 0 and 0", v, n)
				}
				return
			}
			db := openAt(t, p)
			if v := userVersion(t, db); v != SchemaVersion {
				t.Fatalf("user_version = %d, want %d", v, SchemaVersion)
			}
			var got int
			if err := db.w.QueryRow(tt.check).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("%s = %d, want %d", tt.check, got, tt.want)
			}
		})
	}
}

// TestForeignKeysOnEveryConnection drops idle connections, so each
// statement runs on a fresh one: the pragma must come with the DSN, not be
// set once after Open.
func TestForeignKeysOnEveryConnection(t *testing.T) {
	db := openTemp(t)
	db.w.SetMaxIdleConns(0)
	for range 3 {
		var on int
		if err := db.w.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil {
			t.Fatal(err)
		}
		if on != 1 {
			t.Fatal("foreign_keys is off on a new connection")
		}
	}
	if _, err := db.w.Exec(`INSERT INTO chats(account, jid) VALUES ('ghost', 'x@s.whatsapp.net')`); err == nil {
		t.Fatal("a chat of a missing account was inserted: foreign keys are not enforced")
	}
}
