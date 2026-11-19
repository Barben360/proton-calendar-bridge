package mcp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// What an agent that branches on class and hint observes of a bad call:
// the same {"error":{class,message,hint}} envelope as a handler's error,
// whoever refused the call.

func TestArgumentErrorsKeepTheToolErrorEnvelope(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	cs := r.connect(t, deleteGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
		want string // the message names it
	}{
		{"missing arguments", "event_create", map[string]any{"summary": "s"}, "account"},
		{"mistyped number", "events_list", with(monday(), "limit", "ten"), "limit"},
		{"a limit out of range", "events_list", with(monday(), "limit", 501), "limit"},
		{"a limit of zero or less", "events_search", with(monday(), "text", "x", "limit", -1), "limit"},
		{"scope outside the two", "event_update", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "all",
			"changes": map[string]any{"summary": "s"}}, "scope"},
		{"scope of a delete", "event_delete", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "everything"}, "scope"},
		{"a date that is no date", "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "s",
			"start_date": "tomorrow", "end_date": "2026-11-21"}, "start_date"},
		{"a changed date that is no date", "event_update", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "series",
			"changes": map[string]any{"end_date": "21/11/2026"}}, "end_date"},
		{"too many alarms", "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "s",
			"start": "2026-11-20T15:00", "end": "2026-11-20T16:00", "alarms": make([]int, 11)}, "alarms"},
		{"an alarm out of bounds", "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "s",
			"start": "2026-11-20T15:00", "end": "2026-11-20T16:00", "alarms": []int{20000000}}, "alarms"},
		{"an alarm after a day", "event_update", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "series",
			"changes": map[string]any{"alarms": []int{-5000}}}, "alarms"},
	} {
		terr := mustFail(t, cs, tc.tool, tc.args, "refused")
		if terr.Hint == "" || !strings.Contains(terr.Message, tc.want) {
			t.Errorf("%s: %+v, want a refused error with a hint whose message names %q", tc.name, *terr, tc.want)
		}
	}
	if n := alice.asked(); n != 0 {
		t.Errorf("%d write calls for arguments the schema refuses", n)
	}
	// The raw text of the result is the envelope, one object, nothing else.
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "event_create", Arguments: map[string]any{"summary": "s"}})
	if err != nil || !res.IsError || len(res.Content) != 1 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	var env map[string]map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(*sdkmcp.TextContent).Text), &env); err != nil || len(env) != 1 || env["error"]["class"] != "refused" {
		t.Errorf("text %q is not {\"error\":{...}} (%v)", res.Content[0].(*sdkmcp.TextContent).Text, err)
	}
	// The audit line says refused, as it does for the SDK's own refusals.
	var last map[string]any
	for _, l := range toolLines(t, r, "event_create") {
		last = l
	}
	if last == nil || last["outcome"] != "refused" {
		t.Errorf("audit line %v, want outcome refused", last)
	}
}

// What tools/list tells an agent: the allowed values and bounds, not prose.
func TestInputSchemasCarryTheirConstraints(t *testing.T) {
	cs := newRig(t, newHub()).connect(t, deleteGrant, mcp.EntryRemote)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	schemas := map[string]map[string]any{}
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool.InputSchema)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		schemas[tool.Name] = m
	}
	prop := func(tool string, path ...string) map[string]any {
		t.Helper()
		cur := schemas[tool]
		for _, p := range path {
			cur = obj(obj(cur["properties"])[p])
			if cur == nil {
				t.Fatalf("%s: no property %v", tool, path)
			}
		}
		return cur
	}
	for _, tool := range []string{"event_update", "event_delete"} {
		if got := fmt.Sprint(prop(tool, "scope")["enum"]); got != "[occurrence series]" {
			t.Errorf("%s scope enum %s", tool, got)
		}
	}
	for _, tool := range []string{"events_list", "events_search"} {
		l := prop(tool, "limit")
		if l["minimum"] != float64(0) || l["maximum"] != float64(500) {
			t.Errorf("%s limit %v, want 0 (the default) to 500", tool, l)
		}
	}
	for _, p := range [][]string{{"start_date"}, {"end_date"}} {
		if pat, _ := prop("event_create", p...)["pattern"].(string); pat == "" {
			t.Errorf("event_create %v has no pattern", p)
		}
		if pat, _ := prop("event_update", append([]string{"changes"}, p...)...)["pattern"].(string); pat == "" {
			t.Errorf("event_update changes.%v has no pattern", p)
		}
	}
	for _, a := range []map[string]any{prop("event_create", "alarms"), prop("event_update", "changes", "alarms")} {
		items := obj(a["items"])
		if a["maxItems"] != float64(10) || items["minimum"] == nil || items["maximum"] == nil {
			t.Errorf("alarms %v, want at most 10 minutes within bounds", a)
		}
	}
}

