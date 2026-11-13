package mcp_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// ---- the stub's writes ----

type createCall struct {
	cal string
	f   service.EventFields
}

type updateCall struct {
	ref    service.EventRef
	etag   string
	scope  service.EditScope
	f      service.EventFields
	dryRun bool
}

type deleteCall struct {
	ref   service.EventRef
	etag  string
	scope service.EditScope
}

// writeStub is what a stubAgenda was asked to write, and how it answers.
type writeStub struct {
	creates []createCall
	updates []updateCall
	deletes []deleteCall
	written int   // writes that changed an event
	noETag  bool  // the ETag cannot be read back after a write
	err     error // a write fails with it, once the ETag matched
	// A dry run's before and after; the stored event when nil.
	before, after *service.EventData
}

func stale() error {
	return &exitcode.Error{Code: exitcode.Usage, Err: service.ErrPreconditionFailed, Hint: "read the object again, then retry the change"}
}

func noObject() error {
	return &exitcode.Error{Code: exitcode.Usage, Err: service.ErrNoSuchObject, Hint: "list the calendar again"}
}

func (a *stubAgenda) CreateEvent(_ context.Context, calID string, f service.EventFields) (service.EventRef, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.w.creates = append(a.w.creates, createCall{calID, f})
	if a.err != nil {
		return service.EventRef{}, "", a.err
	}
	if a.w.err != nil {
		return service.EventRef{}, "", a.w.err
	}
	uid := fmt.Sprintf("new-%d@x", len(a.w.creates))
	etag := `"` + uid + `-1"`
	if a.events == nil {
		a.events = map[string]service.EventDetail{}
	}
	a.events[calID+"|"+uid] = service.EventDetail{ETag: etag, CalendarType: "normal",
		Data: service.EventData{UID: uid, Summary: *f.Summary, Start: *f.Start, End: *f.End}}
	a.w.written++
	if a.w.noETag {
		etag = ""
	}
	return service.EventRef{CalendarID: calID, UID: uid}, etag, nil
}

// target is the stored event an edit names, if its ETag is still etag.
func (a *stubAgenda) target(ref service.EventRef, etag string) (service.EventDetail, error) {
	if a.err != nil {
		return service.EventDetail{}, a.err
	}
	d, ok := a.events[ref.CalendarID+"|"+ref.UID]
	switch {
	case !ok:
		return d, noObject()
	case d.ETag != etag:
		return d, stale()
	}
	return d, a.w.err
}

func (a *stubAgenda) UpdateEvent(_ context.Context, ref service.EventRef, etag string, scope service.EditScope, f service.EventFields, dryRun bool) (service.UpdateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.w.updates = append(a.w.updates, updateCall{ref, etag, scope, f, dryRun})
	d, err := a.target(ref, etag)
	if err != nil {
		return service.UpdateResult{}, err
	}
	if dryRun {
		res := service.UpdateResult{ETag: etag, Before: d.Data, After: d.Data}
		if a.w.before != nil {
			res.Before = *a.w.before
		}
		if a.w.after != nil {
			res.After = *a.w.after
		}
		return res, nil
	}
	if f.Summary != nil {
		d.Data.Summary = *f.Summary
	}
	d.ETag = strings.TrimSuffix(d.ETag, `"`) + `+"`
	a.events[ref.CalendarID+"|"+ref.UID] = d
	a.w.written++
	if a.w.noETag {
		return service.UpdateResult{}, nil
	}
	return service.UpdateResult{ETag: d.ETag}, nil
}

func (a *stubAgenda) DeleteEvent(_ context.Context, ref service.EventRef, etag string, scope service.EditScope) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.w.deletes = append(a.w.deletes, deleteCall{ref, etag, scope})
	if _, err := a.target(ref, etag); err != nil {
		return err
	}
	delete(a.events, ref.CalendarID+"|"+ref.UID)
	a.w.written++
	return nil
}

func (a *stubAgenda) writes() writeStub {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.w
}

// asked counts the write calls the agenda received, refused ones included.
func (a *stubAgenda) asked() int {
	w := a.writes()
	return len(w.creates) + len(w.updates) + len(w.deletes)
}

