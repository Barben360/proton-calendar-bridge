package service_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The agenda (M8 Task 6) runs on the write-test rig: one logged-in service
// during Serve, its loop paused between passes.

var _ service.Agenda = (*service.Local)(nil)

func at(unix int64) time.Time { return time.Unix(unix, 0).UTC() }

func ptr[T any](v T) *T { return &v }

// occurrences asks for one page and fails the test on an error.
func occurrences(t *testing.T, a service.Agenda, req service.OccurrencesRequest) service.OccurrencePage {
	t.Helper()
	p, err := a.Occurrences(t.Context(), req)
	if err != nil {
		t.Fatalf("Occurrences(%+v): %v", req, err)
	}
	return p
}

// itemKey names an occurrence: calendar, UID and start.
func itemKey(o service.AgendaOccurrence) string {
	return fmt.Sprintf("%s|%s|%d", o.Ref.CalendarID, o.Ref.UID, o.Occurrence.Start.Unix())
}

func summaries(items []service.AgendaOccurrence) []string {
	out := make([]string, len(items))
	for i, o := range items {
		out[i] = o.Summary
	}
	return out
}

// eventCard is a VEVENT body from its lines, with a UID and a DTSTAMP.
func eventCard(uid string, lines ...string) string {
	return strings.Join(append([]string{"UID:" + uid, "DTSTAMP:20270115T080000Z"}, lines...), "\r\n")
}

// Two calendars, a weekly series and a single event: the occurrences of
// both, sorted by start, each with its calendar, UID, ETag and (for the
// series) its recurrence ID.
func TestOccurrencesExpandsAndMerges(t *testing.T) {
	t.Parallel()
	var home string
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		home = f.AddCalendar(f.Username, protontest.Calendar{Name: "Home"})
		f.AddEvent(cal, protontest.Event{UID: "w@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY",
			Card: card("w@x", "Weekly", base, "RRULE:FREQ=WEEKLY", "LOCATION:Room 1")})
		f.AddEvent(home, protontest.Event{UID: "s@x", Start: base + 2*day, End: base + 2*day + 3600,
			Card: card("s@x", "Single", base+2*day)})
		f.AddEvent(home, protontest.Event{UID: "late@x", Start: base + 40*day, End: base + 40*day + 3600,
			Card: card("late@x", "Out of range", base+40*day)})
	}).start(t)
	etags, _ := r.listing(t, r.cal)
	homeTags, _ := r.listing(t, home)

	p := occurrences(t, r.svc, service.OccurrencesRequest{From: at(base - day), To: at(base + 30*day)})
	want := []string{"Weekly", "Single", "Weekly", "Weekly", "Weekly", "Weekly"}
	if got := summaries(p.Items); !slices.Equal(got, want) {
		t.Fatalf("summaries %v, want %v", got, want)
	}
	if p.Truncated || p.Next != "" || p.Skipped != 0 {
		t.Errorf("page truncated %t, next %q, skipped %d; want a whole page", p.Truncated, p.Next, p.Skipped)
	}
	weekly := 0
	for _, o := range p.Items {
		switch o.Ref.UID {
		case "w@x":
			start := at(base + int64(weekly)*7*day)
			weekly++
			if o.Ref.CalendarID != r.cal || o.ETag != etags["w@x"] || !o.Occurrence.Start.Equal(start) || o.Location != "Room 1" {
				t.Errorf("weekly occurrence %+v, want %s at %s with ETag %s", o, r.cal, start, etags["w@x"])
			}
			if rid := o.Ref.RecurrenceID; rid == nil || !rid.At.Equal(start) || rid.Date != "" || rid.Floating {
				t.Errorf("weekly occurrence at %s: recurrence ID %+v", start, rid)
			}
			if !o.Busy || o.CalendarType != "normal" {
				t.Errorf("weekly occurrence: busy %t, calendar type %q", o.Busy, o.CalendarType)
			}
		case "s@x":
			if o.Ref.CalendarID != home || o.ETag != homeTags["s@x"] || o.Ref.RecurrenceID != nil || !o.Occurrence.Start.Equal(at(base+2*day)) {
				t.Errorf("single event %+v, want %s with ETag %s and no recurrence ID", o, home, homeTags["s@x"])
			}
		}
	}

	// One calendar chosen: only its occurrences.
	p = occurrences(t, r.svc, service.OccurrencesRequest{Calendars: []string{home}, From: at(base - day), To: at(base + 30*day)})
	if got := summaries(p.Items); !slices.Equal(got, []string{"Single"}) {
		t.Errorf("calendar %s alone: %v", home, got)
	}
	_, err := r.svc.Occurrences(t.Context(), service.OccurrencesRequest{Calendars: []string{"nope"}, From: at(base), To: at(base + day)})
	wantIs(t, err, service.ErrNoSuchCalendar)
}

