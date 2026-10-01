// Package sqlitedb holds what every SQLite database in the state directory
// shares: archive.db and whatsmeow's store.db must address their files the
// same way. It also registers the driver, so whoever opens a database here
// does not depend on another package having imported it.
package sqlitedb

import (
	"database/sql"
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
const Pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=secure_delete(1)&_txlock=immediate"

// Open opens the database at path; query holds the driver's DSN parameters.
func Open(path, query string) (*sql.DB, error) {
	return sql.Open("sqlite", DSN(path, query))
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
