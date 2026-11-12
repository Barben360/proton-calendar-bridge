// Package cache is C2: the SQLite cache holding every calendar's event rows
// as Proton returned them (ciphertext and plaintext metadata, nothing
// decrypted), tombstones, a per-calendar cursor and one monotonic change
// counter, named by a random epoch that changes whenever the counter
// restarts. It depends on no Proton package: the syncer converts rows.
package cache

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"modernc.org/sqlite" // the "sqlite" driver
	sqlitelib "modernc.org/sqlite/lib"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

// ErrNotFound is returned by Raw for an unknown event.
var ErrNotFound = errors.New("cache: event not found")

// IOError is a file-system failure at the cache boundary (the file or its
// directory cannot be opened or read), as opposed to the same error from
// another file of the state directory.
type IOError struct{ Err error }

func (e *IOError) Error() string { return "cache: " + e.Err.Error() }
func (e *IOError) Unwrap() error { return e.Err }

// Reason is the operating system's reason, without the path.
func (e *IOError) Reason() string {
	var pe *fs.PathError
	if errors.As(e.Err, &pe) {
		return pe.Err.Error()
	}
	var se *sqlite.Error
	if errors.As(e.Err, &se) {
		switch se.Code() & 0xff {
		case sqlitelib.SQLITE_FULL:
			return "the disk is full"
		case sqlitelib.SQLITE_READONLY:
			return "the file is read-only"
		case sqlitelib.SQLITE_PERM:
			return "permission denied"
		case sqlitelib.SQLITE_CANTOPEN:
			return "the file cannot be opened"
		case sqlitelib.SQLITE_IOERR:
			return "an input/output error"
		}
	}
	return e.Err.Error()
}

// ioError wraps a file-system error of the cache's files.
func ioError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &IOError{Err: err}
	}
	return fmt.Errorf("cache: %w", err)
}

// Typed failures of opening a cache file, for callers to classify with
// errors.Is.
var (
	// ErrNoCache: OpenReadOnly found no cache file for the account (none, an
	// empty one, or one recorded for another account).
	ErrNoCache = errors.New("cache: no cache for this account")
	// ErrCorrupt: the file is not a usable SQLite database.
	ErrCorrupt = errors.New("cache: file is not a usable SQLite database")
	// ErrChanged: a read-only handle hit a corrupt-looking page, which with a
	// writer alive is a checkpoint caught half way, not a damaged file. The
	// read is worth repeating; nothing should be removed.
	ErrChanged = errors.New("cache: changed while reading")
	// ErrNewerSchema: the file's schema is newer than this build knows.
	ErrNewerSchema = errors.New("cache: schema is newer than this pcb supports")
)

// Row is one event row as Proton returned it; Raw is the row's JSON, kept
// byte for byte. Times are Unix seconds.
type Row struct {
	ID, UID      string
	Start, End   int64
	FullDay      bool
	RecurrenceID *int64
	RRule        string
	LastEditTime int64
	Raw          json.RawMessage
}

// Event is a stored row without its Raw payload, plus its ETag and the
// counter value of its last write.
type Event struct {
	ID, UID      string
	Start, End   int64
	FullDay      bool
	RecurrenceID *int64
	RRule        string
	LastEditTime int64
	ETag         string
	Seq          int64
}

// Calendar is a calendar's sync state.
type Calendar struct {
	ID, Name, Type string
	// Color and Description are the calendar's display metadata ("" when
	// unknown: a read-only handle on a file from before they were kept).
	Color, Description string
	Cursor             string
	FullAt             time.Time // last full listing
	SyncedAt           time.Time // last successful pass
	Error              string    // last failure, "" if none
	// Resync is why a fallback full listing is pending (the caller's own
	// short code), "" if none; only DeferResync sets it and only a full
	// listing (ReplaceCalendar) clears it.
	Resync string
	Events int // rows held
}

// Changes counts what a write changed.
type Changes struct{ Added, Updated, Deleted int }

// Cache is an open cache file. Safe for concurrent use.
type Cache struct {
	db       *sql.DB
	readOnly bool
	// noMeta: the file lacks the calendars' color, description and floor
	// columns (a read-only handle on a file no writer upgraded yet).
	noMeta bool
	// noAliases: a version 1 file, which has no aliases table (same case).
	noAliases bool
	// noPins: a file without the pins table, read only (same case).
	noPins bool
}

