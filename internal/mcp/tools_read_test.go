package mcp_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestStatusGivesNow(t *testing.T) {
	hub := newHub()
	hub.set("b2", func(a *service.AccountInfo) { a.Phase = "needs-login" })
	r := newRig(t, hub)
	for _, tc := range []struct {
		entry mcp.Entry
		step  string
	}{
		{mcp.EntryRemote, "run `pcb login` on the server, or open pcb-ui"},
		{mcp.EntryStdio, "pcb login"},
	} {
		out := mustCall(t, r.connect(t, readGrant, tc.entry), "status", nil)
		if field(out, "now.at") != "2026-11-14T11:00:00+01:00" || field(out, "now.zone") != "Europe/Paris" ||
			field(out, "now.offset") != "+01:00" || out["zone"] != "Europe/Paris" {
			t.Errorf("%s: now %v, zone %v; want 11:00 Paris (+01:00)", tc.entry, out["now"], out["zone"])
		}
		if got := accountsOf(out); !slices.Equal(got, []string{"alice@example.com", "bob@example.com"}) {
			t.Fatalf("%s: accounts %v", tc.entry, got)
		}
		alice, bob := obj(field(out, "accounts.0")), obj(field(out, "accounts.1"))
		if alice["phase"] != "ready" || alice["id"] != "a1" || alice["last_sync_age_s"] != float64(90) || len(list(alice["alerts"])) != 0 {
			t.Errorf("%s: alice %v; want ready, synced 90 s ago, no alert", tc.entry, alice)
		}
		if bob["phase"] != "needs-login" {
			t.Errorf("%s: bob %v, want needs-login", tc.entry, bob)
		}
		step := fmt.Sprint(field(bob, "alerts.0.step"))
		if field(bob, "alerts.0.code") != "needs-login" || !strings.Contains(step, tc.step) {
			t.Errorf("%s: bob's alert %v, want needs-login with %q", tc.entry, bob["alerts"], tc.step)
		}
		if tc.entry == mcp.EntryRemote && step != tc.step {
			t.Errorf("remote step %q, want exactly %q", step, tc.step)
		}
	}
	// A stopped loop and a calendar that fails to sync are reported.
	hub = newHub()
	hub.set("a1", func(a *service.AccountInfo) {
		a.Stopped = &service.StopInfo{Code: 20, Message: "the event cache is damaged", Hint: "run pcb doctor", At: fixedNow}
	})
	h := hub.health["a1"]
	h.Calendars = append(h.Calendars, service.CalendarHealth{ID: "cal-x", Name: "Broken", ErrorClass: "network", Error: "x", Gate: "open"})
	hub.health["a1"] = h
	out := mustCall(t, newRig(t, hub).connect(t, readGrant, mcp.EntryRemote), "status", nil)
	alice := obj(field(out, "accounts.0"))
	if alice["phase"] != "stopped" || field(alice, "alerts.0.message") != "the event cache is damaged" || field(alice, "alerts.0.step") != "run pcb doctor" {
		t.Errorf("stopped alice %v", alice)
	}
	if got := list(alice["stale_calendars"]); len(got) != 1 || got[0] != "cal-x" {
		t.Errorf("stale calendars %v, want [cal-x]", got)
	}
}

