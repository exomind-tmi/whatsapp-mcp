package wa

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
)

// openStore opens store.db, whatsmeow's device store holding the keys of
// every linked account, and brings its schema up to date. It goes through
// sqlitedb so the path is escaped like archive.db's; foreign_keys is on for
// every connection, which Upgrade checks and the cascade from a deleted
// device to its sessions and keys needs. The pool stays unbounded, as in
// sqlstore.New; _txlock=immediate and busy_timeout serialise the writers.
func openStore(ctx context.Context, path string, log waLog.Logger) (*sqlstore.Container, error) {
	db, err := sqlitedb.Open(path, "_pragma=foreign_keys(1)&"+sqlitedb.Pragmas)
	if err != nil {
		return nil, err
	}
	// NewWithDB, unlike New, does not upgrade by itself (container.go:74-87).
	c := sqlstore.NewWithDB(db, "sqlite", log)
	if err := c.Upgrade(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return c, nil
}
