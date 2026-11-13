package service

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// The agenda (M8, design §5.6) is the per-account API the MCP tools call:
// occurrences over a range, one event in full, free/busy, and edits as
// patches of the stored iCalendar. It reads what the CalDAV read API reads
// (ObjectsByUID: the cache, rendered objects kept by ETag) and writes
// through PutObject and DeleteObject, so every M4 rule holds and writes run
// only during Serve.

// The iCalendar types the agenda speaks, named here so its callers (the MCP
// adapter) need not import internal/ical.
type (
	EventFields = ical.Fields
	EventData   = ical.EventData
	EventTime   = ical.Time
	EventPerson = ical.Person
	EventAlarm  = ical.Alarm
	EditScope   = ical.Scope
	Occurrence  = ical.Occurrence
)

// The edit scopes: the series (its master) or one occurrence.
const (
	ScopeSeries     = ical.ScopeSeries
	ScopeOccurrence = ical.ScopeOccurrence
)

// The agenda's limits (design §5.6, §5.7).
const (
	MaxAgendaRange       = 366 * 24 * time.Hour // the longest range of one call
	DefaultAgendaLimit   = 100                  // occurrences per page when Limit is 0
	MaxAgendaLimit       = 500                  // the largest Limit
	MaxSeriesOccurrences = 1000                 // occurrences of one series per call
	renderBudget         = 2000                 // objects one call may render
)

// zoneSlack widens the cache's plaintext times, which pick the objects to
// render: an all-day or floating time is read in the request's zone, at
// most 14 hours from the UTC instant the row holds.
const zoneSlack = int64(24 * 3600)

// EventRef names an event, or one occurrence of it: RecurrenceID is in the
// form of the series' DTSTART (a date for an all-day series, the wall
// clock with Floating for a floating one, the instant otherwise), so it
// stays valid whatever zone a later call reads in.
type EventRef struct {
	CalendarID, UID string
	RecurrenceID    *EventTime
}

// OccurrencesRequest asks for the occurrences overlapping [From, To).
// Calendars are IDs, empty for every synced calendar. Loc reads all-day
// and floating times (nil: UTC). Text keeps the occurrences whose summary,
// location or description contains it, case-insensitively ("": all).
// Limit is the page size (0: DefaultAgendaLimit, at most MaxAgendaLimit);
// Cursor is a page's Next.
type OccurrencesRequest struct {
	Calendars []string
	From, To  time.Time
	Loc       *time.Location
	Text      string
	Limit     int
	Cursor    string
}

// AgendaOccurrence is one occurrence with what a list shows of it. Summary,
// Location, Description, ExternalOrganizer and Busy are the occurrence's
// own (an override's when it is one). ExternalOrganizer: its ORGANIZER is not one
// of the account's addresses. Busy: TRANSP is not TRANSPARENT and STATUS is
// not CANCELLED.
type AgendaOccurrence struct {
	Ref               EventRef
	ETag              string
	Occurrence        Occurrence
	Summary, Location string
	Description       string
	CalendarType      string
	ExternalOrganizer bool
	Busy              bool
}

// OccurrencePage is a page of occurrences sorted by start (then calendar,
// UID and recurrence ID). Truncated: more follow, from the cursor Next.
// Skipped counts the events of the range that cannot be read (an unknown
// TZID, a recurrence outside Proton's set, a stored object that does not
// parse): they are left out, never failing the page (ruling R9).
type OccurrencePage struct {
	Items     []AgendaOccurrence
	Truncated bool
	Next      string
	Skipped   int
}

// EventDetail is one event in full: Data is the whole object (master and
// overrides). ReadOnly follows the calendar's write gate.
type EventDetail struct {
	Ref          EventRef
	ETag         string
	CalendarType string
	ReadOnly     bool
	Data         EventData
}

// UpdateResult is an update's outcome: the new ETag, or with a dry run the
// unchanged ETag and the object before and after the change.
type UpdateResult struct {
	ETag          string
	Before, After EventData
}

