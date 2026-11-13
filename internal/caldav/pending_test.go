package caldav_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Ruling M3-R25 end to end: during the first pass of `pcb serve` (a new
// cache life while Thunderbird runs), a calendar the pass has not reached
// yet is pending: left out of the home set, 503 + Retry-After: 30 on every
// request (a 4xx would disable it in Thunderbird), ErrCalendarPending from
// the service; once the pass reaches it, it is served.
func TestPendingCalendarIs503UntilTheFirstPassReachesIt(t *testing.T) {
	t.Parallel()
	var ready, later string
	r := newUnsyncedRig(t, func(f *protontest.Fake) {
		ready = f.AddCalendar(f.Username, protontest.Calendar{Name: "Ready"})
		later = f.AddCalendar(f.Username, protontest.Calendar{Name: "Later"})
		f.AddEvent(ready, protontest.Event{UID: "r@x", Start: base, End: base + 3600, Card: card("r@x", "Ready one", base)})
		f.AddEvent(later, protontest.Event{UID: "l@x", Start: base, End: base + 3600, Card: card("l@x", "Later one", base)})
	})

	// The pass blocks in the listing of "Later", after "Ready" is synced.
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.f.OnListPage(func(calID string, _ int) {
		if calID == later {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
	})
	released := sync.OnceFunc(func() { close(release) })
	ctx, cancel := context.WithCancel(t.Context())
	passes, done := make(chan service.WatchPass, 8), make(chan error, 1)
	go func() {
		done <- r.svc.Serve(ctx, service.ServeOptions{OnPass: func(p service.WatchPass) { passes <- p }})
	}()
	t.Cleanup(func() {
		released()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve after the test cancelled = %v, want nil", err)
		}
	})
	select {
	case <-blocked:
	case err := <-done:
		t.Fatalf("Serve returned %v before its first pass", err)
	}

	colls, err := r.svc.Collections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	for _, c := range colls {
		pending[c.ID] = c.Pending
	}
	if len(colls) != 2 || pending[ready] || !pending[later] {
		t.Fatalf("Collections mid first pass = %+v; want Ready synced, Later pending", colls)
	}
	if _, _, err := r.svc.Objects(t.Context(), later); !errors.Is(err, service.ErrCalendarPending) {
		t.Errorf("Objects(pending) = %v", err)
	}
	if _, err := r.svc.ObjectsByUID(t.Context(), later, []string{"l@x"}); !errors.Is(err, service.ErrCalendarPending) {
		t.Errorf("ObjectsByUID(pending) = %v", err)
	}
	if _, err := r.svc.Changes(t.Context(), later, ""); !errors.Is(err, service.ErrCalendarPending) {
		t.Errorf("Changes(pending) = %v", err)
	}

	ms := parseMS(t, r.do("PROPFIND", home, tbHomeSet, "Depth", "1"))
	if _, ok := ms.byPath[calPath(later)]; ok || len(ms.responses) != 2 {
		t.Errorf("the home set mid first pass lists %d responses (Later: %v), want the home and Ready", len(ms.responses), ok)
	}
	ms.get(t, calPath(ready))
	for _, tc := range []struct{ method, path, body, depth string }{
		{"PROPFIND", calPath(later), tbServerInfo, "0"},
		{"PROPFIND", calPath(later), tbEtags, "1"},
		{"REPORT", calPath(later), tbSync(""), "1"},
		{"REPORT", calPath(later), tbMultiget(tbHref(later, "l@x")), "1"},
		{"GET", objPath(later, "l@x"), "", ""},
	} {
		rec := r.do(tc.method, tc.path, tc.body, "Depth", tc.depth)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
			t.Errorf("%s %s mid first pass: %d, Retry-After %q; want 503, 30", tc.method, tc.path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	if changed, _ := listed(t, r.syncReport(t, ready, "")); len(changed) != 1 {
		t.Errorf("Ready mid first pass lists %v", changed)
	}

	released()
	if p := <-passes; p.Err != nil {
		t.Fatalf("first pass: %v", p.Err)
	}
	if changed, _ := listed(t, r.syncReport(t, later, "")); len(changed) != 1 || changed[objPath(later, "l@x")] == "" {
		t.Errorf("Later after the pass lists %v", changed)
	}
	if ms := parseMS(t, r.do("PROPFIND", home, tbHomeSet, "Depth", "1")); len(ms.responses) != 3 {
		t.Errorf("the home set after the pass lists %d responses", len(ms.responses))
	}
}

// Rulings M5-P8 and M3-R22: while pcb serve waits for a login, every
// request that reaches the service answers 503 with Retry-After: 30 and
// no warning (it is expected, like a pending calendar; never a 4xx that
// would disable the calendar in Thunderbird); without credentials it is
// still 401, the service never asked.
func TestWaitingForLoginIs503WithoutAWarning(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newStub()
	waiting := &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrWaitingForLogin, Hint: "run pcb login"}
	s.readErr, s.writeErr = waiting, waiting
	h := handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	for _, tc := range []struct{ method, path, body, depth string }{
		{"PROPFIND", home, tbHomeSet, "1"},
		{"PROPFIND", calPath(workID), tbServerInfo, "0"},
		{"PROPFIND", calPath(workID), tbEtags, "1"},
		{"REPORT", calPath(workID), tbSync(""), "1"},
		{"REPORT", calPath(workID), tbMultiget(tbHref(workID, plainUID)), "1"},
		{"GET", objPath(workID, plainUID), "", ""},
		{"PUT", objPath(workID, plainUID), "x", ""},
		{"DELETE", objPath(workID, plainUID), "", ""},
	} {
		rec := do(h, tc.method, tc.path, tc.body, "Depth", tc.depth, "Content-Type", "text/calendar")
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" || rec.Body.Len() != 0 {
			t.Errorf("%s %s while waiting: %d, Retry-After %q, body %q; want 503, 30, none",
				tc.method, tc.path, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("waiting for a login was logged:\n%s", logs.String())
	}
	if rec := serve(h, call("PROPFIND", home, tbHomeSet, "Depth", "1")); rec.Code != http.StatusUnauthorized {
		t.Errorf("PROPFIND without credentials while waiting: %d, want 401", rec.Code)
	}
}
