package cli_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const baseTime = int64(1_800_000_000)

// syncRig is a rig logged in on a fake with one calendar, "Work".
func syncRig(t *testing.T) (*rig, string) {
	t.Helper()
	r := newRig(t, &protontest.Options{})
	cal := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	return r, cal
}

func TestSyncJSONEnvelope(t *testing.T) {
	t.Parallel()
	r, cal := syncRig(t)
	r.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: baseTime, End: baseTime + 60})
	r.f.AddEvent(cal, protontest.Event{UID: "b@x", Start: baseTime + 100, End: baseTime + 160})
	r.want(exitcode.OK, "sync", "--json")
	e := r.envelope()
	var res service.SyncResult
	mustUnmarshal(t, e.Data, &res)
	if e.Command != "sync" || !e.OK || len(res.Calendars) != 1 {
		t.Fatalf("envelope = %+v", e)
	}
	if c := res.Calendars[0]; c.ID != cal || c.Name != "Work" || c.Mode != "initial" || c.Added != 2 || c.Events != 2 {
		t.Errorf("calendar = %+v", c)
	}
	r.want(exitcode.OK, "sync", "--json", "--full")
	mustUnmarshal(t, r.envelope().Data, &res)
	if res.Calendars[0].Mode != "full" {
		t.Errorf("--full mode = %q, want full", res.Calendars[0].Mode)
	}
}

func TestSyncText(t *testing.T) {
	t.Parallel()
	r, cal := syncRig(t)
	r.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: baseTime, End: baseTime + 60})
	r.want(exitcode.OK, "sync")
	if got, want := r.out.String(), "Work: initial sync, +1 ~0 -0, 1 events\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// A partial pass prints what it did and exits 40, in both modes.
func TestSyncPartialPrintsDataAndExits40(t *testing.T) {
	t.Parallel()
	r, cal := syncRig(t)
	r.f.FailNext("GET", "/calendar/v1/"+cal+"/events", 403, 2011, 0)
	r.want(exitcode.Partial, "sync", "--json")
	e := r.envelope()
	var res service.SyncResult
	mustUnmarshal(t, e.Data, &res)
	if e.OK || e.Error == nil || e.Error.Code != 40 || e.Error.Hint == "" || len(res.Calendars) != 1 || res.Calendars[0].Error == "" {
		t.Fatalf("envelope = %+v, data %+v", e, res)
	}
	r.f.FailNext("GET", "/calendar/v1/"+cal+"/events", 403, 2011, 0)
	r.want(exitcode.Partial, "sync", "--full")
	if !strings.Contains(r.out.String(), "Work: ") || !strings.Contains(r.out.String(), "failed") || !strings.Contains(r.errb.String(), "Hint:") {
		t.Errorf("stdout %q, stderr %q", r.out.String(), r.errb.String())
	}
}

func TestSyncWatchJSONRefused(t *testing.T) {
	t.Parallel()
	s := &watchStub{}
	r := stubRig(t, nil)
	r.svc = s
	r.want(exitcode.Usage, "sync", "--watch", "--json")
	e := r.envelope()
	if e.OK || e.Error == nil || e.Error.Code != 2 || !strings.Contains(e.Error.Message, "--watch") {
		t.Errorf("envelope = %+v", e)
	}
	if s.calls != 0 {
		t.Error("the service was called before the usage error")
	}
}

func TestSyncIntervalBelowMinimumIsUsage(t *testing.T) {
	t.Parallel()
	s := &watchStub{}
	r := stubRig(t, nil)
	r.svc = s
	for _, iv := range []string{"9s", "0s", "1ms"} {
		r.want(exitcode.Usage, "sync", "--watch", "--interval", iv)
	}
	if s.calls != 0 {
		t.Error("the service was called before the usage error")
	}
	r.want(exitcode.OK, "sync", "--watch", "--interval", "10s")
	if s.interval != 10*time.Second {
		t.Errorf("interval = %v", s.interval)
	}
	r.want(exitcode.OK, "sync", "--watch")
	if s.interval != 30*time.Second {
		t.Errorf("default interval = %v, want 30s", s.interval)
	}
}

// watchStub plays a good pass, a failed one (next try in 2 min), then,
// when err is set, the pass the watch stops on with err.
type watchStub struct {
	stubAPI
	calls    int
	interval time.Duration
	err      error
}

func (s *watchStub) WatchSync(_ context.Context, _ service.SyncRequest, iv time.Duration, onPass func(service.WatchPass)) error {
	s.calls++
	s.interval = iv
	onPass(service.WatchPass{Result: service.SyncResult{Calendars: []service.CalendarSync{{ID: "c1", Name: "Work", Mode: "incremental", Added: 1, Events: 3}}},
		Next: 30 * time.Second})
	onPass(service.WatchPass{Err: &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("Proton is unavailable"), Hint: "try again later"},
		Next: 2 * time.Minute})
	if s.err != nil {
		onPass(service.WatchPass{Err: s.err})
	}
	return s.err
}

// Ctrl-C ends a watch normally: exit 0, no envelope, nothing on stderr.
func TestSyncWatchCancelledIsExitZero(t *testing.T) {
	t.Parallel()
	r := stubRig(t, nil)
	r.svc = &stubAPI{} // WatchSync returns nil at once, with no pass
	r.want(exitcode.OK, "sync", "--watch")
	if strings.Contains(r.errb.String(), "Error") {
		t.Errorf("stderr = %q", r.errb.String())
	}
	if r.out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: no envelope or hint after Ctrl-C", r.out.String())
	}
}

// A failed pass says when the next one runs; the error the watch stops on
// is printed once, by the final report.
func TestSyncWatchPrintsAPassLineEach(t *testing.T) {
	t.Parallel()
	s := &watchStub{err: &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the Proton session has ended"), Hint: "run pcb login"}}
	r := stubRig(t, nil)
	r.svc = s
	r.want(exitcode.MustAct, "sync", "--watch")
	lines := strings.Split(strings.TrimSpace(r.out.String()), "\n")
	if len(lines) != 1 || !strings.HasSuffix(lines[0], " Work: incremental sync, +1 ~0 -0, 3 events") {
		t.Fatalf("stdout = %q", r.out.String())
	}
	if _, err := time.Parse(time.RFC3339, strings.Fields(lines[0])[0]); err != nil {
		t.Errorf("line has no timestamp: %q", lines[0])
	}
	stderr := r.errb.String()
	if !strings.Contains(stderr, "sync failed: Proton is unavailable; next try in 2m0s") || !strings.Contains(stderr, "hint: try again later") {
		t.Errorf("stderr = %q, want the failed pass with its next try", stderr)
	}
	if n := strings.Count(stderr, "the Proton session has ended"); n != 1 || !strings.Contains(stderr, "Hint: run pcb login") {
		t.Errorf("stderr = %q: the stopping error is printed %d times, want once with its hint", stderr, n)
	}
}