// Review focus 4: 766 occurrences (two daily series over 366 days, in two
// calendars, starting at the same instant, and 34 single events), limit
// 500: page 1 has 500 and a cursor, page 2 the other 266 and none; no
// repeat, no gap, and the order holds across the pages.
func TestOccurrencesPagesWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	var home string
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		home = f.AddCalendar(f.Username, protontest.Calendar{Name: "Home"})
		f.AddEvent(cal, protontest.Event{UID: "d1@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY",
			Card: card("d1@x", "Daily one", base, "RRULE:FREQ=DAILY")})
		f.AddEvent(home, protontest.Event{UID: "d2@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY",
			Card: card("d2@x", "Daily two", base, "RRULE:FREQ=DAILY")})
		for k := range int64(34) {
			uid := fmt.Sprintf("one%d@x", k)
			start := base + k*day + 1800
			f.AddEvent(cal, protontest.Event{UID: uid, Start: start, End: start + 600, Card: card(uid, "One-off", start)})
		}
	}).start(t)
	want := map[string]bool{}
	for k := range int64(366) {
		want[fmt.Sprintf("%s|d1@x|%d", r.cal, base+k*day)] = true
		want[fmt.Sprintf("%s|d2@x|%d", home, base+k*day)] = true
	}
	for k := range int64(34) {
		want[fmt.Sprintf("%s|one%d@x|%d", r.cal, k, base+k*day+1800)] = true
	}

	req := service.OccurrencesRequest{From: at(base), To: at(base + 366*day), Limit: 500}
	began := time.Now()
	p1 := occurrences(t, r.svc, req)
	if len(p1.Items) != 500 || !p1.Truncated || p1.Next == "" {
		t.Fatalf("page 1: %d items, truncated %t, next %q; want 500, truncated, a cursor", len(p1.Items), p1.Truncated, p1.Next)
	}
	req.Cursor = p1.Next
	p2 := occurrences(t, r.svc, req)
	if len(p2.Items) != 266 || p2.Truncated || p2.Next != "" {
		t.Fatalf("page 2: %d items, truncated %t, next %q; want 266, the end", len(p2.Items), p2.Truncated, p2.Next)
	}
	if d := time.Since(began); d > 20*time.Second {
		t.Errorf("two pages took %s", d)
	}
	all := append(slices.Clone(p1.Items), p2.Items...)
	seen := map[string]bool{}
	for i, o := range all {
		k := itemKey(o)
		if seen[k] {
			t.Errorf("repeated: %s", k)
		}
		seen[k] = true
		if !want[k] {
			t.Errorf("unexpected occurrence %s", k)
		}
		if i > 0 && o.Occurrence.Start.Before(all[i-1].Occurrence.Start) {
			t.Errorf("item %d (%s) starts before item %d", i, k, i-1)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("missing: %s", k)
		}
	}
}

// The render budget cuts a page early; the cursor continues after the
// objects it could not render, without gaps or repeats.
func TestOccurrencesRenderBudgetPages(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		for k := range int64(7) {
			uid := fmt.Sprintf("e%d@x", k)
			start := base + k*day
			f.AddEvent(cal, protontest.Event{UID: uid, Start: start, End: start + 3600, Card: card(uid, uid, start)})
		}
	}).start(t)
	r.svc.SetRenderBudget(3)
	req := service.OccurrencesRequest{From: at(base - day), To: at(base + 10*day), Limit: 100}
	var got []string
	for range 10 {
		p := occurrences(t, r.svc, req)
		got = append(got, summaries(p.Items)...)
		if !p.Truncated {
			break
		}
		if p.Next == "" || len(p.Items) == 0 {
			t.Fatalf("a truncated page with %d items and cursor %q", len(p.Items), p.Next)
		}
		req.Cursor = p.Next
	}
	want := []string{"e0@x", "e1@x", "e2@x", "e3@x", "e4@x", "e5@x", "e6@x"}
	if !slices.Equal(got, want) {
		t.Errorf("paged with a budget of 3 objects: %v, want %v", got, want)
	}
}

