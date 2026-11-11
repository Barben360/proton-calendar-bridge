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
	// Groups returns every UID of the calendar with its rows, ordered by UID.
	Groups() ([]Group, error)
	// Group returns one UID's rows; false when it has no live row.
	Group(uid string) (Group, bool, error)
	// ChangedSince returns the UIDs having a live row with seq > since, or
	// still a live row after one of their rows was tombstoned past since (all
	// their live rows, since the object is the whole UID), and the UIDs having
	// a tombstone with seq > since and no live row now. A deleted exception
	// whose master lives is therefore a changed UID, not a deleted one.
	// Tombstones recorded without a UID (files from early M2 builds) cannot
	// be named and are skipped.
	ChangedSince(since int64) (changed []Group, deletedUIDs []string, err error)
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
	// modernc honours TxOptions.ReadOnly with a deferred BEGIN, whatever the
	// DSN's _txlock says (tx.go), so no second handle is needed. The first read fixes the snapshot: read everything fixed up front.
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'epoch'`).Scan(&v.epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNoEpoch
		}
		return c.classify(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT
		MAX(COALESCE((SELECT MAX(seq) FROM events WHERE calendar_id = ?1), 0), COALESCE((SELECT MAX(seq) FROM tombstones WHERE calendar_id = ?1), 0)),
		COALESCE((SELECT MIN(seq) FROM tombstones WHERE calendar_id = ?1), 0)`, calID).Scan(&v.maxSeq, &v.minTomb); err != nil {
		return c.classify(err)
	}
	if err := fn(v); err != nil {
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
}

func (v *view) Epoch() string          { return v.epoch }
func (v *view) MaxSeq() int64          { return v.maxSeq }
func (v *view) MinTombstoneSeq() int64 { return v.minTomb }

const rowColumns = `uid, id, start_at, end_at, full_day, recurrence_id, last_edit, etag, seq, raw`
const rowOrder = ` ORDER BY uid, recurrence_id IS NOT NULL, recurrence_id, id`

// groups runs a query selecting rowColumns in rowOrder and groups it by UID.
func (v *view) groups(query string, args ...any) ([]Group, error) {
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
		if err := rows.Scan(&r.UID, &r.ID, &r.Start, &r.End, &r.FullDay, &rec, &r.LastEdit, &r.ETag, &r.Seq, &raw); err != nil {
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

func (v *view) Groups() ([]Group, error) {
	return v.groups(`SELECT `+rowColumns+` FROM events WHERE calendar_id = ?`+rowOrder, v.cal)
}

func (v *view) Group(uid string) (Group, bool, error) {
	gs, err := v.groups(`SELECT `+rowColumns+` FROM events WHERE calendar_id = ? AND uid = ?`+rowOrder, v.cal, uid)
	if err != nil || len(gs) == 0 {
		return Group{}, false, err
	}
	return gs[0], true, nil
}

func (v *view) ChangedSince(since int64) ([]Group, []string, error) {
	changed, err := v.groups(`SELECT `+rowColumns+` FROM events WHERE calendar_id = ?1
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
