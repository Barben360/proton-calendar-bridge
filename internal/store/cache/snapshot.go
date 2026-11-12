package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// StoredRow is a stored event row with its Raw payload, as rendering needs it.
type StoredRow struct {
	ID, UID      string
	Start, End   int64
	FullDay      bool
	RecurrenceID *int64
	RRule        string // the row's plaintext RRULE value, "" for none
	LastEdit     int64
	ETag         string
	Seq          int64
	Raw          json.RawMessage
}

// Group is every live row of one UID: one CalDAV object. Rows are ordered
// master first (no recurrence ID), then by recurrence ID, then by row ID.
type Group struct {
	UID  string
	Rows []StoredRow
}

// ListedRow is a row as a listing needs it: its ID and ETag, no payload.
type ListedRow struct {
	ID, ETag string
}

// Listed is one UID's rows as a listing needs them, in Group's order.
type Listed struct {
	UID  string
	Rows []ListedRow
}

// Listed is g without the rows' payloads and plaintext columns.
func (g Group) Listed() Listed {
	out := Listed{UID: g.UID, Rows: make([]ListedRow, len(g.Rows))}
	for i, r := range g.Rows {
		out.Rows[i] = ListedRow{ID: r.ID, ETag: r.ETag}
	}
	return out
}

// View is a calendar as one read transaction sees it. It is valid only inside
// the Snapshot callback.
type View interface {
	// Epoch is the cache's epoch (see Cache.Epoch).
	Epoch() string
	// MaxSeq is the highest counter value held by a row or tombstone of the
	// calendar, 0 for a calendar with none.
	MaxSeq() int64
	// MinTombstoneSeq is the lowest counter value of the calendar's
	// tombstones, 0 when there is none. This cache never prunes tombstones,
	// so every since value >= 0 remains a valid sync-token position; a future
	// pruning must make a token below the oldest retained tombstone invalid.
	MinTombstoneSeq() int64
	// UnnamedTombstoneSeq is the highest counter value of a tombstone
	// recorded without a UID (files from early M2 builds), 0 when none.
	// ChangedSince cannot name those deletions, so a sync token whose seq is
	// below this value is invalid (403 valid-sync-token, full resync).
	UnnamedTombstoneSeq() int64
	// Floor is the counter value the calendar's row took when it was
	// created (0 for a calendar older than floors): no position below it
	// belongs to this calendar's life (ruling M3-R12).
	Floor() int64
	// Listing returns every UID of the calendar with its rows' IDs and ETags,
	// ordered by UID; it reads no payload (final review, minor 1).
	Listing() ([]Listed, error)
	// Group returns one UID's rows; false when it has no live row.
	Group(uid string) (Group, bool, error)
	// ChangedSince returns the UIDs having a live row with seq > since, or
	// still a live row after one of their rows was tombstoned past since (all
	// their live rows, since the object is the whole UID), and the UIDs having
	// a tombstone with seq > since and no live row now. A deleted exception
	// whose master lives is therefore a changed UID, not a deleted one.
	// Tombstones recorded without a UID (files from early M2 builds) cannot
	// be named and are skipped.
	// Like Listing, it reads no payload.
	ChangedSince(since int64) (changed []Listed, deletedUIDs []string, err error)
	// AliasOf returns the UID the client-chosen href name stands for.
	AliasOf(name string) (uid string, ok bool, err error)
	// AliasesFor returns every alias of the calendar, UID to name.
	AliasesFor() (map[string]string, error)
	// Pins returns the pins of the rows ids of the calendar that have one
	// (ruling M4-R30), by row ID.
	Pins(ids []string) (map[string]Pin, error)
}

// Snapshot runs fn inside one read transaction on calID: everything fn reads
// through the View is one committed state, whatever the writer commits
// meanwhile, and the read never takes the write lock.
func (c *Cache) Snapshot(ctx context.Context, calID string, fn func(View) error) error {
	if calID == "" {
		return errors.New("cache: empty calendar ID")
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return c.classify(err)
	}
	defer func() { _ = tx.Rollback() }()
	v := &view{c: c, ctx: ctx, tx: tx, cal: calID}
	floor := `COALESCE((SELECT floor FROM calendars WHERE id = ?1), 0)`
	if c.noMeta {
		floor = `0`
	}
	// modernc honours TxOptions.ReadOnly with a deferred BEGIN, whatever the
	// DSN's _txlock says (tx.go), so no second handle is needed.
	// The first read fixes the snapshot, so the fixed values are read first.
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'epoch'`).Scan(&v.epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNoEpoch
		}
		return c.classify(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT
		MAX(COALESCE((SELECT MAX(seq) FROM events WHERE calendar_id = ?1), 0), COALESCE((SELECT MAX(seq) FROM tombstones WHERE calendar_id = ?1), 0)),
		COALESCE((SELECT MIN(seq) FROM tombstones WHERE calendar_id = ?1), 0),
		COALESCE((SELECT MAX(seq) FROM tombstones WHERE calendar_id = ?1 AND uid = ''), 0),
		`+floor, calID).Scan(&v.maxSeq, &v.minTomb, &v.unnamed, &v.floor); err != nil {
		return c.classify(err)
	}
	err = fn(v)
	v.ended = true
	if err != nil {
		return err
	}
	return c.classify(tx.Commit())
}

type view struct {
	c       *Cache
	ctx     context.Context
	tx      *sql.Tx
	cal     string
	epoch   string
	maxSeq  int64
	minTomb int64
	unnamed int64
	floor   int64
	ended   bool
}