// Agenda is the per-account API of the MCP tools. Errors are
// *exitcode.Error: a stale ETag has cause ErrPreconditionFailed (nothing is
// written); a change pcb refuses (the codec's or the write path's) is a
// MustAct whose cause is a *WriteRefusal naming its code; a range over
// MaxAgendaRange, a Limit over MaxAgendaLimit or a bad cursor is Usage.
type Agenda interface {
	// Occurrences returns a page of the occurrences overlapping the range.
	// A page may hold fewer than Limit and still be Truncated when the
	// call's render budget (2000 objects) ran out.
	Occurrences(ctx context.Context, req OccurrencesRequest) (OccurrencePage, error)
	// Event returns the event ref names (its whole object, whatever
	// ref.RecurrenceID says); one that cannot be read is a MustAct naming
	// why.
	Event(ctx context.Context, ref EventRef) (EventDetail, error)
	// FreeBusy returns every busy occurrence of the range (Text, Limit and
	// Cursor are ignored), read as Occurrences reads them, and how many
	// events of the range it skipped (OccurrencePage.Skipped summed over
	// its pages): their times are unknown, so they may be busy (ruling R13).
	FreeBusy(ctx context.Context, req OccurrencesRequest) ([]Occurrence, int, error)
	// CreateEvent creates an event with a new UID in calendar calID and
	// returns its ref and ETag ("" when it could not be read back).
	CreateEvent(ctx context.Context, calID string, f EventFields) (EventRef, string, error)
	// UpdateEvent applies f to the event (scope series) or to the
	// occurrence ref.RecurrenceID names (scope occurrence), if its ETag is
	// still etag. dryRun writes nothing and returns the before and after.
	UpdateEvent(ctx context.Context, ref EventRef, etag string, scope EditScope, f EventFields, dryRun bool) (UpdateResult, error)
	// DeleteEvent deletes the event (scope series) or the occurrence
	// ref.RecurrenceID names (scope occurrence: an EXDATE), if its ETag is
	// still etag.
	DeleteEvent(ctx context.Context, ref EventRef, etag string, scope EditScope) error
}

var _ Agenda = (*Local)(nil)

// agendaKey orders occurrences: start, calendar, UID, recurrence ID (none
// first). Times are Unix seconds (iCalendar times are whole seconds). A
// cursor is the key of the last occurrence returned, or a key with an empty
// calendar standing before every occurrence starting at Start.
type agendaKey struct {
	Start int64  `json:"s"`
	Cal   string `json:"c,omitempty"`
	UID   string `json:"u,omitempty"`
	RID   *int64 `json:"r,omitempty"`
}

func (k agendaKey) compare(o agendaKey) int {
	if c := cmp.Compare(k.Start, o.Start); c != 0 {
		return c
	}
	if c := cmp.Compare(k.Cal, o.Cal); c != 0 {
		return c
	}
	if c := cmp.Compare(k.UID, o.UID); c != 0 {
		return c
	}
	switch {
	case k.RID == nil && o.RID == nil:
		return 0
	case k.RID == nil:
		return -1
	case o.RID == nil:
		return 1
	}
	return cmp.Compare(*k.RID, *o.RID)
}

// Cursor is the cursor that resumes right after o, for the request whose
// page held it: a caller merging several pages may stop in the middle of
// one.
func (o AgendaOccurrence) Cursor() string { return keyOf(o).cursor() }

func keyOf(o AgendaOccurrence) agendaKey {
	k := agendaKey{Start: o.Occurrence.Start.Unix(), Cal: o.Ref.CalendarID, UID: o.Ref.UID}
	if o.Occurrence.RecurrenceID != nil {
		rid := o.Occurrence.RecurrenceID.Unix()
		k.RID = &rid
	}
	return k
}

func (k agendaKey) cursor() string {
	b, _ := json.Marshal(k)
	return base64.RawURLEncoding.EncodeToString(b)
}

func parseCursor(s string) (agendaKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	var k agendaKey
	if err == nil {
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		err = dec.Decode(&k)
	}
	if err != nil {
		return agendaKey{}, usage("the cursor is not one this agenda returned", "start again without a cursor")
	}
	return k, nil
}

// agendaQuery is a checked OccurrencesRequest: from is the request's From,
// or the cursor's start when later (nothing before it is returned).
type agendaQuery struct {
	from, to time.Time
	loc      *time.Location
	limit    int
	after    *agendaKey
}

