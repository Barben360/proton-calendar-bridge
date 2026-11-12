// Package syncer is C4: it brings the cache up to date with Proton, one pass
// over every calendar, and repeats passes in a watch loop. A calendar is
// listed in full when it was never synced, when Proton asks, when its cursor
// or one of its rows is refused, or its feed is too long (at most once per
// FallbackGap: meanwhile the calendar is skipped and its feed left unread),
// or when its last listing is older than FullEvery; otherwise its
// change feed is read and every changed ID re-read. A cursor is only stored
// in the transaction that stores the rows read up to it, so a failure never
// loses a change: the next pass reads it again. Nothing is decrypted, keys
// are never unlocked, and nothing is logged.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// DefaultFullEvery is how old a calendar's last full listing may get.
const DefaultFullEvery = 24 * time.Hour

// FallbackGap is the shortest time between a calendar's last full listing and
// a fallback one (Refresh, refused cursor, refused row read): a calendar
// whose feed keeps failing is not listed on every pass (good citizen).
const FallbackGap = 5 * time.Minute

// Why a calendar needs a fallback full listing (stored in the cache while
// it is deferred).
const (
	CauseRefresh = "refresh" // the feed answered Refresh
	CauseCursor  = "cursor"  // the feed refused the cursor (a 4xx other than 429)
	CauseRow     = "row"     // a changed row could not be read (ruling R8)
	CauseTooLong = "long"    // the feed ran past events.MaxFeedPages
)

var causeText = map[string]string{
	CauseRefresh: "Proton asked to refresh the feed",
	CauseCursor:  "Proton refused the feed cursor",
	CauseRow:     "Proton refused to read a changed row",
	CauseTooLong: fmt.Sprintf("the feed holds more than %d pages", events.MaxFeedPages),
}

// ResyncDeferredError is a calendar's error when it needed a fallback full
// listing less than FallbackGap after its last one: it is skipped, cursor
// kept and feed unread, until Until, then listed. Cause is one of the
// Cause constants.
type ResyncDeferredError struct {
	Cause string
	Until time.Time
}

// Error implements error.
func (e *ResyncDeferredError) Error() string {
	why, ok := causeText[e.Cause]
	if !ok {
		why = "the calendar needs a full listing"
	}
	return why + "; retrying with a full listing after " + e.Until.UTC().Format(time.RFC3339)
}

// StoreError is a failure of the cache itself during a pass (reading or
// writing it). It stops the whole pass: the cache is shared by every
// calendar, so it is never one calendar's failure. Err keeps the cache's
// typed cause (cache.ErrCorrupt, *cache.IOError) for the caller to map.
type StoreError struct{ Err error }

// Error implements error.
func (e *StoreError) Error() string { return "event cache: " + e.Err.Error() }

// Unwrap returns the cache's error.
func (e *StoreError) Unwrap() error { return e.Err }

func storeErr(format string, args ...any) error {
	return &StoreError{Err: fmt.Errorf(format, args...)}
}

// Modes of a calendar's sync in a pass.
const (
	ModeInitial     = "initial"     // never synced: listed in full
	ModeIncremental = "incremental" // change feed read from the stored cursor
	ModeFull        = "full"        // listed in full again
)

// Options tune a pass. The zero value is a normal pass.
type Options struct {
	Full      bool             // list every calendar in full
	Now       func() time.Time // default time.Now
	FullEvery time.Duration    // default DefaultFullEvery
	// Describe is the text a calendar's failure is stored as (Calendar.Error,
	// what pcb status shows); default err.Error(). The service passes its
	// secret-free describer, so the cache never holds Proton's raw text.
	Describe func(error) string
	// Lock, when set, is called before each calendar is synced or dropped,
	// and its result runs once the calendar's rows are stored or dropped:
	// the service holds a calendar's lock there, so its writes and the
	// pass's reads and applies of that calendar never interleave (ruling
	// M4-R5). An error (ctx ended while waiting) ends the pass with it.
	Lock func(ctx context.Context, calID string) (unlock func(), err error)
}

// Report is what a pass did, one entry per listed calendar in API order.
type Report struct{ Calendars []CalendarReport }