func (s stubAPI) Collections(context.Context) ([]service.Collection, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []service.Collection
	for _, c := range s.cals {
		out = append(out, service.Collection{ID: c.ID, Name: c.Name, ReadOnly: c.ReadOnly})
	}
	return out, nil
}

// ---- fixtures ----

var (
	writeGrant  = grant(mcp.CapFreeBusy, mcp.CapRead, mcp.CapWrite)
	deleteGrant = grant(mcp.CapFreeBusy, mcp.CapRead, mcp.CapWrite, mcp.CapDelete)
)

// eventID is the event_id of an event, as the tools encode it.
func eventID(localID, cal, uid string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(localID + "\x00" + cal + "\x00" + uid))
}

var standupID = eventID("a1", "cal-a", "standup@x")

// connectAsking connects a client that declares form elicitation on
// protocol version and answers every question with answer, recording the
// messages it was shown.
func (r *rig) connectAsking(t *testing.T, g mcp.Grant, version, answer string) (*sdkmcp.ClientSession, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked []string
	)
	st, ct := sdkmcp.NewInMemoryTransports()
	ss, err := r.srv.Session(g, mcp.EntryStdio).Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "0"}, &sdkmcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			mu.Lock()
			asked = append(asked, req.Params.Message)
			mu.Unlock()
			return &sdkmcp.ElicitResult{Action: answer, Content: map[string]any{}}, nil
		},
	})
	cs, err := client.Connect(t.Context(), ct, &sdkmcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

func toolLines(t *testing.T, r *rig, tool string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range auditLines(t, r) {
		if l["tool"] == tool {
			out = append(out, l)
		}
	}
	return out
}

// ---- tests ----

func TestCreateReturnsIDAndETag(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	cs := r.connect(t, writeGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]

	out := mustCall(t, cs, "event_create", map[string]any{
		"account": "alice@example.com", "calendar": "cal-a", "summary": "Dentist",
		"start": "2026-11-20T15:00", "end": "2026-11-20T16:00",
		"location": "Rue X", "alarms": []int{10}, "busy": false,
	})
	if out["etag"] != `"new-1@x-1"` || out["account"] != "alice@example.com" || out["calendar_id"] != "cal-a" || out["zone"] != "Europe/Paris" {
		t.Errorf("create = %v", out)
	}
	w := alice.writes()
	if len(w.creates) != 1 || w.creates[0].cal != "cal-a" {
		t.Fatalf("creates %+v", w.creates)
	}
	f := w.creates[0].f
	wantStart := time.Date(2026, 11, 20, 15, 0, 0, 0, paris)
	if f.Start == nil || !f.Start.At.Equal(wantStart) || f.Start.TZID != "Europe/Paris" || f.Start.Date != "" || f.Start.Floating {
		t.Errorf("start %+v, want 15:00 Europe/Paris", f.Start)
	}
	if f.End == nil || !f.End.At.Equal(wantStart.Add(time.Hour)) || f.End.TZID != "Europe/Paris" {
		t.Errorf("end %+v, want 16:00 Europe/Paris", f.End)
	}
	if f.Summary == nil || *f.Summary != "Dentist" || f.Location == nil || *f.Location != "Rue X" || f.Description != nil ||
		f.RRule != nil || f.Alarms == nil || !slices.Equal(*f.Alarms, []int{10}) || f.Busy == nil || *f.Busy {
		t.Errorf("fields %+v", f)
	}
	// The event_id it returns is one event_get reads.
	got := mustCall(t, cs, "event_get", map[string]any{"event_id": out["event_id"]})
	if field(got, "event.untrusted.summary") != "Dentist" || got["etag"] != out["etag"] {
		t.Errorf("event_get of the new id %v", got)
	}

	// An offset keeps its instant, stored in the call's zone; tz reads a
	// wall clock; all-day dates are dates.
	for _, tc := range []struct {
		args       map[string]any
		start, end service.EventTime
	}{
		{map[string]any{"start": "2026-11-20T15:00:00Z", "end": "2026-11-20T16:00:00Z"},
			service.EventTime{At: time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC), TZID: "Europe/Paris"},
			service.EventTime{At: time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC), TZID: "Europe/Paris"}},
		{map[string]any{"start": "2026-11-20T09:00", "end": "2026-11-20T09:30", "tz": "America/New_York"},
			service.EventTime{At: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC), TZID: "America/New_York"},
			service.EventTime{At: time.Date(2026, 11, 20, 14, 30, 0, 0, time.UTC), TZID: "America/New_York"}},
		{map[string]any{"start_date": "2026-11-20", "end_date": "2026-11-21"},
			service.EventTime{Date: "2026-11-20"}, service.EventTime{Date: "2026-11-21"}},
	} {
		n := len(alice.writes().creates)
		mustCall(t, cs, "event_create", with(tc.args, "account", "a1", "calendar", "cal-a", "summary", "X"))
		c := alice.writes().creates[n].f
		if !c.Start.At.Equal(tc.start.At) || c.Start.TZID != tc.start.TZID || c.Start.Date != tc.start.Date ||
			!c.End.At.Equal(tc.end.At) || c.End.TZID != tc.end.TZID || c.End.Date != tc.end.Date {
			t.Errorf("%v: start %+v end %+v, want %+v %+v", tc.args, *c.Start, *c.End, tc.start, tc.end)
		}
	}

	// The ETag cannot be read back: still a success, pointing at event_get
	// (an error would invite a duplicate create).
	alice.mu.Lock()
	alice.w.noETag = true
	alice.mu.Unlock()
	out = mustCall(t, cs, "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "Y",
		"start": "2026-11-20T15:00", "end": "2026-11-20T16:00"})
	if out["etag"] != nil || out["event_id"] == nil || !strings.Contains(fmt.Sprint(out["note"]), "event_get") {
		t.Errorf("create without an ETag %v, want the event_id and a note naming event_get", out)
	}

	// Malformed arguments are refused, naming the argument, before any write.
	n := alice.asked()
	base := map[string]any{"account": "a1", "calendar": "cal-a", "summary": "Z"}
	for _, tc := range []struct {
		args map[string]any
		name string
	}{
		{with(base, "start", "tomorrow 3pm", "end", "2026-11-20T16:00"), "start"},
		{with(base, "start", "2026-11-20T15:00", "end", "16:00"), "end"},
		{with(base, "start", "2026-11-20", "end", "2026-11-21"), "start"},
		{with(base, "start", "2026-11-20T15:00"), "end"},
		{with(base, "start", "2026-11-20T15:00", "end", "2026-11-20T16:00", "start_date", "2026-11-20", "end_date", "2026-11-21"), "start_date"},
		{with(base, "start_date", "20/11/2026", "end_date", "2026-11-21"), "start_date"},
		{with(base, "start", "2026-11-20T15:00", "end", "2026-11-20T16:00", "tz", "Mars/Olympus"), "tz"},
		{with(base, "start", "2026-11-20T15:00", "end", "2026-11-20T16:00", "summary", " "), "summary"},
	} {
		terr := mustFail(t, cs, "event_create", tc.args, "refused")
		if !strings.Contains(terr.Message, tc.name) {
			t.Errorf("%v: %q does not name %s", tc.args, terr.Message, tc.name)
		}
	}
	if got := alice.asked(); got != n {
		t.Errorf("%d write calls for refused arguments", got-n)
	}
}

