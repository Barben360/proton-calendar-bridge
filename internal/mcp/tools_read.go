package mcp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The read tools (design §5.7): status, calendars_list and free_busy
// (freebusy), events_list, event_get and events_search (read). Every call
// reads the visible accounts again: an account removed, signed out or
// switched mcp off since the last call answers as one that never existed.

func readTools() []toolDef {
	return []toolDef{
		{name: "status", cap: CapFreeBusy, add: addStatus},
		{name: "calendars_list", cap: CapFreeBusy, add: addCalendarsList},
		{name: "free_busy", cap: CapFreeBusy, add: addFreeBusy},
		{name: "events_list", cap: CapRead, add: addEventsList},
		{name: "event_get", cap: CapRead, add: addEventGet},
		{name: "events_search", cap: CapRead, add: addEventsSearch},
	}
}

// The account phases pcb reports (service.AccountInfo.Phase), and the one
// status adds for an account whose serve loop stopped.
const (
	phaseLoggedOut    = "logged-out"
	phaseNeedsLogin   = "needs-login"
	phaseNeedsCaptcha = "needs-captcha"
	phaseStopped      = "stopped"
)

// Text cuts (Global Constraints).
const (
	listDescriptionMax  = 4 << 10
	eventDescriptionMax = 64 << 10
)

// cutUTF8 is s cut to at most n bytes, on a rune boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutText is s cut to n bytes and whether it was cut.
func cutText(s string, n int) (string, bool) {
	c := cutUTF8(s, n)
	return c, len(c) < len(s)
}

// ---- status ----

type statusIn struct{}

type nowView struct {
	At     string `json:"at" jsonschema:"the current time, RFC 3339 in zone"`
	Zone   string `json:"zone" jsonschema:"IANA zone"`
	Offset string `json:"offset" jsonschema:"UTC offset, +hh:mm"`
}

type alertView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Step    string `json:"step" jsonschema:"what a person does about it"`
}

type accountStatus struct {
	Account        string      `json:"account" jsonschema:"the account's address"`
	ID             string      `json:"id" jsonschema:"the account's local ID"`
	Phase          string      `json:"phase" jsonschema:"ready, needs-login, needs-captcha or stopped"`
	LastSyncAgeS   *int        `json:"last_sync_age_s,omitempty" jsonschema:"seconds since the last successful sync"`
	StaleCalendars []string    `json:"stale_calendars,omitempty" jsonschema:"IDs of calendars whose sync fails or has not happened"`
	Alerts         []alertView `json:"alerts,omitempty"`
}

type statusOut struct {
	Now      nowView         `json:"now"`
	Zone     string          `json:"zone" jsonschema:"the zone times without an offset are read in when a call gives no tz"`
	Accounts []accountStatus `json:"accounts"`
}

func addStatus(ss *session) {
	addTool(ss, &sdkmcp.Tool{
		Name:        "status",
		Description: "The current time and zone (agents have no reliable clock), and each account this session can see: its phase, last sync, stale calendars and alerts with the step a person takes.",
		Annotations: readOnly("Status"),
	}, func(ctx context.Context, _ statusIn) (statusOut, string, error) {
		now := ss.srv.o.Now().In(ss.srv.o.Zone)
		out := statusOut{
			Now:      nowView{At: now.Format(time.RFC3339), Zone: ss.srv.o.Zone.String(), Offset: now.Format("-07:00")},
			Zone:     ss.srv.o.Zone.String(),
			Accounts: []accountStatus{},
		}
		rec := auditOf(ctx)
		for _, a := range ss.visible() {
			if !ss.holdsGrantedCalendar(ctx, a.ID) {
				continue
			}
			rec.account(a.ID)
			out.Accounts = append(out.Accounts, ss.accountStatus(ctx, a, now))
		}
		return out, fmt.Sprintf("%d accounts; now %s (%s)", len(out.Accounts), out.Now.At, out.Zone), nil
	})
}

// loginStep is what a person does for an account that needs a login.
func (ss *session) loginStep() string {
	switch {
	case ss.entry == EntryRemote:
		return "run `pcb login` on the server, or open pcb-ui"
	case ss.grant.has(CapLogin):
		return "call the login tool with the account's address, or run `pcb login` in a terminal"
	}
	return "run `pcb login` in a terminal, or open pcb-ui"
}

