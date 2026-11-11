package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

// Watch intervals: DefaultWatchInterval when none is given, and nothing
// shorter than MinWatchInterval (good citizen towards Proton).
const (
	DefaultWatchInterval = syncer.DefaultInterval
	MinWatchInterval     = syncer.MinInterval
)

const syncLockName = "sync.lock"

func partial(msg, hint string) *exitcode.Error { return fail(exitcode.Partial, msg, hint) }

// lockSync takes the sync lock: one sync (or watch) per state directory.
func (l *Local) lockSync() (func(), error) {
	if err := os.MkdirAll(l.o.Dirs.State, 0o700); err != nil {
		return nil, blocked("cannot take the sync lock", "make sure "+l.o.Dirs.State+" is writable by this user")
	}
	unlock, ok, err := filelock.TryLock(filepath.Join(l.o.Dirs.State, syncLockName))
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		return nil, final("sync locking needs a Unix system", "run pcb on Linux or macOS")
	case err != nil:
		return nil, blocked("cannot take the sync lock", "make sure "+l.o.Dirs.State+" is writable by this user")
	case !ok:
		return nil, blocked("a sync is already running", "wait for it or stop pcb sync --watch")
	}
	return unlock, nil
}

// Sync implements API.
func (l *Local) Sync(ctx context.Context, req SyncRequest) (SyncResult, error) {
	if l.closed.Load() {
		return SyncResult{}, errClosed()
	}
	unlock, err := l.lockSync()
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	return l.syncPass(ctx, req)
}

// WatchSync implements API. The sync lock is held for the whole watch; the
// service's own mutex is not (a login or logout may run meanwhile).
// req.Full applies until a pass gets through (exit 0 or 40); a partial pass
// is reported but does not make the watch back off.
func (l *Local) WatchSync(ctx context.Context, req SyncRequest, interval time.Duration, onPass func(WatchPass)) error {
	if l.closed.Load() {
		return errClosed()
	}
	interval, err := watchInterval(interval)
	if err != nil {
		return err
	}
	unlock, err := l.lockSync()
	if err != nil {
		return err
	}
	defer unlock()
	return l.watch(ctx, req, interval, onPass, l.syncPass)
}

// watchInterval is interval with its default, or a Usage error below the
// minimum.
func watchInterval(interval time.Duration) (time.Duration, error) {
	if interval == 0 {
		interval = DefaultWatchInterval
	}
	if interval < MinWatchInterval {
		return 0, usage("the watch interval is below "+MinWatchInterval.String(), "use an interval of "+MinWatchInterval.String()+" or more")
	}
	return interval, nil
}

