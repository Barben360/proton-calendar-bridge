package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// End to end, through pcb mcp on a real service and the fake: an update
// that ends a series early writes nothing to Proton, refused under a grant
// without delete, and declined by the person under one with it.
func TestMCPUpdateEndingASeriesEarlyWritesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, allow, answer string
		reason, message     string
	}{
		{name: "write grant", allow: "write", reason: "pcb:needs-delete", message: "only a grant that may delete"},
		{name: "declined", allow: "write,delete", answer: "decline", message: "not confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, &protontest.Options{})
			cal := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
			first := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC) // after the rig's clock
			until := first.AddDate(0, 0, 9*7).Format("20060102T150405Z")
			r.f.AddEvent(cal, protontest.Event{UID: "s@x", Start: first.Unix(), End: first.Unix() + 3600, RRule: "FREQ=WEEKLY;UNTIL=" + until,
				Color: "#3CBB3A", Card: strings.Join([]string{"UID:s@x", "DTSTAMP:20261101T080000Z", "DTSTART:" + first.Format("20060102T150405Z"),
					"DTEND:" + first.Add(time.Hour).Format("20060102T150405Z"), "SUMMARY:Series", "RRULE:FREQ=WEEKLY;UNTIL=" + until}, "\r\n")})
			r.want(exitcode.OK, "login", "--non-interactive", "--json")
			r.want(exitcode.OK, "sync")
			m := startMCP(t, r, nil, "--allow", tc.allow)
			cs, asked := connectAnswering(t, m, tc.answer)

			res := callTool(t, cs, "events_list", map[string]any{"from": "2026-11-19T00:00:00Z", "to": "2026-11-21T00:00:00Z"})
			items, _ := res["events"].([]any)
			if len(items) != 1 {
				t.Fatalf("events_list = %v, want the series' first occurrence", res)
			}
			item, _ := items[0].(map[string]any)
			id, etag := item["event_id"], item["etag"]
			writes := len(r.f.SyncCalls())

			terr := callToolError(t, cs, "event_update", map[string]any{"event_id": id, "etag": etag, "scope": "series",
				"changes": map[string]any{"rrule": "FREQ=WEEKLY;UNTIL=" + first.AddDate(0, 0, 2*7).Format("20060102T150405Z")}})
			if terr["class"] != "refused" || (tc.reason != "" && terr["reason"] != tc.reason) || !strings.Contains(terr["message"].(string), tc.message) {
				t.Errorf("event_update error %v, want refused (%s) saying %q", terr, tc.reason, tc.message)
			}
			if got := asked(); (tc.answer == "") != (len(got) == 0) {
				t.Errorf("asked %q", got)
			}
			if n := len(r.f.SyncCalls()); n != writes {
				t.Errorf("%d writes reached the fake", n-writes)
			}
			got := callTool(t, cs, "event_get", map[string]any{"event_id": id})
			if got["etag"] != etag || !strings.Contains(jsonText(t, got), "UNTIL="+until) {
				t.Errorf("event_get after the refusal = %v, want etag %v and UNTIL=%s", got, etag, until)
			}
		})
	}
}

// connectAnswering connects to m with a client that declares form
// elicitation and answers answer, or none when answer is "".
func connectAnswering(t *testing.T, m *mcpRun, answer string) (*sdkmcp.ClientSession, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked []string
	)
	var opts *sdkmcp.ClientOptions
	if answer != "" {
		opts = &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			mu.Lock()
			asked = append(asked, req.Params.Message)
			mu.Unlock()
			return &sdkmcp.ElicitResult{Action: answer, Content: map[string]any{}}, nil
		}}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, opts).
		Connect(ctx, m.agent, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, m.errb.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

func callTool(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %+v", name, err, res)
	}
	out, _ := res.StructuredContent.(map[string]any)
	return out
}

// callToolError is the error object of a call that must fail.
func callToolError(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || !res.IsError || len(res.Content) == 0 {
		t.Fatalf("%s: %v %+v, want a tool error", name, err, res)
	}
	text, _ := res.Content[0].(*sdkmcp.TextContent)
	var e struct {
		Error map[string]any `json:"error"`
	}
	if text == nil || json.Unmarshal([]byte(text.Text), &e) != nil || e.Error == nil {
		t.Fatalf("%s: error content %+v", name, res.Content)
	}
	return e.Error
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
