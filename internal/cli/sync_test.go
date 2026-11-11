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

// TestSyncStatusAndEventList walks first use on one logged-in rig (one SRP
// login for the whole story, CI cost): status and event list before any
// sync, the first sync, its text, event list, status after, --full and a
// partial pass. The steps run in order and share the rig: each builds on
// the one before.
func TestSyncStatusAndEventList(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	work := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	empty := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Empty"})
	a := r.f.AddEvent(work, protontest.Event{UID: "a@x", Start: baseTime, End: baseTime + 3600})
	r.f.AddEvent(work, protontest.Event{UID: "far@x", Start: baseTime + 400*24*3600, End: baseTime + 400*24*3600 + 60})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	step := func(name string, fn func(t *testing.T)) {
		if !t.Run(name, func(t *testing.T) { r.t = t; fn(t) }) {
			t.FailNow() // the later steps build on this one
		}
		r.t = t
	}

	step("status before any sync", func(t *testing.T) {
		r.want(exitcode.OK, "status")
		if strings.Count(r.out.String(), "sync:") != 1 || !strings.Contains(r.out.String(), "sync: never (run pcb sync)") {
			t.Errorf("status = %q, want only the never-synced line", r.out.String())
		}
	})
	step("event list before any sync", func(t *testing.T) {
		r.want(exitcode.MustAct, "event", "list", "--calendar", "Work", "--json")
		if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Hint, "pcb sync") {
			t.Errorf("error = %+v", e.Error)
		}
	})
	step("first sync, JSON", func(t *testing.T) {
		r.want(exitcode.OK, "sync", "--json")
		e := r.envelope()
		var res service.SyncResult
		mustUnmarshal(t, e.Data, &res)
		if e.Command != "sync" || !e.OK || len(res.Calendars) != 2 {
			t.Fatalf("envelope = %+v", e)
		}
		if c := res.Calendars[0]; c.ID != work || c.Name != "Work" || c.Mode != "initial" || c.Added != 2 || c.Events != 2 {
			t.Errorf("calendar = %+v", c)
		}
	})
	step("second sync, text", func(t *testing.T) {
		r.f.AddEvent(work, protontest.Event{UID: "b@x", Start: baseTime + 7200, End: baseTime + 7260})
		r.want(exitcode.OK, "sync")
		if got, want := r.out.String(), "Work: incremental sync, +1 ~0 -0, 3 events\nEmpty: incremental sync, +0 ~0 -0, 0 events\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})
	step("event list by name", func(t *testing.T) {
		r.want(exitcode.OK, "event", "list", "--calendar", "Work", "--json")
		var data struct {
			Events []service.Event `json:"events"`
		}
		mustUnmarshal(t, r.envelope().Data, &data)
		if len(data.Events) != 3 || data.Events[0].ID != a || data.Events[0].UID != "a@x" {
			t.Fatalf("events = %+v", data.Events)
		}
		// Text table by ID, with a window that keeps only a.
		to := time.Unix(baseTime, 0).Add(time.Hour).Format(time.RFC3339)
		r.want(exitcode.OK, "event", "list", "--calendar", work, "--to", to)
		lines := strings.Split(strings.TrimRight(r.out.String(), "\n"), "\n")
		if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "START END ALL-DAY RECURRING ID" || !strings.HasSuffix(lines[1], " "+a) {
			t.Fatalf("output = %q", r.out.String())
		}
		if f := strings.Fields(lines[1]); f[2] != "no" || f[3] != "no" {
			t.Errorf("row = %q", lines[1])
		}
	})
	step("event list of an empty calendar", func(t *testing.T) {
		r.want(exitcode.OK, "event", "list", "--calendar", "Empty", "--json")
		if !strings.Contains(r.out.String(), `"events": []`) {
			t.Errorf("stdout = %q, want an empty array", r.out.String())
		}
	})
	step("status after a sync", func(t *testing.T) {
		r.want(exitcode.OK, "status")
		if !strings.Contains(r.out.String(), "sync: Work ("+work+"): 3 events, synced ") || strings.Contains(r.out.String(), "never (run") {
			t.Errorf("status = %q", r.out.String())
		}
	})
	step("sync --full", func(t *testing.T) {
		r.want(exitcode.OK, "sync", "--json", "--full")
		var res service.SyncResult
		mustUnmarshal(t, r.envelope().Data, &res)
		if len(res.Calendars) != 2 || res.Calendars[0].Mode != "full" {
			t.Errorf("--full = %+v, want full", res.Calendars)
		}
	})
	// A partial pass prints what it did and exits 40, in both modes.
	step("partial pass", func(t *testing.T) {
		r.f.FailNext("GET", "/calendar/v1/"+empty+"/events", 403, 2011, 0)
		r.want(exitcode.Partial, "sync", "--json", "--full")
		e := r.envelope()
		var res service.SyncResult
		mustUnmarshal(t, e.Data, &res)
		if e.OK || e.Error == nil || e.Error.Code != 40 || e.Error.Hint == "" || len(res.Calendars) != 2 || res.Calendars[1].Error == "" {
			t.Fatalf("envelope = %+v, data %+v", e, res)
		}
		r.f.FailNext("GET", "/calendar/v1/"+empty+"/events", 403, 2011, 0)
		r.want(exitcode.Partial, "sync", "--full")
		if !strings.Contains(r.out.String(), "Empty: full sync failed") || !strings.Contains(r.errb.String(), "Hint:") {
			t.Errorf("stdout %q, stderr %q", r.out.String(), r.errb.String())
		}
	})
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

// First use: an account without calendars says so instead of printing
// nothing, and --interval means nothing without --watch.
func TestSyncFirstUseMessages(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{}) // Sync answers no calendars
	r.want(exitcode.OK, "sync")
	if got := r.out.String(); got != "no calendars to sync\n" {
		t.Errorf("stdout = %q, want the no-calendar line", got)
	}
	r.want(exitcode.OK, "sync", "--json")
	if !strings.Contains(r.out.String(), `"calendars": []`) {
		t.Errorf("--json stdout = %q, want an empty array", r.out.String())
	}
	r.want(exitcode.Usage, "sync", "--interval", "20s")
	if !strings.Contains(r.errb.String(), "--watch") {
		t.Errorf("stderr = %q, want the usage error to name --watch", r.errb.String())
	}
}