// watch runs pass every interval until ctx ends (nil) or a pass needs a
// person (that error); the caller holds the sync lock.
func (l *Local) watch(ctx context.Context, req SyncRequest, interval time.Duration, onPass func(WatchPass),
	pass func(context.Context, SyncRequest) (SyncResult, error)) error {
	var (
		last    SyncResult
		lastErr error
		full    = req.Full
	)
	err := syncer.Watch(ctx, func(ctx context.Context) (syncer.Report, error) {
		last, lastErr = pass(ctx, SyncRequest{Full: full})
		if lastErr == nil || exitcode.From(lastErr) == exitcode.Partial {
			// The pass got through: --full is done, and a partial pass is
			// no session-wide failure (no back-off for the other calendars).
			full = false
			return syncer.Report{}, nil
		}
		return syncer.Report{}, lastErr
	}, syncer.WatchOptions{
		Interval: interval,
		Sleep:    l.o.Sleep,
		OnPass: func(_ syncer.Report, _ error, next time.Duration) {
			if onPass != nil {
				onPass(WatchPass{Result: last, Err: watchError(lastErr), Next: next})
			}
		},
		Stop:       stopsWatch,
		RetryAfter: retryAfter,
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil // the caller stopped the watch: a normal end
	}
	return l.fail(err)
}

// watchError is err without the "retry in X" hint of a rate limit: the
// watch waits that long by itself.
func watchError(err error) error {
	var (
		ee *exitcode.Error
		d  *detail
	)
	if errors.As(err, &d) && d.limited && errors.As(err, &ee) {
		return &exitcode.Error{Code: ee.Code, Err: ee.Err}
	}
	return err
}

// syncPass runs one pass on the stored session, without the sync lock.
func (l *Local) syncPass(ctx context.Context, req SyncRequest) (SyncResult, error) {
	return l.syncPassOn(ctx, req, nil, "")
}

// syncPassOn is syncPass on db, the cache Serve opened for account; with db
// nil the pass opens the cache itself. A pass on db for another account
// stops (the cache is the served account's).
func (l *Local) syncPassOn(ctx context.Context, req SyncRequest, db *cache.Cache, account string) (SyncResult, error) {
	var res SyncResult
	err := l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		res = SyncResult{}
		db := db
		switch {
		case db == nil:
			var err error
			if db, err = cache.Open(cache.Path(l.o.Dirs.State), acc.ID); err != nil {
				return cacheError(err)
			}
			defer func() { _ = db.Close() }()
		case acc.ID != account:
			return final("pcb is now logged in to another Proton account than pcb serve started with", "restart pcb serve")
		}
		describe := func(err error) string { return calendarError(ctx, err) }
		rep, err := syncer.Pass(ctx, c, db, syncer.Options{Full: req.Full, Now: l.o.Now, Describe: describe})
		if err != nil {
			return err
		}
		res = syncResult(rep, describe)
		return nil
	})
	if err != nil {
		return SyncResult{}, err
	}
	failed := 0
	for _, c := range res.Calendars {
		if c.Error != "" {
			failed++
		}
	}
	if failed > 0 {
		return res, partial(fmt.Sprintf("%d of %d calendars failed to sync", failed, len(res.Calendars)),
			"run pcb sync again; pcb status shows each calendar's last error")
	}
	return res, nil
}

// syncResult is the pass's report, each calendar's failure described as
// the cache stored it.
func syncResult(rep syncer.Report, describe func(error) string) SyncResult {
	res := SyncResult{Calendars: make([]CalendarSync, 0, len(rep.Calendars))}
	for _, r := range rep.Calendars {
		cs := CalendarSync{
			ID: r.ID, Name: r.Name, Mode: r.Mode, Events: r.Events,
			Added: r.Changes.Added, Updated: r.Changes.Updated, Deleted: r.Changes.Deleted,
		}
		if r.Err != nil {
			cs.Error = describe(r.Err)
		}
		res.Calendars = append(res.Calendars, cs)
	}
	return res
}

// calendarError is the secret-free reason a calendar's sync failed: IDs and
// Proton's status only, never row content or the text of an unknown error.
func calendarError(ctx context.Context, err error) string {
	var (
		de *syncer.ResyncDeferredError
		ue *syncer.UnverifiedError
		ae *api.Error
	)
	switch {
	case errors.As(err, &de):
		return de.Error()
	case errors.As(err, &ue):
		return ue.Error()
	case errors.As(err, &ae):
		return protonDown(ae).Error()
	case ctx.Err() != nil:
		return "the sync was cancelled"
	}
	return "the calendar could not be synced"
}

// cacheError maps a failure to open or read the cache.
func cacheError(err error) error {
	var ee *exitcode.Error
	if errors.As(err, &ee) || errors.Is(err, context.Canceled) {
		return err
	}
	if f := cacheFailure(err); f != nil {
		return f
	}
	return blocked("the event cache cannot be opened", "run pcb sync (it updates an older cache), then pcb doctor if it persists")
}

