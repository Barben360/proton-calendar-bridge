package cache

// schemaVersion is the only schema so far, stored in PRAGMA user_version.
const schemaVersion = 1

// events is a rowid table: raw blobs are large, which SQLite advises
// against in WITHOUT ROWID tables. Timestamps are Unix seconds. Nothing decrypted is stored: raw is the row
// as Proton returned it.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS calendars (
	id        TEXT PRIMARY KEY CHECK (id <> ''),
	name      TEXT NOT NULL DEFAULT '',
	type      TEXT NOT NULL DEFAULT '',
	cursor    TEXT NOT NULL DEFAULT '',
	full_at   INTEGER NOT NULL DEFAULT 0,
	synced_at INTEGER NOT NULL DEFAULT 0,
	error     TEXT NOT NULL DEFAULT '',
	resync    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS events (
	calendar_id   TEXT NOT NULL CHECK (calendar_id <> ''),
	id            TEXT NOT NULL CHECK (id <> ''),
	uid           TEXT NOT NULL,
	start_at      INTEGER NOT NULL,
	end_at        INTEGER NOT NULL,
	full_day      INTEGER NOT NULL,
	recurrence_id INTEGER,
	rrule         TEXT NOT NULL,
	last_edit     INTEGER NOT NULL,
	etag          TEXT NOT NULL,
	seq           INTEGER NOT NULL,
	raw           BLOB NOT NULL,
	UNIQUE (calendar_id, id)
);
CREATE INDEX IF NOT EXISTS events_seq ON events (calendar_id, seq);
CREATE INDEX IF NOT EXISTS events_uid ON events (calendar_id, uid);
CREATE TABLE IF NOT EXISTS tombstones (
	calendar_id TEXT NOT NULL,
	id          TEXT NOT NULL,
	uid         TEXT NOT NULL DEFAULT '',
	seq         INTEGER NOT NULL,
	PRIMARY KEY (calendar_id, id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS tombstones_seq ON tombstones (calendar_id, seq);
`
