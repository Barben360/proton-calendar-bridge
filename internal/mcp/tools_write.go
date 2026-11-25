package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The write tools (design §5.7): event_create and event_update (write),
// event_delete (delete). Every change goes through the agenda, which
// writes only with the ETag the agent read (a stale one is a conflict and
// writes nothing) and applies every M4 refusal. Before any write a tool
// checks the account and calendar are visible (an event_id as event_get
// does, ruling R15) and that the calendar's write gate is open. A delete
// is confirmed by the person through a form elicitation when the client
// declares one; otherwise the grant must opt in (unconfirmed-delete), else
// it is refused. An update that removes occurrences after now from a
// series (an earlier UNTIL, a smaller COUNT, a sparser rule, the rule
// removed: the agenda counts them) is a delete too: it needs the delete
// grant and the same confirmation.

func writeTools() []toolDef {
	return []toolDef{
		{name: "event_create", cap: CapWrite, add: addEventCreate},
		{name: "event_update", cap: CapWrite, add: addEventUpdate},
		{name: "event_delete", cap: CapDelete, add: addEventDelete},
	}
}

// writing are the annotations of a write tool: destructive only for a
// delete (design §5.7).
func writing(title string, destructive bool) *sdkmcp.ToolAnnotations {
	return &sdkmcp.ToolAnnotations{Title: title, DestructiveHint: &destructive}
}

// noETagNote is a write's result when the ETag could not be read back: the
// write is done, so an error would invite the agent to write again.
const noETagNote = "the change is written but its etag could not be read back: call event_get with this event_id for it before another change; do not write it again"

const timeArgHelp = "RFC 3339 with an offset, or a wall clock (2026-11-14T15:00) read in tz"

// ---- arguments ----

// fieldArgs are the event fields a call gives: nil when absent; nulls are
// the names given as JSON null (event_update's "clear it").
type fieldArgs struct {
	summary, description, location, rrule *string
	start, end, startDate, endDate        *string
	alarms                                *[]int
	busy                                  *bool
	nulls                                 map[string]bool
}

// storeZone is the TZID new times are written with: loc's IANA name, ""
// (UTC) when it has none.
func storeZone(loc *time.Location) string {
	name := loc.String()
	if name == "UTC" || namedZone(name) == nil {
		return ""
	}
	return name
}

// eventZone is the zone a start is kept in (event_zone names the start's
// zone only, review M6): its TZID, UTC for a UTC time,
// "" for none (absent, all-day or floating).
func eventZone(t *service.EventTime) string {
	switch {
	case t == nil || t.Date != "" || t.Floating:
		return ""
	case t.TZID == "":
		return "UTC"
	}
	return t.TZID
}

// oneTimeForm refuses a call giving both forms of a time: which one is
// meant cannot be told, so neither wins.
func (a fieldArgs) oneTimeForm() error {
	if (a.start != nil || a.end != nil) && (a.startDate != nil || a.endDate != nil) {
		return refusedErr("start or end, and start_date or end_date, are all given: the call is ambiguous, nothing was written",
			"give start and end for a timed event ("+timeArgHelp+"), or start_date and end_date for an all-day one, not both")
	}
	return nil
}

// fields reads a call's fields: times read in loc and written with tzid,
// dates as dates, null as cleared. Refusals name the argument.
func (a fieldArgs) fields(loc *time.Location, tzid string) (service.EventFields, error) {
	var f service.EventFields
	for _, name := range []string{"start", "end", "start_date", "end_date", "busy"} {
		if a.nulls[name] {
			return f, refusedErr(name+" cannot be null", "leave it out to keep it, or give a value")
		}
	}
	if err := a.oneTimeForm(); err != nil {
		return f, err
	}
	cleared := func(name string, v *string) *string {
		if a.nulls[name] {
			return new(string)
		}
		return v
	}
	f.Summary, f.Description, f.Location = cleared("summary", a.summary), cleared("description", a.description), cleared("location", a.location)
	f.RRule = cleared("rrule", a.rrule)
	f.Alarms, f.Busy = a.alarms, a.busy
	if a.nulls["alarms"] {
		f.Alarms = &[]int{}
	}
	var err error
	for _, t := range []struct {
		name string
		v    *string
		out  **service.EventTime
		date bool
	}{
		{"start", a.start, &f.Start, false}, {"end", a.end, &f.End, false},
		{"start_date", a.startDate, &f.Start, true}, {"end_date", a.endDate, &f.End, true},
	} {
		if t.v == nil {
			continue
		}
		var et service.EventTime
		if t.date {
			et, err = writeDate(t.name, *t.v)
		} else {
			et, err = writeTime(t.name, *t.v, loc, tzid)
		}
		if err != nil {
			return f, err
		}
		*t.out = &et
	}
	return f, nil
}