func (v *view) Epoch() string              { return v.epoch }
func (v *view) MaxSeq() int64              { return v.maxSeq }
func (v *view) MinTombstoneSeq() int64     { return v.minTomb }
func (v *view) UnnamedTombstoneSeq() int64 { return v.unnamed }
func (v *view) Floor() int64               { return v.floor }

// errEnded is returned by a View used after its Snapshot callback returned.
var errEnded = errors.New("cache: snapshot used after it ended")

const rowColumns = `uid, id, start_at, end_at, full_day, recurrence_id, rrule, last_edit, etag, seq, raw`
const rowOrder = ` ORDER BY uid, recurrence_id IS NOT NULL, recurrence_id, id`

// groups runs a query selecting rowColumns in rowOrder and groups it by UID.
func (v *view) groups(query string, args ...any) ([]Group, error) {
	if v.ended {
		return nil, errEnded
	}
	rows, err := v.tx.QueryContext(v.ctx, query, args...)
	if err != nil {
		return nil, v.c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	var out []Group
	for rows.Next() {
		var r StoredRow
		var rec sql.NullInt64
		var raw []byte
		if err := rows.Scan(&r.UID, &r.ID, &r.Start, &r.End, &r.FullDay, &rec, &r.RRule, &r.LastEdit, &r.ETag, &r.Seq, &raw); err != nil {
			return nil, v.c.classify(err)
		}
		if rec.Valid {
			r.RecurrenceID = &rec.Int64
		}
		r.Raw = json.RawMessage(raw)
		if n := len(out); n == 0 || out[n-1].UID != r.UID {
			out = append(out, Group{UID: r.UID})
		}
		g := &out[len(out)-1]
		g.Rows = append(g.Rows, r)
	}
	return out, v.c.classify(rows.Err())
}

const listedColumns = `uid, id, etag`

// listing runs a query selecting listedColumns in rowOrder and groups it by
// UID.
func (v *view) listing(query string, args ...any) ([]Listed, error) {
	if v.ended {
		return nil, errEnded
	}
	rows, err := v.tx.QueryContext(v.ctx, query, args...)
	if err != nil {
		return nil, v.c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	var out []Listed
	for rows.Next() {
		var uid string
		var r ListedRow
		if err := rows.Scan(&uid, &r.ID, &r.ETag); err != nil {
			return nil, v.c.classify(err)
		}
		if n := len(out); n == 0 || out[n-1].UID != uid {
			out = append(out, Listed{UID: uid})
		}
		g := &out[len(out)-1]
		g.Rows = append(g.Rows, r)
	}
	return out, v.c.classify(rows.Err())
}

func (v *view) Listing() ([]Listed, error) {
	return v.listing(`SELECT `+listedColumns+` FROM events WHERE calendar_id = ?`+rowOrder, v.cal)
}

func (v *view) Group(uid string) (Group, bool, error) {
	gs, err := v.groups(`SELECT `+rowColumns+` FROM events WHERE calendar_id = ? AND uid = ?`+rowOrder, v.cal, uid)
	if err != nil || len(gs) == 0 {
		return Group{}, false, err
	}
	return gs[0], true, nil
}

func (v *view) ChangedSince(since int64) ([]Listed, []string, error) {
	if v.ended {
		return nil, nil, errEnded
	}
	changed, err := v.listing(`SELECT `+listedColumns+` FROM events WHERE calendar_id = ?1
		AND (uid IN (SELECT uid FROM events WHERE calendar_id = ?1 AND seq > ?2)
		OR uid IN (SELECT uid FROM tombstones WHERE calendar_id = ?1 AND seq > ?2 AND uid <> ''))`+rowOrder, v.cal, since)
	if err != nil {
		return nil, nil, err
	}
	rows, err := v.tx.QueryContext(v.ctx, `SELECT DISTINCT uid FROM tombstones WHERE calendar_id = ?1 AND seq > ?2 AND uid <> ''
		AND uid NOT IN (SELECT uid FROM events WHERE calendar_id = ?1) ORDER BY uid`, v.cal, since)
	if err != nil {
		return nil, nil, v.c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	var deleted []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, nil, v.c.classify(err)
		}
		deleted = append(deleted, uid)
	}
	return changed, deleted, v.c.classify(rows.Err())
}

// The ended checks matter on a version 1 file read-only: there no query runs
// that would fail on the closed transaction.
func (v *view) AliasOf(name string) (string, bool, error) {
	if v.ended {
		return "", false, errEnded
	}
	if v.c.noAliases {
		return "", false, nil
	}
	var uid string
	err := v.tx.QueryRowContext(v.ctx, `SELECT uid FROM aliases WHERE calendar_id = ? AND name = ?`, v.cal, name).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, v.c.classify(err)
	}
	return uid, true, nil
}

func (v *view) AliasesFor() (map[string]string, error) {
	if v.ended {
		return nil, errEnded
	}
	out := map[string]string{}
	if v.c.noAliases {
		return out, nil
	}
	rows, err := v.tx.QueryContext(v.ctx, `SELECT uid, name FROM aliases WHERE calendar_id = ?`, v.cal)
	if err != nil {
		return nil, v.c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid, name string
		if err := rows.Scan(&uid, &name); err != nil {
			return nil, v.c.classify(err)
		}
		out[uid] = name
	}
	return out, v.c.classify(rows.Err())
}