// CalendarReport is one calendar's outcome. Err is set when this calendar
// failed (the pass went on with the others); Events is the calendar's row
// count in the cache after the pass.
type CalendarReport struct {
	ID, Name string
	Mode     string // ModeInitial, ModeIncremental or ModeFull
	Changes  cache.Changes
	Events   int
	Err      error
}

// Pass syncs every calendar once. A session-wide failure (api.SessionWide)
// or any failure of the cache (*StoreError) is returned at once, without a
// report; any other failure is recorded on its calendar
// (CalendarReport.Err and the cached Error) and the pass goes on. A
// calendar's Error is cleared when it syncs.
func Pass(ctx context.Context, c *api.Client, db *cache.Cache, o Options) (Report, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.FullEvery <= 0 {
		o.FullEvery = DefaultFullEvery
	}
	if o.Describe == nil {
		o.Describe = func(err error) string { return err.Error() }
	}
	cals, err := calendar.List(ctx, c)
	if err != nil {
		return Report{}, err
	}
	known, err := db.Calendars(ctx)
	if err != nil {
		return Report{}, storeErr("reading the cache: %w", err)
	}
	listed := make(map[string]bool, len(cals))
	for _, cal := range cals {
		listed[cal.ID] = true
	}
	prev := make(map[string]cache.Calendar, len(known))
	for _, k := range known {
		prev[k.ID] = k
		if !listed[k.ID] {
			unlock, err := lockCalendar(ctx, o, k.ID)
			if err != nil {
				return Report{}, err
			}
			err = db.DropCalendar(ctx, k.ID)
			unlock()
			if err != nil {
				return Report{}, storeErr("dropping calendar %s from the cache: %w", k.ID, err)
			}
		}
	}
	for _, cal := range cals {
		// Keep the last error until this pass knows better.
		rec := cache.Calendar{ID: cal.ID, Name: cal.Name, Type: cal.Type.String(), Color: cal.Color, Description: cal.Description, Error: prev[cal.ID].Error}
		if err := db.PutCalendar(ctx, rec); err != nil {
			return Report{}, storeErr("recording calendar %s in the cache: %w", cal.ID, err)
		}
	}
	rep := Report{Calendars: make([]CalendarReport, 0, len(cals))}
	for _, cal := range cals {
		unlock, err := lockCalendar(ctx, o, cal.ID)
		if err != nil {
			return Report{}, err
		}
		r := syncCalendar(ctx, c, db, cal.ID, prev[cal.ID], o)
		unlock()
		r.ID, r.Name = cal.ID, cal.Name
		var se *StoreError
		if r.Err != nil && (errors.As(r.Err, &se) || api.SessionWide(ctx, r.Err)) {
			return Report{}, r.Err
		}
		rec := cache.Calendar{ID: cal.ID, Name: cal.Name, Type: cal.Type.String(), Color: cal.Color, Description: cal.Description}
		if r.Err != nil {
			rec.Error = o.Describe(r.Err)
		}
		if err := db.PutCalendar(ctx, rec); err != nil {
			return Report{}, storeErr("recording calendar %s in the cache: %w", cal.ID, err)
		}
		rep.Calendars = append(rep.Calendars, r)
	}
	after, err := db.Calendars(ctx)
	if err != nil {
		return Report{}, storeErr("reading the cache: %w", err)
	}
	count := make(map[string]int, len(after))
	for _, a := range after {
		count[a.ID] = a.Events
	}
	for i := range rep.Calendars {
		rep.Calendars[i].Events = count[rep.Calendars[i].ID]
	}
	return rep, nil
}

