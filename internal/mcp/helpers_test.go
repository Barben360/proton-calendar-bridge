package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The stub Hub: accounts with an agenda, calendars and health each, which a
// test changes while a session is open.

type stubHub struct {
	mu       sync.Mutex
	accounts []service.AccountInfo
	agendas  map[string]*stubAgenda
	cals     map[string][]service.Calendar
	health   map[string]service.Health
	calErr   map[string]error // Calendars fails for these accounts
}

func noAccount(sel string) error {
	return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("%w %s", service.ErrNoAccount, sel), Hint: "see pcb account list"}
}

func (h *stubHub) pick(sel string) (service.AccountInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.accounts {
		if a.ID == sel || strings.EqualFold(a.Address, sel) {
			return a, true
		}
	}
	return service.AccountInfo{}, false
}

func (h *stubHub) Accounts() []service.AccountInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.accounts)
}

func (h *stubHub) Agenda(sel string) (service.Agenda, service.AccountInfo, error) {
	a, ok := h.pick(sel)
	if !ok {
		return nil, service.AccountInfo{}, noAccount(sel)
	}
	return h.agendas[a.ID], a, nil
}

type stubAPI struct {
	service.API
	cals []service.Calendar
	err  error
}

func (s stubAPI) Calendars(context.Context) ([]service.Calendar, error) { return s.cals, s.err }

func (h *stubHub) Account(sel string) (service.API, service.AccountInfo, error) {
	a, ok := h.pick(sel)
	if !ok {
		return nil, service.AccountInfo{}, noAccount(sel)
	}
	return stubAPI{cals: h.cals[a.ID], err: h.calErr[a.ID]}, a, nil
}

func (h *stubHub) Health(_ context.Context, sel string) (service.Health, error) {
	a, ok := h.pick(sel)
	if !ok {
		return service.Health{}, noAccount(sel)
	}
	return h.health[a.ID], nil
}

func (h *stubHub) Login(context.Context, service.LoginRequest) (service.LoginResult, error) {
	return service.LoginResult{}, errors.New("not in this test")
}

func (h *stubHub) SignOut(context.Context, string, service.LogoutRequest) (service.LogoutResult, error) {
	return service.LogoutResult{}, errors.New("not in this test")
}

// set replaces the account id's info (MCP switched off, signed out...).
func (h *stubHub) set(id string, change func(*service.AccountInfo)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.accounts {
		if h.accounts[i].ID == id {
			change(&h.accounts[i])
		}
	}
}

func (h *stubHub) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accounts = slices.DeleteFunc(h.accounts, func(a service.AccountInfo) bool { return a.ID == id })
}

type stubAgenda struct {
	service.Agenda
	mu      sync.Mutex
	occ     []service.AgendaOccurrence
	events  map[string]service.EventDetail // by calendar|uid
	skipped int
	budget  int // when set, pages hold at most this many items
	err     error
	reqs    []service.OccurrencesRequest
	refs    []service.EventRef
	w       writeStub // the writes (tools_write_test.go)
}

func (a *stubAgenda) record(req service.OccurrencesRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reqs = append(a.reqs, req)
}

func (a *stubAgenda) requests() []service.OccurrencesRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.reqs)
}

func (a *stubAgenda) matching(req service.OccurrencesRequest, busyOnly bool) []service.AgendaOccurrence {
	var out []service.AgendaOccurrence
	text := strings.ToLower(req.Text)
	for _, o := range a.occ {
		if len(req.Calendars) > 0 && !slices.Contains(req.Calendars, o.Ref.CalendarID) {
			continue
		}
		if !o.Occurrence.Start.Before(req.To) || !o.Occurrence.End.After(req.From) {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(o.Summary+"\x00"+o.Location+"\x00"+o.Description), text) {
			continue
		}
		if busyOnly && !o.Busy {
			continue
		}
		out = append(out, o)
	}
	slices.SortStableFunc(out, func(x, y service.AgendaOccurrence) int { return x.Occurrence.Start.Compare(y.Occurrence.Start) })
	return out
}

