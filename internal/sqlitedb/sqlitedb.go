// Package sqlitedb holds what every SQLite database in the state directory
// shares: archive.db and whatsmeow's store.db must address their files the
// same way. It also registers the driver, so whoever opens a database here
// does not depend on another package having imported it.
package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers "sqlite"; the query parameters (_pragma, _txlock) are its own
)

// Pragmas are the connection settings both databases share; callers add
// foreign_keys, which the archive's migrator needs off. WAL lets readers run
// beside the writer, busy_timeout makes a second connection wait for the
// lock instead of failing, and _txlock=immediate takes the write lock at
// BEGIN: a deferred transaction that upgrades to write later can fail with
// SQLITE_BUSY at once, which busy_timeout does not cover. secure_delete
// zeroes freed pages: manage-accounts remove promises the archive and the
// device keys are gone from this computer, not merely unlinked from the
// b-tree and still readable in the file. The -wal file keeps its copies
// until a checkpoint, so a remove ends with one on each database.
// journal_size_limit cuts the -wal file back to 16 MiB when it is next
// reused: the automatic checkpoints never shrink it, so a WAL that a VACUUM
// blew up to the size of the database while a reader held off the remove's
// TRUNCATE checkpoint would otherwise stay that size for good. The limit sits
// above the ~4 MiB an automatic checkpoint (1000 pages) lets it grow to.
const Pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=secure_delete(1)&_pragma=journal_size_limit(16777216)&_txlock=immediate"

// ReadOnly is the DSN of a pool that only reads a database somebody else has
// already created and put in WAL mode: it sees every commit of the writer and
// never blocks it. mode=ro is what keeps it from writing (SQLite itself
// refuses); query_only is the second lock on the same door, so that a write
// fails with a plain error even if a driver update stopped honouring the URI
// parameter. There is no _txlock: BEGIN IMMEDIATE is a write, and a read-only
// connection cannot make one. Unlike Pragmas it sets no journal_mode: the
// writer's is stored in the file, and changing it needs a write.
const ReadOnly = "mode=ro&_pragma=busy_timeout(10000)&_pragma=query_only(1)"

// Open opens the database at path; query holds the driver's DSN parameters.
func Open(path, query string) (*sql.DB, error) {
	return sql.Open("sqlite", DSN(path, query))
}

// ErrCheckpointBusy means a reader kept a checkpoint from emptying the WAL.
var ErrCheckpointBusy = errors.New("a reader kept the checkpoint from finishing")

// Checkpoint moves the WAL into the database file and truncates it, which a remove
// needs: secure_delete zeroes the pages in the file, but the WAL holds the rows as
// they were written until then. A checkpoint that a reader holds off does not fail
// the statement: it waits busy_timeout, and the answer is a row whose first column
// is 1, which ExecContext would never look at.
func Checkpoint(ctx context.Context, db *sql.DB) error {
	var busy, frames, moved int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &moved); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("%w (%d of %d frames moved)", ErrCheckpointBusy, moved, frames)
	}
	return nil
}

// DSN builds a proper file: URI. SQLite parses the DSN as a URI, so '#', '%'
// and '?' in the path (all legal in a Windows user name) must be escaped, or
// they cut the path short and the database lands somewhere else. A UNC path
// (a redirected profile) becomes file:////server/share/..., which SQLite
// opens as such.
func DSN(path, query string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // file:///C:/x — SQLite drops the slash before a drive letter
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: query}).String()
}
