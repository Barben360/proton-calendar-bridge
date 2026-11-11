package caldav_test

// An independent client: go-webdav's caldav.Client (test-only dependency,
// ruling M3-R5) discovers and reads the calendars over real HTTP.

import (
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	webdavcaldav "github.com/emersion/go-webdav/caldav"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

func TestGoWebdavClientDiscoversAndReads(t *testing.T) {
	t.Parallel()
	var work, hol string
	r := newRig(t, func(f *protontest.Fake) {
		work = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work", Description: "Team"})
		hol = f.AddCalendar(f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
		f.AddEvent(work, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Card: card("a@x", "Alpha", base)})
		f.AddEvent(work, protontest.Event{UID: "b c/é", Start: base, End: base + 3600, Card: card("b c/é", "Beta", base)})
		f.AddEvent(hol, protontest.Event{UID: "ny@x", Start: base, End: base + 3600, Card: card("ny@x", "New year", base)})
	})
	srv := httptest.NewServer(r.h)
	t.Cleanup(srv.Close)
	hc := webdav.HTTPClientWithBasicAuth(srv.Client(), "user@proton.me", r.pass)
	c, err := webdavcaldav.NewClient(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	p, err := c.FindCurrentUserPrincipal(ctx)
	if err != nil || p != principal {
		t.Fatalf("FindCurrentUserPrincipal = %q, %v", p, err)
	}
	hs, err := c.FindCalendarHomeSet(ctx, p)
	if err != nil || hs != home {
		t.Fatalf("FindCalendarHomeSet = %q, %v", hs, err)
	}
	cals, err := c.FindCalendars(ctx, hs)
	if err != nil {
		t.Fatalf("FindCalendars: %v", err)
	}
	byPath := map[string]webdavcaldav.Calendar{}
	for _, cal := range cals {
		byPath[cal.Path] = cal
	}
	w, h := byPath[calPath(work)], byPath[calPath(hol)]
	if len(cals) != 2 || w.Name != "Work" || w.Description != "Team" || h.Name != "Holidays" ||
		!slices.Equal(w.SupportedComponentSet, []string{"VEVENT"}) {
		t.Fatalf("FindCalendars = %+v", cals)
	}

	objs, err := c.MultiGetCalendar(ctx, w.Path, &webdavcaldav.CalendarMultiGet{
		Paths:       []string{objPath(work, "a@x"), objPath(work, "b c/é")},
		CompRequest: webdavcaldav.CalendarCompRequest{Name: "VCALENDAR", AllProps: true, AllComps: true},
	})
	if err != nil {
		t.Fatalf("MultiGetCalendar: %v", err)
	}
	summaries := map[string]string{}
	for _, o := range objs {
		if o.ETag == "" {
			t.Errorf("%s: no ETag", o.Path)
		}
		summaries[o.Path] = summary(t, o.Data)
	}
	if len(objs) != 2 || summaries[objPath(work, "a@x")] != "Alpha" || summaries[objPath(work, "b c/é")] != "Beta" {
		t.Errorf("MultiGetCalendar = %v", summaries)
	}

	got, err := c.GetCalendarObject(ctx, objPath(hol, "ny@x"))
	if err != nil || summary(t, got.Data) != "New year" || got.ETag == "" {
		t.Fatalf("GetCalendarObject = %+v, %v", got, err)
	}
}

// summary is the SUMMARY of the calendar's one VEVENT.
func summary(t *testing.T, cal *ical.Calendar) string {
	t.Helper()
	evs := cal.Events()
	if len(evs) != 1 {
		t.Fatalf("%d events", len(evs))
	}
	s, err := evs[0].Props.Text(ical.PropSummary)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