func TestEventsListGivesTimesIdsAndFlags(t *testing.T) {
	hub := newHub()
	a := hub.agendas["a1"]
	rid := service.EventTime{At: time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC).In(paris), TZID: "Europe/Paris"}
	a.occ[0].Ref.RecurrenceID = &rid
	a.occ[0].Occurrence.RecurrenceID = &rid.At
	a.occ = append(a.occ,
		service.AgendaOccurrence{Ref: service.EventRef{CalendarID: "cal-a", UID: "trip@x"}, ETag: `"t"`, Summary: "Trip", CalendarType: "holidays",
			Occurrence: service.Occurrence{Start: time.Date(2026, 11, 15, 23, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC),
				AllDay: true, StartDate: "2026-11-16", EndDate: "2026-11-17"}},
		service.AgendaOccurrence{Ref: service.EventRef{CalendarID: "cal-a", UID: "nap@x"}, ETag: `"n"`, Summary: "Nap", CalendarType: "normal", ExternalOrganizer: true,
			Occurrence: service.Occurrence{Start: time.Date(2026, 11, 16, 13, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC), Floating: true}},
	)
	cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
	out := mustCall(t, cs, "events_list", with(monday(), "accounts", []string{"alice@example.com"}))
	byUID := map[string]map[string]any{}
	for _, e := range list(out["events"]) {
		call(t, cs, "event_get", map[string]any{"event_id": obj(e)["event_id"]}) // the stub holds only the standup
		ref := a.refs[len(a.refs)-1]                                             // the ref the event ID carried
		byUID[ref.UID] = obj(e)
		if summary := field(e, "untrusted.summary"); summary != map[string]string{
			"standup@x": "Standup", "review@x": "Review", "trip@x": "Trip", "nap@x": "Nap"}[ref.UID] {
			t.Errorf("event %v carried the ref %+v", e, ref)
		}
	}
	if len(byUID) != 4 {
		t.Fatalf("events %v", out["events"])
	}
	standup := byUID["standup@x"]
	if field(standup, "start.at") != "2026-11-16T09:00:00+01:00" || field(standup, "start.tzid") != "Europe/Paris" || field(standup, "start.offset") != "+01:00" ||
		standup["recurring"] != true || standup["all_day"] != false || standup["etag"] != `"standup@x-1"` || standup["account"] != "alice@example.com" ||
		standup["calendar_id"] != "cal-a" || standup["third_party"] != false {
		t.Errorf("standup %v", standup)
	}
	trip := byUID["trip@x"]
	if field(trip, "start.date") != "2026-11-16" || field(trip, "end.date") != "2026-11-17" || field(trip, "start.at") != nil ||
		trip["all_day"] != true || trip["third_party"] != true || trip["calendar_type"] != "holidays" {
		t.Errorf("all-day trip %v: want dates only, end exclusive, flagged", trip)
	}
	nap := byUID["nap@x"]
	if field(nap, "start.floating") != true || field(nap, "start.at") != "2026-11-16T14:00:00+01:00" || field(nap, "start.tzid") != nil ||
		nap["external_organizer"] != true || nap["third_party"] != true {
		t.Errorf("floating nap %v: want the wall clock read in the zone, marked floating, flagged", nap)
	}
	// event_get got back the very ref the list holds, recurrence ID included.
	for _, ref := range a.refs {
		if ref.UID == "standup@x" && (ref.RecurrenceID == nil || !ref.RecurrenceID.At.Equal(rid.At)) {
			t.Errorf("event_get of the standup occurrence asked %+v", ref)
		}
	}
}

func TestEventIDOfAnotherAccountOutsideGrant(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	out := mustCall(t, r.connect(t, readGrant, mcp.EntryRemote), "events_list", with(monday(), "accounts", []string{"bob@example.com"}))
	bobID := fmt.Sprint(field(out, "events.0.event_id"))
	out = mustCall(t, r.connect(t, readGrant, mcp.EntryRemote), "events_list", with(monday(), "accounts", []string{"alice@example.com"}))
	aliceID := fmt.Sprint(field(out, "events.0.event_id"))

	g := readGrant
	g.Accounts = []string{"alice@example.com"}
	cs := r.connect(t, g, mcp.EntryRemote)
	mustCall(t, cs, "event_get", map[string]any{"event_id": aliceID})
	hidden := mustFail(t, cs, "event_get", map[string]any{"event_id": bobID}, "not-found")
	garbage := mustFail(t, cs, "event_get", map[string]any{"event_id": "not-an-id"}, "not-found")
	norm := func(e *toolErr, id string) string { return strings.ReplaceAll(e.Message+"|"+e.Hint, id, "X") }
	if norm(hidden, bobID) != norm(garbage, "not-an-id") {
		t.Errorf("outside the grant %+v, garbage %+v: an agent can tell them apart", *hidden, *garbage)
	}
	if n := len(hub.agendas["b2"].refs); n != 0 {
		t.Errorf("bob's agenda was asked for %d events", n)
	}
	// A calendar outside the grant hides its events the same way.
	g = readGrant
	g.Calendars = []string{"cal-b"}
	cs = r.connect(t, g, mcp.EntryRemote)
	hidden = mustFail(t, cs, "event_get", map[string]any{"event_id": aliceID}, "not-found")
	if norm(hidden, aliceID) != norm(garbage, "not-an-id") {
		t.Errorf("calendar outside the grant %+v, garbage %+v", *hidden, *garbage)
	}
	// A missing event of a visible account reads the same.
	mustCall(t, r.connect(t, readGrant, mcp.EntryRemote), "event_get", map[string]any{"event_id": aliceID})
	delete(hub.agendas["a1"].events, "cal-a|standup@x")
	gone := mustFail(t, r.connect(t, readGrant, mcp.EntryRemote), "event_get", map[string]any{"event_id": aliceID}, "not-found")
	if norm(gone, aliceID) != norm(garbage, "not-an-id") {
		t.Errorf("a missing event %+v, garbage %+v", *gone, *garbage)
	}
}

