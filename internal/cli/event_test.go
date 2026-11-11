package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestEventListBadDate(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	for _, args := range [][]string{
		{"--from", "yesterday"}, {"--to", "2026-13-01"}, {"--from", "11/11/2026"},
	} {
		r.want(exitcode.Usage, append([]string{"event", "list", "--calendar", "Work", "--json"}, args...)...)
		if e := r.envelope(); e.Error == nil || e.Error.Hint == "" || !strings.Contains(e.Error.Message, args[0]) {
			t.Errorf("%v: error = %+v", args, e.Error)
		}
	}
	r.want(exitcode.Usage, "event", "list", "--json") // --calendar is required
}

func TestEventListDatesAreRFC3339OrLocalMidnight(t *testing.T) {
	t.Parallel()
	got := &eventsStub{}
	r := stubRig(t, nil)
	r.svc = got
	r.want(exitcode.OK, "event", "list", "--calendar", "Work", "--from", "2026-11-11", "--to", "2026-11-12T10:00:00+02:00")
	wantFrom := time.Date(2026, 11, 11, 0, 0, 0, 0, time.Local)
	if got.req.From == nil || !got.req.From.Equal(wantFrom) {
		t.Errorf("from = %v, want local midnight %v", got.req.From, wantFrom)
	}
	if want := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC); got.req.To == nil || !got.req.To.Equal(want) {
		t.Errorf("to = %v, want %v", got.req.To, want)
	}
}

type eventsStub struct {
	stubAPI
	req service.EventsRequest
}

func (s *eventsStub) Events(_ context.Context, req service.EventsRequest) ([]service.Event, error) {
	s.req = req
	return nil, nil
}

// syncRig is a rig logged in on a fake with one calendar, "Work".
func syncRig(t *testing.T) (*rig, string) {
	t.Helper()
	r := newRig(t, &protontest.Options{})
	cal := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	return r, cal
}

// All-day events are stored as UTC midnights: a date bound must compare
// dates, not shift the event by a day in a zone west of UTC. Not parallel:
// it sets the CLI's local zone.
func TestEventListAllDayWestOfUTC(t *testing.T) {
	defer cli.SetLocalZone(time.FixedZone("PST", -8*3600))()
	r, cal := syncRig(t)
	utc := func(d int) int64 { return time.Date(2027, 1, d, 0, 0, 0, 0, time.UTC).Unix() }
	r.f.AddEvent(cal, protontest.Event{UID: "d15", Start: utc(15), End: utc(16), FullDay: true})
	d16 := r.f.AddEvent(cal, protontest.Event{UID: "d16", Start: utc(16), End: utc(17), FullDay: true})
	r.f.AddEvent(cal, protontest.Event{UID: "d17", Start: utc(17), End: utc(18), FullDay: true})
	r.want(exitcode.OK, "sync")
	r.want(exitcode.OK, "event", "list", "--calendar", "Work", "--from", "2027-01-16", "--to", "2027-01-17", "--json")
	var data struct {
		Events []service.Event `json:"events"`
	}
	mustUnmarshal(t, r.envelope().Data, &data)
	if len(data.Events) != 1 || data.Events[0].ID != d16 || data.Events[0].StartDate != "2027-01-16" || data.Events[0].EndDate != "2027-01-17" {
		t.Fatalf("events = %+v, want only Jan 16", data.Events)
	}
	r.want(exitcode.OK, "event", "list", "--calendar", "Work", "--from", "2027-01-16", "--to", "2027-01-17")
	lines := strings.Split(strings.TrimRight(r.out.String(), "\n"), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[1]), " ") != "2027-01-16 2027-01-17 yes no "+d16 {
		t.Errorf("text = %q, want dates for the all-day row", r.out.String())
	}
}