// syncCalendar brings one calendar up to date; prev is its cached state
// (zero for a calendar never seen). ID and Name are left to the caller.
func syncCalendar(ctx context.Context, c *api.Client, db *cache.Cache, calID string, prev cache.Calendar, o Options) CalendarReport {
	now := o.Now()
	var r CalendarReport
	next := prev.FullAt.Add(FallbackGap)
	switch {
	case prev.Cursor == "":
		r.Mode = ModeInitial
	case o.Full || now.Sub(prev.FullAt) > o.FullEvery:
		r.Mode = ModeFull
	case prev.Resync != "":
		// A fallback listing is pending: the feed is not read again.
		r.Mode = ModeFull
		if now.Before(next) {
			r.Err = &ResyncDeferredError{Cause: prev.Resync, Until: next}
			return r
		}
	default:
		r.Mode = ModeIncremental
		ch, cause, err := incremental(ctx, c, db, calID, prev.Cursor, now)
		if cause == "" {
			r.Changes, r.Err = ch, err
			return r
		}
		r.Mode = ModeFull
		if now.Before(next) {
			// Keep the cursor; list once the gap is over.
			if err := db.DeferResync(ctx, calID, cause); err != nil {
				r.Err = storeErr("deferring the listing of calendar %s: %w", calID, err)
				return r
			}
			r.Err = &ResyncDeferredError{Cause: cause, Until: next}
			return r
		}
	}
	r.Changes, r.Err = full(ctx, c, db, calID, now)
	return r
}

// MaxListings bounds the listings of one full sync: a listing that looks
// unstable is repeated, at most MaxListings-1 more times (ruling R17).
const MaxListings = 3

// UnverifiedError is a calendar's note when its listing never settled and
// some cached rows missing from every listing could not be re-read: they
// were kept as cached. The rest of the listing was stored; the note says
// the next listing should check again.
type UnverifiedError struct{ Rows int }

// Error implements error.
func (e *UnverifiedError) Error() string {
	return fmt.Sprintf("the listing kept changing and %d cached events could not be checked; they were kept, the next listing checks again", e.Rows)
}

// full lists the calendar and makes the listing its content. The baseline
// cursor is the feed head read before the first listing: a change made
// while a listing runs is then read again from the feed, never skipped.
//
// The listing pages by offset, so a concurrent create or delete can shift a
// page: a row is read twice (List dedupes it) or an unchanged row is
// skipped. A listing is stable when the head did not move while it ran and
// it holds as many unique rows as its Total; an unstable one is repeated
// (ruling R17), and the first stable listing is the content. When none of
// MaxListings is stable, the content is the union of their rows, and every
// cached row missing from all of them is re-read before it is tombstoned
// (gone: tombstoned; found: kept as read; unreadable: kept as cached, and
// the result is an *UnverifiedError note with the changes stored). The
// quiet case costs one listing and one extra head read.
func full(ctx context.Context, c *api.Client, db *cache.Cache, calID string, now time.Time) (cache.Changes, error) {
	baseline, err := events.Latest(ctx, c, calID)
	if err != nil {
		return cache.Changes{}, err
	}
	var (
		rows   []events.Row
		stable bool
		at     = map[string]int{} // union: ID -> index in rows
		before = baseline
	)
	for range MaxListings {
		l, err := events.List(ctx, c, calID)
		if err != nil {
			return cache.Changes{}, err
		}
		after, err := events.Latest(ctx, c, calID)
		if err != nil {
			return cache.Changes{}, err
		}
		if after == before && (l.Total < 0 || len(l.Rows) == l.Total) {
			rows, stable = l.Rows, true
			break
		}
		for _, r := range l.Rows { // the later copy wins
			if i, ok := at[r.ID]; ok {
				rows[i] = r
				continue
			}
			at[r.ID] = len(rows)
			rows = append(rows, r)
		}
		before = after
	}
	out := make([]cache.Row, 0, len(rows))
	listed := make(map[string]bool, len(rows))
	for _, row := range rows {
		out = append(out, toCache(row))
		listed[row.ID] = true
	}
	var note error
	if !stable {
		kept, unverified, err := missing(ctx, c, db, calID, listed)
		if err != nil {
			return cache.Changes{}, err
		}
		out = append(out, kept...)
		if unverified > 0 {
			note = &UnverifiedError{Rows: unverified}
		}
	}
	ch, err := db.ReplaceCalendar(ctx, calID, baseline, out, now)
	if err != nil {
		return cache.Changes{}, storeErr("storing calendar %s: %w", calID, err)
	}
	return ch, note
}