// A read with no account to read is a call that must be acted on, not an
// empty answer; the range is judged first.
func TestReadToolsWithoutAccountsSayToLogIn(t *testing.T) {
	hub := newHub()
	hub.remove("a1")
	hub.remove("b2")
	cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
	for _, tool := range []string{"calendars_list", "free_busy", "events_list", "events_search"} {
		terr := mustFail(t, cs, tool, argsFor(tool, monday()), "must-act")
		if !strings.Contains(terr.Message, "no account") || !strings.Contains(terr.Hint, "pcb login") {
			t.Errorf("%s: %+v, want no account and the way to log in", tool, *terr)
		}
	}
	if out := mustCall(t, cs, "status", nil); len(list(out["accounts"])) != 0 {
		t.Errorf("status %v: it still says what there is", out)
	}
	// 882 days, and a range that ends before it starts, are refused as such
	// whether or not an account exists.
	for _, args := range []map[string]any{
		{"from": "2026-01-01", "to": "2028-05-22"},
		{"from": "2026-11-17", "to": "2026-11-16"},
	} {
		for _, tool := range []string{"free_busy", "events_list"} {
			terr := mustFail(t, cs, tool, args, "refused")
			if terr.Hint == "" {
				t.Errorf("%s %v: no hint", tool, args)
			}
		}
	}
}

// Times without an offset are read as the codec reads a stored wall clock:
// a DST gap takes the offset before it, an overlap its first occurrence.
func TestWallClocksInDSTGapsAndOverlapsAreTheCodecs(t *testing.T) {
	hub := newHub()
	cs := newRig(t, hub).connect(t, writeGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]
	for _, tc := range []struct {
		name, tz, start, end string
		wantStart, wantEnd   string // UTC
	}{
		// 02:00 to 03:00 does not exist in New York on 2026-03-08: 02:30 is 03:30 EDT.
		{"gap", "America/New_York", "2026-03-08T02:30", "2026-03-08T04:30", "2026-03-08T07:30:00Z", "2026-03-08T08:30:00Z"},
		// 02:00 to 03:00 happens twice in Paris on 2026-10-25: the first is CEST.
		{"overlap", "Europe/Paris", "2026-10-25T02:30", "2026-10-25T03:30", "2026-10-25T00:30:00Z", "2026-10-25T02:30:00Z"},
	} {
		n := len(alice.writes().creates)
		mustCall(t, cs, "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": tc.name,
			"start": tc.start, "end": tc.end, "tz": tc.tz})
		f := alice.writes().creates[n].f
		if got := f.Start.At.UTC().Format(time.RFC3339); got != tc.wantStart {
			t.Errorf("%s: start stored as %s, want %s", tc.name, got, tc.wantStart)
		}
		if got := f.End.At.UTC().Format(time.RFC3339); got != tc.wantEnd {
			t.Errorf("%s: end stored as %s, want %s", tc.name, got, tc.wantEnd)
		}
	}
	// The same rule reads a range: events_list over the gap day asks the
	// agenda from 03:30 EDT, which is also what it echoes.
	out := mustCall(t, cs, "events_list", map[string]any{"from": "2026-03-08T02:30", "to": "2026-03-08T12:00", "tz": "America/New_York"})
	if out["from"] != "2026-03-08T03:30:00-04:00" {
		t.Errorf("from echoed as %v, want 03:30 EDT", out["from"])
	}
}