// The text matches summary, location or description, case-insensitively,
// per occurrence: an overridden occurrence matches on its own text.
func TestOccurrencesTextFilter(t *testing.T) {
	t.Parallel()
	rid := base + 7*day
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Card: card("a@x", "Dentist", base)})
		f.AddEvent(cal, protontest.Event{UID: "b@x", Start: base + 3600, End: base + 7200,
			Card: card("b@x", "Errand", base+3600, "LOCATION:Main Street")})
		f.AddEvent(cal, protontest.Event{UID: "c@x", Start: base + 7200, End: base + 10800,
			Card: card("c@x", "Call", base+7200, "DESCRIPTION:ask about the DENTIST bill")})
		f.AddEvent(cal, protontest.Event{UID: "s@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY",
			Card: card("s@x", "Standup", base, "RRULE:FREQ=WEEKLY")})
		f.AddEvent(cal, protontest.Event{UID: "s@x", Start: rid, End: rid + 3600, RecurrenceID: &rid,
			Card: card("s@x", "Dentist instead", rid, "RECURRENCE-ID:"+stamp(rid))})
	}).start(t)
	req := service.OccurrencesRequest{From: at(base - day), To: at(base + 20*day), Text: "dEnTiSt"}
	if got := summaries(occurrences(t, r.svc, req).Items); !slices.Equal(got, []string{"Dentist", "Call", "Dentist instead"}) {
		t.Errorf("text dentist: %v", got)
	}
	req.Text = "main street"
	if got := summaries(occurrences(t, r.svc, req).Items); !slices.Equal(got, []string{"Errand"}) {
		t.Errorf("text main street: %v", got)
	}
	req.Text = "standup"
	if got := summaries(occurrences(t, r.svc, req).Items); !slices.Equal(got, []string{"Standup", "Standup"}) {
		t.Errorf("text standup: %v (the overridden occurrence has its own summary)", got)
	}
}

func TestOccurrencesRefusesTooLongRange(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil).start(t)
	for name, req := range map[string]service.OccurrencesRequest{
		"367 days":    {From: at(base), To: at(base + 367*day)},
		"limit 501":   {From: at(base), To: at(base + day), Limit: 501},
		"limit -1":    {From: at(base), To: at(base + day), Limit: -1},
		"to <= from":  {From: at(base), To: at(base)},
		"bad cursor":  {From: at(base), To: at(base + day), Cursor: "not a cursor"},
		"zero bounds": {},
	} {
		_, err := r.svc.Occurrences(t.Context(), req)
		_ = wantExit(t, err, exitcode.Usage)
		if _, err := r.svc.FreeBusy(t.Context(), req); name == "367 days" && err == nil {
			t.Errorf("FreeBusy over %s: no error", name)
		}
	}
	// 366 days is allowed.
	_ = occurrences(t, r.svc, service.OccurrencesRequest{From: at(base), To: at(base + 366*day)})
}

// Rulings R10: an occurrence's ID keeps the series' DTSTART form, so it
// stays the same whatever zone a later call reads in: a date for an
// all-day series, the wall clock for a floating one, the zoned instant
// otherwise.
func TestOccurrenceIDsKeepTheSeriesForm(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "ad@x", Start: base - 8*3600, End: base - 8*3600 + day, FullDay: true, RRule: "FREQ=WEEKLY",
			Card: eventCard("ad@x", "DTSTART;VALUE=DATE:20270115", "DTEND;VALUE=DATE:20270116", "SUMMARY:All day", "RRULE:FREQ=WEEKLY")})
		f.AddEvent(cal, protontest.Event{UID: "fl@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY",
			Card: eventCard("fl@x", "DTSTART:20270115T080000", "DTEND:20270115T090000", "SUMMARY:Floating", "RRULE:FREQ=WEEKLY")})
		f.AddEvent(cal, protontest.Event{UID: "tz@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY", Color: green,
			Card: eventCard("tz@x", "DTSTART;TZID=Europe/Paris:20270115T090000", "DTEND;TZID=Europe/Paris:20270115T100000",
				"SUMMARY:Zoned", "RRULE:FREQ=WEEKLY")})
	}).start(t)
	ids := func(zone string) map[string]service.EventTime {
		t.Helper()
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		p := occurrences(t, r.svc, service.OccurrencesRequest{From: at(base + 6*day), To: at(base + 9*day), Loc: loc})
		out := map[string]service.EventTime{}
		for _, o := range p.Items {
			if o.Ref.RecurrenceID == nil {
				t.Fatalf("%s: no recurrence ID", o.Ref.UID)
			}
			out[o.Ref.UID] = *o.Ref.RecurrenceID
		}
		return out
	}
	east, west := ids("Pacific/Kiritimati"), ids("America/Los_Angeles")
	if got := east["ad@x"]; got.Date != "2027-01-22" || !got.At.IsZero() {
		t.Errorf("all-day ID %+v, want the date 2027-01-22", got)
	}
	if got := east["fl@x"]; !got.Floating || !got.At.Equal(time.Date(2027, 1, 22, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("floating ID %+v, want the wall clock 2027-01-22 08:00", got)
	}
	if got := east["tz@x"]; got.TZID != "Europe/Paris" || !got.At.Equal(at(base+7*day)) {
		t.Errorf("zoned ID %+v, want 2027-01-22 09:00 Europe/Paris", got)
	}
	for uid, e := range east {
		w := west[uid]
		if e.Date != w.Date || !e.At.Equal(w.At) || e.Floating != w.Floating || e.TZID != w.TZID {
			t.Errorf("%s: ID %+v read from Kiritimati, %+v from Los Angeles", uid, e, w)
		}
	}

	// The ID is enough to delete that occurrence.
	obj := r.object(t, "tz@x")
	rid := east["tz@x"]
	if err := r.svc.DeleteEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "tz@x", RecurrenceID: &rid}, obj.ETag, service.ScopeOccurrence); err != nil {
		t.Fatalf("DeleteEvent(occurrence): %v", err)
	}
	contains(t, "the series", r.object(t, "tz@x").Data, "EXDATE;TZID=Europe/Paris:20270122T090000")
}