func (ss *session) accountStatus(ctx context.Context, a service.AccountInfo, now time.Time) accountStatus {
	st := accountStatus{Account: a.Address, ID: a.ID, Phase: a.Phase}
	if s := a.Stopped; s != nil {
		st.Phase = phaseStopped
		st.Alerts = append(st.Alerts, alertView{Code: "account-stopped", Message: s.Message, Step: s.Hint})
	}
	switch a.Phase {
	case phaseNeedsLogin:
		st.Alerts = append(st.Alerts, alertView{Code: "needs-login", Message: "not logged in to Proton", Step: ss.loginStep()})
	case phaseNeedsCaptcha:
		st.Alerts = append(st.Alerts, alertView{Code: "needs-captcha", Message: "Proton asks for a CAPTCHA before the login completes", Step: ss.loginStep()})
	}
	h, err := ss.srv.o.Hub.Health(ctx, a.ID)
	if err != nil {
		return st
	}
	var last *time.Time
	if p := h.LastPass; p != nil && p.OK {
		last = &p.At
	}
	for _, c := range h.Calendars {
		if !ss.grant.seesCalendar(c.ID) {
			continue
		}
		if last == nil && c.SyncedAt != nil {
			last = c.SyncedAt
		}
		if c.SyncedAt == nil || c.ErrorClass != "" || c.KeysError != "" {
			st.StaleCalendars = append(st.StaleCalendars, c.ID)
		}
		if c.KeysError != "" {
			st.Alerts = append(st.Alerts, alertView{Code: "calendar-keys-failed", Message: "the keys of calendar " + c.ID + " did not unlock: its events are placeholders",
				Step: "run pcb login again; if it stays, check the calendar in the Proton web app"})
		}
	}
	if last != nil {
		age := max(0, int(now.Sub(*last).Seconds()))
		st.LastSyncAgeS = &age
	}
	if len(h.HumanVerification) > 0 {
		st.Alerts = append(st.Alerts, alertView{Code: "human-verification", Message: "Proton asks for a human verification: writes wait for it",
			Step: "open the Proton web app from this machine's network and complete the verification"})
	}
	return st
}

// ---- calendars_list ----

type accountsIn struct {
	Accounts []string `json:"accounts,omitempty" jsonschema:"accounts by address or local ID; default every account this session can see"`
}

type calendarText struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type calendarView struct {
	ID        string       `json:"id"`
	Account   string       `json:"account"`
	Color     string       `json:"color,omitempty"`
	Type      string       `json:"type" jsonschema:"normal, subscribed or holidays"`
	ReadOnly  bool         `json:"read_only" jsonschema:"pcb does not write to it (its write gate)"`
	Default   bool         `json:"default"`
	Untrusted calendarText `json:"untrusted"`
}

type calendarsOut struct {
	Calendars   []calendarView `json:"calendars"`
	Unavailable []accountError `json:"unavailable,omitempty"`
}

func addCalendarsList(ss *session) {
	addTool(ss, &sdkmcp.Tool{
		Name:        "calendars_list",
		Description: "The calendars of the accounts this session can see: ID, account, colour, type, whether pcb writes to it, and the default one.",
		Annotations: readOnly("List calendars"),
	}, func(ctx context.Context, in accountsIn) (calendarsOut, string, error) {
		ts, err := ss.accounts(ctx, in.Accounts)
		if err != nil {
			return calendarsOut{}, "", err
		}
		out := calendarsOut{Calendars: []calendarView{}}
		var f failures
		rec := auditOf(ctx)
		for _, t := range ts {
			if !ss.holdsGrantedCalendar(ctx, t.info.ID) {
				continue
			}
			rec.account(t.info.ID)
			api, info, err := ss.srv.o.Hub.Account(t.info.ID)
			if errors.Is(err, service.ErrNoAccount) || (err == nil && !ss.grant.seesAccount(info)) {
				err = noAccount(t.name)
			}
			var cals []service.Calendar
			if err == nil {
				cals, err = api.Calendars(ctx)
			}
			if err != nil {
				f.add(t, err)
				continue
			}
			f.answered++
			for _, c := range cals {
				if !ss.grant.seesCalendar(c.ID) {
					continue
				}
				out.Calendars = append(out.Calendars, calendarView{ID: c.ID, Account: t.info.Address, Color: c.Color, Type: c.Type,
					ReadOnly: c.ReadOnly, Default: c.Default, Untrusted: calendarText{Name: c.Name, Description: c.Description}})
			}
		}
		if err := f.err(); err != nil {
			return calendarsOut{}, "", err
		}
		out.Unavailable = f.list
		return out, fmt.Sprintf("%d calendars", len(out.Calendars)), nil
	})
}