// The "before" of an occurrence's dry run is the occurrence the series
// makes: as long as the series' own, in elapsed time as Expand reads it,
// even across a DST change.
func TestDryRunBeforeOfAnOccurrenceKeepsTheElapsedDuration(t *testing.T) {
	hub := newHub()
	cs := newRig(t, hub).connect(t, writeGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]
	ny, _ := time.LoadLocation("America/New_York")
	// Saturdays 23:00 to Sunday 04:00: 5 hours elapsed in a week without a change.
	start := time.Date(2026, 2, 28, 23, 0, 0, 0, ny)
	master := alice.events["cal-a|standup@x"]
	master.Data.Start = service.EventTime{At: start, TZID: "America/New_York"}
	master.Data.End = service.EventTime{At: start.Add(5 * time.Hour), TZID: "America/New_York"}
	master.Data.RRule = "FREQ=WEEKLY"
	alice.events["cal-a|standup@x"] = master
	// 2026-03-08 02:00 is skipped in the night of the occurrence on 03-07.
	rid := time.Date(2026, 3, 7, 23, 0, 0, 0, ny)
	override := master.Data
	override.RRule = ""
	override.Summary = "Standup, moved"
	override.Start.At, override.End.At = rid, rid.Add(6*time.Hour)
	override.RecurrenceID = &service.EventTime{At: rid, TZID: "America/New_York"}
	after := master.Data
	after.Overrides = []service.EventData{override}
	alice.w.after = &after
	id := base64.RawURLEncoding.EncodeToString([]byte("a1\x00cal-a\x00standup@x\x00" + rid.UTC().Format(time.RFC3339)))

	out := mustCall(t, cs, "event_update", map[string]any{"event_id": id, "etag": `"standup@x-1"`, "scope": "occurrence", "dry_run": true,
		"tz": "America/New_York", "changes": map[string]any{"end": "2026-03-08T05:00"}})
	// Five elapsed hours after 23:00 EST (04:00Z) end at 09:00Z, 05:00 EDT.
	if got := field(out, "before.end.at"); got != "2026-03-08T05:00:00-04:00" {
		t.Errorf("before.end = %v, want 05:00 EDT (5 h elapsed, as Expand gives); out %v", got, out)
	}
	if got := field(out, "before.start.at"); got != nil {
		t.Errorf("before.start = %v: the start does not change", got)
	}
}

// free_busy answers for the range asked: what lies outside it is not shown
// as busy.
func TestFreeBusyIsClippedToTheRange(t *testing.T) {
	hub := newHub()
	a := hub.agendas["a1"]
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, paris) // a Monday, Paris
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	allDay := occ("cal-a", "allday@x", "Offsite", day, 24*time.Hour)
	allDay.Occurrence.AllDay = true
	timed := []service.AgendaOccurrence{
		occ("cal-a", "early@x", "Early", at(7, 0), 150*time.Minute), // 07:00 to 09:30
		occ("cal-a", "late@x", "Late", at(16, 30), 90*time.Minute),  // 16:30 to 18:00
		occ("cal-a", "mid@x", "Mid", at(12, 0), time.Hour),          // inside the range
	}
	cs := newRig(t, hub).connect(t, grant(mcp.CapFreeBusy), mcp.EntryStdio)
	busyOf := func() []string {
		out := mustCall(t, cs, "free_busy", map[string]any{"from": "2026-11-16T09:00", "to": "2026-11-16T17:00", "accounts": []string{"a1"}})
		var got []string
		for _, b := range list(field(out, "accounts.0.busy")) {
			got = append(got, fmt.Sprint(field(b, "start"), " ", field(b, "end")))
		}
		return got
	}
	// The all-day event is busy for the range, not from 00:00 to 24:00.
	a.occ = append([]service.AgendaOccurrence{allDay}, timed...)
	if got, want := busyOf(), []string{"2026-11-16T09:00:00+01:00 2026-11-16T17:00:00+01:00"}; !slices.Equal(got, want) {
		t.Errorf("with an all-day event: %v, want %v", got, want)
	}
	// Otherwise each interval is cut at the range's bounds.
	a.occ = timed
	want := []string{"2026-11-16T09:00:00+01:00 2026-11-16T09:30:00+01:00", "2026-11-16T12:00:00+01:00 2026-11-16T13:00:00+01:00", "2026-11-16T16:30:00+01:00 2026-11-16T17:00:00+01:00"}
	if got := busyOf(); !slices.Equal(got, want) {
		t.Errorf("busy %v, want %v", got, want)
	}
}