func readQuery(req OccurrencesRequest) (agendaQuery, error) {
	q := agendaQuery{from: req.From, to: req.To, loc: req.Loc, limit: req.Limit}
	switch {
	case req.From.IsZero() || req.To.IsZero():
		return q, usage("the range needs a start and an end", "give both bounds")
	case !req.To.After(req.From):
		return q, usage("the range ends before it starts", "give an end after the start")
	case req.To.Sub(req.From) > MaxAgendaRange:
		return q, usage("the range is longer than 366 days", "ask for at most 366 days at a time")
	case req.Limit < 0 || req.Limit > MaxAgendaLimit:
		return q, usage(fmt.Sprintf("the limit must be between 1 and %d", MaxAgendaLimit), "ask for fewer occurrences per page")
	}
	if q.limit == 0 {
		q.limit = DefaultAgendaLimit
	}
	if q.loc == nil {
		q.loc = time.UTC
	}
	if req.Cursor != "" {
		k, err := parseCursor(req.Cursor)
		if err != nil {
			return q, err
		}
		q.after = &k
		if at := time.Unix(k.Start, 0); at.After(q.from) {
			q.from = at
		}
	}
	return q, nil
}

// candidate is an object that may have occurrences in the range: low is a
// lower bound of the starts of those the page may return.
type candidate struct {
	cal, uid string
	low      int64
}

// keepFunc decides whether an occurrence is returned, from its list view
// and the component it comes from.
type keepFunc func(o *AgendaOccurrence, src *EventData) bool

// Occurrences implements Agenda.
func (l *Local) Occurrences(ctx context.Context, req OccurrencesRequest) (OccurrencePage, error) {
	text := strings.ToLower(req.Text)
	return l.occurrences(ctx, req, func(_ *AgendaOccurrence, src *EventData) bool {
		return text == "" || strings.Contains(strings.ToLower(src.Summary), text) ||
			strings.Contains(strings.ToLower(src.Location), text) || strings.Contains(strings.ToLower(src.Description), text)
	})
}

// FreeBusy implements Agenda: the busy occurrences of every page.
func (l *Local) FreeBusy(ctx context.Context, req OccurrencesRequest) ([]Occurrence, int, error) {
	req.Text, req.Cursor, req.Limit = "", "", MaxAgendaLimit
	out := []Occurrence{}
	skipped := 0
	for {
		p, err := l.occurrences(ctx, req, func(o *AgendaOccurrence, _ *EventData) bool { return o.Busy })
		if err != nil {
			return nil, 0, err
		}
		skipped += p.Skipped
		for _, it := range p.Items {
			out = append(out, it.Occurrence)
		}
		if !p.Truncated {
			return out, skipped, nil
		}
		req.Cursor = p.Next
	}
}