// ---- free_busy ----

type rangeIn struct {
	From      string   `json:"from" jsonschema:"start: RFC 3339 with an offset, or a wall clock (2026-11-14T15:00) or a date read in tz"`
	To        string   `json:"to" jsonschema:"end (exclusive), as from; at most 366 days after it"`
	TZ        string   `json:"tz,omitempty" jsonschema:"IANA zone for times without an offset and for all-day and floating events; default the status zone"`
	Accounts  []string `json:"accounts,omitempty" jsonschema:"accounts by address or local ID; default every account this session can see"`
	Calendars []string `json:"calendars,omitempty" jsonschema:"calendar IDs; default every calendar"`
}

type interval struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type accountBusy struct {
	Account string     `json:"account"`
	Busy    []interval `json:"busy" jsonschema:"merged busy intervals, RFC 3339 in zone"`
	Skipped int        `json:"skipped" jsonschema:"events that cannot be read: their times are unknown"`
	Note    string     `json:"note,omitempty"`
}

type freeBusyOut struct {
	Zone        string         `json:"zone"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	Accounts    []accountBusy  `json:"accounts"`
	Unavailable []accountError `json:"unavailable,omitempty"`
}

func addFreeBusy(ss *session) {
	addTool(ss, &sdkmcp.Tool{
		Name:        "free_busy",
		Description: "Busy intervals per account over a range, merged; transparent and cancelled events are free. No titles nor attendees.",
		Annotations: readOnly("Free/busy"),
	}, func(ctx context.Context, in rangeIn) (freeBusyOut, string, error) {
		sp, err := ss.span(in.From, in.To, in.TZ)
		if err != nil {
			return freeBusyOut{}, "", err
		}
		ts, err := ss.accounts(ctx, in.Accounts)
		if err == nil {
			ts, err = ss.withCalendars(ctx, ts, in.Calendars)
		}
		if err != nil {
			return freeBusyOut{}, "", err
		}
		out := freeBusyOut{Zone: sp.zone, Accounts: []accountBusy{}}
		out.From, out.To = sp.echo()
		var f failures
		rec := auditOf(ctx)
		for _, t := range ts {
			rec.account(t.info.ID)
			for _, c := range t.cals {
				rec.calendar(c)
			}
			ag, err := ss.agenda(t)
			var occ []service.Occurrence
			var skipped int
			if err == nil {
				occ, skipped, err = ag.FreeBusy(ctx, service.OccurrencesRequest{Calendars: t.cals, From: sp.from, To: sp.to, Loc: sp.loc})
			}
			if err != nil {
				f.add(t, err)
				continue
			}
			f.answered++
			out.Accounts = append(out.Accounts, accountBusy{Account: t.info.Address, Busy: mergeBusy(occ, sp.loc), Skipped: skipped, Note: skippedNote(skipped)})
		}
		if err := f.err(); err != nil {
			return freeBusyOut{}, "", err
		}
		out.Unavailable = f.list
		return out, fmt.Sprintf("busy intervals of %d accounts from %s to %s (%s)", len(out.Accounts), out.From, out.To, out.Zone), nil
	})
}

// mergeBusy merges overlapping or touching occurrences into intervals.
func mergeBusy(occ []service.Occurrence, loc *time.Location) []interval {
	slices.SortFunc(occ, func(a, b service.Occurrence) int { return a.Start.Compare(b.Start) })
	type span struct{ from, to time.Time }
	var spans []span
	for _, o := range occ {
		if n := len(spans); n > 0 && !o.Start.After(spans[n-1].to) {
			if o.End.After(spans[n-1].to) {
				spans[n-1].to = o.End
			}
			continue
		}
		spans = append(spans, span{o.Start, o.End})
	}
	out := make([]interval, 0, len(spans))
	for _, s := range spans {
		out = append(out, interval{Start: s.from.In(loc).Format(time.RFC3339), End: s.to.In(loc).Format(time.RFC3339)})
	}
	return out
}

// ---- events_list and events_search ----

type listIn struct {
	From      string   `json:"from" jsonschema:"start: RFC 3339 with an offset, or a wall clock (2026-11-14T15:00) or a date read in tz"`
	To        string   `json:"to" jsonschema:"end (exclusive), as from; at most 366 days after it"`
	TZ        string   `json:"tz,omitempty" jsonschema:"IANA zone for times without an offset and for all-day and floating events; default the status zone"`
	Accounts  []string `json:"accounts,omitempty" jsonschema:"accounts by address or local ID; default every account this session can see"`
	Calendars []string `json:"calendars,omitempty" jsonschema:"calendar IDs; default every calendar"`
	Limit     int      `json:"limit,omitempty" jsonschema:"occurrences per page, 1 to 500; default 100"`
	Cursor    string   `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page, with the same other arguments"`
}