func TestUpdateStaleETagIsConflict(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	cs := r.connect(t, deleteGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]
	update := map[string]any{"event_id": standupID, "etag": `"standup@x-0"`, "scope": "series", "changes": map[string]any{"summary": "Moved"}}

	// Thunderbird changed the event since the agent read it.
	terr := mustFail(t, cs, "event_update", update, "conflict")
	if !strings.Contains(terr.Hint, "read") {
		t.Errorf("conflict hint %q does not say to read the event again", terr.Hint)
	}
	mustFail(t, cs, "event_delete", map[string]any{"event_id": standupID, "etag": `"standup@x-0"`, "scope": "series"}, "conflict")
	if w := alice.writes(); w.written != 0 {
		t.Fatalf("%d writes on a stale ETag", w.written)
	}
	if d := alice.events["cal-a|standup@x"]; d.Data.Summary != "Standup" {
		t.Errorf("the event changed: %+v", d.Data)
	}

	// With the current ETag the update goes through and gives the new one.
	out := mustCall(t, cs, "event_update", with(update, "etag", `"standup@x-1"`))
	if out["etag"] != `"standup@x-1+"` || out["event_id"] != standupID || out["dry_run"] == true {
		t.Errorf("update = %v", out)
	}
	if w := alice.writes(); w.written != 1 || w.updates[len(w.updates)-1].scope != service.ScopeSeries {
		t.Errorf("writes %+v", w)
	}
	// An ETag that cannot be read back is a success pointing at event_get.
	alice.mu.Lock()
	alice.w.noETag = true
	alice.mu.Unlock()
	out = mustCall(t, cs, "event_update", with(update, "etag", `"standup@x-1+"`))
	if out["etag"] != nil || !strings.Contains(fmt.Sprint(out["note"]), "event_get") {
		t.Errorf("update without an ETag back %v", out)
	}
}