func (a *stubAgenda) Occurrences(_ context.Context, req service.OccurrencesRequest) (service.OccurrencePage, error) {
	a.record(req)
	if a.err != nil {
		return service.OccurrencePage{}, a.err
	}
	items := a.matching(req, false)
	if req.Cursor != "" {
		i := slices.IndexFunc(items, func(o service.AgendaOccurrence) bool { return o.Cursor() == req.Cursor })
		if i < 0 {
			return service.OccurrencePage{}, &exitcode.Error{Code: exitcode.Usage, Err: errors.New("bad cursor")}
		}
		items = items[i+1:]
	}
	limit := req.Limit
	if limit == 0 {
		limit = service.DefaultAgendaLimit
	}
	if a.budget > 0 && a.budget < limit { // a page the render budget cut short
		limit = a.budget
	}
	p := service.OccurrencePage{Items: items, Skipped: a.skipped}
	if len(items) > limit {
		p.Items, p.Truncated, p.Next = items[:limit], true, items[limit-1].Cursor()
	}
	return p, nil
}

func (a *stubAgenda) FreeBusy(_ context.Context, req service.OccurrencesRequest) ([]service.Occurrence, int, error) {
	a.record(req)
	if a.err != nil {
		return nil, 0, a.err
	}
	var out []service.Occurrence
	for _, o := range a.matching(req, true) {
		out = append(out, o.Occurrence)
	}
	return out, a.skipped, nil
}

func (a *stubAgenda) Event(_ context.Context, ref service.EventRef) (service.EventDetail, error) {
	a.mu.Lock()
	a.refs = append(a.refs, ref)
	a.mu.Unlock()
	if a.err != nil {
		return service.EventDetail{}, a.err
	}
	d, ok := a.events[ref.CalendarID+"|"+ref.UID]
	if !ok {
		return service.EventDetail{}, &exitcode.Error{Code: exitcode.Usage, Err: service.ErrNoSuchObject, Hint: "list the calendar again"}
	}
	d.Ref = ref
	return d, nil
}

// The fixture: alice (a1) and bob (b2), both ready with MCP on, one
// calendar each.

var (
	paris, _ = time.LoadLocation("Europe/Paris")
	fixedNow = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
)

func occ(cal, uid, summary string, start time.Time, d time.Duration) service.AgendaOccurrence {
	return service.AgendaOccurrence{
		Ref:          service.EventRef{CalendarID: cal, UID: uid},
		ETag:         `"` + uid + `-1"`,
		Occurrence:   service.Occurrence{Start: start, End: start.Add(d), TZID: "Europe/Paris"},
		Summary:      summary,
		CalendarType: "normal",
		Busy:         true,
	}
}

func newHub() *stubHub {
	day := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	alice := &stubAgenda{
		occ: []service.AgendaOccurrence{
			occ("cal-a", "standup@x", "Standup", day, 30*time.Minute),
			occ("cal-a", "review@x", "Review", day.Add(2*time.Hour), time.Hour),
		},
		events: map[string]service.EventDetail{
			"cal-a|standup@x": {ETag: `"standup@x-1"`, CalendarType: "normal", Data: service.EventData{
				UID: "standup@x", Summary: "Standup",
				Start: service.EventTime{At: day.In(paris), TZID: "Europe/Paris"},
				End:   service.EventTime{At: day.Add(30 * time.Minute).In(paris), TZID: "Europe/Paris"},
			}},
		},
	}
	bob := &stubAgenda{
		occ: []service.AgendaOccurrence{occ("cal-b", "lunch@x", "Lunch", day.Add(4*time.Hour), time.Hour)},
		events: map[string]service.EventDetail{
			"cal-b|lunch@x": {ETag: `"lunch@x-1"`, CalendarType: "normal", Data: service.EventData{
				UID: "lunch@x", Summary: "Lunch",
				Start: service.EventTime{At: day.Add(4 * time.Hour)},
				End:   service.EventTime{At: day.Add(5 * time.Hour)},
			}},
		},
	}
	synced := fixedNow.Add(-90 * time.Second)
	return &stubHub{
		accounts: []service.AccountInfo{
			{ID: "a1", Address: "alice@example.com", Phase: "ready", CalDAV: true, MCP: true},
			{ID: "b2", Address: "bob@example.com", Phase: "ready", CalDAV: true, MCP: true},
		},
		agendas: map[string]*stubAgenda{"a1": alice, "b2": bob},
		cals: map[string][]service.Calendar{
			"a1": {{ID: "cal-a", Name: "Work", Color: "#00f", Type: "normal", Default: true}},
			"b2": {{ID: "cal-b", Name: "Home", Color: "#0f0", Type: "normal", Default: true}},
		},
		health: map[string]service.Health{
			"a1": {Serve: service.ServeServing, LastPass: &service.PassHealth{At: synced, OK: true},
				Calendars: []service.CalendarHealth{{ID: "cal-a", Name: "Work", SyncedAt: &synced, Gate: "open"}}},
			"b2": {Serve: service.ServeServing, LastPass: &service.PassHealth{At: synced, OK: true},
				Calendars: []service.CalendarHealth{{ID: "cal-b", Name: "Home", SyncedAt: &synced, Gate: "open"}}},
		},
	}
}