// openCache opens the cache of the logged account for reading only (outside
// Serve: see readCache). The cache is nil (and err nil) when there is none
// for this account.
func (l *Local) openCache() (*cache.Cache, error) {
	st, err := store.LoadState(l.o.Dirs.State)
	if err != nil {
		return nil, stateError(err)
	}
	if st.AccountID == "" {
		return nil, nil
	}
	db, err := cache.OpenReadOnly(cache.Path(l.o.Dirs.State), st.AccountID)
	if errors.Is(err, cache.ErrNoCache) {
		return nil, nil
	}
	if err != nil {
		return nil, cacheError(err)
	}
	return db, nil
}

// syncStatus is the Status section: nil when there is no cache for the
// account, with Error set when there is one that cannot be read.
func (l *Local) syncStatus() *SyncStatus {
	db, release, err := l.readCache()
	if err != nil {
		return &SyncStatus{Calendars: []CalendarStatus{}, Error: err.Error()}
	}
	defer release()
	if db == nil {
		return nil
	}
	cals, err := db.Calendars(context.Background())
	if err != nil {
		return &SyncStatus{Calendars: []CalendarStatus{}, Error: cacheError(err).Error()}
	}
	out := &SyncStatus{Calendars: make([]CalendarStatus, 0, len(cals))}
	out.Undecryptable, out.DuplicateMasters = l.r.counts()
	for _, c := range cals {
		cs := CalendarStatus{ID: c.ID, Name: c.Name, Events: c.Events, Error: c.Error}
		if ck := l.r.calKeys(c.ID); ck != nil {
			cs.KeysError = ck.err
		}
		if !c.SyncedAt.IsZero() {
			cs.SyncedAt = &c.SyncedAt
		}
		if !c.FullAt.IsZero() {
			cs.FullAt = &c.FullAt
		}
		out.Calendars = append(out.Calendars, cs)
	}
	return out
}

// Events implements API.
func (l *Local) Events(ctx context.Context, req EventsRequest) ([]Event, error) {
	if l.closed.Load() {
		return nil, errClosed()
	}
	if strings.TrimSpace(req.Calendar) == "" {
		return nil, usage("a calendar is required", "pass the calendar's ID or exact name (pcb calendar list)")
	}
	win, err := newWindow(req)
	if err != nil {
		return nil, err
	}
	var cal cache.Calendar
	var rows []cache.Event
	err = l.withCache(func(db *cache.Cache) error {
		cals, err := db.Calendars(ctx)
		if err != nil {
			return l.fail(cacheError(err))
		}
		if len(cals) == 0 {
			return notSynced()
		}
		if cal, err = pickCalendar(cals, req.Calendar); err != nil {
			return err
		}
		if cal.SyncedAt.IsZero() {
			return mustAct("calendar "+cal.ID+" has not been synced yet", "run pcb sync")
		}
		if rows, err = db.Events(ctx, cal.ID); err != nil {
			return l.fail(cacheError(err))
		}
		return nil
	})
	if errors.Is(err, errNotCached) {
		return nil, l.notCached(ctx, req.Calendar) // the cache is released: this may call Proton
	}
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		if !win.keeps(r) {
			continue
		}
		ev := Event{
			ID: r.ID, UID: r.UID, CalendarID: cal.ID,
			Start: time.Unix(r.Start, 0).UTC(), End: time.Unix(r.End, 0).UTC(),
			AllDay: r.FullDay, Recurring: r.RRule != "",
			LastEdit: time.Unix(r.LastEditTime, 0).UTC(), ETag: r.ETag,
		}
		if r.FullDay {
			ev.StartDate, ev.EndDate = dayOf(r.Start), dayOf(allDayEnd(r))
		}
		if r.RecurrenceID != nil {
			t := time.Unix(*r.RecurrenceID, 0).UTC()
			ev.RecurrenceID = &t
		}
		out = append(out, ev)
	}
	return out, nil
}

// errNotCached: no cached calendar has that ID or name.
var errNotCached = errors.New("no such calendar in the cache")