// occurrences is one page of the occurrences keep returns. It picks the
// objects to render from the cache's plaintext times (candidates), renders
// at most the budget of them in order of their earliest possible start, and
// expands each. A page cut by the budget, or by a series' cap, stops before
// the first start it cannot vouch for, and its cursor resumes there.
func (l *Local) occurrences(ctx context.Context, req OccurrencesRequest, keep keepFunc) (OccurrencePage, error) {
	if l.closed.Load() {
		return OccurrencePage{}, errClosed()
	}
	q, err := readQuery(req)
	if err != nil {
		return OccurrencePage{}, err
	}
	page := OccurrencePage{Items: []AgendaOccurrence{}}
	if !q.from.Before(q.to) {
		return page, nil
	}
	cands, types, err := l.candidates(ctx, req.Calendars, q)
	if err != nil {
		return OccurrencePage{}, err
	}
	budget := renderBudget
	if l.agendaBudget > 0 {
		budget = l.agendaBudget
	}
	cutoff := int64(math.MaxInt64) // no occurrence starting at or after it is returned
	if len(cands) > budget {
		cutoff = cands[budget].low
		cands = cands[:budget]
	}
	var order []string
	byCal := map[string][]string{}
	for _, c := range cands {
		if byCal[c.cal] == nil {
			order = append(order, c.cal)
		}
		byCal[c.cal] = append(byCal[c.cal], c.uid)
	}
	rendered := map[string][]Object{}
	for _, cal := range order {
		objs, err := l.ObjectsByUID(ctx, cal, byCal[cal])
		if err != nil {
			return OccurrencePage{}, err
		}
		rendered[cal] = objs
	}
	own := l.ownAddresses()
	var items []AgendaOccurrence
	for _, cal := range order {
		for _, obj := range rendered[cal] {
			if obj.Missing {
				continue
			}
			got, last, err := expandObject(cal, types[cal], obj, q, own)
			if err != nil {
				page.Skipped++
				l.log().Warn("the agenda skipped an event it cannot read", "calendar", cal, "uid", obj.UID, "reason", refusalOf(err).Code)
				continue
			}
			if last != nil {
				cutoff = min(cutoff, *last)
			}
			for i := range got {
				d := &got[i]
				if q.after != nil && keyOf(d.AgendaOccurrence).compare(*q.after) <= 0 {
					continue
				}
				if keep(&d.AgendaOccurrence, &d.src) {
					items = append(items, d.AgendaOccurrence)
				}
			}
		}
	}
	items = slices.DeleteFunc(items, func(o AgendaOccurrence) bool { return o.Occurrence.Start.Unix() >= cutoff })
	slices.SortFunc(items, func(a, b AgendaOccurrence) int { return keyOf(a).compare(keyOf(b)) })
	switch {
	case len(items) > q.limit:
		page.Items, page.Truncated, page.Next = items[:q.limit], true, keyOf(items[q.limit-1]).cursor()
	case cutoff != math.MaxInt64:
		if cutoff <= q.from.Unix() {
			return OccurrencePage{}, usage(fmt.Sprintf("more than %d events may start before %s in these calendars", budget, q.from.UTC().Format(time.RFC3339)),
				"ask for fewer calendars")
		}
		page.Items, page.Truncated, page.Next = items, true, agendaKey{Start: cutoff}.cursor()
	default:
		page.Items = append(page.Items, items...)
	}
	return page, nil
}