// Path is the cache file inside the state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, "pcb.db") }

// Open opens (creating it, mode 0600) the cache at path for accountID. A
// cache recorded for another account is emptied first; a schema newer than
// this build knows is refused. The parent directory must exist (the callers
// create the state directory); a file that is not a SQLite database is
// refused with the way out.
func Open(path, accountID string) (*Cache, error) {
	if accountID == "" {
		return nil, errors.New("cache: empty account ID")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Create the file ourselves so its mode never depends on the umask;
	// SQLite gives -wal and -shm the mode of the main file when it creates
	// them. A file restored or copied with another mode is put back to 0600,
	// its -wal and -shm too: they hold event metadata and ciphertext.
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &IOError{Err: fmt.Errorf("directory %s does not exist: create the state directory first: %w", filepath.Dir(abs), err)}
	}
	if err != nil {
		return nil, ioError(err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, ioError(err)
	}
	if err := f.Close(); err != nil {
		return nil, ioError(err)
	}
	for _, side := range []string{abs + "-wal", abs + "-shm"} {
		if err := os.Chmod(side, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, ioError(err)
		}
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "_txlock=immediate" +
		"&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	c := &Cache{db: db}
	if err := c.init(context.Background(), abs, accountID); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Cache) init(ctx context.Context, path, account string) error {
	var version int
	if err := c.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && (se.Code()&0xff == sqlitelib.SQLITE_NOTADB || se.Code()&0xff == sqlitelib.SQLITE_CORRUPT) {
			return fmt.Errorf("cache: %s is not a usable SQLite database: remove it (and its -wal and -shm files), then run `pcb sync --full`: %w: %w", path, ErrCorrupt, err)
		}
		return fmt.Errorf("cache: read %s: %w", path, c.classify(err))
	}
	if version > schemaVersion {
		return fmt.Errorf("cache: %s has schema version %d, newer than the %d this pcb supports: upgrade pcb, or remove the file and run `pcb sync --full`: %w", path, version, schemaVersion, ErrNewerSchema)
	}
	if version >= 1 {
		if err := c.addLateColumns(ctx); err != nil {
			return fmt.Errorf("cache: upgrade %s: %w", path, c.classify(err))
		}
		// Added after the first M3 build: Group(uid) reads by it.
		if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS events_uid ON events (calendar_id, uid)`); err != nil {
			return fmt.Errorf("cache: upgrade %s: %w", path, c.classify(err))
		}
	}
	if err := c.migrate(ctx, version); err != nil {
		return fmt.Errorf("cache: upgrade %s: %w", path, c.classify(err))
	}
	// Read first: opening for the same account must not take the write lock.
	if got, err := c.account(ctx, c.db); err != nil {
		return fmt.Errorf("cache: %w", c.classify(err))
	} else if got == account {
		if _, err := c.Epoch(ctx); err == nil {
			return nil
		} else if !errors.Is(err, errNoEpoch) {
			return fmt.Errorf("cache: %w", err)
		}
	}
	err := c.tx(ctx, func(tx *sql.Tx) error {
		got, err := c.account(ctx, tx)
		if err != nil {
			return err
		}
		if got == account { // lost a race to the same account, or a cache from before the epoch
			_, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('epoch', ?) ON CONFLICT(key) DO NOTHING`, newEpoch())
			return err
		}
		if got != "" {
			for _, t := range []string{"pins", "aliases", "tombstones", "events", "calendars", "meta"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
					return err
				}
			}
		}
		// A new life of the change counter: a new epoch (ruling R14).
		_, err = tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('account', ?), ('epoch', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, account, newEpoch())
		return err
	})
	if err != nil {
		return fmt.Errorf("cache: record account: %w", c.classify(err))
	}
	return nil
}

// The steps of a migration, as migrateStep sees them.
const (
	migrateSchema = "schema" // the tables created, user_version set
	migratePins   = "pins"   // the stored rows pinned
)

// migrateStep, when set, runs after each step of a migration, inside its
// transaction; an error aborts it as a crash there would (tests).
var migrateStep func(step string) error

