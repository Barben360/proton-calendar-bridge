package protontest

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Event describes an event for AddEvent and UpdateEvent. Times are unix
// seconds; the fake serves them as UTC.
type Event struct {
	UID          string
	Start, End   int64
	FullDay      bool
	RRule        string
	RecurrenceID *int64
	// Card is the VEVENT body (content lines, CRLF or LF separated; folded
	// lines and nested components stay with their property), split into
	// Proton's parts per the merge table (see splitCard). Empty: a minimal
	// card from UID, Start, End, FullDay, RRule and RecurrenceID. A card
	// without UID gets UID's.
	Card string
	// Author signs the parts (always with the address key); empty: the
	// email of the calendar's address.
	Author string
	Color  string // "" serves null
	// Notifications: nil serves null (the calendar's defaults), an empty
	// slice [] (none).
	Notifications []Notification
	// AddressKeyPacket wraps the shared session key to the address key
	// (AddressKeyPacket, AddressID) instead of the calendar key
	// (SharedKeyPacket), as on a Proton-to-Proton invitation.
	AddressKeyPacket bool
	BrokenPart       Part // that part is encrypted under an unrelated session key
}

// Feed actions, as the change feed names them.
const (
	actionDelete = 0
	actionCreate = 1
	actionUpdate = 2
)

// maxListPageSize is the largest PageSize the listing accepts.
const maxListPageSize = 100

const defaultFeedPageSize = 50

// eventRec is one stored row.
type eventRec struct {
	id, sharedID                 string
	ev                           Event
	createTime, lastEdit, modify int64
	// parts are the crypto fields, built on the row's first read after each
	// edit and kept, so a row reads the same until edited.
	parts map[string]any
}

// feedEntry is one change in a calendar's feed; seq is its position.
type feedEntry struct {
	seq    int
	id     string
	action int
}

// eventState is a calendar's rows and feed, guarded by calendars.mu.
type eventState struct {
	rows []*eventRec // creation order, deleted rows removed
	feed []feedEntry
	seq  int    // position of the newest change (0: none yet)
	salt string // from the full calendar ID: a cursor of another calendar is unknown here
	// Cursors at or below expired answer 422/2001.
	expired int
	refresh bool // the next feed page answers Refresh 1
}

func (c *calendarRec) events() *eventState {
	if c.ev == nil {
		c.ev = &eventState{salt: "mev" + hex.EncodeToString([]byte(c.id)), expired: -1}
	}
	return c.ev
}

func (s *eventState) cursor(seq int) string { return s.salt + "-" + strconv.Itoa(seq) }

// parseCursor returns the position a cursor names and whether the cursor is
// known and not expired.
func (s *eventState) parseCursor(cur string) (int, bool) {
	rest, ok := strings.CutPrefix(cur, s.salt+"-")
	if !ok {
		return 0, false
	}
	seq, err := strconv.Atoi(rest)
	if err != nil || seq < 0 || seq > s.seq || seq <= s.expired {
		return 0, false
	}
	return seq, true
}

func (s *eventState) record(id string, action int) {
	s.seq++
	s.feed = append(s.feed, feedEntry{seq: s.seq, id: id, action: action})
}

func (s *eventState) find(id string) int {
	return slices.IndexFunc(s.rows, func(r *eventRec) bool { return r.id == id })
}

// calendarForEvents returns the record of calID, failing the test when unknown.
// Caller holds f.cal.mu.
func (f *Fake) calendarForEvents(calID string) *calendarRec {
	rec := f.cal.byID[calID]
	if rec == nil {
		f.t.Helper()
		f.t.Fatalf("protontest: unknown calendar %q", calID)
	}
	return rec
}

// tick returns the next fake timestamp: strictly increasing, so an update
// always moves LastEditTime. Caller holds f.cal.mu.
func (f *Fake) tick() int64 {
	f.cal.clock++
	return 1700000000 + f.cal.clock
}

// AddEvent adds a row to the calendar, appends it to the calendar's change
// feed and returns the row ID.
func (f *Fake) AddEvent(calID string, e Event) (id string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	s := f.calendarForEvents(calID).events()
	now := f.tick()
	id = "ev" + hex.EncodeToString(random(f.t, 8)) + "=="
	s.rows = append(s.rows, &eventRec{
		id: id, sharedID: "sev" + hex.EncodeToString(random(f.t, 8)) + "==",
		ev: e, createTime: now, lastEdit: now, modify: now,
	})
	s.record(id, actionCreate)
	return id
}

