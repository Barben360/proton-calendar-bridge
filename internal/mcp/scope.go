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

// withCalendars chooses each target's calendars: those asked (each
// visible to the grant), else the grant's, else every calendar (nil). A
// target with none of them is dropped; an asked calendar no account holds
// is not-found, as one outside the grant. An account whose calendars are
// unknown is given them all, for its agenda to answer.
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
		owned, known := ss.calendarIDs(ctx, t.info.ID)
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