type searchIn struct {
	Text      string   `json:"text" jsonschema:"matched case-insensitively in summary, location and description"`
	From      string   `json:"from" jsonschema:"start: RFC 3339 with an offset, or a wall clock (2026-11-14T15:00) or a date read in tz"`
	To        string   `json:"to" jsonschema:"end (exclusive), as from; at most 366 days after it"`
	TZ        string   `json:"tz,omitempty" jsonschema:"IANA zone for times without an offset and for all-day and floating events; default the status zone"`
	Accounts  []string `json:"accounts,omitempty" jsonschema:"accounts by address or local ID; default every account this session can see"`
	Calendars []string `json:"calendars,omitempty" jsonschema:"calendar IDs; default every calendar"`
	Limit     int      `json:"limit,omitempty" jsonschema:"occurrences per page, 1 to 500; default 100"`
	Cursor    string   `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page, with the same other arguments"`
}

type occurrenceText struct {
	Summary              string `json:"summary,omitempty"`
	Location             string `json:"location,omitempty"`
	Description          string `json:"description,omitempty" jsonschema:"cut at 4 KiB; event_get gives up to 64 KiB"`
	DescriptionTruncated bool   `json:"description_truncated,omitempty"`
}

type occurrenceView struct {
	EventID           string         `json:"event_id" jsonschema:"opaque: pass it to event_get"`
	Account           string         `json:"account"`
	CalendarID        string         `json:"calendar_id"`
	CalendarType      string         `json:"calendar_type"`
	Start             timeView       `json:"start"`
	End               timeView       `json:"end"`
	AllDay            bool           `json:"all_day"`
	Recurring         bool           `json:"recurring" jsonschema:"one occurrence of a series"`
	Busy              bool           `json:"busy"`
	ETag              string         `json:"etag"`
	ExternalOrganizer bool           `json:"external_organizer" jsonschema:"organized by an address that is not the account's"`
	ThirdParty        bool           `json:"third_party" jsonschema:"external organizer, or a subscribed or holidays calendar"`
	Untrusted         occurrenceText `json:"untrusted"`
}

type eventsOut struct {
	Zone        string           `json:"zone"`
	From        string           `json:"from"`
	To          string           `json:"to"`
	Events      []occurrenceView `json:"events" jsonschema:"sorted by start"`
	Truncated   bool             `json:"truncated" jsonschema:"more follow: call again with next_cursor"`
	NextCursor  string           `json:"next_cursor,omitempty"`
	Skipped     int              `json:"skipped" jsonschema:"events that cannot be read and were left out"`
	Note        string           `json:"note,omitempty"`
	Unavailable []accountError   `json:"unavailable,omitempty"`
}

const listDescription = "Occurrences overlapping a range, across the accounts this session can see, sorted by start, with their event_id, times, etag and flags; summary, location and description (cut at 4 KiB) under untrusted. Pages: limit and cursor."

