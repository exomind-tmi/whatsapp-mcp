// Package archive owns archive.db: the schema and its migrations, the
// accounts, the message archive and the queue of history notifications.
//
// There are two pools. All writes go through the writer (MaxOpenConns(1), so
// every write is serialised and a transaction cannot deadlock against a second
// connection of its own), and all reads through the reader pool, which is
// read-only and has several connections: a tool that reads does not queue
// behind a history sync writing its chunks, WAL lets both run at once.
// Writes of a message and its chat go through DB.Tx; the one rule of a Tx is
// that nothing inside it may call the DB's own methods: a write would wait for
// the writer connection the Tx holds, and a read would not see what the Tx has
// written, which is not committed yet.
package archive

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
)

// ErrNewerSchema means archive.db was written by a newer whatsapp-mcp.
var ErrNewerSchema = errors.New("archive.db was created by a newer whatsapp-mcp")

// readers is the size of the reader pool: enough for the tools of a few
// sessions to read at once, few enough that each keeps its page cache warm.
const readers = 4

type DB struct{ w, r *sql.DB }

// Open opens archive.db, creating it if needed, brings its schema up to
// SchemaVersion and refuses a newer one. foreign_keys is a per-connection
// setting, so it lives in the DSN: every connection the pool opens gets it,
// and without it DeleteAccount would cascade nothing. The writer connects
// first, so that the WAL and its shared memory file are in place for the
// readers: a read-only connection can open a WAL database only if it finds
// them or is allowed to create them.
func Open(path string) (*DB, error) {
	if err := migrate(path); err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	w, err := sqlitedb.Open(path, "_pragma=foreign_keys(1)&"+sqlitedb.Pragmas)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sqlitedb.Open(path, sqlitedb.ReadOnly)
	if err != nil {
		w.Close()
		return nil, err
	}
	// Idle connections are kept: a connection reopened per query would parse
	// the schema again each time.
	r.SetMaxOpenConns(readers)
	r.SetMaxIdleConns(readers)
	db := &DB{w: w, r: r}
	for _, pool := range []*sql.DB{w, r} {
		if err := pool.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	}
	return db, nil
}

// migrate applies the missing steps in one transaction together with the
// new user_version (SQLite keeps it in the database header, which the
// transaction covers), so a crash leaves either the old schema or the new
// one. The version is read inside the transaction: _txlock=immediate takes
// the write lock first, so a second opener cannot migrate the same steps.
//
// It runs on a pool of its own with foreign_keys off, because the pragma is
// a no-op inside a transaction, and with it on, the usual SQLite way to alter
// a table (create a new one, copy, DROP the old, rename) would cascade the
// DROP's implicit DELETE into every chat and message of every account.
// foreign_key_check then stands in for the checks that were off.
func migrate(path string) error {
	m, err := sqlitedb.Open(path, "_pragma=foreign_keys(0)&"+sqlitedb.Pragmas)
	if err != nil {
		return err
	}
	defer m.Close()
	m.SetMaxOpenConns(1)

	tx, err := m.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	var v int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	switch {
	case v < 0: // user_version is signed; slicing migrations with it would panic
		return fmt.Errorf("unknown schema v%d: archive.db is damaged or was not written by whatsapp-mcp", v)
	case v > SchemaVersion:
		return fmt.Errorf("%w: schema v%d, this build knows up to v%d; install a newer whatsapp-mcp (downgrade is not supported)",
			ErrNewerSchema, v, SchemaVersion)
	case v == SchemaVersion:
		return nil
	}
	for i, step := range migrations[v:] {
		if _, err := tx.Exec(step); err != nil {
			return fmt.Errorf("migrate to v%d: %w", v+i+1, err)
		}
	}
	var table, parent string
	switch err := tx.QueryRow(`SELECT "table", parent FROM pragma_foreign_key_check LIMIT 1`).Scan(&table, &parent); {
	case err == nil:
		return fmt.Errorf("migrate to v%d: %s references a missing row of %s", SchemaVersion, table, parent)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	// PRAGMA takes no bound parameters; the value is our own constant.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the readers first and the writer last: SQLite checkpoints the WAL
// and deletes it when the last connection closes, and a read-only connection
// cannot, so one that closed last would leave the WAL and its -shm behind.
// Queries and transactions still running are the caller's to stop first
// (the daemon stops the workers and the server before it closes the archive).
func (db *DB) Close() error {
	rerr := db.r.Close()
	return errors.Join(rerr, db.w.Close())
}