// markers are free text written by third parties: each may appear in a
// result only under an "untrusted" object.
var markers = []string{"Evil summary", "Evil location", "Evil description", "https://evil.example/x", "Evil Organizer", "Evil Attendee", "Evil Calendar", "Evil about"}

func untrustedFixture() *stubHub {
	hub := newHub()
	a := hub.agendas["a1"]
	a.occ[0].Summary, a.occ[0].Location, a.occ[0].Description = "Evil summary", "Evil location", "Evil description"
	d := a.events["cal-a|standup@x"]
	d.Data.Summary, d.Data.Location, d.Data.Description, d.Data.URL = "Evil summary", "Evil location", "Evil description", "https://evil.example/x"
	d.Data = withPeople(d.Data)
	d.Data.Overrides = []service.EventData{withPeople(service.EventData{Summary: "Evil summary", Description: "Evil description",
		Start: d.Data.Start, End: d.Data.End, RecurrenceID: &d.Data.Start})}
	a.events["cal-a|standup@x"] = d
	hub.cals["a1"][0].Name, hub.cals["a1"][0].Description = "Evil Calendar", "Evil about"
	return hub
}

func TestUntrustedFieldsAreGrouped(t *testing.T) {
	hub := untrustedFixture()
	cs := newRig(t, hub).connect(t, readGrant, mcp.EntryRemote)
	alice := with(monday(), "accounts", []string{"a1"})
	listed := mustCall(t, cs, "events_list", alice)
	id := fmt.Sprint(field(listed, "events.0.event_id"))
	results := map[string]map[string]any{
		"events_list":    listed,
		"events_search":  mustCall(t, cs, "events_search", with(alice, "text", "evil")),
		"event_get":      mustCall(t, cs, "event_get", map[string]any{"event_id": id}),
		"calendars_list": mustCall(t, cs, "calendars_list", nil),
	}
	found := map[string]bool{}
	for tool, out := range results {
		walkStrings(out, "", func(path, s string) {
			for _, m := range markers {
				if strings.Contains(s, m) {
					found[m] = true
					if !strings.Contains(path, ".untrusted.") && !strings.HasPrefix(path, "untrusted.") {
						t.Errorf("%s: %q at %s, outside untrusted", tool, m, path)
					}
				}
			}
		})
	}
	for _, m := range markers {
		if !found[m] {
			t.Errorf("%q never shown: the check would pass vacuously", m)
		}
	}
	got := results["event_get"]
	if field(got, "event.untrusted.summary") != "Evil summary" || field(got, "event.untrusted.url") != "https://evil.example/x" ||
		field(got, "event.untrusted.organizer.name") != "Evil Organizer" || field(got, "event.untrusted.attendees.0.name") != "Evil Attendee" ||
		field(got, "overrides.0.untrusted.summary") != "Evil summary" || field(got, "event.start.tzid") != "Europe/Paris" {
		t.Errorf("event_get %v", got)
	}
}

func withPeople(d service.EventData) service.EventData {
	d.Organizer = &service.EventPerson{Name: "Evil Organizer", Email: "boss@evil.example"}
	d.Attendees = []service.EventPerson{{Name: "Evil Attendee", Email: "a@evil.example", Role: "REQ-PARTICIPANT", Status: "ACCEPTED"}}
	d.Alarms = []service.EventAlarm{{MinutesBefore: 10, Action: "DISPLAY"}}
	return d
}