// Rulings R9: an event that cannot be decoded or expanded never fails the
// page: it is skipped, counted and logged with its calendar and UID only;
// Event names why it cannot be read.
func TestOccurrencesSkipsUnreadableEvents(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "ok@x", Start: base, End: base + 3600, Card: card("ok@x", "Fine", base)})
		f.AddEvent(cal, protontest.Event{UID: "tz@x", Start: base, End: base + 3600,
			Card: eventCard("tz@x", "DTSTART;TZID=Mars/Olympus:20270115T080000", "DTEND;TZID=Mars/Olympus:20270115T090000", "SUMMARY:Secret Mars")})
		f.AddEvent(cal, protontest.Event{UID: "hr@x", Start: base, End: base + 3600, RRule: "FREQ=HOURLY",
			Card: card("hr@x", "Secret hourly", base, "RRULE:FREQ=HOURLY")})
	}).start(t)
	p := occurrences(t, r.svc, service.OccurrencesRequest{From: at(base - day), To: at(base + day)})
	if got := summaries(p.Items); !slices.Equal(got, []string{"Fine"}) || p.Skipped != 2 {
		t.Errorf("items %v, skipped %d; want [Fine] and 2 skipped", got, p.Skipped)
	}
	logs := r.logs.String()
	for _, uid := range []string{"tz@x", "hr@x"} {
		if !strings.Contains(logs, "uid="+uid) || !strings.Contains(logs, r.cal) {
			t.Errorf("no warning naming %s in calendar %s:\n%s", uid, r.cal, logs)
		}
	}
	if strings.Contains(logs, "Secret") {
		t.Errorf("the log quotes event content:\n%s", logs)
	}
	fb, err := r.svc.FreeBusy(t.Context(), service.OccurrencesRequest{From: at(base - day), To: at(base + day)})
	if err != nil || len(fb) != 1 {
		t.Errorf("FreeBusy = %d, %v; want the one readable event", len(fb), err)
	}
	for uid, code := range map[string]string{"tz@x": "pcb:unknown-tzid", "hr@x": "pcb:unsupported-recurrence"} {
		_, err := r.svc.Event(t.Context(), service.EventRef{CalendarID: r.cal, UID: uid})
		if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), code) {
			t.Errorf("Event(%s) = %q, want it to name %s", uid, ee.Error(), code)
		}
	}
}

func TestEventDetailHasEveryField(t *testing.T) {
	t.Parallel()
	var hol string
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		hol = f.AddCalendar(f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
		f.AddEvent(cal, protontest.Event{UID: "full@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY", Color: green,
			Card: card("full@x", "Review", base, "RRULE:FREQ=WEEKLY", "EXDATE:"+stamp(base+7*day), "DESCRIPTION:Quarterly numbers",
				"LOCATION:Room 4", "URL:https://example.org/r", "STATUS:CONFIRMED", "TRANSP:TRANSPARENT"),
			Notifications: []protontest.Notification{{Type: 1, Trigger: "-PT15M"}}})
		f.AddEvent(hol, protontest.Event{UID: "h@x", Start: base, End: base + day, Card: card("h@x", "Holiday", base)})
	}).start(t)
	obj := r.object(t, "full@x")
	ref := service.EventRef{CalendarID: r.cal, UID: "full@x"}
	d, err := r.svc.Event(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if d.Ref != ref || d.ETag != obj.ETag || d.CalendarType != "normal" || d.ReadOnly {
		t.Errorf("detail ref %+v, etag %q (want %q), type %q, read only %t", d.Ref, d.ETag, obj.ETag, d.CalendarType, d.ReadOnly)
	}
	e := d.Data
	if e.UID != "full@x" || e.Summary != "Review" || e.Description != "Quarterly numbers" || e.Location != "Room 4" ||
		e.URL != "https://example.org/r" || e.Status != "CONFIRMED" || e.Transp != "TRANSPARENT" || e.RRule != "FREQ=WEEKLY" ||
		!e.Start.At.Equal(at(base)) || !e.End.At.Equal(at(base+3600)) {
		t.Errorf("event data %+v", e)
	}
	if len(e.ExDates) != 1 || !e.ExDates[0].At.Equal(at(base+7*day)) {
		t.Errorf("exdates %+v", e.ExDates)
	}
	if len(e.Alarms) != 1 || e.Alarms[0].MinutesBefore != 15 {
		t.Errorf("alarms %+v", e.Alarms)
	}
	h, err := r.svc.Event(t.Context(), service.EventRef{CalendarID: hol, UID: "h@x"})
	if err != nil || h.CalendarType != "holidays" || !h.ReadOnly || h.Data.Summary != "Holiday" {
		t.Errorf("holiday detail %+v, %v; want a read-only holidays event", h, err)
	}
	_, err = r.svc.Event(t.Context(), service.EventRef{CalendarID: r.cal, UID: "gone@x"})
	wantIs(t, err, service.ErrNoSuchObject)
}