func addEventsList(ss *session) {
	addTool(ss, &sdkmcp.Tool{Name: "events_list", Description: listDescription, Annotations: readOnly("List events")},
		func(ctx context.Context, in listIn) (eventsOut, string, error) {
			return ss.listEvents(ctx, listQuery{listIn: in})
		})
}

func addEventsSearch(ss *session) {
	addTool(ss, &sdkmcp.Tool{
		Name:        "events_search",
		Description: "As events_list, keeping the occurrences whose summary, location or description contains text, case-insensitively.",
		Annotations: readOnly("Search events"),
	}, func(ctx context.Context, in searchIn) (eventsOut, string, error) {
		if strings.TrimSpace(in.Text) == "" {
			return eventsOut{}, "", refusedErr("text is empty", "give the text to search for, or use events_list")
		}
		return ss.listEvents(ctx, listQuery{text: in.Text, listIn: listIn{From: in.From, To: in.To, TZ: in.TZ,
			Accounts: in.Accounts, Calendars: in.Calendars, Limit: in.Limit, Cursor: in.Cursor}})
	})
}

type listQuery struct {
	listIn
	text string
}

// listCursor is where each account's listing stands: its agenda's cursor
// ("" for its start) by local ID; an account absent is done.
type listCursor struct {
	V int               `json:"v"`
	A map[string]string `json:"a"`
}

func (c listCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeListCursor(s string) (listCursor, error) {
	var c listCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.V != 1 || c.A == nil {
		return listCursor{}, refusedErr("cursor is not one this tool returned", "start again without a cursor")
	}
	return c, nil
}

// fetched is one account's page.
type fetched struct {
	t    target
	page service.OccurrencePage
	used int // items returned from it
}

// listEvents lists one page over several accounts. Each account's agenda
// gives a page from its own cursor; the pages are merged by start, never
// past the last item of a page that has more (an earlier occurrence of
// that account may follow), and each account's cursor moves past the items
// returned: no gap and no repeat, whatever accounts the pages cut.
func (ss *session) listEvents(ctx context.Context, q listQuery) (eventsOut, string, error) {
	sp, err := ss.span(q.From, q.To, q.TZ)
	if err != nil {
		return eventsOut{}, "", err
	}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = service.DefaultAgendaLimit
	case limit < 0 || limit > service.MaxAgendaLimit:
		return eventsOut{}, "", refusedErr(fmt.Sprintf("limit must be between 1 and %d", service.MaxAgendaLimit), "ask for fewer occurrences per page")
	}
	ts, err := ss.accounts(ctx, q.Accounts)
	if err == nil {
		ts, err = ss.withCalendars(ctx, ts, q.Calendars)
	}
	if err != nil {
		return eventsOut{}, "", err
	}
	state := map[string]string{}
	if q.Cursor == "" {
		for _, t := range ts {
			state[t.info.ID] = ""
		}
	} else {
		c, err := decodeListCursor(q.Cursor)
		if err != nil {
			return eventsOut{}, "", err
		}
		for _, t := range ts {
			if cur, ok := c.A[t.info.ID]; ok {
				state[t.info.ID] = cur
			}
		}
	}

	out := eventsOut{Zone: sp.zone, Events: []occurrenceView{}}
	out.From, out.To = sp.echo()
	var (
		f     failures
		pages []*fetched
	)
	rec := auditOf(ctx)
	for _, t := range ts {
		cur, ok := state[t.info.ID]
		if !ok {
			continue
		}
		rec.account(t.info.ID)
		for _, c := range t.cals {
			rec.calendar(c)
		}
		ag, err := ss.agenda(t)
		var p service.OccurrencePage
		if err == nil {
			p, err = ag.Occurrences(ctx, service.OccurrencesRequest{Calendars: t.cals, From: sp.from, To: sp.to, Loc: sp.loc,
				Text: q.text, Limit: limit, Cursor: cur})
		}
		if err != nil {
			f.add(t, err)
			continue
		}
		f.answered++
		out.Skipped += p.Skipped
		pages = append(pages, &fetched{t: t, page: p})
	}
	if err := f.err(); err != nil {
		return eventsOut{}, "", err
	}
	out.Unavailable = f.list

	// The merge: nothing after the bound, the earliest last start of a page
	// that has more.
	bound, bounded := time.Time{}, false
	for _, p := range pages {
		if !p.page.Truncated {
			continue
		}
		last := time.Time{} // a page cut before its first item: nothing is returned this time
		if n := len(p.page.Items); n > 0 {
			last = p.page.Items[n-1].Occurrence.Start
		}
		if !bounded || last.Before(bound) {
			bound, bounded = last, true
		}
	}
	type item struct {
		p *fetched
		i int
	}
	var items []item
	for _, p := range pages {
		for i, o := range p.page.Items {
			if bounded && o.Occurrence.Start.After(bound) {
				break
			}
			items = append(items, item{p, i})
		}
	}
	order := map[*fetched]int{}
	for i, p := range pages {
		order[p] = i
	}
	slices.SortStableFunc(items, func(a, b item) int {
		return cmp.Or(a.p.page.Items[a.i].Occurrence.Start.Compare(b.p.page.Items[b.i].Occurrence.Start),
			cmp.Compare(order[a.p], order[b.p]), cmp.Compare(a.i, b.i))
	})
	if len(items) > limit {
		items = items[:limit]
	}
	for _, it := range items {
		it.p.used++
		out.Events = append(out.Events, ss.occurrenceView(it.p.t.info, it.p.page.Items[it.i], sp.loc))
	}
	for _, p := range pages {
		id := p.t.info.ID
		switch n := len(p.page.Items); {
		case p.used == n && p.page.Truncated:
			state[id] = p.page.Next
		case p.used == n:
			delete(state, id)
		case p.used > 0:
			state[id] = p.page.Items[p.used-1].Cursor()
		}
	}
	// Accounts that failed keep their cursor: a next page retries them.
	if len(state) > 0 {
		out.Truncated, out.NextCursor = true, listCursor{V: 1, A: state}.encode()
	}
	out.Note = skippedNote(out.Skipped)
	text := fmt.Sprintf("%d occurrences from %s to %s (%s)", len(out.Events), out.From, out.To, out.Zone)
	if out.Truncated {
		text += "; more with next_cursor"
	}
	return out, text, nil
}