// candidates lists the objects of the chosen calendars that may have an
// occurrence in the query's range, sorted by low (then calendar and UID),
// and each calendar's type. A recurring master is one whatever its start
// before the range's end; any other row when its times, widened by
// zoneSlack, overlap the range.
func (l *Local) candidates(ctx context.Context, calendars []string, q agendaQuery) ([]candidate, map[string]string, error) {
	var out []candidate
	types := map[string]string{}
	err := l.withCache(func(db *cache.Cache) error {
		cals, err := db.Calendars(ctx)
		if err != nil {
			return l.fail(cacheError(err))
		}
		if len(cals) == 0 {
			return l.notSynced()
		}
		var chosen []cache.Calendar
		if len(calendars) == 0 {
			for _, c := range cals {
				if !c.SyncedAt.IsZero() {
					chosen = append(chosen, c)
				}
			}
		}
		for _, id := range calendars {
			if _, dup := types[id]; dup {
				continue
			}
			c, err := l.syncedCalendar(ctx, db, id)
			if err != nil {
				return l.fail(err)
			}
			types[id] = c.Type
			chosen = append(chosen, c)
		}
		lo, hi := q.from.Unix()-zoneSlack, q.to.Unix()+zoneSlack
		for _, c := range chosen {
			types[c.ID] = c.Type
			rows, err := db.Events(ctx, c.ID)
			if err != nil {
				return l.fail(cacheError(err))
			}
			low, wanted := map[string]int64{}, map[string]bool{}
			for _, r := range rows {
				if r.UID == "" {
					continue
				}
				end := r.End
				if r.FullDay {
					end = allDayEnd(r)
				}
				master := r.RecurrenceID == nil && r.RRule != ""
				if r.Start < hi && (master || end > lo || r.Start >= lo) {
					wanted[r.UID] = true
				}
				if v, ok := low[r.UID]; !ok || r.Start-zoneSlack < v {
					low[r.UID] = r.Start - zoneSlack
				}
			}
			for uid := range wanted {
				v := low[uid]
				if q.after != nil {
					// Past page 1, nothing before the cursor's start is
					// returned: an earlier bound would only cut pages short.
					v = max(v, q.from.Unix())
				}
				out = append(out, candidate{cal: c.ID, uid: uid, low: v})
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(out, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.low, b.low), cmp.Compare(a.cal, b.cal), cmp.Compare(a.uid, b.uid))
	})
	return out, types, nil
}

// ownAddresses reports whether an address is the account's: one of its
// unlocked address keys, or the address it logged in with.
func (l *Local) ownAddresses() func(string) bool {
	own := map[string]bool{}
	l.r.kmu.RLock()
	if ks := l.r.ks; ks != nil {
		for email := range ks.own {
			own[email] = true
		}
	}
	l.r.kmu.RUnlock()
	if st, err := store.LoadState(l.o.Dirs.State); err == nil && st.Address != "" {
		own[strings.ToLower(st.Address)] = true
	}
	return func(email string) bool { return own[strings.ToLower(email)] }
}

// sourced is an occurrence with the component it comes from.
type sourced struct {
	AgendaOccurrence
	src EventData
}

// expandObject expands one rendered object over the query's range. last is
// set when the series' cap cut the expansion: the start of its last
// occurrence, which the page must not reach. An object that cannot be read
// is an error refusalOf names.
func expandObject(cal, calType string, obj Object, q agendaQuery, own func(string) bool) (out []sourced, last *int64, err error) {
	master, overrides, err := splitObject(obj.Data)
	if err != nil {
		return nil, nil, err
	}
	occ, truncated, err := ical.Expand(master, overrides, q.from, q.to, q.loc, MaxSeriesOccurrences)
	if err != nil {
		return nil, nil, err
	}
	data, err := ical.DecodeEvent(obj.Data)
	if err != nil {
		return nil, nil, err
	}
	if truncated && len(occ) > 0 {
		v := occ[len(occ)-1].Start.Unix()
		last = &v
	}
	form := data.Start
	if data.RecurrenceID != nil { // an occurrence without its series
		form = *data.RecurrenceID
	}
	for _, o := range occ {
		src := data
		var rid *EventTime
		if o.RecurrenceID != nil {
			if ov := overrideOf(data, *o.RecurrenceID, q.loc); o.Override && ov != nil {
				src, rid = *ov, ov.RecurrenceID
			} else {
				t := ridInForm(*o.RecurrenceID, form, q.loc)
				rid = &t
			}
		}
		organizer := src.Organizer
		if organizer == nil {
			organizer = data.Organizer
		}
		out = append(out, sourced{src: src, AgendaOccurrence: AgendaOccurrence{
			Ref:               EventRef{CalendarID: cal, UID: obj.UID, RecurrenceID: rid},
			ETag:              obj.ETag,
			Occurrence:        o,
			Summary:           src.Summary,
			Location:          src.Location,
			Description:       src.Description,
			CalendarType:      calType,
			ExternalOrganizer: organizer != nil && !own(organizer.Email),
			Busy:              src.Transp != "TRANSPARENT" && src.Status != "CANCELLED",
		}})
	}
	return out, last, nil
}

// splitObject is a rendered object's master (zero when it has none) and
// overrides.
func splitObject(data []byte) (master ical.Component, overrides []ical.Component, err error) {
	comps, err := ical.Parse(data)
	if err != nil {
		return master, nil, err
	}
	if len(comps) != 1 || !strings.EqualFold(comps[0].Name, "VCALENDAR") {
		return master, nil, errors.New("an event object is one VCALENDAR")
	}
	for _, ev := range comps[0].Subs("VEVENT") {
		if _, ok := ev.Get("RECURRENCE-ID"); ok {
			overrides = append(overrides, ev)
			continue
		}
		if master.Name != "" {
			return master, nil, errors.New("two VEVENTs without RECURRENCE-ID")
		}
		master = ev
	}
	return master, overrides, nil
}

// overrideOf is the component of data whose RECURRENCE-ID, read in loc,
// is the instant rid; nil if none.
func overrideOf(data EventData, rid time.Time, loc *time.Location) *EventData {
	if data.RecurrenceID != nil {
		return &data
	}
	for i := range data.Overrides {
		if t := data.Overrides[i].RecurrenceID; t != nil && sameInstant(*t, rid, loc) {
			return &data.Overrides[i]
		}
	}
	return nil
}

// sameInstant: t, read in loc, is the occurrence instant at.
func sameInstant(t EventTime, at time.Time, loc *time.Location) bool {
	w := at.In(loc)
	switch {
	case t.Date != "":
		return w.Format(time.DateOnly) == t.Date
	case t.Floating:
		return w.Format("20060102T150405") == t.At.Format("20060102T150405")
	}
	return t.At.Equal(at)
}

// ridInForm is the recurrence ID at (an occurrence's original start, read
// in loc) written in form's value type (rulings R10).
func ridInForm(at time.Time, form EventTime, loc *time.Location) EventTime {
	w := at.In(loc)
	switch {
	case form.Date != "":
		return EventTime{Date: w.Format(time.DateOnly)}
	case form.Floating:
		return EventTime{At: time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, time.UTC), Floating: true}
	case form.TZID != "":
		return EventTime{At: at.In(form.At.Location()), TZID: form.TZID}
	}
	return EventTime{At: at.UTC()}
}

