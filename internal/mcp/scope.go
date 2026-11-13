package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// What every read tool resolves first: the accounts and calendars a call
// may read, its range in its zone, and the accounts that could not answer.

// ---- accounts and calendars in scope ----

// target is an account a call reads, and the name to answer with when it
// is gone: the one the agent gave, else its address.
type target struct {
	info service.AccountInfo
	name string
	cals []string // calendar IDs to read, nil for every one
}

func noAccount(name string) *toolError {
	return notFound("no account "+name, "the status tool lists the accounts this session can see")
}

func noCalendar(id string) *toolError {
	return notFound("no calendar "+id, "calendars_list lists the calendars this session can see")
}

func noEvent(id string) *toolError {
	return notFound("no event "+id, "list the events again: it may be gone, and an event_id is only one a tool returned")
}

// visible lists the accounts the grant sees now, in the Hub's order.
func (ss *session) visible() []service.AccountInfo {
	var out []service.AccountInfo
	for _, a := range ss.srv.o.Hub.Accounts() {
		if ss.grant.seesAccount(a) {
			out = append(out, a)
		}
	}
	return out
}

// accounts resolves a call's accounts argument: every visible account when
// empty, else those named (local ID or address), in the Hub's order. A
// name that is no visible account is not-found, the same as an unknown one.
func (ss *session) accounts(names []string) ([]target, error) {
	vis := ss.visible()
	var out []target
	if len(names) == 0 {
		for _, a := range vis {
			out = append(out, target{info: a, name: a.Address})
		}
		return out, nil
	}
	asked := map[string]string{} // local ID → the name given
	for _, n := range names {
		i := slices.IndexFunc(vis, func(a service.AccountInfo) bool { return namesAccount(n, a) })
		if i < 0 {
			return nil, noAccount(n)
		}
		if _, dup := asked[vis[i].ID]; !dup {
			asked[vis[i].ID] = n
		}
	}
	for _, a := range vis {
		if n, ok := asked[a.ID]; ok {
			out = append(out, target{info: a, name: n})
		}
	}
	return out, nil
}

// calendarIDs are the account's synced calendars as Serve's health lists
// them (no network); ok is false when that list is unknown (Serve does not
// serve, or the health is unavailable).
func (ss *session) calendarIDs(ctx context.Context, id string) (ids []string, ok bool) {
	h, err := ss.srv.o.Hub.Health(ctx, id)
	if err != nil || len(h.Calendars) == 0 {
		return nil, false
	}
	for _, c := range h.Calendars {
		ids = append(ids, c.ID)
	}
	return ids, true
}

// holdsGrantedCalendar: the grant names no calendar, or the account is
// known to hold one of them. An account whose calendars are unknown (not
// synced yet, Serve not serving) holds none: a calendar grant fails closed
// (ruling R14), and the account appears once its first sync lists them.
// Every tool leaves out an account it is false for, never naming it, not
// even under unavailable.
func (ss *session) holdsGrantedCalendar(ctx context.Context, id string) bool {
	if len(ss.grant.Calendars) == 0 {
		return true
	}
	owned, known := ss.calendarIDs(ctx, id)
	return known && slices.ContainsFunc(owned, ss.grant.seesCalendar)
}

// withCalendars chooses each target's calendars: those asked (each
// visible to the grant), else the grant's, else every calendar (nil). A
// target with none of them is dropped; an asked calendar no account holds
// is not-found, as one outside the grant. Under a calendar grant an
// account must hold one of them (holdsGrantedCalendar); without one, an
// account whose calendars are unknown is given those asked, for its agenda
// to answer.
func (ss *session) withCalendars(ctx context.Context, ts []target, asked []string) ([]target, error) {
	for _, id := range asked {
		if !ss.grant.seesCalendar(id) {
			return nil, noCalendar(id)
		}
	}
	want := asked
	if len(want) == 0 {
		want = ss.grant.Calendars
	}
	if len(want) == 0 {
		return ts, nil
	}
	held := map[string]bool{}
	var out []target
	for _, t := range ts {
		if !ss.holdsGrantedCalendar(ctx, t.info.ID) {
			continue
		}
		owned, known := ss.calendarIDs(ctx, t.info.ID) // unknown only without a calendar grant
		t.cals = nil
		for _, id := range want {
			if !known || slices.Contains(owned, id) {
				t.cals = append(t.cals, id)
				held[id] = true
			}
		}
		if len(t.cals) > 0 {
			out = append(out, t)
		}
	}
	for _, id := range asked {
		if !held[id] {
			return nil, noCalendar(id)
		}
	}
	return out, nil
}