// notCached tells a calendar Proton has but the cache does not (added since
// the last sync: run pcb sync) from one that does not exist (check the
// name). It asks Proton for the calendar list, the one network call of a
// read, and only on this error path; when Proton cannot be asked, the
// message covers both.
func (l *Local) notCached(ctx context.Context, ref string) error {
	var cals []calendar.Calendar
	err := l.withSession(ctx, func(ctx context.Context, c *api.Client, _ store.Account) error {
		var err error
		cals, err = calendar.List(ctx, c)
		return err
	})
	var ee *exitcode.Error
	switch {
	case errors.Is(err, context.Canceled), errors.As(err, &ee) && ee.Code == exitcode.MustAct:
		return err // Ctrl-C, or a person is needed (log in again): not a naming problem
	case err != nil:
		return usage(fmt.Sprintf("no calendar named %q in the event cache", ref),
			"pcb calendar list shows your calendars; one added since the last sync needs pcb sync first")
	case slices.ContainsFunc(cals, func(c calendar.Calendar) bool { return c.ID == ref || c.Name == ref }):
		return mustAct(fmt.Sprintf("calendar %q is not synced yet", ref), "run pcb sync")
	}
	return usage(fmt.Sprintf("no calendar named %q", ref), "pcb calendar list")
}

// pickCalendar finds the calendar whose ID is ref, else the one whose name
// is exactly ref; errNotCached when there is none.
func pickCalendar(cals []cache.Calendar, ref string) (cache.Calendar, error) {
	if i := slices.IndexFunc(cals, func(c cache.Calendar) bool { return c.ID == ref }); i >= 0 {
		return cals[i], nil
	}
	var named []cache.Calendar
	for _, c := range cals {
		if c.Name == ref {
			named = append(named, c)
		}
	}
	switch len(named) {
	case 0:
		return cache.Calendar{}, errNotCached
	case 1:
		return named[0], nil
	}
	ids := make([]string, len(named))
	for i, c := range named {
		ids[i] = c.ID
	}
	return cache.Calendar{}, usage(fmt.Sprintf("%d calendars are named %q: %s", len(named), ref, strings.Join(ids, ", ")),
		"pass the calendar's ID instead of its name")
}

const dateLayout = time.DateOnly

func dayOf(unix int64) string { return time.Unix(unix, 0).UTC().Format(dateLayout) }

// allDayEnd is an all-day row's exclusive end: a row ending at or before its
// start still covers its one day.
func allDayEnd(r cache.Event) int64 {
	if r.End <= r.Start {
		return r.Start + 24*3600
	}
	return r.End
}

// window is an EventsRequest's bounds.
type window struct {
	from, to       *time.Time
	fromDay, toDay string
}

func newWindow(req EventsRequest) (window, error) {
	for _, d := range []string{req.FromDate, req.ToDate} {
		if _, err := time.Parse(dateLayout, d); d != "" && err != nil {
			return window{}, usage(fmt.Sprintf("%q is not a YYYY-MM-DD date", d), "write dates as YYYY-MM-DD")
		}
	}
	return window{req.From, req.To, req.FromDate, req.ToDate}, nil
}

// keeps reports whether the row overlaps [from, to), and always keeps a
// recurring master starting before to. All-day rows use the date bounds
// when there are any, compared as dates; everything else is compared as
// instants.
func (w window) keeps(r cache.Event) bool {
	start := time.Unix(r.Start, 0)
	switch {
	case r.FullDay && w.toDay != "":
		if dayOf(r.Start) >= w.toDay {
			return false
		}
	case w.to != nil && !start.Before(*w.to):
		return false
	}
	if r.RRule != "" && r.RecurrenceID == nil {
		return true
	}
	switch {
	case r.FullDay && w.fromDay != "":
		return dayOf(allDayEnd(r)) > w.fromDay
	case w.from != nil:
		end := time.Unix(r.End, 0)
		return end.After(*w.from) || (!end.After(start) && !start.Before(*w.from))
	}
	return true
}