// migrate brings a file of schema version to schemaVersion in one
// transaction (state-I6): the tables, the pins of the stored rows and
// user_version commit together or not at all, so a crash, SQLITE_BUSY or a
// full disk half way leaves the file as it was and the next open migrates it
// in full. A file with no pins table (version 1, or a version 2 file of a
// build before the pins) has its stored rows pinned: they are the first
// seen. Both are decided again under the write lock, since another process
// may have migrated the file meanwhile.
func (c *Cache) migrate(ctx context.Context, version int) error {
	pinned, err := c.hasTable(ctx, "pins")
	if err != nil || (version >= schemaVersion && pinned) {
		return err
	}
	return c.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'pins'`).Scan(&n); err != nil {
			return err
		}
		hadPins := n > 0
		if version < schemaVersion {
			for _, ddl := range []string{schemaV1, schemaV2} {
				if _, err := tx.ExecContext(ctx, ddl); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `PRAGMA user_version = `+strconv.Itoa(schemaVersion)); err != nil {
				return err
			}
			if err := step(migrateSchema); err != nil {
				return err
			}
		}
		if !hadPins {
			if err := backfillPins(ctx, tx); err != nil {
				return err
			}
			if err := step(migratePins); err != nil {
				return err
			}
		}
		return nil
	})
}

func step(name string) error {
	if migrateStep == nil {
		return nil
	}
	return migrateStep(name)
}

// errNoEpoch: the cache has no epoch yet (a file written before it existed).
var errNoEpoch = errors.New("cache: no epoch recorded")

// newEpoch is 128 random bits in hex.
func newEpoch() string {
	return hex.EncodeToString(randomBytes(16))
}

// Epoch names this life of the change counter: 128 random bits in hex,
// drawn when the file is created and again when it is emptied for another
// account (ruling R14). Seq restarts only under a new epoch, so a token
// built as "epoch:seq" (M3's CTags and sync tokens) never comes back with a
// different content, even after pcb logout removed the file.
func (c *Cache) Epoch(ctx context.Context) (string, error) {
	var e string
	err := c.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'epoch'`).Scan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoEpoch
	}
	if err != nil {
		return "", c.classify(err)
	}
	return e, nil
}

// lateColumns were added to schema version 1 before it was released (M2
// development builds wrote files without them).
var lateColumns = []struct{ table, column, def string }{
	{"calendars", "resync", "TEXT NOT NULL DEFAULT ''"},
	{"tombstones", "uid", "TEXT NOT NULL DEFAULT ''"},
	// Added in M3 (released M2 files lack them): CalDAV collections show them.
	{"calendars", "color", "TEXT NOT NULL DEFAULT ''"},
	{"calendars", "description", "TEXT NOT NULL DEFAULT ''"},
	// Added in M3 (ruling M3-R12): see createCalendar. Existing calendars get
	// 0: every position they ever had stays valid.
	{"calendars", "floor", "INTEGER NOT NULL DEFAULT 0"},
}