func TestUpdateDryRunShowsBeforeAfter(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	cs := r.connect(t, writeGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]
	before := alice.events["cal-a|standup@x"].Data
	after := before
	after.Summary = "Daily standup"
	after.Start.At, after.End.At = before.Start.At.Add(time.Hour), before.End.At.Add(time.Hour)
	alice.w.after = &after

	out := mustCall(t, cs, "event_update", map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series", "dry_run": true,
		"changes": map[string]any{"summary": "Daily standup", "start": "2026-11-16T10:00", "end": "2026-11-16T10:30"}})
	if out["dry_run"] != true || out["etag"] != `"standup@x-1"` {
		t.Errorf("dry run %v, want dry_run and the unchanged etag", out)
	}
	changed := list(out["changed"])
	if len(changed) != 3 || !slices.Contains(changed, any("summary")) || !slices.Contains(changed, any("start")) || !slices.Contains(changed, any("end")) {
		t.Errorf("changed %v, want summary, start and end", changed)
	}
	if field(out, "before.untrusted.summary") != "Standup" || field(out, "after.untrusted.summary") != "Daily standup" {
		t.Errorf("summary before %v after %v", field(out, "before.untrusted.summary"), field(out, "after.untrusted.summary"))
	}
	if field(out, "before.start.at") != "2026-11-16T09:00:00+01:00" || field(out, "after.start.at") != "2026-11-16T10:00:00+01:00" ||
		field(out, "after.end.at") != "2026-11-16T10:30:00+01:00" {
		t.Errorf("times before %v after %v", out["before"], out["after"])
	}
	for _, side := range []string{"before", "after"} {
		for _, k := range []string{"rrule", "alarms", "busy"} {
			if v, ok := obj(out[side])[k]; ok {
				t.Errorf("%s.%s = %v: unchanged fields are left out", side, k, v)
			}
		}
		if v, ok := obj(field(out, side+".untrusted"))["location"]; ok {
			t.Errorf("%s.untrusted.location = %v: unchanged", side, v)
		}
	}
	w := alice.writes()
	if len(w.updates) != 1 || !w.updates[0].dryRun || w.written != 0 {
		t.Fatalf("dry run wrote: %+v", w)
	}
	// The new times reach the service in the event's own zone.
	f := w.updates[0].f
	if f.Start == nil || f.Start.TZID != "Europe/Paris" || !f.Start.At.Equal(time.Date(2026, 11, 16, 10, 0, 0, 0, paris)) || f.Summary == nil || f.Location != nil {
		t.Errorf("fields %+v", f)
	}

	// One occurrence of a series: before is the occurrence the rule makes
	// (no override yet), after its new override; only the summary changed.
	series := before
	series.RRule = "FREQ=DAILY"
	d := alice.events["cal-a|standup@x"]
	d.Data = series
	alice.events["cal-a|standup@x"] = d
	rid := before.Start.At.AddDate(0, 0, 2)
	override := series
	override.RRule, override.Summary = "", "Standup, moved room"
	override.Start.At, override.End.At = rid, rid.Add(30*time.Minute)
	override.RecurrenceID = &service.EventTime{At: rid, TZID: "Europe/Paris"}
	withOverride := series
	withOverride.Overrides = []service.EventData{override}
	alice.w.after = &withOverride
	occID := base64.RawURLEncoding.EncodeToString([]byte("a1\x00cal-a\x00standup@x\x00" + rid.UTC().Format(time.RFC3339)))
	out = mustCall(t, cs, "event_update", map[string]any{"event_id": occID, "etag": `"standup@x-1"`, "scope": "occurrence", "dry_run": true,
		"changes": map[string]any{"summary": "Standup, moved room"}})
	if c := list(out["changed"]); len(c) != 1 || c[0] != "summary" {
		t.Errorf("occurrence dry run changed %v, want only summary; out %v", c, out)
	}
	if field(out, "before.untrusted.summary") != "Standup" || field(out, "after.untrusted.summary") != "Standup, moved room" {
		t.Errorf("occurrence before %v after %v", out["before"], out["after"])
	}
}