// Organizer and busy flags: an event organized by another address is
// flagged, ours is not.
func TestOccurrencesFlagExternalOrganizer(t *testing.T) {
	t.Parallel()
	var me string
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "plain@x", Start: base, End: base + 3600, Card: card("plain@x", "Plain", base)})
	})
	r.signIn(t)
	me = r.loadState(t).Address
	if me == "" {
		t.Fatal("no account address")
	}
	r.f.AddEvent(r.cal, protontest.Event{UID: "mine@x", Start: base + 3600, End: base + 7200,
		Card: card("mine@x", "Mine", base+3600, "ORGANIZER;CN=Me:mailto:"+strings.ToUpper(me))})
	r.f.AddEvent(r.cal, protontest.Event{UID: "theirs@x", Start: base + 7200, End: base + 10800,
		Card: card("theirs@x", "Theirs", base+7200, "ORGANIZER;CN=Them:mailto:them@example.org")})
	r.sync(t, false)
	got := map[string]bool{}
	for _, o := range occurrences(t, r.svc, service.OccurrencesRequest{From: at(base - day), To: at(base + day)}).Items {
		got[o.Summary] = o.ExternalOrganizer
	}
	if want := map[string]bool{"Plain": false, "Mine": false, "Theirs": true}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("external organizer %v, want %v", got, want)
	}
}

// Free/busy keeps the busy occurrences only: neither transparent nor
// cancelled, an overridden occurrence judged on its own.
func TestFreeBusySkipsTransparentAndCancelled(t *testing.T) {
	t.Parallel()
	rid := base + 7*day
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "busy@x", Start: base, End: base + 3600, Card: card("busy@x", "Busy", base, "TRANSP:OPAQUE")})
		f.AddEvent(cal, protontest.Event{UID: "free@x", Start: base + 3600, End: base + 7200, Card: card("free@x", "Free", base+3600, "TRANSP:TRANSPARENT")})
		f.AddEvent(cal, protontest.Event{UID: "off@x", Start: base + 7200, End: base + 10800, Card: card("off@x", "Off", base+7200, "STATUS:CANCELLED")})
		f.AddEvent(cal, protontest.Event{UID: "s@x", Start: base + day, End: base + day + 3600, RRule: "FREQ=WEEKLY",
			Card: card("s@x", "Series", base+day, "RRULE:FREQ=WEEKLY")})
		f.AddEvent(cal, protontest.Event{UID: "s@x", Start: rid + day, End: rid + day + 3600, RecurrenceID: ptr(rid + day),
			Card: card("s@x", "Series off", rid+day, "RECURRENCE-ID:"+stamp(rid+day), "STATUS:CANCELLED")})
	}).start(t)
	req := service.OccurrencesRequest{From: at(base - day), To: at(base + 20*day), Text: "ignored"}
	fb, err := r.svc.FreeBusy(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	var starts []time.Time
	for _, o := range fb {
		starts = append(starts, o.Start)
	}
	want := []time.Time{at(base), at(base + day), at(base + 15*day)}
	if !slices.EqualFunc(starts, want, time.Time.Equal) {
		t.Errorf("busy starts %v, want %v", starts, want)
	}
	req.Text = ""
	busy := map[string]bool{}
	for _, o := range occurrences(t, r.svc, req).Items {
		busy[o.Summary] = o.Busy
	}
	if want := map[string]bool{"Busy": true, "Free": false, "Off": false, "Series": true, "Series off": false}; fmt.Sprint(busy) != fmt.Sprint(want) {
		t.Errorf("busy flags %v, want %v", busy, want)
	}
}