// eventTarget is the event an event_id names, its account and agenda.
type eventTarget struct {
	info service.AccountInfo
	ref  service.EventRef
	ag   service.Agenda
}

// event resolves an event_id for every tool taking one (ruling R15): its
// account must be visible to the grant and, under a calendar grant, hold
// one of them (holdsGrantedCalendar), and its calendar be granted. Anything
// else is noEvent, the answer to a garbage id, before any agenda is asked:
// a hidden account reads as a missing one whatever its agenda would say.
func (ss *session) event(ctx context.Context, id string) (eventTarget, error) {
	localID, ref, err := decodeEventID(id)
	if err != nil {
		return eventTarget{}, noEvent(id)
	}
	if !slices.ContainsFunc(ss.visible(), func(a service.AccountInfo) bool { return a.ID == localID }) ||
		!ss.grant.seesCalendar(ref.CalendarID) || !ss.holdsGrantedCalendar(ctx, localID) {
		return eventTarget{}, noEvent(id)
	}
	rec := auditOf(ctx)
	rec.account(localID)
	rec.calendar(ref.CalendarID)
	rec.uid = ref.UID
	ag, info, err := ss.srv.o.Hub.Agenda(localID)
	if errors.Is(err, service.ErrNoAccount) || (err == nil && !ss.grant.seesAccount(info)) {
		return eventTarget{}, noEvent(id)
	}
	if err != nil {
		return eventTarget{}, err
	}
	return eventTarget{info: info, ref: ref, ag: ag}, nil
}

// eventGone maps an agenda's "no such event or calendar" to noEvent, the
// same answer as for an id that never named anything; other errors stay.
func eventGone(err error, id string) error {
	if errors.Is(err, service.ErrNoSuchObject) || errors.Is(err, service.ErrNoSuchCalendar) {
		return noEvent(id)
	}
	return err
}

// agenda is the target's agenda, refused as not-found when the account is
// gone or no longer visible since the call began.
func (ss *session) agenda(t target) (service.Agenda, error) {
	ag, info, err := ss.srv.o.Hub.Agenda(t.info.ID)
	if errors.Is(err, service.ErrNoAccount) || (err == nil && !ss.grant.seesAccount(info)) {
		return nil, noAccount(t.name)
	}
	return ag, err
}

// accountError is an account a call could not read while others answered.
type accountError struct {
	Account     string `json:"account"`
	Class       string `json:"class"`
	Message     string `json:"message"`
	Hint        string `json:"hint,omitempty"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
}

// failures collects the accounts a call could not read. Its error is the
// first one when no account answered: an agent asking one account gets
// that account's error, not an empty result.
type failures struct {
	list     []accountError
	first    error
	answered int
}

func (f *failures) add(t target, err error) {
	if f.first == nil {
		f.first = err
	}
	te := toToolError(err)
	f.list = append(f.list, accountError{Account: t.info.Address, Class: te.Class, Message: te.Message, Hint: te.Hint, RetryAfterS: te.RetryAfterS})
}

func (f *failures) err() error {
	if f.answered == 0 && f.first != nil {
		return f.first
	}
	return nil
}

// ---- time ranges ----

// span is a call's range, read in its zone.
type span struct {
	from, to time.Time
	loc      *time.Location
	zone     string
}

func (ss *session) span(from, to, tz string) (span, error) {
	loc, err := callZone(tz, ss.srv.o.Zone)
	if err != nil {
		return span{}, err
	}
	f, err := parseTime("from", from, loc)
	if err != nil {
		return span{}, err
	}
	t, err := parseTime("to", to, loc)
	if err != nil {
		return span{}, err
	}
	return span{from: f, to: t, loc: loc, zone: loc.String()}, nil
}

func (s span) echo() (from, to string) {
	return s.from.In(s.loc).Format(time.RFC3339), s.to.In(s.loc).Format(time.RFC3339)
}

func skippedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d events of the range cannot be read and were left out: their times are unknown, they may be busy", n)
}
