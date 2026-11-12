package service_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

func (e *env) health(t *testing.T) service.Health {
	t.Helper()
	h, err := e.svc.Health(t.Context())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	return h
}

func calHealth(t *testing.T, h service.Health, id string) service.CalendarHealth {
	t.Helper()
	for _, c := range h.Calendars {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no calendar %s in %+v", id, h.Calendars)
	return service.CalendarHealth{}
}

// Outside Serve there is no live state to report.
func TestHealthOutsideServeIsStopped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	check := func(step string) {
		t.Helper()
		h := e.health(t)
		if h.Serve != service.ServeStopped || h.Since != nil || h.Calendars == nil || len(h.Calendars) != 0 || h.LastPass != nil || h.Idle {
			t.Errorf("%s: %+v, want stopped with no calendars (an empty list, not null)", step, h)
		}
	}
	check("logged out")
	e.login(t, e.req())
	check("logged in")
	s := e.serve(t)
	s.next(t, true)
	if h := e.health(t); h.Serve != service.ServeServing || h.Since == nil || len(h.Calendars) != 1 {
		t.Fatalf("serving: %+v", h)
	}
	s.stop(t)
	check("after Serve")
}

// Every field of the render counters is a snake_case key, present when
// zero, and Health carries them.
func TestRenderCountsJSONKeys(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(service.RenderCounts{Undecryptable: 1, Unverified: 2, DuplicateMasters: 3, KeysErrors: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"undecryptable":1,"unverified":2,"duplicate_masters":3,"keys_errors":["c1"]}`; string(b) != want {
		t.Errorf("RenderCounts JSON %s, want %s", b, want)
	}
	e := newEnv(t, protontest.Options{})
	b, err = json.Marshal(e.health(t))
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Render map[string]json.RawMessage
		Cals   json.RawMessage `json:"calendars"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if string(top.Render["keys_errors"]) != "[]" || len(top.Render) != 4 || string(top.Cals) != "[]" {
		t.Errorf("Health JSON %s: want render with four keys, keys_errors and calendars as empty lists", b)
	}
}

// Each failure is sorted by what it is made of, not by its text: no retry
// or mapping hides the case.
func TestErrorClass(t *testing.T) {
	t.Parallel()
	ctxEnded := fmt.Errorf("wrapped: %w", api.ErrSessionEnded)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"none", nil, ""},
		{"429", &api.Error{Status: 429, RetryAfter: time.Minute}, "rate-limited"},
		{"503", &api.Error{Status: 503}, "proton-unavailable"},
		{"500 wrapped", fmt.Errorf("listing: %w", &api.Error{Status: 500}), "proton-unavailable"},
		{"network", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "network"},
		{"a person must act", &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("x")}, "session"},
		{"session ended", ctxEnded, "session"},
		{"another 4xx", &api.Error{Status: 422, Code: 9001}, "refused"},
		{"a 404", fmt.Errorf("x: %w", &api.Error{Status: 404}), "refused"},
		{"deferred calendar", &syncer.ResyncDeferredError{Cause: "x", Until: time.Now()}, "deferred"},
		{"cache", &syncer.StoreError{Err: cache.ErrCorrupt}, "cache"},
		{"plain", errors.New("boom"), "other"},
		{"unverified listing", &syncer.UnverifiedError{Rows: 2}, "other"},
		// The pass-level errors the service maps keep their class.
		{"mapped 503", service.MapError(&api.Error{Status: 503}), "proton-unavailable"},
		{"mapped 429", service.MapError(&api.Error{Status: 429}), "rate-limited"},
		{"mapped 4xx", service.MapError(&api.Error{Status: 400, Code: 2000}), "refused"},
		{"mapped network", service.MapError(&net.OpError{Op: "dial", Err: errors.New("down")}), "network"},
		{"mapped cache", service.MapError(&syncer.StoreError{Err: cache.ErrCorrupt}), "cache"},
		{"mapped session", service.MapError(api.ErrSessionEnded), "session"},
	} {
		if got := service.ErrorClass(tc.err); got != tc.want {
			t.Errorf("%s: class %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The write gate and why, per calendar, as the keys decided them (M4-R39);
// unknown until the keys were tried.
func TestHealthReportsGatesWithReasons(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	u := e.f.Username
	open := e.f.AddCalendar(u, protontest.Calendar{Name: "Open"})
	feed := e.f.AddCalendar(u, protontest.Calendar{Name: "Feed", Type: 1})
	badSig := e.f.AddCalendar(u, protontest.Calendar{Name: "BadSig"})
	noSig := e.f.AddCalendar(u, protontest.Calendar{Name: "NoSig"})
	viewer := e.f.AddCalendar(u, protontest.Calendar{Name: "Viewer"})
	broken := e.f.AddCalendar(u, protontest.Calendar{Name: "Broken", BrokenPassphrase: true})
	noPrimary := e.f.AddCalendar(u, protontest.Calendar{Name: "NoPrimary", NoPrimaryKey: true})
	e.login(t, e.req())
	e.calendars(t) // the fake serves a calendar's member from its first bootstrap
	e.f.SetPassphraseSignature(badSig, protontest.PassphraseSignedByStranger)
	e.f.SetPassphraseSignature(noSig, protontest.PassphraseUnsigned)
	e.f.SetPermissions(viewer, 7) // the Write bit (8) off
	s := e.serve(t)
	s.next(t, true)

	h := e.health(t)
	for _, c := range h.Calendars {
		if c.Gate != "unknown" || c.GateReason != "the keys were not loaded yet" {
			t.Errorf("before any read, %s: gate %q (%q), want unknown", c.Name, c.Gate, c.GateReason)
		}
	}
	_ = e.collection(t, open) // loads every calendar's keys
	h = e.health(t)
	for _, tc := range []struct {
		id, gate, reason string
	}{
		{open, "open", ""},
		{feed, "closed", "not a normal calendar (subscribed or holidays)"},
		{badSig, "closed", "the member passphrase signature does not verify"},
		{viewer, "closed", "our member lacks the Write permission"},
		{noSig, "undecided", "the member passphrase signature cannot be checked (none served, or our address key is locked)"},
		{broken, "undecided", "the calendar keys did not unlock"},
		{noPrimary, "undecided", "no usable primary calendar key (none unlocked, or two flagged primary)"},
	} {
		c := calHealth(t, h, tc.id)
		if c.Gate != tc.gate || c.GateReason != tc.reason {
			t.Errorf("%s: gate %q (%q), want %q (%q)", c.Name, c.Gate, c.GateReason, tc.gate, tc.reason)
		}
		if wantKeys := tc.id == broken; (c.KeysError != "") != wantKeys {
			t.Errorf("%s: keys error %q, want one only for the calendar whose keys do not unlock", c.Name, c.KeysError)
		}
	}
	if got := h.Render.KeysErrors; !slices.Equal(got, []string{broken}) {
		t.Errorf("render keys errors %v, want [%s]", got, broken)
	}
	s.stop(t)
}

// A calendar's sync state: its last success and lag from the fake clock, and
// the class of a failure that is cleared by the next pass that works.
func TestHealthCarriesTheErrorClass(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	good := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Good"})
	bad := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Bad"})
	e.f.AddEvent(good, protontest.Event{UID: "a@x", Start: base, End: base + 3600})
	e.login(t, e.req())
	e.f.FailNext(http.MethodGet, "/calendar/v1/"+bad+"/events", http.StatusUnprocessableEntity, 2000, 0)
	s := e.serve(t)
	p := s.nextAny(t, true)
	if exitcode.From(p.Err) != exitcode.Partial {
		t.Fatalf("first pass err %v, want partial", p.Err)
	}
	h := e.health(t)
	if h.LastPass == nil || !h.LastPass.OK || h.LastPass.ErrorClass != "" {
		t.Errorf("last pass %+v: a partial pass got through", h.LastPass)
	}
	g, b := calHealth(t, h, good), calHealth(t, h, bad)
	if g.ErrorClass != "" || g.Error != "" || g.Events != 1 || g.SyncedAt == nil || g.LagSeconds == nil {
		t.Errorf("good calendar: %+v", g)
	}
	if b.ErrorClass != "refused" || b.Error == "" || b.SyncedAt != nil || b.LagSeconds != nil {
		t.Errorf("bad calendar: %+v, want class refused, an error and never synced", b)
	}
	e.clk.advance(90 * time.Second)
	if g := calHealth(t, e.health(t), good); g.LagSeconds == nil || *g.LagSeconds != 90 {
		t.Errorf("lag %v, want 90 s of fake time", g.LagSeconds)
	}
	s.next(t, false) // the failure was a one-off: this pass syncs it
	h = e.health(t)
	if b := calHealth(t, h, bad); b.ErrorClass != "" || b.Error != "" || b.SyncedAt == nil {
		t.Errorf("after a good pass: %+v, want no error class", b)
	}
	s.stop(t)
}

// A 9001 is held until what it blocked works again: a write's until a write
// lands, a pass's until a pass gets through. They are told apart.
func TestHumanVerificationIsHeldUntilAWriteLands(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil).start(t)
	events := "/calendar/v1/" + r.cal + "/events"
	hv := func() []service.HumanVerification { return r.health(t).HumanVerification }
	if got := hv(); len(got) != 0 {
		t.Fatalf("human verification %+v before anything", got)
	}

	// A write's Sync call is answered 9001.
	r.f.FailNext(http.MethodPut, r.syncPath(), http.StatusUnprocessableEntity, api.CodeHumanVerification, 0)
	_, err := r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true})
	wantIs(t, err, service.ErrHumanVerification)
	got := hv()
	if len(got) != 1 || got[0].Source != "write" {
		t.Fatalf("human verification %+v, want one from a write", got)
	}
	since := got[0].Since
	// Asked again later: still one, with its first time.
	r.clk.advance(time.Minute)
	r.f.FailNext(http.MethodGet, events, http.StatusUnprocessableEntity, api.CodeHumanVerification, 0)
	_, err = r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true})
	wantIs(t, err, service.ErrHumanVerification)
	if got := hv(); len(got) != 1 || !got[0].Since.Equal(since) {
		t.Fatalf("human verification %+v, want the first ask's time %v", got, since)
	}
	// A refused write that is no 9001 does not clear it.
	_, err = r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfMatch: `"nope"`})
	if err == nil {
		t.Fatal("a stale If-Match was accepted")
	}
	if got := hv(); len(got) != 1 {
		t.Fatalf("human verification %+v after a write that did not land", got)
	}
	// A pass that gets through does not clear a write's either.
	r.s.next(t, false)
	if got := hv(); len(got) != 1 || got[0].Source != "write" {
		t.Fatalf("human verification %+v after a good pass, want the write's still held", got)
	}
	// A pass's call is answered 9001: a second source.
	second := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Second"})
	r.f.FailNext(http.MethodGet, "/calendar/v1/"+second+"/events", http.StatusUnprocessableEntity, api.CodeHumanVerification, 0)
	p := r.s.nextAny(t, false)
	if exitcode.From(p.Err) != exitcode.Partial {
		t.Fatalf("pass err %v result %+v, want a partial pass", p.Err, p.Result)
	}

	if got := hv(); len(got) != 2 || got[0].Source != "write" || got[1].Source != "sync" {
		t.Fatalf("human verification %+v, want write then sync", got)
	}
	// The next pass gets through: only the sync one clears.
	r.s.next(t, false)
	if got := hv(); len(got) != 1 || got[0].Source != "write" {
		t.Fatalf("human verification %+v after a pass that got through, want the write's only", got)
	}
	// A write lands: it clears.
	if _, err := r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := hv(); len(got) != 0 {
		t.Fatalf("human verification %+v after a write landed", got)
	}
}

// nextAny is next for a pass that may have failed or been partial.
func (s *serving) nextAny(t *testing.T, first bool) service.WatchPass {
	t.Helper()
	if !first {
		s.resume <- struct{}{}
	}
	select {
	case p := <-s.passes:
		return p
	case err := <-s.done:
		t.Fatalf("Serve returned %v before its pass", err)
	}
	return service.WatchPass{}
}