// addLateColumns adds the lateColumns a version 1 file lacks.
func (c *Cache) addLateColumns(ctx context.Context) error {
	has := func(table, column string) (bool, error) {
		var n int
		err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
		return n == 1, err
	}
	for _, lc := range lateColumns {
		if ok, err := has(lc.table, lc.column); err != nil || ok {
			if err != nil {
				return err
			}
			continue
		}
		if _, err := c.db.ExecContext(ctx, `ALTER TABLE `+lc.table+` ADD COLUMN `+lc.column+` `+lc.def); err != nil {
			// Another process may have added it first.
			if ok, qerr := has(lc.table, lc.column); qerr != nil || !ok {
				return err
			}
		}
	}
	return nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (c *Cache) account(ctx context.Context, q querier) (string, error) {
	var a string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'account'`).Scan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return a, err
}

// randomBytes reads n bytes from crypto/rand, which never fails on the
// supported systems (it panics rather than return a short read).
func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// Close closes the cache.
func (c *Cache) Close() error { return c.db.Close() }

// OpenReadOnly opens the existing cache at path for reading only: it never
// creates, migrates or empties anything, so it is safe beside a writer and
// after a logout removed the file. A missing or empty file, or one recorded
// for another account, is ErrNoCache; a file that is not a database is
// ErrCorrupt; a newer schema is ErrNewerSchema. Writing through the handle
// fails.
//
// With no -wal file the handle reads the file as immutable=1: SQLite then
// takes no lock and trusts nothing changes, so a writer that starts
// meanwhile (its checkpoint rewriting pages) can make a read see a stale or
// inconsistent snapshot, or fail with ErrChanged. That is fine for the CLI's
// one-shot reads (status, event list), which a person repeats; it is not for
// a long-lived reader such as M3's CalDAV read path, which must open a
// normal shared-lock read handle instead (SPEC §7.2).
func OpenReadOnly(path, accountID string) (*Cache, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c, err := openReadOnly(abs, accountID)
	if errors.Is(err, ErrCorrupt) {
		// A writer's checkpoint can make an immutable read look corrupt for
		// a moment: look once more before calling the file damaged.
		time.Sleep(retryDelay)
		c, err = openReadOnly(abs, accountID)
	}
	return c, err
}

// retryDelay is how long OpenReadOnly waits before its second look.
var retryDelay = 100 * time.Millisecond

func openReadOnly(abs, accountID string) (*Cache, error) {
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCache
	} else if err != nil {
		return nil, ioError(err)
	}
	// A read-only open of a WAL database needs its -shm; with no -wal there
	// is no writer and nothing pending, so read the file as immutable.
	query := "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	if _, err := os.Stat(abs + "-wal"); errors.Is(err, os.ErrNotExist) {
		query = "immutable=1&_pragma=query_only(1)"
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: query}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	db.SetMaxOpenConns(1)
	c := &Cache{db: db, readOnly: true}
	if err := c.checkReadOnly(context.Background(), abs, accountID); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

// classify types a failure of an operation by SQLite's primary result code
// (ruling R13): a damaged file is ErrCorrupt, except on a read-only handle,
// where it is ErrChanged (only the writer may conclude the file is damaged);
// a file system refusing (disk full, I/O error, read-only, cannot open,
// permission) is an *IOError. Anything else is returned as is.
func (c *Cache) classify(err error) error {
	var se *sqlite.Error
	if err == nil || !errors.As(err, &se) {
		return err
	}
	switch se.Code() & 0xff {
	case sqlitelib.SQLITE_CORRUPT, sqlitelib.SQLITE_NOTADB:
		if c.readOnly {
			return fmt.Errorf("%w: %w", ErrChanged, err)
		}
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	case sqlitelib.SQLITE_FULL, sqlitelib.SQLITE_IOERR, sqlitelib.SQLITE_READONLY, sqlitelib.SQLITE_CANTOPEN, sqlitelib.SQLITE_PERM:
		return &IOError{Err: err}
	}
	return err
}

func (c *Cache) checkReadOnly(ctx context.Context, path, account string) error {
	var version int
	if err := c.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && (se.Code()&0xff == sqlitelib.SQLITE_NOTADB || se.Code()&0xff == sqlitelib.SQLITE_CORRUPT) {
			return fmt.Errorf("cache: %s is not a usable SQLite database: %w: %w", path, ErrCorrupt, err)
		}
		return fmt.Errorf("cache: read %s: %w", path, err)
	}
	switch {
	case version > schemaVersion:
		return fmt.Errorf("cache: %s has schema version %d, newer than the %d this pcb supports: %w", path, version, schemaVersion, ErrNewerSchema)
	case version < 1:
		return ErrNoCache // empty file: nothing was ever stored
	}
	c.noAliases = version < 2 // a read-only handle never upgrades
	pins, err := c.hasTable(ctx, "pins")
	if err != nil {
		return fmt.Errorf("cache: %w", c.classify(err))
	}
	c.noPins = !pins
	got, err := c.account(ctx, c.db)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if got != account {
		return ErrNoCache
	}
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('calendars') WHERE name IN ('color', 'description', 'floor')`).Scan(&n); err != nil {
		return fmt.Errorf("cache: %w", c.classify(err))
	}
	c.noMeta = n != 3
	return nil
}

