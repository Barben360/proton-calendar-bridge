package mcp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// lastRequest is the last request alice's agenda received.
func lastRequest(t *testing.T, hub *stubHub) service.OccurrencesRequest {
	t.Helper()
	reqs := hub.agendas["a1"].requests()
	if len(reqs) == 0 {
		t.Fatal("alice's agenda was not asked")
	}
	return reqs[len(reqs)-1]
}

// Review Focus 1: a wall-clock time or a date without an offset is read in
// the tz argument, else Options.Zone; the zone used is echoed. An RFC 3339
// time with an offset is kept as it is.
func TestNaiveTimeUsesTzThenZone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		from, to time.Time
		zone     string
	}{
		{"tz", map[string]any{"from": "2026-11-14T15:00", "to": "2026-11-14T18:30:00", "tz": "America/New_York"},
			time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 23, 30, 0, 0, time.UTC), "America/New_York"},
		{"zone", map[string]any{"from": "2026-11-14T15:00", "to": "2026-11-14T18:30:00"},
			time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 17, 30, 0, 0, time.UTC), "Europe/Paris"},
		{"dates in tz", map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "America/New_York"},
			time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 5, 0, 0, 0, time.UTC), "America/New_York"},
		{"dates in zone", map[string]any{"from": "2026-11-14", "to": "2026-11-15"},
			time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC), "Europe/Paris"},
		{"offsets kept", map[string]any{"from": "2026-11-14T15:00:00+09:00", "to": "2026-11-14T16:00:00Z", "tz": "America/New_York"},
			time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 16, 0, 0, 0, time.UTC), "America/New_York"},
	} {
		for _, tool := range []string{"events_list", "free_busy", "events_search"} {
			hub := newHub()
			cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
			args := argsFor(tool, with(tc.args, "accounts", []string{"a1"}))
			out := mustCall(t, cs, tool, args)
			req := lastRequest(t, hub)
			if !req.From.Equal(tc.from) || !req.To.Equal(tc.to) {
				t.Errorf("%s, %s: range %s – %s, want %s – %s", tc.name, tool, req.From.UTC(), req.To.UTC(), tc.from, tc.to)
			}
			if req.Loc == nil || req.Loc.String() != tc.zone {
				t.Errorf("%s, %s: agenda read in %v, want %s", tc.name, tool, req.Loc, tc.zone)
			}
			if out["zone"] != tc.zone {
				t.Errorf("%s, %s: zone %v, want %s echoed", tc.name, tool, out["zone"], tc.zone)
			}
			// The range is echoed in the zone, with its offset.
			loc, _ := time.LoadLocation(tc.zone)
			if want := tc.from.In(loc).Format(time.RFC3339); out["from"] != want {
				t.Errorf("%s, %s: from %v, want %s", tc.name, tool, out["from"], want)
			}
		}
	}
}

func TestMalformedTimeIsRefused(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		arg  string
	}{
		{map[string]any{"from": "tomorrow", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "2026-11-14", "to": "2026-13-01"}, "to"},
		{map[string]any{"from": "2026-11-14 15:00", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "2026-11-14T25:00", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15T10:00:00+0100"}, "to"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "Mars/Olympus"}, "tz"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "Local"}, "tz"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "+01:00"}, "tz"},
	} {
		for _, tool := range []string{"events_list", "free_busy", "events_search"} {
			hub := newHub()
			cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
			terr := mustFail(t, cs, tool, argsFor(tool, tc.args), "refused")
			if !strings.HasPrefix(terr.Message, tc.arg+" ") {
				t.Errorf("%s(%v): %q does not start with the argument %s", tool, tc.args, terr.Message, tc.arg)
			}
			for id, a := range hub.agendas {
				if n := len(a.requests()); n != 0 {
					t.Errorf("%s(%v): agenda %s asked %d times", tool, tc.args, id, n)
				}
			}
		}
	}
}
