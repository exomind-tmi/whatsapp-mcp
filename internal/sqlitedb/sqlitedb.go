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
