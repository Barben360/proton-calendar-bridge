package service_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// Live 2026-11-13, on the path pcb serve runs since M6: while one account's
// passes fail, its interval_seconds is the growing back-off wait, and
// sync-stalled must still be measured against its cadence_seconds (the wait
// after a good pass), per account: the failing account gets the warning
// once five cadences passed without a good sync, though five back-off waits
// have not, and the other account's cadence and interval are its own.
func TestHubSyncStalledFollowsEachAccountsCadence(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	calA := e.f.AddCalendar(userA, protontest.Calendar{Name: "A"})
	e.f.AddCalendar(userB, protontest.Calendar{Name: "B"})
	e.sleep = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-time.After(20 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := e.open(t)
	a, b := e.loginBoth(t, h)
	for range 1000 { // every pass of a's until the test ends
		e.f.FailNextFor(userA, http.MethodGet, "/calendar/v1/"+calA+"/events", http.StatusBadRequest, 2000, 0)
	}
	r := newServeRig(h)
	r.serve(t, r.options(true))
	r.waitFor(t, "two failed passes of a and a good one of b", func() bool {
		return len(r.passes[a]) >= 2 && r.okPasses(a) == 0 && r.okPasses(b) > 0
	})
	ha := health(t, h, a)
	if ha.CadenceSeconds != 30 || ha.IntervalSeconds < 54 {
		t.Fatalf("a: cadence_seconds %d, interval_seconds %d; want 30 and the back-off (54 s or more)", ha.CadenceSeconds, ha.IntervalSeconds)
	}
	if hb := health(t, h, b); hb.CadenceSeconds != 30 || hb.IntervalSeconds != 30 {
		t.Errorf("b: cadence_seconds %d, interval_seconds %d; want 30 and 30 (a's back-off is a's)", hb.CadenceSeconds, hb.IntervalSeconds)
	}
	if ha.Since == nil {
		t.Fatal("a's health has no since")
	}
	// 200 s without a good sync: past five cadences (150 s), short of five
	// back-off waits (270 s or more).
	st := ctl.BuildStatus(t.Context(), h, ctl.Daemon{Kind: "serve"}, nil, ha.Since.Add(200*time.Second))
	var stalled []ctl.Alert
	for _, as := range st.Accounts {
		if as.ID != a {
			continue
		}
		for _, al := range as.Alerts {
			if al.Code == ctl.CodeSyncStalled {
				stalled = append(stalled, al)
			}
		}
	}
	if len(stalled) != 1 || stalled[0].Account != a || stalled[0].Calendar != calA {
		t.Errorf("a's sync-stalled alerts = %+v, want one for calendar %s of account %s", stalled, calA, a)
	}
}