// Remove deletes the cache file with its -wal and -shm; missing is fine.
func Remove(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir fsyncs a directory, best effort; tests watch it.
var syncDir = atomicfile.SyncDir

// tx runs fn in one immediate (write-locking) transaction.
func (c *Cache) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Seq is the change counter: the value taken by the latest row write,
// tombstone or calendar creation (its floor), 0 for a cache never written.
func (c *Cache) Seq(ctx context.Context) (int64, error) {
	n, err := readSeq(ctx, c.db)
	return n, c.classify(err)
}

func readSeq(ctx context.Context, q querier) (int64, error) {
	var s string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'seq'`).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

// writer is a transaction's view of one calendar's writes.
type writer struct {
	ctx   context.Context
	tx    *sql.Tx
	cal   string
	start int64 // the counter before this write
	seq   int64
	ch    Changes
}

func (c *Cache) begin(ctx context.Context, tx *sql.Tx, cal string) (*writer, error) {
	if cal == "" {
		return nil, errors.New("cache: empty calendar ID")
	}
	seq, err := readSeq(ctx, tx)
	if err != nil {
		return nil, err
	}
	return &writer{ctx: ctx, tx: tx, cal: cal, start: seq, seq: seq}, nil
}

func (w *writer) next() int64 { w.seq++; return w.seq }

func (w *writer) etags() (map[string]string, error) {
	rows, err := w.tx.QueryContext(w.ctx, `SELECT id, etag FROM events WHERE calendar_id = ?`, w.cal)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	m := map[string]string{}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		m[id] = tag
	}
	return m, rows.Err()
}

// upsert writes r unless the stored ETag equals its own.
func (w *writer) upsert(r Row, known map[string]string) error {
	tag, err := ETag(r.Raw)
	if err != nil {
		return fmt.Errorf("event %q: %w", r.ID, err)
	}
	old, existed := known[r.ID]
	if err := w.pin(r); err != nil {
		return fmt.Errorf("event %q: %w", r.ID, err)
	}
	if existed && old == tag {
		return nil
	}
	var rec any
	if r.RecurrenceID != nil {
		rec = *r.RecurrenceID
	}
	_, err = w.tx.ExecContext(w.ctx, `INSERT INTO events
		(calendar_id, id, uid, start_at, end_at, full_day, recurrence_id, rrule, last_edit, etag, seq, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(calendar_id, id) DO UPDATE SET
		uid = excluded.uid, start_at = excluded.start_at, end_at = excluded.end_at,
		full_day = excluded.full_day, recurrence_id = excluded.recurrence_id, rrule = excluded.rrule,
		last_edit = excluded.last_edit, etag = excluded.etag, seq = excluded.seq, raw = excluded.raw`,
		w.cal, r.ID, r.UID, r.Start, r.End, r.FullDay, rec, r.RRule, r.LastEditTime, tag, w.next(), []byte(r.Raw))
	if err != nil {
		return fmt.Errorf("event %q: %w", r.ID, err)
	}
	if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM tombstones WHERE calendar_id = ? AND id = ?`, w.cal, r.ID); err != nil {
		return err
	}
	known[r.ID] = tag
	if existed {
		w.ch.Updated++
	} else {
		w.ch.Added++
	}
	return nil
}

func (w *writer) tombstone(id string, known map[string]string) error {
	if _, ok := known[id]; !ok {
		return nil
	}
	var uid string
	if err := w.tx.QueryRowContext(w.ctx, `DELETE FROM events WHERE calendar_id = ? AND id = ? RETURNING uid`, w.cal, id).Scan(&uid); err != nil {
		return err
	}
	// The pin stays (ruling M4-R35): Proton never reuses an event ID, so
	// a row hidden once and served again must match its first packets.
	// The UID names the deleted CalDAV resource (ruling R16).
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO tombstones (calendar_id, id, uid, seq) VALUES (?, ?, ?, ?)
		ON CONFLICT(calendar_id, id) DO UPDATE SET uid = excluded.uid, seq = excluded.seq`, w.cal, id, uid, w.next()); err != nil {
		return err
	}
	delete(known, id)
	w.ch.Deleted++
	return nil
}

// settle creates the calendar's row when it has none and stores the counter.
func (w *writer) settle() error {
	created, floor, err := createCalendar(w.ctx, w.tx, w.cal, w.start)
	if err != nil {
		return err
	}
	if created && w.seq == w.start {
		w.seq = floor // no row took the floor's value: the creation does
	}
	return writeSeq(w.ctx, w.tx, w.seq)
}

func (w *writer) finish(cursor string, at time.Time, full bool) error {
	if err := w.settle(); err != nil {
		return err
	}
	fullAt := int64(0)
	if full {
		fullAt = at.Unix()
	}
	_, err := w.tx.ExecContext(w.ctx, `INSERT INTO calendars (id, cursor, full_at, synced_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET cursor = excluded.cursor, synced_at = excluded.synced_at,
		full_at = CASE WHEN ? THEN excluded.full_at ELSE calendars.full_at END,
		resync = CASE WHEN ? THEN '' ELSE calendars.resync END`,
		w.cal, cursor, fullAt, at.Unix(), full, full)
	return err
}

// ReplaceCalendar makes rows the calendar's content in one transaction:
// rows whose ETag changed are written, stored rows absent from rows are
// tombstoned, and the cursor, FullAt and SyncedAt are set.
func (c *Cache) ReplaceCalendar(ctx context.Context, calID, cursor string, rows []Row, at time.Time) (Changes, error) {
	var ch Changes
	err := c.tx(ctx, func(tx *sql.Tx) error {
		w, err := c.begin(ctx, tx, calID)
		if err != nil {
			return err
		}
		known, err := w.etags()
		if err != nil {
			return err
		}
		keep := make(map[string]bool, len(rows))
		for _, r := range rows {
			keep[r.ID] = true
			if err := w.upsert(r, known); err != nil {
				return err
			}
		}
		for id := range known {
			if !keep[id] {
				if err := w.tombstone(id, known); err != nil {
					return err
				}
			}
		}
		ch = w.ch
		return w.finish(cursor, at, true)
	})
	if err != nil {
		return Changes{}, c.classify(err)
	}
	return ch, nil
}

// ApplyDelta applies a change-feed pass in one transaction: upserts (only
// on an ETag change), tombstones for the gone IDs that exist, the new cursor
// and SyncedAt.
func (c *Cache) ApplyDelta(ctx context.Context, calID, cursor string, upserts []Row, gone []string, at time.Time) (Changes, error) {
	var ch Changes
	err := c.tx(ctx, func(tx *sql.Tx) error {
		w, err := c.begin(ctx, tx, calID)
		if err != nil {
			return err
		}
		known, err := w.etags()
		if err != nil {
			return err
		}
		for _, r := range upserts {
			if err := w.upsert(r, known); err != nil {
				return err
			}
		}
		for _, id := range gone {
			if err := w.tombstone(id, known); err != nil {
				return err
			}
		}
		ch = w.ch
		return w.finish(cursor, at, false)
	})
	if err != nil {
		return Changes{}, c.classify(err)
	}
	return ch, nil
}

// ApplyWrite makes rows the content of one UID in the calendar, as Proton
// returned them after a write (ruling M4-Rpre2): rows whose ETag changed are
// written, the UID's stored rows absent from rows are tombstoned (with the
// UID), and empty rows deletes the UID. A row of another UID is an error and
// nothing is written. Unlike ApplyDelta it never touches the feed cursor, so
// the syncer's next pass re-reads the same rows and finds them unchanged.
func (c *Cache) ApplyWrite(ctx context.Context, calID, uid string, rows []Row, at time.Time) (Changes, error) {
	return c.ApplyWritePinned(ctx, calID, uid, rows, nil, at)
}

// ApplyWritePinned is ApplyWrite that first pins the rows of pins (row ID
// to its packets) that have no pin yet: a create's rows are pinned with the
// packets the write sent, never with the ones Proton echoed (ruling
// M4-R36).
func (c *Cache) ApplyWritePinned(ctx context.Context, calID, uid string, rows []Row, pins map[string]Pin, at time.Time) (Changes, error) {
	if uid == "" {
		return Changes{}, errors.New("cache: empty UID")
	}
	for _, r := range rows {
		if r.UID != uid {
			return Changes{}, fmt.Errorf("cache: row %q has another UID than the one written", r.ID)
		}
	}
	var ch Changes
	err := c.tx(ctx, func(tx *sql.Tx) error {
		w, err := c.begin(ctx, tx, calID)
		if err != nil {
			return err
		}
		known, err := w.etags()
		if err != nil {
			return err
		}
		own, err := w.idsOf(uid)
		if err != nil {
			return err
		}
		for id, p := range pins {
			if err := w.pinAs(id, p); err != nil {
				return err
			}
		}
		keep := make(map[string]bool, len(rows))
		for _, r := range rows {
			keep[r.ID] = true
			if err := w.upsert(r, known); err != nil {
				return err
			}
		}
		for _, id := range own {
			if !keep[id] {
				if err := w.tombstone(id, known); err != nil {
					return err
				}
			}
		}
		ch = w.ch
		return w.settle()
	})
	if err != nil {
		return Changes{}, c.classify(err)
	}
	return ch, nil
}

// idsOf lists the IDs of the stored rows of one UID.
func (w *writer) idsOf(uid string) ([]string, error) {
	rows, err := w.tx.QueryContext(w.ctx, `SELECT id FROM events WHERE calendar_id = ? AND uid = ?`, w.cal, uid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PutAlias records that the client's href name stands for uid in the
// calendar (ruling M4-R6). A UID has one name: the previous one is replaced,
// and so is another UID's claim on the same name. Aliases outlive the UID's
// deletion and die with the calendar or the epoch (ruling M4-R22).
func (c *Cache) PutAlias(ctx context.Context, calID, name, uid string) error {
	return c.PutAliasKeeping(ctx, calID, name, uid, nil)
}

// PutAliasKeeping is PutAlias that also records keep (UID to name) in the
// same transaction: the alternative names of UIDs whose href the new alias
// would otherwise move (ruling M4-R41). A keep entry replaces any alias of
// its UID or name, as PutAlias does.
func (c *Cache) PutAliasKeeping(ctx context.Context, calID, name, uid string, keep map[string]string) error {
	if calID == "" || name == "" || uid == "" {
		return errors.New("cache: empty calendar ID, alias name or UID")
	}
	for u, n := range keep {
		if u == "" || n == "" || u == uid || n == name {
			return errors.New("cache: a kept alias is empty or collides with the new one")
		}
	}
	return c.classify(c.tx(ctx, func(tx *sql.Tx) error {
		put := func(name, uid string) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM aliases WHERE calendar_id = ?1 AND (uid = ?2 OR name = ?3)`, calID, uid, name); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO aliases (calendar_id, name, uid) VALUES (?, ?, ?)`, calID, name, uid)
			return err
		}
		for u, n := range keep {
			if err := put(n, u); err != nil {
				return err
			}
		}
		return put(name, uid)
	}))
}

// Calendars lists the calendars, ordered by ID.
func (c *Cache) Calendars(ctx context.Context) ([]Calendar, error) {
	meta := `c.color, c.description`
	if c.noMeta {
		meta = `'', ''`
	}
	rows, err := c.db.QueryContext(ctx, `SELECT c.id, c.name, c.type, `+meta+`, c.cursor, c.full_at, c.synced_at, c.error, c.resync,
		(SELECT count(*) FROM events e WHERE e.calendar_id = c.id) FROM calendars c ORDER BY c.id`)
	if err != nil {
		return nil, c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	var out []Calendar
	for rows.Next() {
		var cal Calendar
		var full, synced int64
		if err := rows.Scan(&cal.ID, &cal.Name, &cal.Type, &cal.Color, &cal.Description, &cal.Cursor, &full, &synced, &cal.Error, &cal.Resync, &cal.Events); err != nil {
			return nil, c.classify(err)
		}
		cal.FullAt, cal.SyncedAt = fromUnix(full), fromUnix(synced)
		out = append(out, cal)
	}
	return out, c.classify(rows.Err())
}

func fromUnix(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0)
}

// PutCalendar records a calendar's name, type, colour, description and
// error. The cursor, FullAt and SyncedAt are only ever set by ReplaceCalendar
// and ApplyDelta, so a caller cannot erase them (cal's own values for them
// are ignored).
func (c *Cache) PutCalendar(ctx context.Context, cal Calendar) error {
	if cal.ID == "" {
		return errors.New("cache: empty calendar ID")
	}
	return c.classify(c.tx(ctx, func(tx *sql.Tx) error {
		seq, err := readSeq(ctx, tx)
		if err != nil {
			return err
		}
		created, floor, err := createCalendar(ctx, tx, cal.ID, seq)
		if err != nil {
			return err
		}
		if created {
			if err := writeSeq(ctx, tx, floor); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE calendars SET name = ?, type = ?, color = ?, description = ?, error = ? WHERE id = ?`,
			cal.Name, cal.Type, cal.Color, cal.Description, cal.Error, cal.ID)
		return err
	}))
}

