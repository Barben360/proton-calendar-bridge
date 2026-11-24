package cli

import (
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The pass line says only what the pass knows so far.
func TestPassTextAtEachStage(t *testing.T) {
	start := time.Date(2026, 11, 12, 11, 58, 48, 0, time.UTC)
	n := func(v int) *int { return &v }
	for _, tc := range []struct {
		name string
		p    service.PassProgress
		want string
	}{
		{"calendars not listed yet", service.PassProgress{StartedAt: start},
			"running since 2026-11-12T11:58:48Z"},
		{"before the first calendar", service.PassProgress{StartedAt: start, Calendars: 3},
			"running since 2026-11-12T11:58:48Z, 3 calendars to sync"},
		{"first sync, no estimate yet", service.PassProgress{StartedAt: start, Calendar: 2, Calendars: 3, CalendarName: "Sport", Mode: "initial", EventsFetched: 300},
			"running since 2026-11-12T11:58:48Z, calendar 2 of 3 Sport (initial), 300 events listed"},
		{"incremental: nothing to list", service.PassProgress{StartedAt: start, Calendar: 1, Calendars: 1, CalendarName: "Work", Mode: "incremental", EventsExpected: n(0)},
			"running since 2026-11-12T11:58:48Z, calendar 1 of 1 Work (incremental)"},
	} {
		if got := passText(tc.p); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