func TestUpdateChangesNullClears(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	cs := r.connect(t, writeGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]
	upd := func(changes map[string]any) map[string]any {
		return map[string]any{"event_id": standupID, "etag": alice.events["cal-a|standup@x"].ETag, "scope": "series", "changes": changes}
	}
	mustCall(t, cs, "event_update", upd(map[string]any{"location": nil, "rrule": nil, "alarms": nil, "description": "Notes"}))
	w := alice.writes()
	f := w.updates[len(w.updates)-1].f
	if f.Location == nil || *f.Location != "" || f.RRule == nil || *f.RRule != "" || f.Alarms == nil || len(*f.Alarms) != 0 {
		t.Errorf("null did not clear: %+v", f)
	}
	if f.Description == nil || *f.Description != "Notes" || f.Summary != nil || f.Start != nil || f.End != nil || f.Busy != nil {
		t.Errorf("fields not named must stay nil: %+v", f)
	}
	n := alice.asked()
	for _, tc := range []struct {
		changes map[string]any
		name    string
	}{
		{map[string]any{"start": nil}, "start"},
		{map[string]any{"busy": nil}, "busy"},
		{map[string]any{}, "changes"},
		{map[string]any{"start": "2026-11-16T10:00", "start_date": "2026-11-16"}, "start_date"},
	} {
		terr := mustFail(t, cs, "event_update", upd(tc.changes), "refused")
		if !strings.Contains(terr.Message, tc.name) {
			t.Errorf("%v: %q does not name %s", tc.changes, terr.Message, tc.name)
		}
	}
	for _, tc := range []map[string]any{
		with(upd(map[string]any{"summary": "x"}), "scope", "this"),
		with(upd(map[string]any{"summary": "x"}), "scope", "occurrence"), // the id names no occurrence
		with(upd(map[string]any{"summary": "x"}), "etag", ""),
	} {
		mustFail(t, cs, "event_update", tc, "refused")
	}
	if got := alice.asked(); got != n {
		t.Errorf("%d write calls for refused changes", got-n)
	}
}

func TestUpdateSeriesWithExceptionsRefused(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	cs := r.connect(t, writeGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]
	refusal := func(code, reason string) error {
		return &exitcode.Error{Code: exitcode.MustAct, Err: &service.WriteRefusal{Code: code, Reason: reason},
			Hint: "pcb does not make this change; make it in the Proton web app"}
	}
	alice.w.err = refusal("pcb:series-has-exceptions", "the series has 2 exceptions the new rule would no longer match")
	args := map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series", "changes": map[string]any{"rrule": "FREQ=WEEKLY"}}
	terr := mustFail(t, cs, "event_update", args, "refused")
	if terr.Reason != "pcb:series-has-exceptions" || !strings.Contains(terr.Message, "2 exceptions") {
		t.Errorf("refusal %+v", *terr)
	}
	// A floating time the service refuses names the fix: a zone.
	alice.w.err = refusal("pcb:floating-time", "DTSTART is a floating time: give a zone or an offset")
	terr = mustFail(t, cs, "event_update", args, "refused")
	if terr.Reason != "pcb:floating-time" || !strings.Contains(terr.Message, "floating") || !strings.Contains(terr.Hint, "tz") {
		t.Errorf("floating refusal %+v, want its reason and a hint naming tz", *terr)
	}
	if w := alice.writes(); w.written != 0 {
		t.Errorf("%d writes", w.written)
	}
}

