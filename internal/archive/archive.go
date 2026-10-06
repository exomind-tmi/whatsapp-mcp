// Package archive owns archive.db: the schema and its migrations, the
// accounts and, later, the message archive itself.
//
// There is one pool, the writer (MaxOpenConns(1)). A read-only pool for the
// message queries is to come, so that reads do not queue behind history writes;
// for now only a few small statements per manage-accounts call run, and none is
// needed.
package archive

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
)

// ErrNewerSchema means archive.db was written by a newer whatsapp-mcp.
var ErrNewerSchema = errors.New("archive.db was created by a newer whatsapp-mcp")

type DB struct{ w *sql.DB }

// Open opens archive.db, creating it if needed, brings its schema up to
// SchemaVersion and refuses a newer one. foreign_keys is a per-connection
// setting, so it lives in the DSN: every connection the pool opens gets it,
// and without it DeleteAccount would cascade nothing.
func Open(path string) (*DB, error) {
	if err := migrate(path); err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	w, err := sqlitedb.Open(path, "_pragma=foreign_keys(1)&"+sqlitedb.Pragmas)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	return &DB{w: w}, nil
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

func (db *DB) Close() error { return db.w.Close() }
