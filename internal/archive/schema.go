package archive

// migrations[i] takes the schema from version i to i+1, so SchemaVersion is
// simply how many there are. Until the first release v1 is still edited in
// place; after it a released step is never changed, the next one is appended
// (plan 14: backups before migrating arrive together with v2). An array, not
// a slice, so that its length is a constant. A step runs with foreign_keys
// off (see migrate), so it may rebuild a table others reference.
var migrations = [...]string{schemaV1}

// SchemaVersion is the newest PRAGMA user_version this build understands.
const SchemaVersion = len(migrations)

// schemaV1 is the whole archive of plan 8. Everything an account owns
// references accounts(nick) with ON DELETE CASCADE, so DeleteAccount is a
// single DELETE; messages_fts has no foreign key and is cleared by the
// messages_ad trigger, which SQLite fires for cascaded deletes too. STRICT
// makes a wrongly bound value (a time.Time, which the driver sends as TEXT,
// in an INTEGER column) an error instead of a row that sorts wrong; it can
// only be added later by rebuilding the table. messages_au skips updates that
// leave the text as it was (the upsert of plan 8 sets text even onto a
// placeholder an edit already filled), which would only pile up FTS
// tombstones.
const schemaV1 = `
CREATE TABLE accounts (
  nick       TEXT PRIMARY KEY,         -- ^[a-z0-9_-]{1,64}$, checked in Go
  jid        TEXT,                     -- full AD-JID of the device in store.db; NULL until first linked
  created_at INTEGER NOT NULL          -- unix seconds
) STRICT, WITHOUT ROWID;

CREATE TABLE chats (
  account  TEXT NOT NULL REFERENCES accounts(nick) ON DELETE CASCADE,
  jid      TEXT NOT NULL,              -- canonical: the LID for a direct chat when known
  pn       TEXT,                       -- phone number, for display and search
  name     TEXT,
  is_group INTEGER NOT NULL DEFAULT 0,
  last_message_ts INTEGER,
  PRIMARY KEY (account, jid)
) STRICT, WITHOUT ROWID;

CREATE TABLE messages (
  id         INTEGER PRIMARY KEY,      -- rowid in messages_fts
  account    TEXT NOT NULL REFERENCES accounts(nick) ON DELETE CASCADE,
  chat_jid   TEXT NOT NULL,            -- canonical
  msg_id     TEXT NOT NULL,
  sender_jid TEXT NOT NULL,            -- as received (a quote needs it verbatim)
  from_me    INTEGER NOT NULL,
  ts         INTEGER NOT NULL,         -- unix seconds
  text       TEXT,                     -- conversation | extendedText | caption | document name
  media_type TEXT,                     -- image|video|audio|ptt|document|sticker; NULL = text
  media_mime TEXT,
  media_name TEXT,
  media_size INTEGER,
  media_path TEXT,                     -- the last downloaded file
  quoted_id  TEXT,
  edited_at  INTEGER,
  revoked_at INTEGER,
  raw        BLOB,                     -- proto.Marshal(waE2E.Message); NULL for a placeholder
  UNIQUE (account, chat_jid, msg_id)
) STRICT;
CREATE INDEX messages_chat_ts ON messages(account, chat_jid, ts);

CREATE VIRTUAL TABLE messages_fts USING fts5(
  text, content='', contentless_delete=1, tokenize='trigram remove_diacritics 1');

CREATE TRIGGER messages_ai AFTER INSERT ON messages WHEN new.text IS NOT NULL BEGIN
  INSERT INTO messages_fts(rowid, text) VALUES (new.id, replace(replace(new.text,'ё','е'),'Ё','Е'));
END;
CREATE TRIGGER messages_au AFTER UPDATE OF text ON messages WHEN old.text IS NOT new.text BEGIN
  DELETE FROM messages_fts WHERE rowid = old.id;
  INSERT INTO messages_fts(rowid, text)
  SELECT new.id, replace(replace(new.text,'ё','е'),'Ё','Е') WHERE new.text IS NOT NULL;
END;
CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
  DELETE FROM messages_fts WHERE rowid = old.id;
END;

CREATE TABLE history_queue (
  id         INTEGER PRIMARY KEY,
  account    TEXT NOT NULL REFERENCES accounts(nick) ON DELETE CASCADE,
  notif      BLOB NOT NULL,            -- proto.Marshal(HistorySyncNotification)
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at INTEGER NOT NULL          -- unix seconds
) STRICT;
`