// createCalendar inserts calendar id's row when it has none, with a floor
// one past seq, the counter's value: the creation takes that value (the
// caller stores it as the counter), so every position of a former calendar
// of the same ID is below the floor and no token of that life is answered
// (ruling M3-R12). created is false when the row exists.
func createCalendar(ctx context.Context, tx *sql.Tx, id string, seq int64) (created bool, floor int64, err error) {
	floor = seq + 1
	res, err := tx.ExecContext(ctx, `INSERT INTO calendars (id, floor) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`, id, floor)
	if err != nil {
		return false, 0, err
	}
	n, err := res.RowsAffected()
	return n == 1, floor, err
}

func writeSeq(ctx context.Context, tx *sql.Tx, seq int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('seq', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(seq, 10))
	return err
}

// DeferResync records that the calendar needs a full listing it could not
// have yet, and why (a short code of the caller's). The calendar must be
// known; ReplaceCalendar clears it.
func (c *Cache) DeferResync(ctx context.Context, calID, why string) error {
	if why == "" {
		return errors.New("cache: empty resync reason")
	}
	res, err := c.db.ExecContext(ctx, `UPDATE calendars SET resync = ? WHERE id = ?`, why, calID)
	if err != nil {
		return c.classify(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("cache: no calendar %q to defer", calID)
	}
	return nil
}

// DropCalendar removes a calendar with its rows, tombstones and aliases.
// Its pins stay until the epoch changes (ruling M4-R35): a calendar hidden
// once and listed again must serve its rows' first packets.
func (c *Cache) DropCalendar(ctx context.Context, id string) error {
	return c.classify(c.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM events WHERE calendar_id = ?`,
			`DELETE FROM tombstones WHERE calendar_id = ?`,
			`DELETE FROM aliases WHERE calendar_id = ?`,
			`DELETE FROM calendars WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	}))
}

