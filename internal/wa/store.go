package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/exomind-tmi/whatsapp-mcp/internal/sqlitedb"
)

// deviceStore is store.db: whatsmeow's container of the devices, and the
// handle on the file that its container does not give out.
type deviceStore struct {
	*sqlstore.Container
	db *sql.DB
}

// forget is what follows the deletion of a device, as the plan 6.5 promises that
// nothing of it stays on this computer. It deletes the privacy tokens of devices
// that are no longer there, which the deletion of a device leaves (see below), and
// then empties store.db's WAL. secure_delete zeroes the pages of a deleted device's
// keys, but the WAL keeps the pages as they were written until a checkpoint moves
// them into the file and truncates it (sqlitedb.Checkpoint). Both steps are tried,
// whatever happens to the other, and their errors are joined; a failure of either
// only leaves what the next start's sweep (load) and the automatic checkpoints
// clean up.
//
// Deleting a device cascades to all that has a foreign key on it, except two
// tables of whatsmeow's schema: whatsmeow_privacy_tokens, which holds the token
// and JID of each chat partner the device has talked to, keyed by the device's
// JID, and whatsmeow_lid_map, a map of LIDs to numbers that all devices share, the
// device's own among them. The tokens go here. The map stays: whatsmeow caches it
// in memory and writes a mapping only when the cache does not have it, so a row
// deleted here would not come back until a restart, and the number of a device
// that is linked again would be missing from the file; the plan (6.5) lists it as
// what a remove leaves.
func (s *deviceStore) forget(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM whatsmeow_privacy_tokens WHERE our_jid NOT IN (SELECT jid FROM whatsmeow_device)`)
	if err != nil {
		err = fmt.Errorf("delete the privacy tokens of deleted devices: %w", err)
	}
	return errors.Join(err, sqlitedb.Checkpoint(ctx, s.db))
}

// openStore opens store.db, whatsmeow's device store holding the keys of
// every linked account, and brings its schema up to date. It goes through
// sqlitedb so the path is escaped like archive.db's; foreign_keys is on for
// every connection, which Upgrade checks and the cascade from a deleted
// device to its sessions and keys needs. The pool stays unbounded, as in
// sqlstore.New; _txlock=immediate and busy_timeout serialise the writers.
func openStore(ctx context.Context, path string, log waLog.Logger) (*deviceStore, error) {
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
	return &deviceStore{Container: c, db: db}, nil
}