// writeTime reads a time argument as Task 9's rules do, refusing a bare
// date: an all-day event takes start_date and end_date.
func writeTime(name, v string, loc *time.Location, tzid string) (service.EventTime, error) {
	if _, err := time.Parse(time.DateOnly, v); err == nil {
		return service.EventTime{}, refusedErr(name+" is a date, not a time: "+quote(v),
			"give start_date and end_date for an all-day event, or a time ("+timeArgHelp+")")
	}
	t, err := parseTime(name, v, loc)
	if err != nil {
		return service.EventTime{}, err
	}
	return service.EventTime{At: t, TZID: tzid}, nil
}

func writeDate(name, v string) (service.EventTime, error) {
	if d, err := time.Parse(time.DateOnly, v); err != nil || d.Format(time.DateOnly) != v {
		return service.EventTime{}, refusedErr(name+" is not a date: "+quote(v), "give a date YYYY-MM-DD; an end date is exclusive (the day after the last day)")
	}
	return service.EventTime{Date: v}, nil
}

func checkScope(scope string, ref service.EventRef) (service.EditScope, error) {
	switch s := service.EditScope(scope); {
	case s != service.ScopeSeries && s != service.ScopeOccurrence:
		return "", refusedErr("scope must be occurrence or series, not "+quote(scope),
			"series: the event or its whole series; occurrence: only the occurrence this event_id names")
	case s == service.ScopeOccurrence && ref.RecurrenceID == nil:
		return "", refusedErr("scope occurrence needs the event_id of one occurrence", "use the event_id events_list gives for that occurrence, or scope series")
	default:
		return s, nil
	}
}

func checkETag(etag string) error {
	if etag == "" {
		return refusedErr("etag is empty", "give the etag events_list or event_get returned with the event")
	}
	return nil
}

// writable refuses, before any write, an account whose MCP switch keeps
// agents to reads (mayWrite) and a calendar pcb's write gate keeps
// read-only (design §5.4). gone answers an account that left the grant,
// missing a calendar the account does not hold. A calendar not synced yet
// is left to the agenda, which says so.
func (ss *session) writable(ctx context.Context, localID, calID string, gone, missing error) error {
	api, info, err := ss.srv.o.Hub.Account(localID)
	if errors.Is(err, service.ErrNoAccount) || (err == nil && !ss.grant.seesAccount(info)) {
		return gone
	}
	if err != nil {
		return err
	}
	if err := mayWrite(info); err != nil {
		return err
	}
	cols, err := api.Collections(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cols, func(c service.Collection) bool { return c.ID == calID })
	switch {
	case i < 0:
		return missing
	case cols[i].ReadOnly && !cols[i].Pending:
		return refusedErr("calendar "+calID+" is read-only for pcb: nothing was written",
			"pcb writes only to calendars calendars_list shows with read_only false; choose another calendar")
	}
	return nil
}

// reasonAccountReadOnly is the refusal of a write to an account whose MCP
// switch is read-only.
const reasonAccountReadOnly = "pcb:account-read-only"

// mayWrite refuses an account whose MCP switch is not read-write (the
// person's switch, above every grant): its agents only read. It reads the
// account as the Hub has it now, so a switch turned during a session
// counts from its next call.
func mayWrite(a service.AccountInfo) error {
	if a.MCPMode == service.MCPReadWrite {
		return nil
	}
	return &toolError{Class: classRefused, Reason: reasonAccountReadOnly,
		Message: "account " + a.Address + " is read-only for agents: nothing was changed",
		Hint:    "reading still works; only a person can allow changes, in pcb-ui or with pcb account mcp read-write; do not retry"}
}

// writeError is a write's error as the agent reads it: a stale ETag says
// to read the event again, a floating time names the fix, a missing event
// reads as an id that never named one.
func writeError(err error, eventID string) error {
	if eventID != "" {
		err = eventGone(err, eventID)
	}
	te := toToolError(err)
	switch {
	case te.Class == classConflict:
		te.Message = "the event changed since its etag was read: nothing was written"
		te.Hint = "read the event again with event_get for its current etag, check the change still applies, then retry"
	case te.Reason == "pcb:floating-time":
		te.Hint = "give each time with an offset (2026-11-14T15:00:00+01:00) or as a wall clock read in tz: a time in no zone cannot be stored"
	}
	return te
}

// ---- event_create ----

