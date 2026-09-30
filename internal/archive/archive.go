// Package archive owns archive.db. In M0 it only guards the schema version;
// tables and migrations arrive in M2.
package archive

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the newest PRAGMA user_version this build understands.
const SchemaVersion = 0

// ErrNewerSchema means archive.db was written by a newer whatsapp-mcp.
var ErrNewerSchema = errors.New("archive.db was created by a newer whatsapp-mcp")

type DB struct{ w *sql.DB }

// Open opens archive.db, creating it if needed, and refuses a newer schema.
func Open(path string) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(path,
		"_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate"))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	var v int
	if err := w.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if v > SchemaVersion {
		w.Close()
		return nil, fmt.Errorf("%w: schema v%d, this build knows up to v%d; install a newer whatsapp-mcp (downgrade is not supported)",
			ErrNewerSchema, v, SchemaVersion)
	}
	return &DB{w: w}, nil
}

// dsn builds a proper file: URI. SQLite parses the DSN as a URI, so '#', '%'
// and '?' in the path (all legal in a Windows user name) must be escaped, or
// they cut the path short and the database lands somewhere else.
func dsn(path, query string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // file:///C:/x — SQLite drops the slash before a drive letter
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: query}).String()
}

func (db *DB) Close() error { return db.w.Close() }