// missing re-reads the cached rows of calID absent from listed and returns
// those to keep: as Proton returned them, or as cached when Proton refused
// the read (counted in unverified). A session-wide failure is returned.
func missing(ctx context.Context, c *api.Client, db *cache.Cache, calID string, listed map[string]bool) (kept []cache.Row, unverified int, err error) {
	cached, err := db.Events(ctx, calID)
	if err != nil {
		return nil, 0, storeErr("reading calendar %s: %w", calID, err)
	}
	for _, e := range cached {
		if listed[e.ID] {
			continue
		}
		row, err := events.Get(ctx, c, calID, e.ID)
		switch {
		case errors.Is(err, events.ErrGone):
		case err != nil && api.SessionWide(ctx, err):
			return nil, 0, fmt.Errorf("event %s: %w", e.ID, err)
		case err != nil:
			raw, err := db.Raw(ctx, calID, e.ID)
			if err != nil {
				return nil, 0, storeErr("reading event %s: %w", e.ID, err)
			}
			kept = append(kept, cache.Row{ID: e.ID, UID: e.UID, Start: e.Start, End: e.End, FullDay: e.FullDay,
				RecurrenceID: e.RecurrenceID, RRule: e.RRule, LastEditTime: e.LastEditTime, Raw: raw})
			unverified++
		default:
			kept = append(kept, toCache(row))
		}
	}
	return kept, unverified, nil
}

// incremental applies the feed after cursor. cause is set (one of the Cause
// constants) when the calendar must be listed instead: Proton asked
// (Refresh), the feed is too long, Proton refused the cursor with a 4xx
// other than 429, or refused to read a row with an error neither
// session-wide nor gone (ruling R8: listing beats holding the cursor
// forever). Every listed ID is re-read: a row Proton says is gone is
// deleted, whatever the feed said. Nothing is stored unless every row was
// read.
func incremental(ctx context.Context, c *api.Client, db *cache.Cache, calID, cursor string, now time.Time) (ch cache.Changes, cause string, err error) {
	d, err := events.Changes(ctx, c, calID, cursor)
	if err != nil {
		if !api.SessionWide(ctx, err) && cursorRefused(err) {
			return cache.Changes{}, CauseCursor, err
		}
		return cache.Changes{}, "", err
	}
	switch {
	case d.TooLong:
		return cache.Changes{}, CauseTooLong, nil
	case d.Refresh:
		return cache.Changes{}, CauseRefresh, nil
	}
	var upserts []cache.Row
	var gone []string
	for _, id := range d.IDs {
		row, err := events.Get(ctx, c, calID, id)
		switch {
		case errors.Is(err, events.ErrGone):
			gone = append(gone, id)
		case err != nil:
			if api.SessionWide(ctx, err) {
				return cache.Changes{}, "", fmt.Errorf("event %s: %w", id, err)
			}
			return cache.Changes{}, CauseRow, fmt.Errorf("event %s: %w", id, err)
		default:
			upserts = append(upserts, toCache(row))
		}
	}
	ch, err = db.ApplyDelta(ctx, calID, d.Cursor, upserts, gone, now)
	if err != nil {
		return cache.Changes{}, "", storeErr("storing calendar %s: %w", calID, err)
	}
	return ch, "", nil
}

// cursorRefused reports whether the feed answered a 4xx other than 429 (and
// 401, which the client already turned into a refresh or a session end).
func cursorRefused(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 &&
		ae.Status != http.StatusTooManyRequests && ae.Status != http.StatusUnauthorized
}

// toCache converts a Proton row to the cache's own type (ruling R6).
func toCache(r events.Row) cache.Row {
	return cache.Row{
		ID: r.ID, UID: r.UID, Start: r.Start, End: r.End, FullDay: r.FullDay,
		RecurrenceID: r.RecurrenceID, RRule: r.RRule, LastEditTime: r.LastEditTime, Raw: r.Raw,
	}
}

// lockCalendar takes calID's lock through o.Lock, when set.
func lockCalendar(ctx context.Context, o Options, calID string) (func(), error) {
	if o.Lock == nil {
		return func() {}, nil
	}
	return o.Lock(ctx, calID)
}