// A session: the server's options (fixed clock, Paris), its audit log, and a
// client connected over the SDK's in-memory transports.

type rig struct {
	hub *stubHub
	srv *mcp.Server
	log *bytes.Buffer
}

func newRig(t *testing.T, hub *stubHub) *rig {
	t.Helper()
	r := &rig{hub: hub, log: &bytes.Buffer{}}
	o := mcp.Options{
		Hub:    hub,
		Now:    func() time.Time { return fixedNow },
		Zone:   paris,
		Logger: slog.New(slog.NewJSONHandler(&lockedWriter{w: r.log}, nil)),
	}
	r.srv = mcp.NewServer(o)
	return r
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (r *rig) connect(t *testing.T, g mcp.Grant, e mcp.Entry) *sdkmcp.ClientSession {
	t.Helper()
	st, ct := sdkmcp.NewInMemoryTransports()
	ss, err := r.srv.Session(g, e).Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func grant(caps ...mcp.Capability) mcp.Grant {
	return mcp.Grant{Name: "test-token", Caps: caps}
}

var readGrant = grant(mcp.CapFreeBusy, mcp.CapRead)

// toolErr is a tool error as an agent reads it.
type toolErr struct {
	Class       string `json:"class"`
	Message     string `json:"message"`
	Hint        string `json:"hint"`
	Reason      string `json:"reason"`
	RetryAfterS int    `json:"retry_after_s"`
}

// call calls a tool and returns its structured content, or its error.
func call(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any) (map[string]any, *toolErr) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		if len(res.Content) != 1 {
			t.Fatalf("%s: error with %d contents", name, len(res.Content))
		}
		text := res.Content[0].(*sdkmcp.TextContent).Text
		var env struct {
			Error *toolErr `json:"error"`
		}
		if err := json.Unmarshal([]byte(text), &env); err != nil || env.Error == nil {
			return nil, &toolErr{Class: "(sdk)", Message: text}
		}
		return nil, env.Error
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s: structured content %T", name, res.StructuredContent)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d contents, want one line of text", name, len(res.Content))
	}
	if text := res.Content[0].(*sdkmcp.TextContent).Text; text == "" || strings.Contains(text, "\n") {
		t.Errorf("%s: text %q, want one line", name, text)
	}
	return out, nil
}

// mustCall is call that fails the test on a tool error.
func mustCall(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	out, terr := call(t, cs, name, args)
	if terr != nil {
		t.Fatalf("%s(%v): tool error %+v", name, args, *terr)
	}
	return out
}

// mustFail is call that fails the test unless the tool answers class.
func mustFail(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any, class string) *toolErr {
	t.Helper()
	out, terr := call(t, cs, name, args)
	if terr == nil {
		t.Fatalf("%s(%v) = %v, want a %s error", name, args, out, class)
	}
	if terr.Class != class {
		t.Fatalf("%s(%v): error %+v, want class %s", name, args, *terr, class)
	}
	return terr
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// field walks a path of keys and indexes ("events.0.untrusted.summary").
func field(v any, path string) any {
	for k := range strings.SplitSeq(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[k]
		case []any:
			var i int
			if _, err := fmt.Sscan(k, &i); err != nil || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

func accountsOf(out map[string]any) []string {
	var got []string
	for _, a := range list(out["accounts"]) {
		got = append(got, fmt.Sprint(obj(a)["account"]))
	}
	return got
}

// The arguments of a range over the fixture's Monday.
func monday() map[string]any {
	return map[string]any{"from": "2026-11-16T00:00:00Z", "to": "2026-11-17T00:00:00Z"}
}

func with(args map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// argsFor is args as tool takes them: a text for events_search, no range
// for calendars_list.
func argsFor(tool string, args map[string]any) map[string]any {
	out := with(args)
	switch tool {
	case "status":
		return map[string]any{}
	case "events_search":
		if _, ok := out["text"]; !ok {
			out["text"] = "x"
		}
	case "calendars_list":
		for _, k := range []string{"from", "to", "tz", "calendars", "limit", "cursor", "text"} {
			delete(out, k)
		}
	default:
		delete(out, "text")
	}
	return out
}