// refusalOf names why an object cannot be read or changed: the codec's
// refusal under the pcb prefix, else a stored object that does not parse.
func refusalOf(err error) *WriteRefusal {
	var wr *WriteRefusal
	if errors.As(err, &wr) {
		return wr
	}
	var r *ical.Refusal
	if errors.As(err, &r) {
		code := string(r.Code)
		if !strings.HasPrefix(code, "pcb:") {
			code = "pcb:" + code
		}
		return refused(code, r.Reason)
	}
	return refused("pcb:"+string(ical.BadData), "the stored event cannot be read")
}

// unreadable is a MustAct: the event cannot be read (ruling R9).
func unreadable(err error) error {
	r := refusalOf(err)
	return writeFail(exitcode.MustAct, fmt.Errorf("the event cannot be read (%s): %s", r.Code, r.Reason),
		"open it in the Proton web app")
}

// editRefused is a MustAct for a change pcb refuses: the codec's refusal
// or a stored object that does not parse, as a *WriteRefusal.
func editRefused(err error) error {
	return writeFail(exitcode.MustAct, refusalOf(err), "pcb does not make this change; make it in the Proton web app")
}

// writeError maps PutObject's and DeleteObject's errors: a *WriteRefusal
// becomes a MustAct (the agent cannot fix it by asking again), the rest is
// kept.
func writeError(err error) error {
	var ee *exitcode.Error
	var wr *WriteRefusal
	if errors.As(err, &ee) && errors.As(err, &wr) {
		return &exitcode.Error{Code: exitcode.MustAct, Err: ee.Err, Hint: ee.Hint}
	}
	return err
}

// Event implements Agenda.
func (l *Local) Event(ctx context.Context, ref EventRef) (EventDetail, error) {
	if err := checkRef(ref); err != nil {
		return EventDetail{}, err
	}
	obj, err := l.agendaObject(ctx, ref)
	if err != nil {
		return EventDetail{}, err
	}
	master, overrides, err := splitObject(obj.Data)
	if err == nil {
		// Expand reads every component's times and rule whatever the
		// range: it refuses what the agenda cannot list, with a code.
		_, _, err = ical.Expand(master, overrides, time.Unix(0, 0), time.Unix(1, 0), time.UTC, 1)
	}
	var data EventData
	if err == nil {
		data, err = ical.DecodeEvent(obj.Data)
	}
	if err != nil {
		return EventDetail{}, unreadable(err)
	}
	d := EventDetail{Ref: ref, ETag: obj.ETag, ReadOnly: true, Data: data}
	err = l.withCache(func(db *cache.Cache) error {
		c, err := l.syncedCalendar(ctx, db, ref.CalendarID)
		d.CalendarType = c.Type
		return l.fail(err)
	})
	if err != nil {
		return EventDetail{}, err
	}
	if ck := l.r.calKeys(ref.CalendarID); ck != nil {
		writable, _ := l.r.advertised(ref.CalendarID, ck)
		d.ReadOnly = !writable
	}
	return d, nil
}

// CreateEvent implements Agenda: a new UID, written with If-None-Match: *.
func (l *Local) CreateEvent(ctx context.Context, calID string, f EventFields) (EventRef, string, error) {
	if calID == "" {
		return EventRef{}, "", usage("a calendar is required", "pass the calendar's ID")
	}
	uid := newEventUID()
	body, err := ical.NewEvent(uid, f, l.o.Now())
	if err != nil {
		return EventRef{}, "", editRefused(err)
	}
	if _, err := l.PutObject(ctx, calID, ObjectName(uid), body, Precondition{IfNoneMatchAny: true}); err != nil {
		return EventRef{}, "", writeError(err)
	}
	ref := EventRef{CalendarID: calID, UID: uid}
	return ref, l.etagOf(ctx, ref), nil
}