func TestWriteToReadOnlyCalendarRefused(t *testing.T) {
	hub := newHub()
	hub.cals["a1"][0].ReadOnly = true
	r := newRig(t, hub)
	cs := r.connect(t, deleteGrant, mcp.EntryRemote)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "X", "start": "2026-11-20T15:00", "end": "2026-11-20T16:00"}},
		{"event_update", map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series", "changes": map[string]any{"summary": "X"}, "dry_run": true}},
		{"event_delete", map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series"}},
	} {
		terr := mustFail(t, cs, tc.tool, tc.args, "refused")
		if !strings.Contains(terr.Message, "read-only") {
			t.Errorf("%s: %q does not say read-only", tc.tool, terr.Message)
		}
	}
	if n := hub.agendas["a1"].asked(); n != 0 {
		t.Errorf("the agenda was asked %d writes for a read-only calendar", n)
	}
	// A calendar the account does not hold is not-found.
	mustFail(t, cs, "event_create", map[string]any{"account": "a1", "calendar": "cal-b", "summary": "X",
		"start": "2026-11-20T15:00", "end": "2026-11-20T16:00"}, "not-found")
}

func TestDeleteAsksWhenElicitationDeclared(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			hub := newHub()
			r := newRig(t, hub)
			alice := hub.agendas["a1"]
			args := map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series"}
			for _, answer := range []string{"decline", "cancel"} {
				cs, asked := r.connectAsking(t, deleteGrant, version, answer)
				terr := mustFail(t, cs, "event_delete", args, "refused")
				if !strings.Contains(terr.Message, "not confirmed") {
					t.Errorf("%s: %+v", answer, *terr)
				}
				if got := asked(); len(got) != 1 || got[0] != "Delete «Standup» on 2026-11-16 09:00 Europe/Paris?" {
					t.Errorf("%s: asked %q", answer, got)
				}
				if w := alice.writes(); len(w.deletes) != 0 || w.written != 0 {
					t.Fatalf("%s: deleted %+v", answer, w)
				}
			}
			// A stale ETag is a conflict before the person is asked.
			cs, asked := r.connectAsking(t, deleteGrant, version, "accept")
			mustFail(t, cs, "event_delete", with(args, "etag", `"standup@x-0"`), "conflict")
			if got := asked(); len(got) != 0 {
				t.Errorf("asked %q about a stale event", got)
			}
			out := mustCall(t, cs, "event_delete", args)
			if out["deleted"] != true || out["event_id"] != standupID || out["scope"] != "series" {
				t.Errorf("delete = %v", out)
			}
			if len(asked()) != 1 {
				t.Errorf("asked %q, want one question", asked())
			}
			if w := alice.writes(); len(w.deletes) != 1 || w.deletes[0].etag != `"standup@x-1"` || w.deletes[0].scope != service.ScopeSeries {
				t.Errorf("deletes %+v", w.deletes)
			}
			// One audit line per call: the run that only asked writes none.
			lines := toolLines(t, r, "event_delete")
			if len(lines) != 4 {
				t.Fatalf("%d audit lines for 4 deletes:\n%s", len(lines), r.log.String())
			}
			for i, want := range []string{"refused", "refused", "conflict", "ok"} {
				if lines[i]["outcome"] != want || lines[i]["uid"] != "standup@x" {
					t.Errorf("line %d: %v, want outcome %s", i, lines[i], want)
				}
			}
		})
	}

	// An occurrence names its time, and a recurring series says so.
	hub := newHub()
	alice := hub.agendas["a1"]
	d := alice.events["cal-a|standup@x"]
	d.Data.RRule = "FREQ=DAILY"
	alice.events["cal-a|standup@x"] = d
	r := newRig(t, hub)
	rid := d.Data.Start.At.AddDate(0, 0, 3)
	occID := base64.RawURLEncoding.EncodeToString([]byte("a1\x00cal-a\x00standup@x\x00" + rid.UTC().Format(time.RFC3339)))
	cs, asked := r.connectAsking(t, deleteGrant, "2025-11-25", "decline")
	mustFail(t, cs, "event_delete", map[string]any{"event_id": occID, "etag": `"standup@x-1"`, "scope": "occurrence"}, "refused")
	mustFail(t, cs, "event_delete", map[string]any{"event_id": occID, "etag": `"standup@x-1"`, "scope": "series"}, "refused")
	want := []string{"Delete «Standup» on 2026-11-19 09:00 Europe/Paris?", "Delete «Standup» on 2026-11-16 09:00 Europe/Paris and every occurrence of its series?"}
	if got := asked(); !slices.Equal(got, want) {
		t.Errorf("asked %q, want %q", got, want)
	}
}