func TestCreateEventLandsInProton(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil).start(t)
	paris, _ := time.LoadLocation("Europe/Paris")
	f := service.EventFields{
		Summary:  ptr("Planned by an agent"),
		Location: ptr("Room 2"),
		Start:    &service.EventTime{At: time.Date(2027, 1, 20, 14, 0, 0, 0, paris), TZID: "Europe/Paris"},
		End:      &service.EventTime{At: time.Date(2027, 1, 20, 15, 0, 0, 0, paris), TZID: "Europe/Paris"},
		Alarms:   &[]int{10},
	}
	ref, etag, err := r.svc.CreateEvent(t.Context(), r.cal, f)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if ref.CalendarID != r.cal || ref.UID == "" || ref.RecurrenceID != nil || etag == "" {
		t.Fatalf("CreateEvent = %+v, %q", ref, etag)
	}
	if rows := r.proton(t, ref.UID); len(rows) != 1 {
		t.Fatalf("Proton holds %d rows of %s", len(rows), ref.UID)
	}
	d, err := r.svc.Event(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if d.ETag != etag || d.Data.Summary != "Planned by an agent" || d.Data.Location != "Room 2" || d.Data.Start.TZID != "Europe/Paris" ||
		!d.Data.Start.At.Equal(f.Start.At) || !d.Data.End.At.Equal(f.End.At) {
		t.Errorf("read back %+v (etag %q, created %q)", d.Data, d.ETag, etag)
	}
	// A second create gets its own UID.
	ref2, _, err := r.svc.CreateEvent(t.Context(), r.cal, f)
	if err != nil || ref2.UID == ref.UID {
		t.Errorf("second create = %+v, %v; want another UID than %s", ref2, err, ref.UID)
	}
	// A refused create is a MustAct naming the reason; nothing is sent.
	calls := len(r.f.SyncCalls())
	_, _, err = r.svc.CreateEvent(t.Context(), r.cal, service.EventFields{Summary: ptr("No times")})
	_ = wantExit(t, err, exitcode.MustAct)
	if len(r.f.SyncCalls()) != calls {
		t.Error("a refused create was sent")
	}
}

// Review focus 3: a stale ETag is a conflict and nothing is written,
// whether the cache already shows the newer version (an edit through
// CalDAV) or only Proton does (an edit in the web app since the last pass).
func TestUpdateEventStaleETagIsConflict(t *testing.T) {
	t.Parallel()
	seed := func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}
	change := service.EventFields{Summary: ptr("By the agent")}
	t.Run("thunderbird", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, protontest.Options{}, seed).start(t)
		old := r.object(t, "a@x")
		if _, err := r.put(t, "a@x.ics", edit(t, old.Data, "SUMMARY:Lunch", "SUMMARY:By Thunderbird"), service.Precondition{IfMatch: old.ETag}); err != nil {
			t.Fatal(err)
		}
		before := raws(r.proton(t, "a@x"))
		calls := len(r.f.SyncCalls())
		_, err := r.svc.UpdateEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "a@x"}, old.ETag, service.ScopeSeries, change, false)
		wantIs(t, err, service.ErrPreconditionFailed)
		if len(r.f.SyncCalls()) != calls || !slices.Equal(raws(r.proton(t, "a@x")), before) {
			t.Error("the stale update wrote to Proton")
		}
	})
	t.Run("web app", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, protontest.Options{}, seed).start(t)
		old := r.object(t, "a@x")
		r.webPut(t, "a@x", edit(t, old.Data, "SUMMARY:Lunch", "SUMMARY:In the web app"))
		before := raws(r.proton(t, "a@x"))
		calls := r.sendCalls()
		_, err := r.svc.UpdateEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "a@x"}, old.ETag, service.ScopeSeries, change, false)
		wantIs(t, err, service.ErrPreconditionFailed)
		if r.sendCalls() != calls || !slices.Equal(raws(r.proton(t, "a@x")), before) {
			t.Error("the stale update wrote to Proton")
		}
	})
}

// A dry run returns the event before and after and writes nothing; the
// same update for real lands in Proton with a new ETag.
func TestUpdateEventDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}).start(t)
	obj := r.object(t, "a@x")
	ref := service.EventRef{CalendarID: r.cal, UID: "a@x"}
	change := service.EventFields{Summary: ptr("Dinner")}
	before := raws(r.proton(t, "a@x"))
	res, err := r.svc.UpdateEvent(t.Context(), ref, obj.ETag, service.ScopeSeries, change, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Before.Summary != "Lunch" || res.After.Summary != "Dinner" || res.ETag != obj.ETag {
		t.Errorf("dry run = before %q, after %q, etag %q (want %q)", res.Before.Summary, res.After.Summary, res.ETag, obj.ETag)
	}
	if len(r.f.SyncCalls()) != 0 || !slices.Equal(raws(r.proton(t, "a@x")), before) {
		t.Fatal("the dry run wrote to Proton")
	}

	res, err = r.svc.UpdateEvent(t.Context(), ref, obj.ETag, service.ScopeSeries, change, false)
	if err != nil {
		t.Fatal(err)
	}
	now := r.object(t, "a@x")
	if res.ETag == obj.ETag || res.ETag != now.ETag || res.Before.Summary != "" {
		t.Errorf("update = %+v; ETag before %q, now %q", res, obj.ETag, now.ETag)
	}
	contains(t, "the object read back", now.Data, "SUMMARY:Dinner")
	if slices.Equal(raws(r.proton(t, "a@x")), before) {
		t.Error("Proton's row did not change")
	}
	if d, err := r.svc.Event(t.Context(), ref); err != nil || d.Data.Summary != "Dinner" {
		t.Errorf("Event after the update: %q, %v", d.Data.Summary, err)
	}
}