// Events lists a calendar's rows (without Raw), ordered by start then ID.
func (c *Cache) Events(ctx context.Context, calID string) ([]Event, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, uid, start_at, end_at, full_day, recurrence_id, rrule, last_edit, etag, seq
		FROM events WHERE calendar_id = ? ORDER BY start_at, id`, calID)
	if err != nil {
		return nil, c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var rec sql.NullInt64
		if err := rows.Scan(&e.ID, &e.UID, &e.Start, &e.End, &e.FullDay, &rec, &e.RRule, &e.LastEditTime, &e.ETag, &e.Seq); err != nil {
			return nil, c.classify(err)
		}
		if rec.Valid {
			e.RecurrenceID = &rec.Int64
		}
		out = append(out, e)
	}
	return out, c.classify(rows.Err())
}

// Change is one entry of Changes: a row written (Deleted false) or
// tombstoned at counter value Seq.
type Change struct {
	ID, UID string
	Seq     int64
	Deleted bool
}

// Changes lists the calendar's rows written and tombstones recorded after
// counter value since, in counter order (what a sync token "epoch:since"
// asks for). A row written several times appears once, at its last write.
func (c *Cache) Changes(ctx context.Context, calID string, since int64) ([]Change, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, uid, seq, 0 FROM events WHERE calendar_id = ? AND seq > ?
		UNION ALL SELECT id, uid, seq, 1 FROM tombstones WHERE calendar_id = ? AND seq > ?
		ORDER BY 3`, calID, since, calID, since)
	if err != nil {
		return nil, c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	var out []Change
	for rows.Next() {
		var ch Change
		if err := rows.Scan(&ch.ID, &ch.UID, &ch.Seq, &ch.Deleted); err != nil {
			return nil, c.classify(err)
		}
		out = append(out, ch)
	}
	return out, c.classify(rows.Err())
}

// Raw returns a row's JSON exactly as stored, or ErrNotFound.
func (c *Cache) Raw(ctx context.Context, calID, id string) (json.RawMessage, error) {
	var b []byte
	err := c.db.QueryRowContext(ctx, `SELECT raw FROM events WHERE calendar_id = ? AND id = ?`, calID, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, c.classify(err)
	}
	return json.RawMessage(b), nil
}