func TestDeleteWithoutElicitationNeedsDeleteCap(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	got, _ := toolNames(t, r.connect(t, writeGrant, mcp.EntryRemote))
	if slices.Contains(got, "event_delete") || !slices.Contains(got, "event_update") {
		t.Errorf("write grant tools %v: want event_update, not event_delete", got)
	}
	cs := r.connect(t, writeGrant, mcp.EntryRemote)
	args := map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series"}
	if res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "event_delete", Arguments: args}); err == nil && !res.IsError {
		t.Errorf("event_delete under a write grant answered %v", res.StructuredContent)
	}
	if n := len(hub.agendas["a1"].writes().deletes); n != 0 {
		t.Fatalf("%d deletes under a write grant", n)
	}
	// The delete grant alone allows it when the client cannot be asked.
	out := mustCall(t, r.connect(t, deleteGrant, mcp.EntryRemote), "event_delete", args)
	if out["deleted"] != true {
		t.Errorf("delete = %v", out)
	}
	if w := hub.agendas["a1"].writes(); len(w.deletes) != 1 || w.written != 1 {
		t.Errorf("deletes %+v", w.deletes)
	}
}

func TestWriteToolsAbsentForReadGrant(t *testing.T) {
	r := newRig(t, newHub())
	readOnly, _ := mcp.Profile("read-only")
	full, _ := mcp.Profile("full")
	writes := []string{"event_create", "event_delete", "event_update"}
	for _, g := range []mcp.Grant{readGrant, readOnly, grant(mcp.CapFreeBusy)} {
		got, _ := toolNames(t, r.connect(t, g, mcp.EntryStdio))
		for _, w := range writes {
			if slices.Contains(got, w) {
				t.Errorf("grant %v lists %s", g.Caps, w)
			}
		}
	}
	got, tools := toolNames(t, r.connect(t, full, mcp.EntryRemote))
	for _, w := range writes {
		if !slices.Contains(got, w) {
			t.Errorf("full grant lacks %s: %v", w, got)
		}
	}
	for _, tl := range tools {
		if !slices.Contains(writes, tl.Name) {
			continue
		}
		a := tl.Annotations
		if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint != (tl.Name == "event_delete") {
			t.Errorf("%s annotations %+v: want destructive only for event_delete", tl.Name, a)
		}
		if tl.OutputSchema == nil || !strings.HasSuffix(tl.Description, untrustedSentence) {
			t.Errorf("%s: output schema %v, description %q", tl.Name, tl.OutputSchema, tl.Description)
		}
	}
}

