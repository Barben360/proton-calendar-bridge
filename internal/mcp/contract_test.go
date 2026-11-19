package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
)

// What an agent that branches on class and hint observes of a bad call:
// the same {"error":{class,message,hint}} envelope as a handler's error,
// whoever refused the call.

func TestArgumentErrorsKeepTheToolErrorEnvelope(t *testing.T) {
	hub := newHub()
	r := newRig(t, hub)
	cs := r.connect(t, deleteGrant, mcp.EntryRemote)
	alice := hub.agendas["a1"]

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
		want string // the message names it
	}{
		{"missing arguments", "event_create", map[string]any{"summary": "s"}, "account"},
		{"mistyped number", "events_list", with(monday(), "limit", "ten"), "limit"},
		{"a limit out of range", "events_list", with(monday(), "limit", 501), "limit"},
		{"a limit of zero or less", "events_search", with(monday(), "text", "x", "limit", -1), "limit"},
		{"scope outside the two", "event_update", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "all",
			"changes": map[string]any{"summary": "s"}}, "scope"},
		{"scope of a delete", "event_delete", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "everything"}, "scope"},
		{"a date that is no date", "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "s",
			"start_date": "tomorrow", "end_date": "2026-11-21"}, "start_date"},
		{"a changed date that is no date", "event_update", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "series",
			"changes": map[string]any{"end_date": "21/11/2026"}}, "end_date"},
		{"too many alarms", "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "s",
			"start": "2026-11-20T15:00", "end": "2026-11-20T16:00", "alarms": make([]int, 11)}, "alarms"},
		{"an alarm out of bounds", "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "s",
			"start": "2026-11-20T15:00", "end": "2026-11-20T16:00", "alarms": []int{20000000}}, "alarms"},
		{"an alarm after a day", "event_update", map[string]any{"event_id": standupID, "etag": `"x"`, "scope": "series",
			"changes": map[string]any{"alarms": []int{-5000}}}, "alarms"},
	} {
		terr := mustFail(t, cs, tc.tool, tc.args, "refused")
		if terr.Hint == "" || !strings.Contains(terr.Message, tc.want) {
			t.Errorf("%s: %+v, want a refused error with a hint whose message names %q", tc.name, *terr, tc.want)
		}
	}
	if n := alice.asked(); n != 0 {
		t.Errorf("%d write calls for arguments the schema refuses", n)
	}
	// The raw text of the result is the envelope, one object, nothing else.
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "event_create", Arguments: map[string]any{"summary": "s"}})
	if err != nil || !res.IsError || len(res.Content) != 1 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	var env map[string]map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(*sdkmcp.TextContent).Text), &env); err != nil || len(env) != 1 || env["error"]["class"] != "refused" {
		t.Errorf("text %q is not {\"error\":{...}} (%v)", res.Content[0].(*sdkmcp.TextContent).Text, err)
	}
	// The audit line says refused, as it does for the SDK's own refusals.
	var last map[string]any
	for _, l := range toolLines(t, r, "event_create") {
		last = l
	}
	if last == nil || last["outcome"] != "refused" {
		t.Errorf("audit line %v, want outcome refused", last)
	}
}

// What tools/list tells an agent: the allowed values and bounds, not prose.
func TestInputSchemasCarryTheirConstraints(t *testing.T) {
	cs := newRig(t, newHub()).connect(t, deleteGrant, mcp.EntryRemote)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	schemas := map[string]map[string]any{}
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool.InputSchema)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		schemas[tool.Name] = m
	}
	prop := func(tool string, path ...string) map[string]any {
		t.Helper()
		cur := schemas[tool]
		for _, p := range path {
			cur = obj(obj(cur["properties"])[p])
			if cur == nil {
				t.Fatalf("%s: no property %v", tool, path)
			}
		}
		return cur
	}
	for _, tool := range []string{"event_update", "event_delete"} {
		if got := fmt.Sprint(prop(tool, "scope")["enum"]); got != "[occurrence series]" {
			t.Errorf("%s scope enum %s", tool, got)
		}
	}
	for _, tool := range []string{"events_list", "events_search"} {
		l := prop(tool, "limit")
		if l["minimum"] != float64(1) || l["maximum"] != float64(500) {
			t.Errorf("%s limit %v, want 1 to 500", tool, l)
		}
	}
	for _, p := range [][]string{{"start_date"}, {"end_date"}} {
		if pat, _ := prop("event_create", p...)["pattern"].(string); pat == "" {
			t.Errorf("event_create %v has no pattern", p)
		}
		if pat, _ := prop("event_update", append([]string{"changes"}, p...)...)["pattern"].(string); pat == "" {
			t.Errorf("event_update changes.%v has no pattern", p)
		}
	}
	for _, a := range []map[string]any{prop("event_create", "alarms"), prop("event_update", "changes", "alarms")} {
		items := obj(a["items"])
		if a["maxItems"] != float64(10) || items["minimum"] == nil || items["maximum"] == nil {
			t.Errorf("alarms %v, want at most 10 minutes within bounds", a)
		}
	}
}

// A read with no account to read is a call that must be acted on, not an
// empty answer; the range is judged first.
func TestReadToolsWithoutAccountsSayToLogIn(t *testing.T) {
	hub := newHub()
	hub.remove("a1")
	hub.remove("b2")
	cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
	for _, tool := range []string{"calendars_list", "free_busy", "events_list", "events_search"} {
		terr := mustFail(t, cs, tool, argsFor(tool, monday()), "must-act")
		if !strings.Contains(terr.Message, "no account") || !strings.Contains(terr.Hint, "pcb login") {
			t.Errorf("%s: %+v, want no account and the way to log in", tool, *terr)
		}
	}
	if out := mustCall(t, cs, "status", nil); len(list(out["accounts"])) != 0 {
		t.Errorf("status %v: it still says what there is", out)
	}
	// 882 days, and a range that ends before it starts, are refused as such
	// whether or not an account exists.
	for _, args := range []map[string]any{
		{"from": "2026-01-01", "to": "2028-05-22"},
		{"from": "2026-11-17", "to": "2026-11-16"},
	} {
		for _, tool := range []string{"free_busy", "events_list"} {
			terr := mustFail(t, cs, tool, args, "refused")
			if terr.Hint == "" {
				t.Errorf("%s %v: no hint", tool, args)
			}
		}
	}
}