func walkStrings(v any, path string, f func(path, s string)) {
	switch x := v.(type) {
	case string:
		f(path, x)
	case map[string]any:
		for k, e := range x {
			walkStrings(e, strings.TrimPrefix(path+"."+k, "."), f)
		}
	case []any:
		for i, e := range x {
			walkStrings(e, fmt.Sprintf("%s.%d", path, i), f)
		}
	}
}

func TestDescriptionCutAt4KiBInLists(t *testing.T) {
	hub := newHub()
	long := strings.Repeat("é", 40*1024) // 80 KiB of two-byte runes
	a := hub.agendas["a1"]
	a.occ[0].Description = long
	a.occ[1].Description = "short"
	d := a.events["cal-a|standup@x"]
	d.Data.Description = long
	a.events["cal-a|standup@x"] = d
	cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
	alice := with(monday(), "accounts", []string{"a1"})
	for _, tool := range []string{"events_list", "events_search"} {
		out := mustCall(t, cs, tool, argsFor(tool, with(alice, "text", "é")))
		got := fmt.Sprint(field(out, "events.0.untrusted.description"))
		if len(got) > 4096 || len(got) < 4094 || !utf8.ValidString(got) || !strings.HasPrefix(long, got) ||
			field(out, "events.0.untrusted.description_truncated") != true {
			t.Errorf("%s: description of %d bytes (valid %t), truncated %v; want ≤ 4 KiB, a prefix, flagged",
				tool, len(got), utf8.ValidString(got), field(out, "events.0.untrusted.description_truncated"))
		}
	}
	out := mustCall(t, cs, "events_list", alice)
	if field(out, "events.1.untrusted.description") != "short" || field(out, "events.1.untrusted.description_truncated") == true {
		t.Errorf("short description %v", field(out, "events.1.untrusted"))
	}
	full := mustCall(t, cs, "event_get", map[string]any{"event_id": field(out, "events.0.event_id")})
	got := fmt.Sprint(field(full, "event.untrusted.description"))
	if len(got) > 65536 || len(got) < 65534 || !utf8.ValidString(got) || field(full, "event.untrusted.description_truncated") != true {
		t.Errorf("event_get: description of %d bytes, want ≤ 64 KiB and flagged", len(got))
	}
}

// Pages over several accounts: every occurrence once, each page sorted.
func TestEventsListMergesAccountsWithoutGaps(t *testing.T) {
	hub := newHub()
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	want := map[string]bool{}
	for _, id := range []string{"a1", "b2"} {
		a := hub.agendas[id]
		a.occ = nil
		cal := map[string]string{"a1": "cal-a", "b2": "cal-b"}[id]
		for i := range 17 {
			step := map[string]time.Duration{"a1": 50 * time.Minute, "b2": 70 * time.Minute}[id]
			uid := fmt.Sprintf("%s-%d@x", id, i)
			a.occ = append(a.occ, occ(cal, uid, uid, start.Add(time.Duration(i)*step), 10*time.Minute))
			want[uid] = true
		}
	}
	// Alice's pages are cut short (her render budget): Bob's later
	// occurrences must wait for hers.
	hub.agendas["a1"].budget = 2
	cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
	args := with(monday(), "limit", 5)
	seen := map[string]bool{}
	var last time.Time
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatal("no end after 20 pages")
		}
		out := mustCall(t, cs, "events_list", args)
		events := list(out["events"])
		if len(events) > 5 {
			t.Errorf("page %d: %d events, limit 5", page, len(events))
		}
		for _, e := range events {
			uid := fmt.Sprint(field(e, "untrusted.summary"))
			if seen[uid] {
				t.Errorf("page %d: %s repeated", page, uid)
			}
			seen[uid] = true
			at, _ := time.Parse(time.RFC3339, fmt.Sprint(field(e, "start.at")))
			if at.Before(last) {
				t.Errorf("page %d: %s at %s comes after %s", page, uid, at, last)
			}
			last = at
		}
		if out["truncated"] != true {
			if out["next_cursor"] != nil {
				t.Errorf("last page has a cursor %v", out["next_cursor"])
			}
			break
		}
		args = with(args, "cursor", out["next_cursor"])
	}
	for uid := range want {
		if !seen[uid] {
			t.Errorf("%s missing", uid)
		}
	}
	mustFail(t, cs, "events_list", with(monday(), "cursor", "bogus"), "refused")
	mustFail(t, cs, "events_list", with(monday(), "limit", 501), "refused")
}