// A series edit that would orphan an exception is refused (MustAct,
// naming pcb:series-has-exceptions); so is a write the write path refuses
// (a *WriteRefusal); nothing is sent.
func TestUpdateSeriesWithExceptionsIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "s@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY", Color: green,
			Card: card("s@x", "Series", base, "RRULE:FREQ=WEEKLY", "EXDATE:"+stamp(base+7*day))})
		f.AddEvent(cal, protontest.Event{UID: "p@x", Start: base, End: base + 3600, Color: green, PersonalAlarms: true,
			Card: card("p@x", "Personal", base, "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "DESCRIPTION:x", "END:VALARM")})
	}).start(t)
	obj := r.object(t, "s@x")
	_, err := r.svc.UpdateEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "s@x"}, obj.ETag, service.ScopeSeries,
		service.EventFields{RRule: ptr("FREQ=DAILY")}, false)
	ee := wantExit(t, err, exitcode.MustAct)
	var wr *service.WriteRefusal
	if !strings.Contains(ee.Error(), "pcb:series-has-exceptions") || !errors.As(err, &wr) || wr.Code != "pcb:series-has-exceptions" {
		t.Errorf("error %q (%+v), want a refusal naming pcb:series-has-exceptions", ee.Error(), wr)
	}
	// The dry run shows the same refusal.
	_, err = r.svc.UpdateEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "s@x"}, obj.ETag, service.ScopeSeries,
		service.EventFields{RRule: ptr("FREQ=DAILY")}, true)
	_ = wantExit(t, err, exitcode.MustAct)

	p := r.object(t, "p@x")
	_, err = r.svc.UpdateEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "p@x"}, p.ETag, service.ScopeSeries,
		service.EventFields{Summary: ptr("Renamed")}, false)
	ee = wantExit(t, err, exitcode.MustAct)
	if !errors.As(err, &wr) || wr.Code != service.RefusePersonalPart || !strings.Contains(ee.Error(), service.RefusePersonalPart) {
		t.Errorf("error %q, want a MustAct naming %s", ee.Error(), service.RefusePersonalPart)
	}
	if len(r.f.SyncCalls()) != 0 {
		t.Error("a refused update was sent")
	}
}

func TestDeleteOneOccurrence(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "s@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY", Color: green,
			Card: card("s@x", "Series", base, "RRULE:FREQ=WEEKLY")})
	}).start(t)
	req := service.OccurrencesRequest{From: at(base - day), To: at(base + 20*day)}
	p := occurrences(t, r.svc, req)
	if len(p.Items) != 3 {
		t.Fatalf("%d occurrences, want 3", len(p.Items))
	}
	second := p.Items[1]
	if err := r.svc.DeleteEvent(t.Context(), second.Ref, second.ETag, service.ScopeOccurrence); err != nil {
		t.Fatalf("DeleteEvent(occurrence): %v", err)
	}
	var starts []time.Time
	for _, o := range occurrences(t, r.svc, req).Items {
		starts = append(starts, o.Occurrence.Start)
	}
	if want := []time.Time{at(base), at(base + 14*day)}; !slices.EqualFunc(starts, want, time.Time.Equal) {
		t.Errorf("after deleting the second: %v, want %v", starts, want)
	}
	contains(t, "Proton's master", []byte(strings.Join(raws(r.proton(t, "s@x")), "")), "EXDATE")
	// The old ETag no longer holds; a missing recurrence ID is refused.
	err := r.svc.DeleteEvent(t.Context(), p.Items[2].Ref, second.ETag, service.ScopeOccurrence)
	wantIs(t, err, service.ErrPreconditionFailed)
	err = r.svc.DeleteEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "s@x"}, r.object(t, "s@x").ETag, service.ScopeOccurrence)
	_ = wantExit(t, err, exitcode.Usage)
}