// AddEventAt is AddEvent placing the row at position pos of the listing
// (0: first) instead of last, as if Proton's listing order put it there: a
// row inserted before a page already served shifts the later pages.
func (f *Fake) AddEventAt(calID string, pos int, e Event) (id string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	s := f.calendarForEvents(calID).events()
	if pos < 0 || pos > len(s.rows) {
		f.t.Fatalf("protontest: position %d outside the %d rows of calendar %q", pos, len(s.rows), calID)
	}
	now := f.tick()
	id = "ev" + hex.EncodeToString(random(f.t, 8)) + "=="
	s.rows = slices.Insert(s.rows, pos, &eventRec{
		id: id, sharedID: "sev" + hex.EncodeToString(random(f.t, 8)) + "==",
		ev: e, createTime: now, lastEdit: now, modify: now,
	})
	s.record(id, actionCreate)
	return id
}

// UpdateEvent replaces the row's fields, bumps its LastEditTime and appends
// it to the feed.
func (f *Fake) UpdateEvent(calID, id string, e Event) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	s := f.calendarForEvents(calID).events()
	i := s.find(id)
	if i < 0 {
		f.t.Fatalf("protontest: no event %q in calendar %q", id, calID)
	}
	now := f.tick()
	s.rows[i].ev, s.rows[i].lastEdit, s.rows[i].modify, s.rows[i].parts = e, now, now, nil
	s.record(id, actionUpdate)
}

// DeleteEvent removes the row and appends the deletion to the feed.
func (f *Fake) DeleteEvent(calID, id string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	s := f.calendarForEvents(calID).events()
	i := s.find(id)
	if i < 0 {
		f.t.Fatalf("protontest: no event %q in calendar %q", id, calID)
	}
	s.rows = slices.Delete(s.rows, i, i+1)
	s.record(id, actionDelete)
}

// ForceRefresh makes the next feed page of the calendar answer Refresh 1.
func (f *Fake) ForceRefresh(calID string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	f.calendarForEvents(calID).events().refresh = true
}

// ExpireFeed makes every cursor issued so far answer 422 with code 2001, as
// if Proton no longer remembered it. The head ("latest") moves on, so a
// cursor read afterwards is valid.
func (f *Fake) ExpireFeed(calID string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	s := f.calendarForEvents(calID).events()
	s.expired = s.seq
	s.seq++ // a gap: no entry, but the old head is not the new head
}

// SetFeedPageSize sets how many entries a feed page holds (default 50).
func (f *Fake) SetFeedPageSize(n int) {
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	f.cal.feedPage = n
}

// OnListPage makes fn run each time a listing page of a calendar's events
// (page 0-based) has been read from the calendar and before it is sent: a
// row fn adds is not on that page, and the client sees the page only after
// fn returns. fn may call the fake (AddEvent…); nil removes the hook.
// fn runs on the server's handler goroutine, not the test's: it must not
// call t.Fatal or t.FailNow, and what it writes is visible to the test after
// the listing answer arrives and OnListPage (which takes the fake's lock) is
// called again.
func (f *Fake) OnListPage(fn func(calID string, page int)) {
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	f.cal.onList = fn
}

func (f *Fake) feedPageSize() int {
	if f.cal.feedPage > 0 {
		return f.cal.feedPage
	}
	return defaultFeedPageSize
}

// serveEvents answers /calendar/v1/{cal}/events..., /modelevents... for the
// calendars of u. segs are the unescaped path segments after /calendar/v1/.
// Caller holds f.cal.mu.
func (f *Fake) serveEvents(w http.ResponseWriter, r *http.Request, u *userCalendars, segs []string) {
	i := slices.IndexFunc(u.list, func(c *calendarRec) bool { return c.id == segs[0] })
	if i < 0 {
		reply(w, http.StatusUnprocessableEntity, 2501, "Calendar does not exist")
		return
	}
	cal := u.list[i]
	s := cal.events()
	switch {
	case len(segs) == 2 && segs[1] == "events":
		f.listEvents(w, r.URL.Query(), cal, s)
	case len(segs) == 3 && segs[1] == "events":
		j := s.find(segs[2])
		if j < 0 {
			reply(w, http.StatusUnprocessableEntity, 2501, "Event does not exist")
			return
		}
		row, ok := f.rowJSON(cal, s.rows[j])
		if !ok {
			reply(w, http.StatusInternalServerError, 0, "fake: cannot build the event parts")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"Code": 1000, "Event": row})
	case len(segs) == 3 && segs[1] == "modelevents" && segs[2] == "latest":
		writeJSON(w, http.StatusOK, map[string]any{"Code": 1000, "CalendarModelEventID": s.cursor(s.seq)})
	case len(segs) == 3 && segs[1] == "modelevents":
		f.feedPage(w, s, segs[2])
	default:
		// Not 404: a client treats that as "row gone", which would hide a path bug.
		reply(w, http.StatusBadRequest, 2000, "fake: route not implemented")
	}
}