func (ss *session) occurrenceView(a service.AccountInfo, o service.AgendaOccurrence, loc *time.Location) occurrenceView {
	start, end := occurrenceTimes(o.Occurrence, loc)
	desc, cut := cutText(o.Description, listDescriptionMax)
	return occurrenceView{
		EventID:           encodeEventID(a.ID, o.Ref),
		Account:           a.Address,
		CalendarID:        o.Ref.CalendarID,
		CalendarType:      o.CalendarType,
		Start:             start,
		End:               end,
		AllDay:            o.Occurrence.AllDay,
		Recurring:         o.Occurrence.RecurrenceID != nil,
		Busy:              o.Busy,
		ETag:              o.ETag,
		ExternalOrganizer: o.ExternalOrganizer,
		ThirdParty:        o.ExternalOrganizer || o.CalendarType != "normal",
		Untrusted:         occurrenceText{Summary: o.Summary, Location: o.Location, Description: desc, DescriptionTruncated: cut},
	}
}

// ---- event_get ----

type eventGetIn struct {
	EventID string `json:"event_id" jsonschema:"an event_id a list returned"`
}

type personView struct {
	Name   string `json:"name,omitempty"`
	Email  string `json:"email,omitempty"`
	Role   string `json:"role,omitempty"`
	Status string `json:"status,omitempty" jsonschema:"PARTSTAT"`
}

type alarmView struct {
	MinutesBefore int    `json:"minutes_before" jsonschema:"negative: after the start"`
	Action        string `json:"action"`
}

type eventText struct {
	Summary              string       `json:"summary,omitempty"`
	Description          string       `json:"description,omitempty" jsonschema:"cut at 64 KiB"`
	DescriptionTruncated bool         `json:"description_truncated,omitempty"`
	Location             string       `json:"location,omitempty"`
	URL                  string       `json:"url,omitempty"`
	Organizer            *personView  `json:"organizer,omitempty"`
	Attendees            []personView `json:"attendees,omitempty"`
}