type createIn struct {
	Account     string `json:"account" jsonschema:"the account, by address or local ID"`
	Calendar    string `json:"calendar" jsonschema:"the calendar's ID (calendars_list): one with read_only false"`
	Summary     string `json:"summary" jsonschema:"the title"`
	Start       string `json:"start,omitempty" jsonschema:"start of a timed event: RFC 3339 with an offset, or a wall clock (2026-11-14T15:00) read in tz"`
	End         string `json:"end,omitempty" jsonschema:"end of a timed event (exclusive), as start"`
	StartDate   string `json:"start_date,omitempty" jsonschema:"first day of an all-day event, YYYY-MM-DD (instead of start)"`
	EndDate     string `json:"end_date,omitempty" jsonschema:"day after the last day of an all-day event, YYYY-MM-DD (exclusive)"`
	TZ          string `json:"tz,omitempty" jsonschema:"IANA zone the times are read in and written with; default the status zone"`
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
	RRule       string `json:"rrule,omitempty" jsonschema:"recurrence rule without RRULE: (FREQ=WEEKLY;BYDAY=MO)"`
	Alarms      []int  `json:"alarms,omitempty" jsonschema:"alarms, in minutes before the start"`
	Busy        *bool  `json:"busy,omitempty" jsonschema:"shown as busy; default true"`
}