func (f *Fake) listEvents(w http.ResponseWriter, q url.Values, cal *calendarRec, s *eventState) {
	page, size := 0, maxListPageSize
	var err error
	if v := q.Get("Page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 0 {
			reply(w, http.StatusBadRequest, 2001, "Invalid Page")
			return
		}
	}
	if v := q.Get("PageSize"); v != "" {
		if size, err = strconv.Atoi(v); err != nil || size < 1 {
			reply(w, http.StatusBadRequest, 2001, "Invalid PageSize")
			return
		}
	}
	if size > maxListPageSize {
		reply(w, http.StatusBadRequest, 2021, "PageSize is too large")
		return
	}
	lo := min(page*size, len(s.rows))
	hi := min(lo+size, len(s.rows))
	rows := make([]any, 0, hi-lo)
	for _, r := range s.rows[lo:hi] {
		row, ok := f.rowJSON(cal, r)
		if !ok {
			reply(w, http.StatusInternalServerError, 0, "fake: cannot build the event parts")
			return
		}
		rows = append(rows, row)
	}
	total := len(s.rows)
	if fn := f.cal.onList; fn != nil {
		// The caller holds f.cal.mu; fn may need it (AddEvent).
		f.cal.mu.Unlock()
		fn(cal.id, page)
		f.cal.mu.Lock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"Code": 1000, "Events": rows, "Total": total})
}

func (f *Fake) feedPage(w http.ResponseWriter, s *eventState, cursor string) {
	seq, ok := s.parseCursor(cursor)
	if !ok {
		reply(w, http.StatusUnprocessableEntity, 2001, "Invalid or expired event ID")
		return
	}
	var entries []any
	last, more := seq, 0
	pageSize := f.feedPageSize()
	for _, e := range s.feed {
		if e.seq <= seq {
			continue
		}
		if len(entries) == pageSize {
			more = 1
			break
		}
		entries = append(entries, map[string]any{"ID": e.id, "Action": e.action})
		last = e.seq
	}
	refresh := 0
	if s.refresh {
		refresh, s.refresh = 1, false
	}
	if entries == nil {
		entries = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Code": 1000, "CalendarModelEventID": s.cursor(last), "More": more, "Refresh": refresh, "CalendarEvents": entries,
	})
}

// rowJSON is the row as the API serves it: the plaintext columns of the
// research's field set and real parts (see buildParts). It reports false,
// after failing the test, when the parts cannot be built. Caller holds
// f.cal.mu.
func (f *Fake) rowJSON(cal *calendarRec, r *eventRec) (map[string]any, bool) {
	if r.parts == nil {
		parts, err := f.buildParts(cal, r)
		if err != nil {
			f.t.Errorf("protontest: event parts: %v", err) // a handler goroutine: no Fatal
			return nil, false
		}
		r.parts = parts
	}
	var recurrence, rrule, color any
	if r.ev.RecurrenceID != nil {
		recurrence = *r.ev.RecurrenceID
	}
	if r.ev.RRule != "" {
		rrule = r.ev.RRule
	}
	if r.ev.Color != "" {
		color = r.ev.Color
	}
	fullDay := 0
	if r.ev.FullDay {
		fullDay = 1
	}
	row := map[string]any{
		"ID": r.id, "CalendarID": cal.id, "SharedEventID": r.sharedID,
		"CreateTime": r.createTime, "ModifyTime": r.modify, "LastEditTime": r.lastEdit,
		"StartTime": r.ev.Start, "EndTime": r.ev.End, "StartTimezone": "UTC", "EndTimezone": "UTC",
		"FullDay": fullDay, "UID": r.ev.UID, "IsOrganizer": 1,
		"RecurrenceID": recurrence, "Exdates": []int64{}, "RRule": rrule,
		"Permissions": 127, "Color": color, "Notifications": r.ev.Notifications,
	}
	for k, v := range r.parts {
		row[k] = v
	}
	return row, true
}