func TestFreeBusyMergesAndReportsSkipped(t *testing.T) {
	hub := newHub()
	a := hub.agendas["a1"]
	base := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	a.occ = []service.AgendaOccurrence{
		occ("cal-a", "one@x", "One", base, time.Hour),
		occ("cal-a", "two@x", "Two", base.Add(30*time.Minute), time.Hour),
		occ("cal-a", "three@x", "Three", base.Add(3*time.Hour), time.Hour),
		occ("cal-a", "free@x", "Free", base.Add(5*time.Hour), time.Hour),
	}
	a.occ[3].Busy = false
	a.skipped = 2
	cs := newRig(t, hub).connect(t, grant(mcp.CapFreeBusy), mcp.EntryStdio)
	out := mustCall(t, cs, "free_busy", monday())
	if got := accountsOf(out); !slices.Equal(got, []string{"alice@example.com", "bob@example.com"}) {
		t.Fatalf("accounts %v", got)
	}
	alice := obj(field(out, "accounts.0"))
	busy := list(alice["busy"])
	if len(busy) != 2 || field(busy, "0.start") != "2026-11-16T09:00:00+01:00" || field(busy, "0.end") != "2026-11-16T10:30:00+01:00" ||
		field(busy, "1.start") != "2026-11-16T12:00:00+01:00" {
		t.Errorf("busy %v; want 09:00–10:30 and 12:00–13:00 Paris", busy)
	}
	if alice["skipped"] != float64(2) || !strings.Contains(fmt.Sprint(alice["note"]), "unknown") {
		t.Errorf("alice skipped %v, note %v; want 2 with a note that their times are unknown", alice["skipped"], alice["note"])
	}
	walkStrings(out, "", func(path, s string) {
		for _, m := range []string{"One", "Two", "Three"} {
			if s == m {
				t.Errorf("free_busy shows a title at %s", path)
			}
		}
	})
}

// An account whose agenda fails is listed as unavailable when others answer;
// alone, its error is the tool's.
var errWaiting = &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrWaitingForLogin, Hint: "log in"}

func TestOneAccountFailingDoesNotHideTheOthers(t *testing.T) {
	hub := newHub()
	hub.agendas["b2"].err = errWaiting
	cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
	out := mustCall(t, cs, "events_list", monday())
	if field(out, "unavailable.0.account") != "bob@example.com" || field(out, "unavailable.0.class") != "retry" || len(list(out["events"])) != 2 {
		t.Errorf("events_list %v; want alice's two events and bob unavailable", out)
	}
	mustFail(t, cs, "events_list", with(monday(), "accounts", []string{"b2"}), "retry")
}

// event_get judges the organizer as the list does (the service's view of
// the account's addresses, aliases included), never against the login
// address alone.
func TestEventGetAgreesWithTheListOnTheOrganizer(t *testing.T) {
	hub := newHub()
	a := hub.agendas["a1"]
	d := a.events["cal-a|standup@x"]
	d.Data.Organizer = &service.EventPerson{Name: "Alice", Email: "alias@example.com"}
	for _, external := range []bool{false, true} {
		a.occ[0].ExternalOrganizer = external
		d.ExternalOrganizer = external
		a.events["cal-a|standup@x"] = d
		cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
		listed := mustCall(t, cs, "events_list", with(monday(), "accounts", []string{"a1"}))
		got := mustCall(t, cs, "event_get", map[string]any{"event_id": field(listed, "events.0.event_id")})
		if field(listed, "events.0.external_organizer") != external || got["external_organizer"] != external ||
			field(listed, "events.0.third_party") != external || got["third_party"] != external {
			t.Errorf("external %t: list %v / %v, event_get %v / %v", external, field(listed, "events.0.external_organizer"),
				field(listed, "events.0.third_party"), got["external_organizer"], got["third_party"])
		}
	}
}