// UpdateEvent implements Agenda.
func (l *Local) UpdateEvent(ctx context.Context, ref EventRef, etag string, scope EditScope, f EventFields, dryRun bool) (UpdateResult, error) {
	obj, err := l.editTarget(ctx, ref, etag, scope)
	if err != nil {
		return UpdateResult{}, err
	}
	body, err := ical.ApplyEdit(obj.Data, scope, ref.RecurrenceID, f, l.o.Now())
	if err != nil {
		return UpdateResult{}, editRefused(err)
	}
	if dryRun {
		before, err := ical.DecodeEvent(obj.Data)
		if err != nil {
			return UpdateResult{}, unreadable(err)
		}
		after, err := ical.DecodeEvent(body)
		if err != nil {
			return UpdateResult{}, unreadable(err)
		}
		return UpdateResult{ETag: etag, Before: before, After: after}, nil
	}
	if _, err := l.PutObject(ctx, ref.CalendarID, obj.Name, body, Precondition{IfMatch: etag}); err != nil {
		return UpdateResult{}, writeError(err)
	}
	return UpdateResult{ETag: l.etagOf(ctx, ref)}, nil
}

// DeleteEvent implements Agenda: the whole object (series), or an EXDATE
// written with PutObject (occurrence).
func (l *Local) DeleteEvent(ctx context.Context, ref EventRef, etag string, scope EditScope) error {
	obj, err := l.editTarget(ctx, ref, etag, scope)
	if err != nil {
		return err
	}
	if scope == ScopeSeries {
		return writeError(l.DeleteObject(ctx, ref.CalendarID, obj.Name, Precondition{IfMatch: etag}))
	}
	body, err := ical.DeleteOccurrence(obj.Data, *ref.RecurrenceID, l.o.Now())
	if err != nil {
		return editRefused(err)
	}
	if _, err := l.PutObject(ctx, ref.CalendarID, obj.Name, body, Precondition{IfMatch: etag}); err != nil {
		return writeError(err)
	}
	return nil
}

func checkRef(ref EventRef) error {
	if ref.CalendarID == "" || ref.UID == "" {
		return usage("an event needs its calendar and UID", "use an event's ID from a list")
	}
	return nil
}

// editTarget checks an edit's arguments and returns the object it changes,
// whose ETag must still be etag (as the cache knows it; the write checks
// Proton's).
func (l *Local) editTarget(ctx context.Context, ref EventRef, etag string, scope EditScope) (Object, error) {
	if err := checkRef(ref); err != nil {
		return Object{}, err
	}
	switch {
	case etag == "":
		return Object{}, usage("a change needs the ETag the event was read with", "read the event, then retry with its ETag")
	case scope != ScopeSeries && scope != ScopeOccurrence:
		return Object{}, usage(fmt.Sprintf("the scope must be %s or %s", ScopeSeries, ScopeOccurrence), "say which the change is for")
	case scope == ScopeOccurrence && ref.RecurrenceID == nil:
		return Object{}, usage("an occurrence change needs the occurrence's ID", "use the occurrence's ID from a list, or the series scope")
	}
	obj, err := l.agendaObject(ctx, ref)
	if err != nil {
		return Object{}, err
	}
	if obj.ETag != etag {
		return Object{}, preconditionFailed()
	}
	return obj, nil
}

// agendaObject is the rendered object ref names; ErrNoSuchObject when it
// has none.
func (l *Local) agendaObject(ctx context.Context, ref EventRef) (Object, error) {
	objs, err := l.ObjectsByUID(ctx, ref.CalendarID, []string{ref.UID})
	if err != nil {
		return Object{}, err
	}
	if len(objs) != 1 || objs[0].Missing {
		return Object{}, noSuchObject()
	}
	return objs[0], nil
}

// etagOf is the ETag of the object ref names after a write, "" if it
// cannot be read: the write is done either way.
func (l *Local) etagOf(ctx context.Context, ref EventRef) string {
	obj, err := l.agendaObject(ctx, ref)
	if err != nil {
		return ""
	}
	return obj.ETag
}

// newEventUID is a random UUID (version 4), as Thunderbird names events.
func newEventUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
