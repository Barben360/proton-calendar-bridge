package service_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// The render counters follow the rows: a placeholder, an unverified row or a
// row with a dropped reminder that Proton deletes stops counting once a
// listing sees it gone (audit read-I1); a malformed reminder is dropped and
// counted while its event renders (read-M4).
func TestRenderCountsFollowDeletedRows(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	other := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Other"})
	bad := e.f.AddEvent(work, protontest.Event{UID: "broken@x", Start: base, End: base + 3600, BrokenPart: protontest.SharedEncryptedPart,
		Card: card("broken@x", "Secret", base)})
	forged := e.f.AddEvent(work, protontest.Event{UID: "forged@x", Start: base, End: base + 3600, ForeignKey: true, Card: card("forged@x", "Forged", base)})
	alarm := e.f.AddEvent(work, protontest.Event{UID: "alarm@x", Start: base, End: base + 3600, Card: card("alarm@x", "Keep me", base),
		Notifications: []protontest.Notification{{Type: 1, Trigger: "whenever"}, {Type: 1, Trigger: "-PT10M"}}})
	// Another calendar's rows are not pruned by work's listing.
	e.f.AddEvent(other, protontest.Event{UID: "otherbroken@x", Start: base, End: base + 3600, BrokenPart: protontest.SharedEncryptedPart,
		Card: card("otherbroken@x", "Secret", base)})
	e.login(t, e.req())
	e.sync(t, false)

	e.objects(t, work, "broken@x", "forged@x", "alarm@x")
	e.objects(t, other, "otherbroken@x")
	o := e.objects(t, work, "alarm@x")["alarm@x"].Data
	contains(t, "alarm@x", o, "SUMMARY:Keep me\r\n", "TRIGGER:-PT10M\r\n")
	lacks(t, "alarm@x", o, "whenever", "X-PCB-PLACEHOLDER")
	if bytes.Count(o, []byte("BEGIN:VALARM")) != 1 {
		t.Errorf("alarm@x: want the one good reminder:\n%s", o)
	}
	if rc := e.svc.RenderCounts(); rc.Undecryptable != 2 || rc.Unverified != 1 || rc.DroppedAlarms != 1 {
		t.Fatalf("RenderCounts = %+v, want 2 undecryptable, 1 unverified, 1 dropped alarm", rc)
	}

	e.f.DeleteEvent(work, bad)
	e.f.DeleteEvent(work, forged)
	e.f.DeleteEvent(work, alarm)
	e.sync(t, false)
	e.listing(t, work)
	if rc := e.svc.RenderCounts(); rc.Undecryptable != 1 || rc.Unverified != 0 || rc.DroppedAlarms != 0 {
		t.Errorf("after the rows were deleted RenderCounts = %+v, want only other's placeholder left", rc)
	}
}

// Two exceptions of one RECURRENCE-ID: only the most recently edited is
// served, and the other counts as a duplicate (audit read-M1).
func TestDuplicateExceptionMostRecentWins(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	rid := base + day
	e.f.AddEvent(work, protontest.Event{UID: "s@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Card: card("s@x", "Series", base, "RRULE:FREQ=DAILY")})
	old := e.f.AddEvent(work, protontest.Event{UID: "s@x", Start: rid, End: rid + 3600, RecurrenceID: &rid,
		Card: card("s@x", "Old exception", rid, "RECURRENCE-ID:"+stamp(rid))})
	e.f.AddEvent(work, protontest.Event{UID: "s@x", Start: rid, End: rid + 3600, RecurrenceID: &rid,
		Card: card("s@x", "Mid exception", rid, "RECURRENCE-ID:"+stamp(rid))})
	e.f.UpdateEvent(work, old, protontest.Event{UID: "s@x", Start: rid, End: rid + 3600, RecurrenceID: &rid,
		Card: card("s@x", "Newest exception", rid, "RECURRENCE-ID:"+stamp(rid))})
	e.login(t, e.req())
	e.sync(t, false)
	d := e.objects(t, work, "s@x")["s@x"].Data
	if n := strings.Count(string(d), "RECURRENCE-ID:"); n != 1 {
		t.Errorf("%d RECURRENCE-ID lines, want 1:\n%s", n, d)
	}
	contains(t, "s@x", d, "SUMMARY:Newest exception\r\n")
	lacks(t, "s@x", d, "Old exception", "Mid exception")
	if n := e.svc.RenderCounts().DuplicateMasters; n != 1 {
		t.Errorf("DuplicateMasters = %d, want 1 (the exception left out)", n)
	}
}
