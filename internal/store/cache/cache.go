// Package cache is C2: the SQLite cache holding every calendar's event rows
// as Proton returned them (ciphertext and plaintext metadata, nothing
// decrypted), tombstones, a per-calendar cursor and one monotonic change
// counter. It depends on no Proton package: the syncer converts rows.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver
)

// ErrNotFound is returned by Raw for an unknown event.
var ErrNotFound = errors.New("cache: event not found")

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
	Cursor         string
	FullAt         time.Time // last full listing
	SyncedAt       time.Time // last successful pass
	Error          string    // last failure, "" if none
	Events         int       // rows held
}

// Changes counts what a write changed.
type Changes struct{ Added, Updated, Deleted int }

// Cache is an open cache file. Safe for concurrent use.
type Cache struct{ db *sql.DB }

// Path is the cache file inside the state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, "pcb.db") }

// Open opens (creating it, mode 0600) the cache at path for accountID. A
// cache recorded for another account is emptied first; a schema newer than
// this build knows is refused.
func Open(path, accountID string) (*Cache, error) {
	if accountID == "" {
		return nil, errors.New("cache: empty account ID")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Create the file ourselves so its mode never depends on the umask;
	// SQLite gives -wal and -shm the mode of the main file.
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "_txlock=immediate" +
		"&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"}
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
		return fmt.Errorf("cache: read %s: %w", path, err)
	}
	if version > schemaVersion {
		return fmt.Errorf("cache: %s has schema version %d, newer than the %d this pcb supports: upgrade pcb, or remove the file and run `pcb sync --full`", path, version, schemaVersion)
	}
	if version < schemaVersion {
		err := c.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `PRAGMA user_version = `+strconv.Itoa(schemaVersion))
			return err
		})
		if err != nil {
			return fmt.Errorf("cache: create schema: %w", err)
		}
	}
	// Read first: opening for the same account must not take the write lock.
	if got, err := c.account(ctx, c.db); err != nil {
		return fmt.Errorf("cache: %w", err)
	} else if got == account {
		return nil
	}
	err := c.tx(ctx, func(tx *sql.Tx) error {
		got, err := c.account(ctx, tx)
		if err != nil || got == account { // lost a race to the same account
			return err
		}
		if got != "" {
			for _, t := range []string{"tombstones", "events", "calendars", "meta"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('account', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, account)
		return err
	})
	if err != nil {
		return fmt.Errorf("cache: record account: %w", err)
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

// Close closes the cache.
func (c *Cache) Close() error { return c.db.Close() }

// Remove deletes the cache file with its -wal and -shm; missing is fine.
func Remove(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

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

// Seq is the change counter: the value taken by the latest row write or
// tombstone, 0 for a cache never written.
func (c *Cache) Seq(ctx context.Context) (int64, error) {
	return readSeq(ctx, c.db)
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
	ctx context.Context
	tx  *sql.Tx
	cal string
	seq int64
	ch  Changes
}

func (c *Cache) begin(ctx context.Context, tx *sql.Tx, cal string) (*writer, error) {
	if cal == "" {
		return nil, errors.New("cache: empty calendar ID")
	}
	seq, err := readSeq(ctx, tx)
	if err != nil {
		return nil, err
	}
	return &writer{ctx: ctx, tx: tx, cal: cal, seq: seq}, nil
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
	if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM events WHERE calendar_id = ? AND id = ?`, w.cal, id); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO tombstones (calendar_id, id, seq) VALUES (?, ?, ?)
		ON CONFLICT(calendar_id, id) DO UPDATE SET seq = excluded.seq`, w.cal, id, w.next()); err != nil {
		return err
	}
	delete(known, id)
	w.ch.Deleted++
	return nil
}

func (w *writer) finish(cursor string, at time.Time, full bool) error {
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO meta (key, value) VALUES ('seq', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(w.seq, 10)); err != nil {
		return err
	}
	fullAt := int64(0)
	if full {
		fullAt = at.Unix()
	}
	_, err := w.tx.ExecContext(w.ctx, `INSERT INTO calendars (id, cursor, full_at, synced_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET cursor = excluded.cursor, synced_at = excluded.synced_at,
		full_at = CASE WHEN ? THEN excluded.full_at ELSE calendars.full_at END`,
		w.cal, cursor, fullAt, at.Unix(), full)
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
		return Changes{}, err
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
		return Changes{}, err
	}
	return ch, nil
}

// Calendars lists the calendars, ordered by ID.
func (c *Cache) Calendars(ctx context.Context) ([]Calendar, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT c.id, c.name, c.type, c.cursor, c.full_at, c.synced_at, c.error,
		(SELECT count(*) FROM events e WHERE e.calendar_id = c.id) FROM calendars c ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Calendar
	for rows.Next() {
		var cal Calendar
		var full, synced int64
		if err := rows.Scan(&cal.ID, &cal.Name, &cal.Type, &cal.Cursor, &full, &synced, &cal.Error, &cal.Events); err != nil {
			return nil, err
		}
		cal.FullAt, cal.SyncedAt = fromUnix(full), fromUnix(synced)
		out = append(out, cal)
	}
	return out, rows.Err()
}

func fromUnix(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0)
}

func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// PutCalendar records a calendar's name, type, error and sync times; the
// cursor is only ever set by ReplaceCalendar and ApplyDelta.
func (c *Cache) PutCalendar(ctx context.Context, cal Calendar) error {
	if cal.ID == "" {
		return errors.New("cache: empty calendar ID")
	}
	_, err := c.db.ExecContext(ctx, `INSERT INTO calendars (id, name, type, full_at, synced_at, error)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, type = excluded.type,
		full_at = excluded.full_at, synced_at = excluded.synced_at, error = excluded.error`,
		cal.ID, cal.Name, cal.Type, toUnix(cal.FullAt), toUnix(cal.SyncedAt), cal.Error)
	return err
}

// DropCalendar removes a calendar with its rows and tombstones.
func (c *Cache) DropCalendar(ctx context.Context, id string) error {
	return c.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM events WHERE calendar_id = ?`,
			`DELETE FROM tombstones WHERE calendar_id = ?`,
			`DELETE FROM calendars WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Events lists a calendar's rows (without Raw), ordered by start then ID.
func (c *Cache) Events(ctx context.Context, calID string) ([]Event, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, uid, start_at, end_at, full_day, recurrence_id, rrule, last_edit, etag, seq
		FROM events WHERE calendar_id = ? ORDER BY start_at, id`, calID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var rec sql.NullInt64
		if err := rows.Scan(&e.ID, &e.UID, &e.Start, &e.End, &e.FullDay, &rec, &e.RRule, &e.LastEditTime, &e.ETag, &e.Seq); err != nil {
			return nil, err
		}
		if rec.Valid {
			e.RecurrenceID = &rec.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Raw returns a row's JSON exactly as stored, or ErrNotFound.
func (c *Cache) Raw(ctx context.Context, calID, id string) (json.RawMessage, error) {
	var b []byte
	err := c.db.QueryRowContext(ctx, `SELECT raw FROM events WHERE calendar_id = ? AND id = ?`, calID, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
