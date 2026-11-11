package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestEventListByName(t *testing.T) {
	t.Parallel()
	r, cal := syncRig(t)
	a := r.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: baseTime, End: baseTime + 3600})
	r.f.AddEvent(cal, protontest.Event{UID: "far@x", Start: baseTime + 400*24*3600, End: baseTime + 400*24*3600 + 60})
	r.want(exitcode.OK, "sync")
	r.want(exitcode.OK, "event", "list", "--calendar", "Work", "--json")
	var data struct {
		Events []service.Event `json:"events"`
	}
	mustUnmarshal(t, r.envelope().Data, &data)
	if len(data.Events) != 2 || data.Events[0].ID != a || data.Events[0].UID != "a@x" {
		t.Fatalf("events = %+v", data.Events)
	}
	// Text table, with a window that drops the far event.
	to := time.Unix(baseTime, 0).Add(48 * time.Hour).Format(time.RFC3339)
	r.want(exitcode.OK, "event", "list", "--calendar", cal, "--to", to)
	lines := strings.Split(strings.TrimRight(r.out.String(), "\n"), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "START END ALL-DAY RECURRING ID" || !strings.HasSuffix(lines[1], " "+a) {
		t.Fatalf("output = %q", r.out.String())
	}
	if f := strings.Fields(lines[1]); f[2] != "no" || f[3] != "no" {
		t.Errorf("row = %q", lines[1])
	}
}

func TestEventListEmptyIsAnArray(t *testing.T) {
	t.Parallel()
	r, _ := syncRig(t)
	r.want(exitcode.OK, "sync")
	r.want(exitcode.OK, "event", "list", "--calendar", "Work", "--json")
	if !strings.Contains(r.out.String(), `"events": []`) {
		t.Errorf("stdout = %q, want an empty array", r.out.String())
	}
}

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

func TestEventListNeverSyncedMustAct(t *testing.T) {
	t.Parallel()
	r, _ := syncRig(t)
	r.want(exitcode.MustAct, "event", "list", "--calendar", "Work", "--json")
	if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Hint, "pcb sync") {
		t.Errorf("error = %+v", e.Error)
	}
}

func TestStatusShowsSyncSection(t *testing.T) {
	t.Parallel()
	r, cal := syncRig(t)
	r.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: baseTime, End: baseTime + 60})
	r.want(exitcode.OK, "status")
	if strings.Contains(r.out.String(), "sync:") {
		t.Errorf("status shows a sync line before any sync: %q", r.out.String())
	}
	r.want(exitcode.OK, "sync")
	r.want(exitcode.OK, "status")
	if !strings.Contains(r.out.String(), "sync: Work ("+cal+"): 1 events, synced ") {
		t.Errorf("status = %q", r.out.String())
	}
}