// Both time forms in one call are ambiguous, whichever fields are given:
// refused, never one form silently winning, and nothing is written.
func TestMixedTimeFormsAreRefusedAsAmbiguous(t *testing.T) {
	hub := newHub()
	cs := newRig(t, hub).connect(t, writeGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]
	base := map[string]any{"account": "a1", "calendar": "cal-a", "summary": "Z"}
	for name, args := range map[string]map[string]any{
		"both pairs":            with(base, "start", "2026-11-20T15:00", "end", "2026-11-20T16:00", "start_date", "2026-11-20", "end_date", "2026-11-21"),
		"start with the dates":  with(base, "start", "2026-11-20T15:00", "start_date", "2026-11-20", "end_date", "2026-11-21"),
		"end with the dates":    with(base, "end", "2026-11-20T16:00", "start_date", "2026-11-20", "end_date", "2026-11-21"),
		"the times with a date": with(base, "start", "2026-11-20T15:00", "end", "2026-11-20T16:00", "end_date", "2026-11-21"),
		"a time with a date":    with(base, "start", "2026-11-20T15:00", "end_date", "2026-11-21"),
	} {
		terr := mustFail(t, cs, "event_create", args, "refused")
		if !strings.Contains(terr.Message, "ambiguous") || terr.Hint == "" {
			t.Errorf("%s: %+v, want a refusal saying the call is ambiguous", name, *terr)
		}
	}
	// event_update takes the same rule.
	for name, changes := range map[string]map[string]any{
		"start and start_date": {"start": "2026-11-20T15:00", "start_date": "2026-11-20"},
		"end and end_date":     {"end": "2026-11-20T16:00", "end_date": "2026-11-21"},
	} {
		terr := mustFail(t, cs, "event_update", map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series", "changes": changes}, "refused")
		if !strings.Contains(terr.Message, "ambiguous") {
			t.Errorf("update %s: %+v, want ambiguous", name, *terr)
		}
	}
	if n := alice.asked(); n != 0 {
		t.Errorf("%d write calls for ambiguous arguments", n)
	}
}

// The schema refuses nothing the handlers accept.
func TestSchemasRefuseNothingTheHandlersAccept(t *testing.T) {
	hub := newHub()
	cs := newRig(t, hub).connect(t, writeGrant, mcp.EntryRemote)
	base := map[string]any{"account": "a1", "calendar": "cal-a", "summary": "Z"}
	// An agent sends "" for the optionals it does not use.
	mustCall(t, cs, "event_create", with(base, "start", "2026-11-20T15:00", "end", "2026-11-20T16:00",
		"start_date", "", "end_date", "", "tz", "", "location", "", "rrule", ""))
	mustCall(t, cs, "event_create", with(base, "start_date", "2026-11-20", "end_date", "2026-11-21", "start", "", "end", ""))
	// limit 0 is the default page.
	for _, tool := range []string{"events_list", "events_search"} {
		mustCall(t, cs, tool, argsFor(tool, with(monday(), "limit", 0)))
	}
	// The largest alarm the codec writes: 6999 days and 23:59 before an
	// all-day event; and the least: 23:59 after it.
	for _, m := range []int{service.MaxAlarmMinutes, -1439, 0} {
		mustCall(t, cs, "event_create", with(base, "start_date", "2026-11-20", "end_date", "2026-11-21", "alarms", []int{m}))
	}
	mustFail(t, cs, "event_create", with(base, "start_date", "2026-11-20", "end_date", "2026-11-21", "alarms", []int{service.MaxAlarmMinutes + 1}), "refused")
}