type writeOut struct {
	EventID    string `json:"event_id" jsonschema:"opaque: pass it to event_get, event_update or event_delete"`
	Account    string `json:"account"`
	CalendarID string `json:"calendar_id"`
	ETag       string `json:"etag,omitempty" jsonschema:"the event's etag now; absent when it could not be read back: call event_get"`
	Zone       string `json:"zone" jsonschema:"the zone times without an offset were read in"`
	EventZone  string `json:"event_zone,omitempty" jsonschema:"the zone the event's start is kept in (UTC for a UTC time); absent for an all-day or floating event"`
	Note       string `json:"note,omitempty"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func addEventCreate(ss *session) {
	addTool(ss, &sdkmcp.Tool{
		Name: "event_create",
		Description: "Create an event in one calendar of one account: a timed event (start, end) or an all-day one (start_date, end_date), " +
			"optionally recurring (rrule), with alarms. Returns its event_id and etag.",
		Annotations: writing("Create an event", false),
	}, func(ctx context.Context, in createIn) (writeOut, string, error) {
		switch {
		case in.Account == "":
			return writeOut{}, "", refusedErr("account is empty", "give the account's address or local ID (status lists them)")
		case in.Calendar == "":
			return writeOut{}, "", refusedErr("calendar is empty", "give a calendar's ID (calendars_list lists them)")
		}
		ts, err := ss.accounts(ctx, []string{in.Account})
		if err != nil {
			return writeOut{}, "", err
		}
		t := ts[0] // holds a granted calendar, else accounts answered noAccount
		if !ss.grant.seesCalendar(in.Calendar) {
			return writeOut{}, "", noCalendar(in.Calendar)
		}
		loc, err := callZone(in.TZ, ss.srv.o.Zone)
		if err != nil {
			return writeOut{}, "", err
		}
		args := fieldArgs{summary: &in.Summary, description: optional(in.Description), location: optional(in.Location), rrule: optional(in.RRule),
			start: optional(in.Start), end: optional(in.End), startDate: optional(in.StartDate), endDate: optional(in.EndDate), busy: in.Busy}
		if in.Alarms != nil {
			args.alarms = &in.Alarms
		}
		if err := args.oneTimeForm(); err != nil {
			return writeOut{}, "", err
		}
		switch {
		case strings.TrimSpace(in.Summary) == "":
			return writeOut{}, "", refusedErr("summary is empty", "give the event a title")
		case in.Start != "" && in.End == "":
			return writeOut{}, "", refusedErr("end is missing", "a timed event takes start and end ("+timeArgHelp+")")
		case in.StartDate != "" && in.EndDate == "":
			return writeOut{}, "", refusedErr("end_date is missing", "an all-day event takes start_date and end_date (exclusive)")
		case in.Start == "" && in.StartDate == "":
			return writeOut{}, "", refusedErr("start is missing", "give start and end for a timed event, or start_date and end_date for an all-day one")
		}
		f, err := args.fields(loc, storeZone(loc))
		if err != nil {
			return writeOut{}, "", err
		}
		rec := auditOf(ctx)
		rec.account(t.info.ID)
		rec.calendar(in.Calendar)
		ag, err := ss.agenda(t)
		if err == nil {
			err = ss.writable(ctx, t.info.ID, in.Calendar, noAccount(in.Account), noCalendar(in.Calendar))
		}
		if err != nil {
			return writeOut{}, "", err
		}
		ref, etag, err := ag.CreateEvent(ctx, in.Calendar, f)
		if errors.Is(err, service.ErrNoSuchCalendar) {
			err = noCalendar(in.Calendar)
		}
		if err != nil {
			return writeOut{}, "", writeError(err, "")
		}
		rec.uid = ref.UID
		out := writeOut{EventID: encodeEventID(t.info.ID, ref), Account: t.info.Address, CalendarID: in.Calendar, ETag: etag, Zone: zoneLabel(loc, ss.srv.o.Now()),
			EventZone: eventZone(f.Start)}
		if etag == "" {
			out.Note = noETagNote
			return out, "event created; call event_get for its etag", nil
		}
		return out, "event created in calendar " + in.Calendar, nil
	}, dateFields(), alarmBounds())
}

// ---- event_update ----

// changesIn are event_update's changes: create's fields, each optional;
// null clears a text, the rule or the alarms.
type changesIn struct {
	Summary     *string `json:"summary,omitempty"`
	Start       *string `json:"start,omitempty" jsonschema:"new start of a timed event: RFC 3339 with an offset, or a wall clock read in tz. Without end, the end moves by the same amount: the event is moved and keeps its length"`
	End         *string `json:"end,omitempty" jsonschema:"new end (exclusive), as start; given, it wins: the length changes with it"`
	StartDate   *string `json:"start_date,omitempty" jsonschema:"new first day of an all-day event, YYYY-MM-DD. Without end_date, the end date moves by the same number of days"`
	EndDate     *string `json:"end_date,omitempty" jsonschema:"new day after the last day of an all-day event (exclusive)"`
	Location    *string `json:"location,omitempty" jsonschema:"null clears it"`
	Description *string `json:"description,omitempty" jsonschema:"null clears it"`
	RRule       *string `json:"rrule,omitempty" jsonschema:"new recurrence rule; null makes the event single (series scope only)"`
	Alarms      *[]int  `json:"alarms,omitempty" jsonschema:"replaces every alarm, in minutes before the start; null or [] removes them"`
	Busy        *bool   `json:"busy,omitempty"`

	nulls map[string]bool // the keys given as null
}

// UnmarshalJSON records which keys are null: a pointer cannot tell null
// from absent.
func (c *changesIn) UnmarshalJSON(b []byte) error {
	type plain changesIn
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*c = changesIn(p)
	c.nulls = map[string]bool{}
	for k, v := range raw {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			c.nulls[k] = true
		}
	}
	return nil
}

func (c changesIn) args() fieldArgs {
	return fieldArgs{summary: c.Summary, description: c.Description, location: c.Location, rrule: c.RRule,
		start: c.Start, end: c.End, startDate: c.StartDate, endDate: c.EndDate, alarms: c.Alarms, busy: c.Busy, nulls: c.nulls}
}

func (c changesIn) empty() bool {
	return len(c.nulls) == 0 && c.Summary == nil && c.Start == nil && c.End == nil && c.StartDate == nil && c.EndDate == nil &&
		c.Location == nil && c.Description == nil && c.RRule == nil && c.Alarms == nil && c.Busy == nil
}

type updateIn struct {
	EventID string    `json:"event_id" jsonschema:"an event_id a list returned"`
	ETag    string    `json:"etag" jsonschema:"the etag the event was read with (events_list, event_get)"`
	Scope   string    `json:"scope" jsonschema:"series: the event or its whole series; occurrence: only the occurrence this event_id names"`
	Changes changesIn `json:"changes" jsonschema:"the fields to change, as event_create takes them; a field left out is kept, null clears it"`
	TZ      string    `json:"tz,omitempty" jsonschema:"IANA zone times without an offset are read in and new times written with; default the status zone for reading, the event's own zone for writing"`
	DryRun  bool      `json:"dry_run,omitempty" jsonschema:"write nothing: show the changed fields before and after"`
}

type changeText struct {
	Summary     *string `json:"summary,omitempty"`
	Location    *string `json:"location,omitempty"`
	Description *string `json:"description,omitempty" jsonschema:"cut at 64 KiB"`
}

// changeView is one side of a dry run: only the fields that change.
type changeView struct {
	Start     *timeView    `json:"start,omitempty"`
	End       *timeView    `json:"end,omitempty"`
	RRule     *string      `json:"rrule,omitempty"`
	Alarms    *[]alarmView `json:"alarms,omitempty"`
	Busy      *bool        `json:"busy,omitempty"`
	Untrusted changeText   `json:"untrusted"`
}

type updateOut struct {
	EventID    string      `json:"event_id"`
	Account    string      `json:"account"`
	CalendarID string      `json:"calendar_id"`
	ETag       string      `json:"etag,omitempty" jsonschema:"the event's etag now (unchanged by a dry run); absent when it could not be read back: call event_get"`
	Zone       string      `json:"zone" jsonschema:"the zone times without an offset were read in"`
	EventZone  string      `json:"event_zone,omitempty" jsonschema:"the zone the event's start is kept in after the change (UTC for a UTC time): its own unless tz moved it; absent for an all-day or floating event. An end changed alone with tz takes tz's zone, apart from the start's"`
	DryRun     bool        `json:"dry_run"`
	Changed    []string    `json:"changed,omitempty" jsonschema:"dry run: the fields that would change"`
	Dropped    int         `json:"dropped_occurrences,omitempty" jsonschema:"dry run: how many occurrences after now the change would remove from the series (-1: some, how many cannot be counted); such a change needs the delete grant and the person's confirmation"`
	Before     *changeView `json:"before,omitempty"`
	After      *changeView `json:"after,omitempty"`
	Note       string      `json:"note,omitempty"`
}

// confirmUpdateKey names an update's question in InputRequests.
const confirmUpdateKey = "confirm_update"

func addEventUpdate(ss *session) {
	t := &sdkmcp.Tool{
		Name: "event_update",
		Description: "Change an event, its whole series or one occurrence, if it is still as read (etag; else a conflict and nothing is written). " +
			"A series change that would orphan its exceptions is refused. A series change that removes occurrences after now (an earlier UNTIL, " +
			"a smaller COUNT, a sparser rule, rrule null) is a delete: it needs the delete grant, and the person may be asked to confirm it; " +
			"declined, nothing is written. dry_run shows the change without writing.",
		Annotations: writing("Update an event", false),
	}
	t.Description += " " + untrustedNote
	t.InputSchema = inputSchema[updateIn](t.Name, []tuner{scopeEnum, dateFields("changes"), alarmBounds("changes")})
	sdkmcp.AddTool(ss.sdk, t, func(ctx context.Context, req *sdkmcp.CallToolRequest, in updateIn) (*sdkmcp.CallToolResult, updateOut, error) {
		out, text, ask, err := ss.updateEvent(ctx, req, in)
		switch {
		case err != nil:
			return nil, updateOut{}, toToolError(err)
		case ask != nil:
			return &sdkmcp.CallToolResult{InputRequests: sdkmcp.InputRequestMap{confirmUpdateKey: ask}}, updateOut{}, nil
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}}}, out, nil
	})
}

// updateEvent runs once, or twice when the person is asked (E19), as
// deleteEvent does: an update the agenda's dry run finds removing
// occurrences after now is held to a delete's rules before it writes.
func (ss *session) updateEvent(ctx context.Context, req *sdkmcp.CallToolRequest, in updateIn) (updateOut, string, *sdkmcp.ElicitParams, error) {
	tgt, err := ss.event(ctx, in.EventID)
	if err != nil {
		return updateOut{}, "", nil, err
	}
	scope, err := checkScope(in.Scope, tgt.ref)
	if err == nil {
		err = checkETag(in.ETag)
	}
	if err == nil && in.Changes.empty() {
		err = refusedErr("changes is empty", "name the fields to change, as event_create takes them")
	}
	if err != nil {
		return updateOut{}, "", nil, err
	}
	loc, err := callZone(in.TZ, ss.srv.o.Zone)
	if err != nil {
		return updateOut{}, "", nil, err
	}
	f, err := in.Changes.args().fields(loc, storeZone(loc))
	if err != nil {
		return updateOut{}, "", nil, err
	}
	if err := ss.writable(ctx, tgt.info.ID, tgt.ref.CalendarID, noEvent(in.EventID), noEvent(in.EventID)); err != nil {
		return updateOut{}, "", nil, err
	}
	// New times keep the event's own zone unless tz names one: a series
	// moved in its zone keeps its exceptions (design §5.6). The result
	// names the zone the event keeps (M8-F1).
	d, err := tgt.ag.Event(ctx, tgt.ref)
	if err != nil {
		return updateOut{}, "", nil, writeError(err, in.EventID)
	}
	own := d.Data.Start
	if in.TZ == "" && own.TZID != "" && own.Date == "" && !own.Floating {
		for _, t := range []*service.EventTime{f.Start, f.End} {
			if t != nil && t.Date == "" {
				t.TZID = own.TZID
			}
		}
	}
	kept := eventZone(&own)
	if f.Start != nil {
		kept = eventZone(f.Start)
	}
	if !in.DryRun {
		// The dry run counts what the change removes; the write that follows
		// holds the same etag, so it writes what was counted or nothing.
		preview, err := tgt.ag.UpdateEvent(ctx, tgt.ref, in.ETag, scope, f, true)
		if err != nil {
			return updateOut{}, "", nil, writeError(err, in.EventID)
		}
		if preview.Dropped != 0 {
			if ask, err := ss.mayDrop(req, d.Data, preview.Dropped); ask != nil || err != nil {
				return updateOut{}, "", ask, err
			}
		}
	}
	res, err := tgt.ag.UpdateEvent(ctx, tgt.ref, in.ETag, scope, f, in.DryRun)
	if err != nil {
		return updateOut{}, "", nil, writeError(err, in.EventID)
	}
	out := updateOut{EventID: in.EventID, Account: tgt.info.Address, CalendarID: tgt.ref.CalendarID, ETag: res.ETag, Zone: zoneLabel(loc, ss.srv.o.Now()),
		EventZone: kept, DryRun: in.DryRun}
	if in.DryRun {
		before, after := res.Before, res.After
		if scope == service.ScopeOccurrence {
			before, after = occurrenceOf(before, *tgt.ref.RecurrenceID), occurrenceOf(after, *tgt.ref.RecurrenceID)
		}
		out.Changed, out.Before, out.After = diffEvent(before, after)
		out.Dropped = res.Dropped
		return out, fmt.Sprintf("dry run, nothing written: %d fields would change", len(out.Changed)), nil, nil
	}
	if res.ETag == "" {
		out.Note = noETagNote
		return out, "event updated; call event_get for its etag", nil, nil
	}
	return out, "event updated (" + string(scope) + ")", nil, nil
}

// reasonNeedsDelete is the refusal of an update that removes occurrences
// under a grant that may not delete.
const reasonNeedsDelete = "pcb:needs-delete"

// mayDrop holds an update that removes dropped occurrences after now from
// the series d (service.DroppedUnknown: some) to a delete's rules: the
// grant must hold delete; then the person confirms it when the client can
// ask (the question, or nil once accepted), else the grant must opt in
// with unconfirmed-delete.
func (ss *session) mayDrop(req *sdkmcp.CallToolRequest, d service.EventData, dropped int) (*sdkmcp.ElicitParams, error) {
	removes := "removes future occurrences of the series (how many cannot be counted)"
	switch {
	case dropped == 1:
		removes = "removes 1 future occurrence of the series"
	case dropped > 1:
		removes = fmt.Sprintf("removes %d future occurrences of the series", dropped)
	}
	if !ss.grant.has(CapDelete) {
		return nil, &toolError{Class: classRefused, Reason: reasonNeedsDelete,
			Message: "this change " + removes + ", which only a grant that may delete can do: nothing was changed",
			Hint: "change the series without ending it early, or have the person allow deletes: re-create the token with pcb mcp token add NAME --allow ...,delete " +
				"(pcb mcp --allow ...,delete for stdio); do not retry as is"}
	}
	if err := ss.mayActUnasked(req, "change, which "+removes+",", "changes that remove occurrences"); err != nil {
		return nil, err
	}
	if !asksForm(req) {
		return nil, nil
	}
	answer, asked := req.Params.InputResponses[confirmUpdateKey]
	if !asked {
		return &sdkmcp.ElicitParams{
			Mode:            "form",
			Message:         dropPrompt(d, dropped),
			RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}, nil
	}
	if r, ok := answer.(*sdkmcp.ElicitResult); !ok || r.Action != "accept" {
		return nil, refusedErr("the change was not confirmed: nothing was changed",
			"the person declined it; do not retry unless they ask for it again")
	}
	return nil, nil
}

// dropPrompt is the question about an update that removes occurrences: the
// series and how many go first, from pcb; the third party's summary last,
// as deletePrompt quotes it.
func dropPrompt(d service.EventData, dropped int) string {
	head := "Change the series starting on " + when(d.Start)
	title := "«" + promptText(d.Summary) + "»?"
	if dropped < 0 {
		return head + ", which removes future occurrences pcb cannot count: " + title
	}
	what := fmt.Sprintf("%d of its future occurrences are removed", dropped)
	if dropped == 1 {
		what = "1 of its future occurrences is removed"
	}
	return head + " so that " + what + ": " + title
}

// sameTime: a and b name the same date, wall clock or instant.
func sameTime(a, b service.EventTime) bool {
	if a.Date != "" || b.Date != "" {
		return a.Date == b.Date
	}
	return a.Floating == b.Floating && a.At.Equal(b.At)
}

// occurrenceOf is the occurrence of d at rid: its override, else the one
// its master generates there (the master's fields at that start, as long as
// the master in elapsed time, as ical.Expand makes it).
func occurrenceOf(d service.EventData, rid service.EventTime) service.EventData {
	for _, o := range d.Overrides {
		if o.RecurrenceID != nil && sameTime(*o.RecurrenceID, rid) {
			return o
		}
	}
	o := d
	o.Overrides, o.RRule, o.ExDates = nil, "", nil
	switch {
	case d.Start.Date != "":
		s, err1 := time.Parse(time.DateOnly, d.Start.Date)
		e, err2 := time.Parse(time.DateOnly, d.End.Date)
		r, err3 := time.Parse(time.DateOnly, rid.Date)
		if err1 == nil && err2 == nil && err3 == nil {
			o.Start.Date, o.End.Date = rid.Date, r.Add(e.Sub(s)).Format(time.DateOnly)
		}
	case !rid.At.IsZero():
		// rid is an instant: the occurrence starts there and lasts as long
		// as the master (a floating time holds its wall clock as UTC, so
		// the difference is the same).
		o.Start.At = rid.At.In(d.Start.At.Location())
		o.End.At = rid.At.Add(d.End.At.Sub(d.Start.At)).In(d.End.At.Location())
	}
	return o
}

// diffEvent lists the fields that differ between before and after, and
// both sides with those fields only.
func diffEvent(before, after service.EventData) (changed []string, b, a *changeView) {
	b, a = &changeView{}, &changeView{}
	bc, ac := component(before), component(after)
	for _, x := range []struct {
		name string
		b, a any
		set  func(v *changeView, c componentView)
	}{
		{"start", bc.Start, ac.Start, func(v *changeView, c componentView) { v.Start = &c.Start }},
		{"end", bc.End, ac.End, func(v *changeView, c componentView) { v.End = &c.End }},
		{"rrule", bc.RRule, ac.RRule, func(v *changeView, c componentView) { v.RRule = &c.RRule }},
		{"alarms", bc.Alarms, ac.Alarms, func(v *changeView, c componentView) { al := append([]alarmView{}, c.Alarms...); v.Alarms = &al }},
		{"busy", bc.Busy, ac.Busy, func(v *changeView, c componentView) { v.Busy = &c.Busy }},
		{"summary", bc.Untrusted.Summary, ac.Untrusted.Summary, func(v *changeView, c componentView) { v.Untrusted.Summary = &c.Untrusted.Summary }},
		{"location", bc.Untrusted.Location, ac.Untrusted.Location, func(v *changeView, c componentView) { v.Untrusted.Location = &c.Untrusted.Location }},
		{"description", bc.Untrusted.Description, ac.Untrusted.Description, func(v *changeView, c componentView) {
			v.Untrusted.Description = &c.Untrusted.Description
		}},
	} {
		if reflect.DeepEqual(x.b, x.a) || (x.name == "alarms" && len(bc.Alarms) == 0 && len(ac.Alarms) == 0) {
			continue
		}
		changed = append(changed, x.name)
		x.set(b, bc)
		x.set(a, ac)
	}
	return changed, b, a
}

// ---- event_delete ----

type deleteIn struct {
	EventID string `json:"event_id" jsonschema:"an event_id a list returned"`
	ETag    string `json:"etag" jsonschema:"the etag the event was read with (events_list, event_get)"`
	Scope   string `json:"scope" jsonschema:"series: the event or its whole series; occurrence: only the occurrence this event_id names"`
}

type deleteOut struct {
	EventID    string `json:"event_id"`
	Account    string `json:"account"`
	CalendarID string `json:"calendar_id"`
	Scope      string `json:"scope"`
	Deleted    bool   `json:"deleted"`
}

// confirmKey names the delete's question in InputRequests.
const confirmKey = "confirm_delete"

func addEventDelete(ss *session) {
	t := &sdkmcp.Tool{
		Name: "event_delete",
		Description: "Delete an event, its whole series or one occurrence, if it is still as read (etag; else a conflict and nothing is deleted). " +
			"The person may be asked to confirm; declined, nothing is deleted.",
		Annotations: writing("Delete an event", true),
	}
	t.Description += " " + untrustedNote
	t.InputSchema = inputSchema[deleteIn](t.Name, []tuner{scopeEnum})
	sdkmcp.AddTool(ss.sdk, t, func(ctx context.Context, req *sdkmcp.CallToolRequest, in deleteIn) (*sdkmcp.CallToolResult, deleteOut, error) {
		out, ask, err := ss.deleteEvent(ctx, req, in)
		switch {
		case err != nil:
			return nil, deleteOut{}, toToolError(err)
		case ask != nil:
			return &sdkmcp.CallToolResult{InputRequests: sdkmcp.InputRequestMap{confirmKey: ask}}, deleteOut{}, nil
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "event deleted (" + out.Scope + ")"}}}, out, nil
	})
}

// asksForm: the client declared form elicitation on this request (E19:
// an elicitation capability naming neither mode means form).
func asksForm(req *sdkmcp.CallToolRequest) bool {
	caps := req.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil {
		return false
	}
	return caps.Elicitation.Form != nil || caps.Elicitation.URL == nil
}

// mayActUnasked refuses a destructive call the person cannot confirm: the
// client declared no form elicitation and the grant does not opt in with
// unconfirmed-delete. A delete and a sign-out ask it first of all (before
// the event is read, so a stale etag or a missing event reveals nothing).
// act is the call in words, acts their plural.
func (ss *session) mayActUnasked(req *sdkmcp.CallToolRequest, act, acts string) error {
	if asksForm(req) || ss.grant.has(CapUnconfirmedDelete) {
		return nil
	}
	return &toolError{Class: classRefused, Reason: reasonUnconfirmed,
		Message: "this " + act + " needs the person's confirmation, which this client cannot ask for: nothing was changed",
		Hint: "use a client that supports form elicitation, or have the person allow unconfirmed " + acts + ": re-create the token with pcb mcp token add NAME --allow ...,unconfirmed-delete " +
			"(pcb mcp --allow ...,unconfirmed-delete for stdio); do not retry as is"}
}

// reasonUnconfirmed is the refusal of a delete, a sign-out or an update
// removing occurrences that nobody can confirm.
const reasonUnconfirmed = "pcb:unconfirmed-delete"

// deleteEvent runs once, or twice when the person is asked (E19): the run
// that asks only reads, and the answer is read on the next run, which
// checks everything again before it deletes.
func (ss *session) deleteEvent(ctx context.Context, req *sdkmcp.CallToolRequest, in deleteIn) (deleteOut, *sdkmcp.ElicitParams, error) {
	if err := ss.mayActUnasked(req, "delete", "deletes"); err != nil {
		return deleteOut{}, nil, err
	}
	tgt, err := ss.event(ctx, in.EventID)
	if err != nil {
		return deleteOut{}, nil, err
	}
	scope, err := checkScope(in.Scope, tgt.ref)
	if err == nil {
		err = checkETag(in.ETag)
	}
	if err == nil {
		err = ss.writable(ctx, tgt.info.ID, tgt.ref.CalendarID, noEvent(in.EventID), noEvent(in.EventID))
	}
	if err != nil {
		return deleteOut{}, nil, err
	}
	if asksForm(req) {
		d, err := tgt.ag.Event(ctx, tgt.ref)
		if err != nil {
			return deleteOut{}, nil, writeError(err, in.EventID)
		}
		if d.ETag != in.ETag {
			return deleteOut{}, nil, writeError(&toolError{Class: classConflict}, in.EventID)
		}
		answer, asked := req.Params.InputResponses[confirmKey]
		if !asked {
			return deleteOut{}, &sdkmcp.ElicitParams{
				Mode:            "form",
				Message:         deletePrompt(d.Data, tgt.ref, scope),
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			}, nil
		}
		if r, ok := answer.(*sdkmcp.ElicitResult); !ok || r.Action != "accept" {
			return deleteOut{}, nil, refusedErr("the deletion was not confirmed: nothing was deleted",
				"the person declined it; do not retry unless they ask for it again")
		}
	}
	if err := tgt.ag.DeleteEvent(ctx, tgt.ref, in.ETag, scope); err != nil {
		return deleteOut{}, nil, writeError(err, in.EventID)
	}
	return deleteOut{EventID: in.EventID, Account: tgt.info.Address, CalendarID: tgt.ref.CalendarID, Scope: string(scope), Deleted: true}, nil, nil
}

// deletePrompt is the question shown to the person. What goes (the event,
// one occurrence, or a whole series) and when come first, from pcb; the
// third party's summary comes last, inside quotes it cannot close.
func deletePrompt(d service.EventData, ref service.EventRef, scope service.EditScope) string {
	title := "«" + promptText(d.Summary) + "»?"
	switch {
	case scope != service.ScopeSeries:
		c := occurrenceOf(d, *ref.RecurrenceID)
		return "Delete one occurrence, on " + when(c.Start) + ", of the series «" + promptText(c.Summary) + "»?"
	case d.RRule != "":
		return "Delete the whole series (every occurrence, first on " + when(d.Start) + "): " + title
	}
	return "Delete the event on " + when(d.Start) + ": " + title
}

// promptText is a third party's text made fit for one line of a dialog
// that it must not rewrite: no control or format characters (bidirectional
// overrides and isolates, zero-width characters), the guillemets that quote
// it replaced by single ones, spaces folded, cut short.
func promptText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case r == '«':
			return '‹'
		case r == '»':
			return '›'
		}
		return r
	}, s)
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if s == "" {
		return "(no title)"
	}
	if c := cutUTF8(s, 100); c != s {
		return c + "…"
	}
	return s
}

// when is a time as the person reads it: a date, a wall clock with its
// zone, or a floating wall clock.
func when(t service.EventTime) string {
	const layout = "2006-01-02 15:04"
	switch {
	case t.Date != "":
		return t.Date
	case t.Floating:
		return t.At.Format(layout)
	case t.TZID != "":
		if z := zoneOf(t.TZID); z != nil {
			return t.At.In(z).Format(layout) + " " + t.TZID
		}
	}
	return t.At.UTC().Format(layout) + " UTC"
}