type componentView struct {
	RecurrenceID      *timeView   `json:"recurrence_id,omitempty" jsonschema:"the occurrence an override replaces"`
	Start             timeView    `json:"start"`
	End               timeView    `json:"end"`
	RRule             string      `json:"rrule,omitempty"`
	ExDates           []timeView  `json:"exdates,omitempty"`
	Status            string      `json:"status,omitempty"`
	Transp            string      `json:"transp,omitempty"`
	Busy              bool        `json:"busy"`
	Alarms            []alarmView `json:"alarms,omitempty"`
	UnknownProperties int         `json:"unknown_properties" jsonschema:"properties pcb keeps but does not show"`
	Untrusted         eventText   `json:"untrusted"`
}

type eventOut struct {
	EventID           string          `json:"event_id"`
	Account           string          `json:"account"`
	CalendarID        string          `json:"calendar_id"`
	CalendarType      string          `json:"calendar_type"`
	ReadOnly          bool            `json:"read_only" jsonschema:"pcb does not write to its calendar"`
	ETag              string          `json:"etag"`
	ExternalOrganizer bool            `json:"external_organizer"`
	ThirdParty        bool            `json:"third_party"`
	Event             componentView   `json:"event" jsonschema:"the series (or the single event)"`
	Overrides         []componentView `json:"overrides,omitempty" jsonschema:"occurrences changed from the series"`
}

func addEventGet(ss *session) {
	addTool(ss, &sdkmcp.Tool{
		Name:        "event_get",
		Description: "One event in full by its event_id: times, recurrence, exceptions, alarms, status, its overridden occurrences, and its etag; text and people under untrusted.",
		Annotations: readOnly("Get an event"),
	}, func(ctx context.Context, in eventGetIn) (eventOut, string, error) {
		tgt, err := ss.event(ctx, in.EventID)
		if err != nil {
			return eventOut{}, "", err
		}
		ref, info := tgt.ref, tgt.info
		d, err := tgt.ag.Event(ctx, ref)
		if err != nil {
			return eventOut{}, "", eventGone(err, in.EventID)
		}
		external := d.ExternalOrganizer // the service's judgement, as the lists'
		out := eventOut{
			EventID: in.EventID, Account: info.Address, CalendarID: ref.CalendarID, CalendarType: d.CalendarType,
			ReadOnly: d.ReadOnly, ETag: d.ETag, ExternalOrganizer: external, ThirdParty: external || d.CalendarType != "normal",
			Event: component(d.Data),
		}
		for _, ov := range d.Data.Overrides {
			out.Overrides = append(out.Overrides, component(ov))
		}
		return out, fmt.Sprintf("one event, %d overridden occurrences", len(out.Overrides)), nil
	})
}

func component(d service.EventData) componentView {
	desc, cut := cutText(d.Description, eventDescriptionMax)
	c := componentView{
		Start: eventTime(d.Start), End: eventTime(d.End), RRule: d.RRule, Status: d.Status, Transp: d.Transp,
		Busy: d.Transp != "TRANSPARENT" && d.Status != "CANCELLED", UnknownProperties: d.Unknown,
		Untrusted: eventText{Summary: d.Summary, Description: desc, DescriptionTruncated: cut, Location: d.Location, URL: d.URL},
	}
	if d.RecurrenceID != nil {
		v := eventTime(*d.RecurrenceID)
		c.RecurrenceID = &v
	}
	for _, x := range d.ExDates {
		c.ExDates = append(c.ExDates, eventTime(x))
	}
	for _, a := range d.Alarms {
		c.Alarms = append(c.Alarms, alarmView{MinutesBefore: a.MinutesBefore, Action: a.Action})
	}
	if o := d.Organizer; o != nil {
		c.Untrusted.Organizer = &personView{Name: o.Name, Email: o.Email, Role: o.Role, Status: o.Status}
	}
	for _, p := range d.Attendees {
		c.Untrusted.Attendees = append(c.Untrusted.Attendees, personView{Name: p.Name, Email: p.Email, Role: p.Role, Status: p.Status})
	}
	return c
}