func TestDeleteSeries(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "s@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY", Color: green,
			Card: card("s@x", "Series", base, "RRULE:FREQ=WEEKLY")})
		f.AddEvent(cal, protontest.Event{UID: "k@x", Start: base, End: base + 3600, Color: green, Card: card("k@x", "Kept", base)})
	}).start(t)
	req := service.OccurrencesRequest{From: at(base - day), To: at(base + 20*day)}
	first := occurrences(t, r.svc, req).Items[0]
	if first.Ref.UID != "s@x" && first.Ref.UID != "k@x" {
		t.Fatalf("first occurrence %+v", first)
	}
	obj := r.object(t, "s@x")
	// An occurrence's ref names its series too.
	ref := service.EventRef{CalendarID: r.cal, UID: "s@x", RecurrenceID: &service.EventTime{At: at(base + 7*day)}}
	if err := r.svc.DeleteEvent(t.Context(), ref, obj.ETag, service.ScopeSeries); err != nil {
		t.Fatalf("DeleteEvent(series): %v", err)
	}
	if rows := r.proton(t, "s@x"); len(rows) != 0 {
		t.Errorf("Proton still holds %d rows of the series", len(rows))
	}
	if got := summaries(occurrences(t, r.svc, req).Items); !slices.Equal(got, []string{"Kept"}) {
		t.Errorf("after the delete: %v", got)
	}
	err := r.svc.DeleteEvent(t.Context(), ref, obj.ETag, service.ScopeSeries)
	wantIs(t, err, service.ErrNoSuchObject)
}

// Writes run only during Serve; the reads (and a dry run) do not need it.
func TestAgendaWritesNeedServe(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	})
	r.signIn(t)
	r.sync(t, false)
	obj := r.object(t, "a@x")
	ref := service.EventRef{CalendarID: r.cal, UID: "a@x"}
	start, end := service.EventTime{At: at(base)}, service.EventTime{At: at(base + 3600)}
	_, _, err := r.svc.CreateEvent(t.Context(), r.cal, service.EventFields{Summary: ptr("New"), Start: &start, End: &end})
	wantIs(t, err, service.ErrNotServing)
	_, err = r.svc.UpdateEvent(t.Context(), ref, obj.ETag, service.ScopeSeries, service.EventFields{Summary: ptr("Dinner")}, false)
	wantIs(t, err, service.ErrNotServing)
	err = r.svc.DeleteEvent(t.Context(), ref, obj.ETag, service.ScopeSeries)
	wantIs(t, err, service.ErrNotServing)
	if len(r.f.SyncCalls()) != 0 {
		t.Error("a write outside Serve reached Proton")
	}
	if p := occurrences(t, r.svc, service.OccurrencesRequest{From: at(base - day), To: at(base + day)}); len(p.Items) != 1 {
		t.Errorf("read outside Serve: %d items", len(p.Items))
	}
	if _, err := r.svc.UpdateEvent(t.Context(), ref, obj.ETag, service.ScopeSeries, service.EventFields{Summary: ptr("Dinner")}, true); err != nil {
		t.Errorf("dry run outside Serve: %v", err)
	}
}

// Hub.Agenda chooses as Hub.Account does.
func TestHubAgendaSelects(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	_, _, err := h.Agenda("")
	if ee := wantExit(t, err, exitcode.MustAct); !errors.Is(err, service.ErrNoAccount) {
		t.Errorf("zero accounts: %q, want no account", ee.Error())
	}
	_, _, err = h.Agenda("nobody@x")
	_ = wantExit(t, err, exitcode.Usage)

	ra := e.mustLogin(t, h, userA)
	if a, got, err := h.Agenda(""); err != nil || got.ID != ra.Account || a == nil {
		t.Fatalf(`one account: Agenda("") = %+v, %v`, got, err)
	}
	rb := e.mustLogin(t, h, userB)
	_, _, err = h.Agenda("")
	var several *service.SeveralAccountsError
	if _ = wantExit(t, err, exitcode.Usage); !errors.As(err, &several) {
		t.Errorf("two accounts: %v, want a SeveralAccountsError", err)
	}
	for sel, want := range map[string]string{rb.Account: rb.Account, "B@X": rb.Account, "a@x": ra.Account} {
		if _, got, err := h.Agenda(sel); err != nil || got.ID != want {
			t.Errorf("Agenda(%q) = %+v, %v, want %s", sel, got, err, want)
		}
	}
	// The agenda handed out answers for its account; once the account is
	// removed it answers like Account's API (no account).
	a, _, err := h.Agenda(rb.Account)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Remove(t.Context(), rb.Account, service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	_, err = a.Occurrences(t.Context(), service.OccurrencesRequest{From: at(base), To: at(base + day)})
	if ee := wantExit(t, err, exitcode.Usage); !strings.HasPrefix(ee.Error(), "no account") {
		t.Errorf("removed account: %q", ee.Error())
	}
}
