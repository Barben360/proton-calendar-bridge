package mcp_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
)

func auditLines(t *testing.T, r *rig) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(r.log.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestAuditLineHasNoContent(t *testing.T) {
	hub := newHub()
	a := hub.agendas["a1"]
	a.occ[0].Summary, a.occ[0].Description = "secret-marker title", "secret-marker body"
	d := a.events["cal-a|standup@x"]
	d.Data.Description = "secret-marker body"
	a.events["cal-a|standup@x"] = d
	r := newRig(t, hub)
	g := readGrant
	g.Name = "laptop-agent"
	cs := r.connect(t, g, mcp.EntryRemote)

	alice := with(monday(), "accounts", []string{"alice@example.com"}, "calendars", []string{"cal-a"})
	out := mustCall(t, cs, "events_search", with(alice, "text", "secret-marker"))
	id := field(out, "events.0.event_id")
	mustCall(t, cs, "event_get", map[string]any{"event_id": id})
	mustFail(t, cs, "events_list", with(monday(), "from", "secret-marker"), "refused")
	mustFail(t, cs, "events_list", with(monday(), "accounts", []string{"secret-marker@example.com"}), "not-found")
	if res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "event_get", Arguments: map[string]any{"event_id": 7}}); err != nil || !res.IsError {
		t.Fatalf("event_get with a number: %v, %v", res, err)
	}

	if strings.Contains(r.log.String(), "secret-marker") {
		t.Errorf("the audit log quotes arguments or content:\n%s", r.log.String())
	}
	lines := auditLines(t, r)
	if len(lines) != 5 {
		t.Fatalf("%d log lines for 5 calls:\n%s", len(lines), r.log.String())
	}
	want := []string{"account", "calendar", "entry", "grant", "level", "ms", "msg", "outcome", "time", "tool", "uid"}
	for i, l := range lines {
		if got := slices.Sorted(maps.Keys(l)); !slices.Equal(got, want) {
			t.Errorf("line %d has keys %v, want %v", i, got, want)
		}
		if l["msg"] != "mcp call" || l["level"] != "INFO" || l["entry"] != "remote" || l["grant"] != "laptop-agent" {
			t.Errorf("line %d: %v", i, l)
		}
	}
	for i, w := range []struct{ tool, account, calendar, uid, outcome string }{
		{"events_search", "a1", "cal-a", "", "ok"},
		{"event_get", "a1", "cal-a", "standup@x", "ok"},
		{"events_list", "", "", "", "refused"},
		{"events_list", "", "", "", "not-found"},
		{"event_get", "", "", "", "refused"},
	} {
		l := lines[i]
		if l["tool"] != w.tool || l["account"] != w.account || l["calendar"] != w.calendar || l["uid"] != w.uid || l["outcome"] != w.outcome {
			t.Errorf("line %d: %v; want %+v", i, l, w)
		}
	}
}