// Ruling R15: a tool taking an event_id answers for an account the session
// cannot see exactly as for one that does not exist, whatever its agenda
// would have said (a transient error included), and never asks it.
func TestEventIDToolsHideAccountsLikeMissing(t *testing.T) {
	norm := func(e *toolErr, id string) string { return strings.ReplaceAll(e.Message+"|"+e.Hint, id, "X") }
	argsOf := func(tool, id string) map[string]any {
		switch tool {
		case "event_get":
			return map[string]any{"event_id": id}
		case "event_update":
			return map[string]any{"event_id": id, "etag": `"e"`, "scope": "series", "changes": map[string]any{"summary": "x"}}
		}
		return map[string]any{"event_id": id, "etag": `"e"`, "scope": "series"}
	}
	for _, tool := range []string{"event_get", "event_update", "event_delete"} {
		for _, tc := range []struct {
			name  string
			setup func(*stubHub) (mcp.Grant, string)
		}{
			{"mcp off", func(h *stubHub) (mcp.Grant, string) {
				h.set("b2", func(a *service.AccountInfo) { a.MCP = false })
				return deleteGrant, eventID("b2", "cal-b", "lunch@x")
			}},
			{"signed out", func(h *stubHub) (mcp.Grant, string) {
				h.set("b2", func(a *service.AccountInfo) { a.Phase = "logged-out" })
				return deleteGrant, eventID("b2", "cal-b", "lunch@x")
			}},
			{"calendar grant, account without it", func(*stubHub) (mcp.Grant, string) {
				g := deleteGrant
				g.Calendars = []string{"cal-a"}
				return g, eventID("b2", "cal-a", "lunch@x")
			}},
			{"calendar grant, calendars unknown", func(h *stubHub) (mcp.Grant, string) {
				h.health["b2"] = service.Health{Serve: service.ServeStopped}
				g := deleteGrant
				g.Calendars = []string{"cal-b"}
				return g, eventID("b2", "cal-b", "lunch@x")
			}},
		} {
			hub := newHub()
			g, id := tc.setup(hub)
			hub.agendas["b2"].err = errWaiting
			hub.calErr = map[string]error{"b2": errWaiting}
			cs := newRig(t, hub).connect(t, g, mcp.EntryRemote)
			hidden := mustFail(t, cs, tool, argsOf(tool, id), "not-found")
			garbage := mustFail(t, cs, tool, argsOf(tool, "not-an-id"), "not-found")
			if norm(hidden, id) != norm(garbage, "not-an-id") {
				t.Errorf("%s, %s: hidden %+v, garbage %+v: an agent can tell them apart", tool, tc.name, *hidden, *garbage)
			}
			if n := len(hub.agendas["b2"].refs) + hub.agendas["b2"].asked(); n != 0 {
				t.Errorf("%s, %s: bob's agenda was asked %d times", tool, tc.name, n)
			}
		}
	}
	// event_create names the account as an unknown one.
	hub := newHub()
	hub.set("b2", func(a *service.AccountInfo) { a.MCP = false })
	cs := newRig(t, hub).connect(t, deleteGrant, mcp.EntryRemote)
	args := map[string]any{"calendar": "cal-b", "summary": "X", "start": "2026-11-20T15:00", "end": "2026-11-20T16:00"}
	hidden := notFoundText(t, cs, "event_create", with(args, "account", "bob@example.com"), "bob@example.com")
	unknown := notFoundText(t, cs, "event_create", with(args, "account", "carol@example.com"), "carol@example.com")
	if hidden != unknown {
		t.Errorf("hidden %q, unknown %q", hidden, unknown)
	}
	g := deleteGrant
	g.Calendars = []string{"cal-a"}
	cs = newRig(t, newHub()).connect(t, g, mcp.EntryRemote)
	if got := notFoundText(t, cs, "event_create", with(args, "account", "bob@example.com"), "bob@example.com"); got != unknown {
		t.Errorf("bob without a granted calendar %q, unknown %q", got, unknown)
	}
	if n := hub.agendas["b2"].asked(); n != 0 {
		t.Errorf("bob's agenda was asked %d writes", n)
	}
}

// New times are read as Task 9 reads them (tz, else the status zone) and
// written in the event's own zone unless tz names one: a series moved in
// its zone keeps its exceptions.
func TestUpdateTimesKeepTheEventZone(t *testing.T) {
	hub := newHub()
	alice := hub.agendas["a1"]
	d := alice.events["cal-a|standup@x"]
	d.Data.Start.TZID, d.Data.End.TZID = "Asia/Tokyo", "Asia/Tokyo"
	alice.events["cal-a|standup@x"] = d
	cs := newRig(t, hub).connect(t, writeGrant, mcp.EntryRemote)
	ny, _ := time.LoadLocation("America/New_York")
	for _, tc := range []struct {
		tz   string
		at   time.Time
		tzid string
	}{
		{"", time.Date(2026, 11, 16, 10, 0, 0, 0, paris), "Asia/Tokyo"},
		{"America/New_York", time.Date(2026, 11, 16, 10, 0, 0, 0, ny), "America/New_York"},
	} {
		args := map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series", "dry_run": true,
			"changes": map[string]any{"start": "2026-11-16T10:00"}}
		if tc.tz != "" {
			args["tz"] = tc.tz
		}
		mustCall(t, cs, "event_update", args)
		w := alice.writes()
		s := w.updates[len(w.updates)-1].f.Start
		if s == nil || !s.At.Equal(tc.at) || s.TZID != tc.tzid {
			t.Errorf("tz %q: start %+v, want %v in %s", tc.tz, s, tc.at, tc.tzid)
		}
	}
}
