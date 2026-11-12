package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The executor's tests run the service against the fake Proton during
// Serve: each rig is one logged-in service whose sync loop is paused between
// passes (serving), so a write is the only thing that changes the cache
// unless a test lets a pass run. Its HTTP transport can be hooked (a web-app
// edit in the window between the refresh and the Sync call, an answer lost
// after the write was applied), and its write log is kept.

// green is a Proton palette colour: an update re-sends the row's colour,
// and the fake refuses one off the palette (2011), as Proton does.
const green = "#3CBB3A"

// errReset stands for a connection that broke.
var errReset = errors.New("connection reset by peer")

// transport is the rig's HTTP transport: fn, when set, handles each request
// and may send it (send) or not.
type transport struct {
	mu sync.Mutex
	fn func(r *http.Request, send func() (*http.Response, error)) (*http.Response, error)
}

func (h *transport) set(fn func(r *http.Request, send func() (*http.Response, error)) (*http.Response, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fn = fn
}

func (h *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	fn := h.fn
	h.mu.Unlock()
	send := func() (*http.Response, error) { return http.DefaultTransport.RoundTrip(r) }
	if fn == nil {
		return send()
	}
	return fn(r, send)
}

// logBuffer collects the write log.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type rig struct {
	*env
	cal  string
	tr   *transport
	logs *logBuffer
	s    *serving
	pc   *api.Client // a web-app session on the fake
}

// newRig starts a rig with calendar Work, seeded by seed (nil: empty), after
// Serve's first pass.
func newRig(t *testing.T, fo protontest.Options, seed func(f *protontest.Fake, cal string)) *rig {
	t.Helper()
	e := newEnv(t, fo)
	r := &rig{env: e, tr: &transport{}, logs: &logBuffer{}}
	r.cal = e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	if seed != nil {
		seed(e.f, r.cal)
	}
	svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
		HTTP: &http.Client{Timeout: 10 * time.Second, Transport: r.tr}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	e.svc = svc
	r.pc = api.New(e.f.Session(t, e.f.Username), api.Options{BaseURL: e.f.URL})
	return r
}

// start logs in and runs Serve until its first pass.
func (r *rig) start(t *testing.T) *rig {
	t.Helper()
	r.login(t, r.req())
	ctx, cancel := context.WithCancel(t.Context())
	s := &serving{passes: make(chan service.WatchPass), resume: make(chan struct{}), done: make(chan error, 1), cancel: cancel}
	log := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() {
		s.done <- r.svc.Serve(ctx, service.ServeOptions{Logger: log, OnPass: func(p service.WatchPass) {
			select {
			case s.passes <- p:
			case <-ctx.Done():
				return
			}
			select {
			case <-s.resume:
			case <-ctx.Done():
			}
		}})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-s.done; err != nil {
			t.Errorf("Serve = %v", err)
		}
	})
	r.s = s
	s.next(t, true)
	return r
}

func (r *rig) syncPath() string { return "/calendar/v1/" + r.cal + "/events/sync" }

func (r *rig) isSync(req *http.Request) bool {
	return req.Method == http.MethodPut && req.URL.Path == r.syncPath()
}

func (r *rig) isReadBack(req *http.Request) bool {
	return req.Method == http.MethodGet && req.URL.Path == "/calendar/v1/"+r.cal+"/events" && req.URL.Query().Get("UID") != ""
}

// sendCalls is how many requests reached the sync endpoint's route,
// injected failures included.
func (r *rig) sendCalls() int { return r.f.Calls(http.MethodPut, r.syncPath()) }

// proton lists uid's rows as Proton holds them.
func (r *rig) proton(t *testing.T, uid string) []events.Row {
	t.Helper()
	rows, err := events.ListByUID(t.Context(), r.pc, r.cal, uid)
	if err != nil {
		t.Fatalf("ListByUID: %v", err)
	}
	return rows
}

// raws is the rows' JSON, for an exact comparison.
func raws(rows []events.Row) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = string(row.Raw)
	}
	slices.Sort(out)
	return out
}

// webEdit is an edit made in Proton's web app: the row re-sent with its own
// parts (so its key packets stay, as Proton keeps them, research B.5) and
// other notifications.
func (r *rig) webEdit(t *testing.T, row events.Row, notifications events.NotificationList) {
	t.Helper()
	var w struct {
		SharedEvents, CalendarEvents, AttendeesEvents []events.Part
		Color                                         *string
	}
	if err := json.Unmarshal(row.Raw, &w); err != nil {
		t.Fatal(err)
	}
	one := 1
	body := events.Body{
		WireEvent:     events.WireEvent{SharedEventContent: w.SharedEvents, CalendarEventContent: w.CalendarEvents, AttendeesEventContent: w.AttendeesEvents},
		IsOrganizer:   &one,
		Notifications: events.Value(notifications),
	}
	if w.Color != nil {
		body.Color = events.Value(*w.Color)
	}
	res, err := events.Sync(t.Context(), r.pc, r.cal, r.f.MemberID(r.cal), []events.Op{{Update: &events.Update{ID: row.ID, Body: body}}})
	if err != nil || events.FirstError(res) != nil {
		t.Fatalf("web edit: %v, %v", err, events.FirstError(res))
	}
}

func (r *rig) object(t *testing.T, uid string) service.Object {
	t.Helper()
	return r.objects(t, r.cal, uid)[uid]
}

func (r *rig) put(t *testing.T, name string, body []byte, cond service.Precondition) (service.PutResult, error) {
	t.Helper()
	return r.svc.PutObject(t.Context(), r.cal, name, body, cond)
}

func (r *rig) changes(t *testing.T, token string) service.ChangeSet {
	t.Helper()
	cs, err := r.svc.Changes(t.Context(), r.cal, token)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	return cs
}

// vcal is a PUT body: one VCALENDAR holding the VEVENTs, each given as its
// content lines.
func vcal(vevents ...[]string) []byte {
	lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN"}
	for _, ev := range vevents {
		lines = append(lines, "BEGIN:VEVENT")
		lines = append(lines, ev...)
		lines = append(lines, "END:VEVENT")
	}
	lines = append(lines, "END:VCALENDAR")
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

func newEvent(uid, summary string) []byte {
	return vcal([]string{"UID:" + uid, "DTSTAMP:20270101T000000Z", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600), "SUMMARY:" + summary})
}

// edit is data with old replaced by new, once; it fails the test when data
// lacks old.
func edit(t *testing.T, data []byte, old, new string) []byte {
	t.Helper()
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("the object lacks %q:\n%s", old, data)
	}
	return bytes.Replace(data, []byte(old), []byte(new), 1)
}

// withoutOccurrence is a rendered object without the VEVENT whose
// RECURRENCE-ID is rid.
func withoutOccurrence(t *testing.T, data []byte, rid string) []byte {
	t.Helper()
	parts := strings.Split(string(data), "BEGIN:VEVENT\r\n")
	out := parts[0]
	found := false
	for _, p := range parts[1:] {
		if strings.Contains(p, "RECURRENCE-ID:"+rid+"\r\n") {
			found = true
			// Keep what follows the VEVENT (END:VCALENDAR for the last one).
			_, rest, _ := strings.Cut(p, "END:VEVENT\r\n")
			out += rest
			continue
		}
		out += "BEGIN:VEVENT\r\n" + p
	}
	if !found {
		t.Fatalf("no occurrence %s in:\n%s", rid, data)
	}
	return []byte(out)
}

// wantUnconfirmed: the write's outcome is not confirmed (503).
func wantUnconfirmed(t *testing.T, err error) {
	t.Helper()
	_ = wantExit(t, err, exitcode.Blocked)
	if !errors.Is(err, service.ErrWriteNotConfirmed) {
		t.Fatalf("error %v, want ErrWriteNotConfirmed", err)
	}
}

// wantRefused: a *WriteRefusal with code.
func wantRefused(t *testing.T, err error, code string) {
	t.Helper()
	_ = wantExit(t, err, exitcode.Usage)
	var wr *service.WriteRefusal
	if !errors.As(err, &wr) || wr.Code != code {
		t.Fatalf("error %v, want a refusal %s", err, code)
	}
}

func wantIs(t *testing.T, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) {
		t.Fatalf("error %v, want %v", err, cause)
	}
}

// A create is sent once, read back, and the cache holds Proton's row: the
// object is served at once, the change is in the next Changes, and the
// syncer's next pass finds nothing new.
func TestPutCreatesAndReadsBack(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil).start(t)
	_, tok := r.listing(t, r.cal)

	res, err := r.put(t, "new@x.ics", newEvent("new@x", "Kickoff"), service.Precondition{IfNoneMatchAny: true})
	if err != nil || !res.Created {
		t.Fatalf("PutObject = %+v, %v; want created", res, err)
	}
	calls := r.f.SyncCalls()
	if len(calls) != 1 || len(calls[0].Body.Events) != 1 || calls[0].Body.Events[0].ID != "" || calls[0].Body.Events[0].Event == nil {
		t.Fatalf("sync calls %+v, want one call with one create", calls)
	}
	rows := r.proton(t, "new@x")
	if len(rows) != 1 {
		t.Fatalf("Proton holds %d rows of new@x", len(rows))
	}
	cached := r.events(t, service.EventsRequest{Calendar: r.cal})
	if len(cached) != 1 || cached[0].ID != rows[0].ID || cached[0].UID != "new@x" {
		t.Errorf("cached rows %+v, want Proton's row %s", cached, rows[0].ID)
	}
	contains(t, "the object", r.object(t, "new@x").Data, "SUMMARY:Kickoff")
	if got := changedUIDs(r.changes(t, tok)); !slices.Equal(got, []string{"new@x"}) {
		t.Errorf("changes since the create: %v", got)
	}
	p := r.s.next(t, false)
	if cs := calSync(t, p.Result, r.cal); cs.Added+cs.Updated+cs.Deleted != 0 {
		t.Errorf("the pass after the create changed %+v: the read-back row is not the feed's", cs)
	}

	// No DTSTAMP: the write time, from the service's clock (ruling M4-R8).
	stampless := vcal([]string{"UID:nostamp@x", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600), "SUMMARY:Stampless"})
	if _, err := r.put(t, "nostamp@x.ics", stampless, service.Precondition{IfNoneMatchAny: true}); err != nil {
		t.Fatal(err)
	}
	contains(t, "the object", r.object(t, "nostamp@x").Data, "DTSTAMP:"+t0.UTC().Format("20060102T150405Z"))
}

// An update is visible in the next Changes, with Proton's row, before any
// pass.
func TestPutUpdateIsVisibleInTheNextChanges(t *testing.T) {
	t.Parallel()
	var id string
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		id = f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}).start(t)
	etags, tok := r.listing(t, r.cal)
	obj := r.object(t, "a@x")

	res, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: obj.ETag})
	if err != nil || res.Created {
		t.Fatalf("PutObject = %+v, %v; want an update", res, err)
	}
	calls := r.f.SyncCalls()
	if len(calls) != 1 || len(calls[0].Body.Events) != 1 || calls[0].Body.Events[0].ID != id || calls[0].Body.Events[0].Event == nil {
		t.Fatalf("sync calls %+v, want one update of %s", calls, id)
	}
	// The alarms sent back are the calendar's defaults: the row keeps null.
	if n := string(calls[0].Body.Events[0].Event.Notifications); n != "null" {
		t.Errorf("Notifications sent %s, want null", n)
	}
	cs := r.changes(t, tok)
	now, _ := r.listing(t, r.cal)
	if len(cs.Changed) != 1 || cs.Changed[0].UID != "a@x" || cs.Changed[0].ETag == etags["a@x"] || cs.Changed[0].ETag != now["a@x"] {
		t.Errorf("changes %+v; ETag before %s, now %s", cs, etags["a@x"], now["a@x"])
	}
	data := r.object(t, "a@x").Data
	contains(t, "the object", data, "SUMMARY:Dinner")
	lacks(t, "the object", data, "SUMMARY:Lunch")
	if p := r.s.next(t, false); calSync(t, p.Result, r.cal).Updated != 0 {
		t.Errorf("the pass after the update re-applied it: %+v", calSync(t, p.Result, r.cal))
	}
}

// The precondition is evaluated on Proton's state, not the cache's: an edit
// made in the web app since the last pass makes a stale If-Match fail (412,
// nothing sent), and the cache holds that edit at once.
func TestRefreshBeforePrecondition(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil).start(t)
	if _, err := r.put(t, "r@x.ics", newEvent("r@x", "Review"), service.Precondition{IfNoneMatchAny: true}); err != nil {
		t.Fatal(err)
	}
	obj := r.object(t, "r@x")
	_, tok := r.listing(t, r.cal)
	r.webEdit(t, r.proton(t, "r@x")[0], events.NotificationList{{Type: 1, Trigger: "-PT5M"}})

	_, err := r.put(t, "r@x.ics", edit(t, obj.Data, "SUMMARY:Review", "SUMMARY:Retro"), service.Precondition{IfMatch: obj.ETag})
	_ = wantExit(t, err, exitcode.Usage)
	wantIs(t, err, service.ErrPreconditionFailed)
	if n := len(r.f.SyncCalls()); n != 2 {
		t.Fatalf("%d sync calls, want 2 (the create and the web edit)", n)
	}
	fresh := r.object(t, "r@x")
	if fresh.ETag == obj.ETag {
		t.Error("the cache was not refreshed: same ETag")
	}
	contains(t, "the refreshed object", fresh.Data, "TRIGGER:-PT5M", "SUMMARY:Review")
	if got := changedUIDs(r.changes(t, tok)); !slices.Equal(got, []string{"r@x"}) {
		t.Errorf("changes since the web edit: %v", got)
	}

	if _, err := r.put(t, "r@x.ics", edit(t, fresh.Data, "SUMMARY:Review", "SUMMARY:Retro"), service.Precondition{IfMatch: fresh.ETag}); err != nil {
		t.Fatalf("PutObject on the fresh ETag: %v", err)
	}
	contains(t, "the object", r.object(t, "r@x").Data, "SUMMARY:Retro", "TRIGGER:-PT5M")
}

// A Proton 5xx is answered 503 and sent once; so is a 429, with Proton's
// Retry-After; a whole-call 4xx that is not session-wide is a 403. Nothing
// reached the endpoint, and the cache keeps the object.
func TestProton5xxIs503AndNeverRetried(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}).start(t)
	obj := r.object(t, "a@x")
	body := edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner")
	unchanged := func(t *testing.T) {
		t.Helper()
		if got := r.object(t, "a@x"); got.ETag != obj.ETag || !bytes.Equal(got.Data, obj.Data) {
			t.Errorf("the cached object changed:\n%s", got.Data)
		}
		if n := len(r.f.SyncCalls()); n != 0 {
			t.Errorf("%d calls reached the sync endpoint", n)
		}
	}
	for _, status := range []int{500, 502, 503} {
		before := r.sendCalls()
		r.f.FailNext(http.MethodPut, r.syncPath(), status, 0, 0)
		_, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag})
		wantUnconfirmed(t, err)
		if n := r.sendCalls() - before; n != 1 {
			t.Errorf("HTTP %d: %d requests, want 1 (never retried)", status, n)
		}
		unchanged(t)
	}

	before := r.sendCalls()
	r.f.FailNext(http.MethodPut, r.syncPath(), http.StatusTooManyRequests, 0, 7)
	_, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag})
	_ = wantExit(t, err, exitcode.Blocked)
	if errors.Is(err, service.ErrWriteNotConfirmed) || service.ProtonRetryAfter(err) != 7*time.Second {
		t.Errorf("429: error %v, retry after %v; want a rate limit of 7s", err, service.ProtonRetryAfter(err))
	}
	if n := r.sendCalls() - before; n != 1 {
		t.Errorf("429: %d requests, want 1", n)
	}
	unchanged(t)

	// The keys were read while the member could write: the gate lets the
	// write through and Proton refuses it as a whole.
	r.f.SetPermissions(r.cal, 7)
	before = r.sendCalls()
	_, err = r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag})
	wantRefused(t, err, service.RefuseProtonRefused)
	if strings.Contains(err.Error(), "fake") || strings.Contains(err.Error(), "member") {
		t.Errorf("the refusal quotes Proton: %v", err)
	}
	if n := r.sendCalls() - before; n != 1 {
		t.Errorf("403: %d requests, want 1", n)
	}
	if got := r.object(t, "a@x"); got.ETag != obj.ETag {
		t.Error("the refused write changed the cache")
	}

	r.f.SetPermissions(r.cal, 0)
	if _, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("the write once Proton answers: %v", err)
	}
	contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:Dinner")
}

// A call that may have reached Proton is read back: confirmed, it is a
// success; not applied, or not readable back, it is a 503; the read-back
// runs even when the caller hung up meanwhile.
func TestAmbiguousFailureReadsBack(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
		f.AddEvent(cal, protontest.Event{UID: "b@x", Start: base, End: base + 3600, Color: green, Card: card("b@x", "Gym", base)})
	}).start(t)
	// lost: the request reaches Proton, the answer does not come back.
	lost := func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
		if !r.isSync(req) {
			return send()
		}
		resp, err := send()
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return nil, errReset
	}

	t.Run("AppliedIsASuccess", func(t *testing.T) {
		obj := r.object(t, "a@x")
		r.tr.set(lost)
		defer r.tr.set(nil)
		if _, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: obj.ETag}); err != nil {
			t.Fatalf("PutObject = %v, want the read-back to confirm it", err)
		}
		contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:Dinner")
	})
	t.Run("NotAppliedIs503", func(t *testing.T) {
		obj := r.object(t, "a@x")
		calls := len(r.f.SyncCalls())
		r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
			if r.isSync(req) {
				return nil, errReset // broke before Proton read it: nothing applied
			}
			return send()
		})
		defer r.tr.set(nil)
		_, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Dinner", "SUMMARY:Supper"), service.Precondition{IfMatch: obj.ETag})
		wantUnconfirmed(t, err)
		if n := len(r.f.SyncCalls()) - calls; n != 0 {
			t.Errorf("%d calls reached Proton", n)
		}
		if got := r.object(t, "a@x"); got.ETag != obj.ETag {
			t.Errorf("the cache changed:\n%s", got.Data)
		}
	})
	t.Run("NoReadBackIs503", func(t *testing.T) {
		obj := r.object(t, "a@x")
		var sent sync.Once
		var after bool
		var mu sync.Mutex
		r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.isSync(req):
				sent.Do(func() { after = true })
			case after && r.isReadBack(req):
				return nil, errReset
			}
			return send()
		})
		defer r.tr.set(nil)
		_, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Dinner", "SUMMARY:Brunch"), service.Precondition{IfMatch: obj.ETag})
		wantUnconfirmed(t, err)
		if got := r.object(t, "a@x"); got.ETag != obj.ETag {
			t.Error("the cache changed without a read-back")
		}
	})
	t.Run("CallerGoneStillReadsBack", func(t *testing.T) {
		r.tr.set(nil)
		obj := r.object(t, "b@x") // b@x: the previous subtest left a@x's cache behind Proton
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
			if !r.isSync(req) {
				return send()
			}
			resp, err := send()
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			cancel() // the client hung up while Proton applied the write
			return nil, context.Canceled
		})
		defer r.tr.set(nil)
		_, _ = r.svc.PutObject(ctx, r.cal, "b@x.ics", edit(t, obj.Data, "SUMMARY:Gym", "SUMMARY:Swim"), service.Precondition{IfMatch: obj.ETag})
		contains(t, "the object", r.object(t, "b@x").Data, "SUMMARY:Swim")
	})
	t.Run("AmbiguousDeleteNotAppliedIs503", func(t *testing.T) {
		r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
			if r.isSync(req) {
				return nil, errReset
			}
			return send()
		})
		defer r.tr.set(nil)
		err := r.svc.DeleteObject(t.Context(), r.cal, "b@x.ics", service.Precondition{})
		wantUnconfirmed(t, err)
		if r.object(t, "b@x").Missing || len(r.proton(t, "b@x")) != 1 {
			t.Error("b@x is gone")
		}
	})
	t.Run("AmbiguousDeleteIsReadBack", func(t *testing.T) {
		r.tr.set(lost)
		defer r.tr.set(nil)
		if err := r.svc.DeleteObject(t.Context(), r.cal, "b@x.ics", service.Precondition{}); err != nil {
			t.Fatalf("DeleteObject = %v, want the read-back to confirm it", err)
		}
		if !r.object(t, "b@x").Missing || len(r.proton(t, "b@x")) != 0 {
			t.Error("the delete is not in the cache or not in Proton")
		}
	})
}

// A refused entry is a 403 once the rows are read back: the cache holds
// what Proton has. A duplicate UID (another client created the event in the
// window between the refresh and the call), and a 2001 (the web app bumped
// the master's SEQUENCE in that window: ruling M4-R24) both refresh it.
func TestEntryRefusalIs403AndCacheRefreshed(t *testing.T) {
	t.Parallel()
	var master string
	rid := base + day
	r := newRig(t, protontest.Options{Write: protontest.WriteOptions{RejectDuplicateUID: true}}, func(f *protontest.Fake, cal string) {
		master = f.AddEvent(cal, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Color: green,
			Card: card("series@x", "Standup", base, "RRULE:FREQ=DAILY")})
	}).start(t)
	// before runs once, as the call leaves.
	once := func(before func()) {
		var done sync.Once
		r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
			if r.isSync(req) {
				done.Do(before)
			}
			return send()
		})
	}

	t.Run("DuplicateUID", func(t *testing.T) {
		_, tok := r.listing(t, r.cal)
		once(func() {
			r.f.AddEvent(r.cal, protontest.Event{UID: "dup@x", Start: base, End: base + 3600, Color: green, Card: card("dup@x", "Web", base)})
		})
		defer r.tr.set(nil)
		calls := len(r.f.SyncCalls())
		_, err := r.put(t, "dup@x.ics", newEvent("dup@x", "Mine"), service.Precondition{IfNoneMatchAny: true})
		wantRefused(t, err, service.RefuseProtonRefused)
		if n := len(r.f.SyncCalls()) - calls; n != 1 {
			t.Errorf("%d sync calls, want 1", n)
		}
		if n := len(r.proton(t, "dup@x")); n != 1 {
			t.Errorf("Proton holds %d rows of dup@x, want the web app's one", n)
		}
		contains(t, "the refreshed object", r.object(t, "dup@x").Data, "SUMMARY:Web")
		if got := changedUIDs(r.changes(t, tok)); !slices.Equal(got, []string{"dup@x"}) {
			t.Errorf("changes: %v", got)
		}
	})
	t.Run("SequenceRefusedThenRefreshed", func(t *testing.T) {
		obj := r.object(t, "series@x")
		exception := []string{"UID:series@x", "DTSTAMP:20270101T000000Z", "RECURRENCE-ID:" + stamp(rid),
			"DTSTART:" + stamp(rid+1800), "DTEND:" + stamp(rid+5400), "SUMMARY:Standup moved"}
		body := edit(t, obj.Data, "END:VCALENDAR", "BEGIN:VEVENT\r\n"+strings.Join(exception, "\r\n")+"\r\nEND:VEVENT\r\nEND:VCALENDAR")
		once(func() {
			r.f.UpdateEvent(r.cal, master, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Color: green,
				Card: card("series@x", "Standup", base, "RRULE:FREQ=DAILY", "SEQUENCE:5")})
		})
		defer r.tr.set(nil)
		calls := len(r.f.SyncCalls())
		_, err := r.put(t, "series@x.ics", body, service.Precondition{IfMatch: obj.ETag})
		wantRefused(t, err, service.RefuseProtonRefused)
		if n := len(r.f.SyncCalls()) - calls; n != 1 {
			t.Errorf("%d sync calls, want 1 (never retried)", n)
		}
		fresh := r.object(t, "series@x")
		contains(t, "the refreshed object", fresh.Data, "SEQUENCE:5")
		lacks(t, "the refreshed object", fresh.Data, "RECURRENCE-ID")

		// The client reads the object again and retries: the exception now
		// takes the master's SEQUENCE.
		retry := edit(t, fresh.Data, "END:VCALENDAR", "BEGIN:VEVENT\r\n"+strings.Join(exception, "\r\n")+"\r\nEND:VEVENT\r\nEND:VCALENDAR")
		if _, err := r.put(t, "series@x.ics", retry, service.Precondition{IfMatch: fresh.ETag}); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		contains(t, "the object", r.object(t, "series@x").Data, "RECURRENCE-ID:"+stamp(rid), "SUMMARY:Standup moved")
	})
	t.Run("CreateOfAnEventProtonHasIsNoDuplicate", func(t *testing.T) {
		// Created in the web app after the last pass: the refresh finds it.
		r.f.AddEvent(r.cal, protontest.Event{UID: "late@x", Start: base, End: base + 3600, Color: green, Card: card("late@x", "Late", base)})
		calls := len(r.f.SyncCalls())
		_, err := r.put(t, "late@x.ics", newEvent("late@x", "Mine"), service.Precondition{IfNoneMatchAny: true})
		wantIs(t, err, service.ErrPreconditionFailed)
		if n := len(r.f.SyncCalls()) - calls; n != 0 {
			t.Errorf("%d sync calls", n)
		}
		contains(t, "the refreshed object", r.object(t, "late@x").Data, "SUMMARY:Late")
	})
}

// A write and the syncer's pass of its calendar run one at a time (ruling
// M4-R5): while a pass holds the calendar (its feed request is held here),
// a write of that calendar waits, and reaches Proton only once the pass is
// done.
func TestWritesSerialiseWithTheSyncer(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	})
	waiting := make(chan string, 1)
	r.svc.OnWriteWait(func(calID string) { waiting <- calID })
	r.start(t)
	obj := r.object(t, "a@x")

	inPass, release := make(chan struct{}), make(chan struct{})
	violated := make(chan string, 1)
	var mu sync.Mutex
	held := false
	r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
		calendarRequest := strings.HasPrefix(req.URL.Path, "/calendar/v1/"+r.cal+"/")
		mu.Lock()
		switch {
		case calendarRequest && req.URL.Query().Get("UID") != "" && held:
			select {
			case violated <- req.Method + " " + req.URL.Path:
			default:
			}
		case calendarRequest && req.Method == http.MethodGet && req.URL.Query().Get("UID") == "" && !held:
			held = true
			mu.Unlock()
			close(inPass)
			<-release
			mu.Lock()
			held = false
		}
		mu.Unlock()
		return send()
	})
	r.s.resume <- struct{}{} // the second pass starts and is held in the calendar
	<-inPass

	done := make(chan error, 1)
	go func() {
		_, err := r.svc.PutObject(context.Background(), r.cal, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"),
			service.Precondition{IfMatch: obj.ETag})
		done <- err
	}()
	select {
	case cal := <-waiting:
		if cal != r.cal {
			t.Errorf("the write waited for calendar %q", cal)
		}
	case req := <-violated:
		t.Fatalf("the write sent %s while the pass held the calendar", req)
	case err := <-done:
		t.Fatalf("the write ended (%v) while the pass held the calendar", err)
	}
	close(release)
	r.tr.set(nil)
	select {
	case p := <-r.s.passes:
		if p.Err != nil {
			t.Fatalf("pass: %v", p.Err)
		}
	case err := <-r.s.done:
		t.Fatalf("Serve ended: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the write after the pass: %v", err)
	}
	contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:Dinner")
}

// DropUnbumpedSequence on: Proton answers success but keeps the row for an
// edit that does not bump SEQUENCE (research A.4). The read-back shows the
// row unchanged, so the answer is a 503, never a false success; an edit
// that bumps it is stored and confirmed.
func TestDroppedEditIsNeverASuccess(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{Write: protontest.WriteOptions{DropUnbumpedSequence: true}}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}).start(t)
	before := raws(r.proton(t, "a@x"))
	obj := r.object(t, "a@x")
	_, tok := r.listing(t, r.cal)
	for name, body := range map[string][]byte{
		"title": edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"),
		"alarm": edit(t, obj.Data, "TRIGGER:-PT1H", "TRIGGER:-PT10M"),
	} {
		_, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag})
		if !errors.Is(err, service.ErrWriteNotConfirmed) {
			t.Errorf("%s edit dropped by Proton: %v, want ErrWriteNotConfirmed", name, err)
		}
	}
	if n := len(r.f.SyncCalls()); n != 2 {
		t.Fatalf("%d sync calls, want 2", n)
	}
	if after := raws(r.proton(t, "a@x")); !slices.Equal(after, before) {
		t.Error("Proton's row changed: the switch did not drop the edits")
	}
	if got := r.object(t, "a@x"); got.ETag != obj.ETag {
		t.Errorf("the cache changed:\n%s", got.Data)
	}
	if cs := r.changes(t, tok); len(cs.Changed)+len(cs.Deleted) != 0 {
		t.Errorf("changes after dropped edits: %+v", cs)
	}

	moved := edit(t, edit(t, obj.Data, "DTSTART:"+stamp(base), "DTSTART:"+stamp(base+7200)), "DTEND:"+stamp(base+3600), "DTEND:"+stamp(base+10800))
	if _, err := r.put(t, "a@x.ics", moved, service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("a time edit (SEQUENCE bumped): %v", err)
	}
	contains(t, "the object", r.object(t, "a@x").Data, "DTSTART:"+stamp(base+7200), "SEQUENCE:1")
}

// RefuseNewKeyPacketsOnUpdate on: an update carries no key packet, so
// Proton has nothing to refuse.
func TestUpdatesCarryNoKeyPackets(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{Write: protontest.WriteOptions{RefuseNewKeyPacketsOnUpdate: true}}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green,
			Card: card("a@x", "Lunch", base, "COMMENT:bring the slides")})
	}).start(t)
	obj := r.object(t, "a@x")
	body := edit(t, edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), "COMMENT:bring the slides", "COMMENT:bring nothing")
	if _, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	calls := r.f.SyncCalls()
	if len(calls) != 1 || len(calls[0].Body.Events) != 1 {
		t.Fatalf("sync calls %+v", calls)
	}
	if ev := calls[0].Body.Events[0].Event; ev == nil || ev.SharedKeyPacket != nil || ev.CalendarKeyPacket != nil {
		t.Errorf("the update carries key packets: %+v", ev)
	}
	contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:Dinner", "COMMENT:bring nothing")
}

// Deleting one occurrence (the master's new EXDATE and the deletion of its
// exception row, one call) when the web app deleted that row in the window:
// entries standing alone apply the master and refuse the delete (403); with
// AllOrNothing nothing applies (a whole-call 422: 403). Either way the cache
// holds what Proton has.
func TestOccurrenceDeleteRaceIsTruthful(t *testing.T) {
	t.Parallel()
	rid := base + day
	for _, allOrNothing := range []bool{false, true} {
		name := "EntriesStandAlone"
		if allOrNothing {
			name = "AllOrNothing"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var exception string
			r := newRig(t, protontest.Options{Write: protontest.WriteOptions{AllOrNothing: allOrNothing}}, func(f *protontest.Fake, cal string) {
				f.AddEvent(cal, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Color: green,
					Card: card("series@x", "Standup", base, "RRULE:FREQ=DAILY")})
				exception = f.AddEvent(cal, protontest.Event{UID: "series@x", Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid, Color: green,
					Card: card("series@x", "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid))})
			}).start(t)
			obj := r.object(t, "series@x")
			body := edit(t, withoutOccurrence(t, obj.Data, stamp(rid)), "RRULE:FREQ=DAILY", "RRULE:FREQ=DAILY\r\nEXDATE:"+stamp(rid))
			var done sync.Once
			r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
				if r.isSync(req) {
					done.Do(func() { r.f.DeleteEvent(r.cal, exception) })
				}
				return send()
			})
			_, err := r.put(t, "series@x.ics", body, service.Precondition{IfMatch: obj.ETag})
			wantRefused(t, err, service.RefuseProtonRefused)
			if n := len(r.f.SyncCalls()); n != 1 {
				t.Errorf("%d sync calls, want 1", n)
			}
			rows := r.proton(t, "series@x")
			var exdates struct{ Exdates []int64 }
			if len(rows) != 1 || json.Unmarshal(rows[0].Raw, &exdates) != nil {
				t.Fatalf("Proton holds %d rows", len(rows))
			}
			data := r.object(t, "series@x").Data
			lacks(t, "the refreshed object", data, "RECURRENCE-ID")
			if allOrNothing {
				if len(exdates.Exdates) != 0 {
					t.Errorf("all or nothing, yet Proton's master has EXDATEs %v", exdates.Exdates)
				}
				lacks(t, "the refreshed object", data, "EXDATE")
			} else {
				if !slices.Equal(exdates.Exdates, []int64{rid}) {
					t.Errorf("Proton's master EXDATEs %v, want [%d]", exdates.Exdates, rid)
				}
				contains(t, "the refreshed object", data, "EXDATE:"+stamp(rid))
			}
		})
	}
}

// Key-packet pinning (ruling M4-R17): a row whose key packet changed since
// the cache read it is not written to (403 untrusted-key, logged without
// secrets), nothing is sent, and the cache keeps the packets it trusts.
func TestChangedKeyPacketRefusesTheWrite(t *testing.T) {
	t.Parallel()
	var id string
	seed := protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)}
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) { id = f.AddEvent(cal, seed) }).start(t)
	obj := r.object(t, "a@x")
	old := r.proton(t, "a@x")[0]
	// The same content under another session key: the server swapped the
	// row's key packets.
	r.f.UpdateEvent(r.cal, id, seed)
	swapped := r.proton(t, "a@x")[0]
	var packets struct{ SharedKeyPacket string }
	if json.Unmarshal(swapped.Raw, &packets) != nil || packets.SharedKeyPacket == "" || bytes.Contains(old.Raw, []byte(packets.SharedKeyPacket)) {
		t.Fatal("the fake did not change the key packet")
	}

	_, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: obj.ETag})
	wantRefused(t, err, service.RefuseUntrustedKey)
	if n := len(r.f.SyncCalls()); n != 0 {
		t.Errorf("%d sync calls", n)
	}
	if got := r.object(t, "a@x"); got.ETag != obj.ETag {
		t.Error("the untrusted row reached the cache")
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "key packet changed") || !strings.Contains(logs, id) || !strings.Contains(logs, "SharedKeyPacket") {
		t.Errorf("no warning naming the row and the packet:\n%s", logs)
	}
	for _, secret := range []string{packets.SharedKeyPacket, "Lunch", "Dinner"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}

	// A delete encrypts nothing: the owner may remove the event (ruling
	// M4-R27).
	if err := r.svc.DeleteObject(t.Context(), r.cal, "a@x.ics", service.Precondition{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if len(r.proton(t, "a@x")) != 0 || !r.object(t, "a@x").Missing {
		t.Error("the delete did not happen")
	}
}

// Aliases (ruling M4-R6): a create at a name that is not its UID's
// canonical name is served there; the UID and the name belong to one object
// each (409 with the other object's name); bad names are refused before
// anything is read.
func TestAliasesAndNames(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}).start(t)
	conflict := func(t *testing.T, err error, href string) {
		t.Helper()
		_ = wantExit(t, err, exitcode.Usage)
		var ce *service.UIDConflictError
		if !errors.As(err, &ce) || ce.Href != href {
			t.Fatalf("error %v, want a UID conflict naming %q", err, href)
		}
	}

	t.Run("CreateAtAnAlias", func(t *testing.T) {
		res, err := r.put(t, "1234-abcd.ics", newEvent("u1@x", "Aliased"), service.Precondition{IfNoneMatchAny: true})
		if err != nil || !res.Created {
			t.Fatalf("PutObject = %+v, %v", res, err)
		}
		obj := r.object(t, "u1@x")
		if _, err := r.put(t, "1234-abcd.ics", edit(t, obj.Data, "SUMMARY:Aliased", "SUMMARY:Renamed"), service.Precondition{IfMatch: obj.ETag}); err != nil {
			t.Fatalf("an update at the alias: %v", err)
		}
		contains(t, "the object", r.object(t, "u1@x").Data, "SUMMARY:Renamed")
	})
	t.Run("Conflicts", func(t *testing.T) {
		calls := len(r.f.SyncCalls())
		// The UID lives at its alias, not at its canonical name.
		_, err := r.put(t, "u1@x.ics", newEvent("u1@x", "Again"), service.Precondition{})
		conflict(t, err, "1234-abcd.ics")
		// The alias names another UID.
		_, err = r.put(t, "1234-abcd.ics", newEvent("u2@x", "Other"), service.Precondition{})
		conflict(t, err, "1234-abcd.ics")
		// The canonical name of a UID that is not the body's.
		_, err = r.put(t, "a@x.ics", newEvent("u3@x", "Other"), service.Precondition{})
		conflict(t, err, "a@x.ics")
		// A UID that lives at its canonical name, sent to another name.
		_, err = r.put(t, "elsewhere.ics", newEvent("a@x", "Moved"), service.Precondition{})
		conflict(t, err, "a@x.ics")
		if err := r.svc.DeleteObject(t.Context(), r.cal, "u1@x.ics", service.Precondition{}); !errors.Is(err, service.ErrNoSuchObject) {
			t.Errorf("DeleteObject at the aliased UID's canonical name = %v, want ErrNoSuchObject", err)
		}
		if n := len(r.f.SyncCalls()) - calls; n != 0 {
			t.Errorf("%d sync calls", n)
		}
	})
	t.Run("OddUIDAtItsCanonicalName", func(t *testing.T) {
		name := service.ObjectName("odd uid/1")
		if !strings.HasPrefix(name, "~") {
			t.Fatalf("canonical name %q", name)
		}
		if _, err := r.put(t, name, newEvent("odd uid/1", "Odd"), service.Precondition{IfNoneMatchAny: true}); err != nil {
			t.Fatal(err)
		}
		if err := r.svc.DeleteObject(t.Context(), r.cal, name, service.Precondition{}); err != nil {
			t.Fatalf("DeleteObject at the canonical name: %v", err)
		}
	})
	t.Run("DeleteAtTheAlias", func(t *testing.T) {
		if err := r.svc.DeleteObject(t.Context(), r.cal, "1234-abcd.ics", service.Precondition{}); err != nil {
			t.Fatalf("DeleteObject at the alias: %v", err)
		}
		if len(r.proton(t, "u1@x")) != 0 || !r.object(t, "u1@x").Missing {
			t.Error("u1@x is still in Proton or in the cache")
		}
	})
	t.Run("BadNames", func(t *testing.T) {
		requests := r.f.Calls("", "/")
		for _, name := range []string{"", ".", "..", "a/b.ics", "tab\t.ics", "nul\x00.ics", "del\x7f.ics", "\xff.ics", strings.Repeat("n", 252) + ".ics"} {
			_, err := r.put(t, name, newEvent("bad@x", "Bad"), service.Precondition{})
			wantRefused(t, err, service.RefuseBadName)
			if err := r.svc.DeleteObject(t.Context(), r.cal, name, service.Precondition{}); !errors.Is(err, service.ErrNoSuchObject) {
				t.Errorf("DeleteObject(%q) = %v", name, err)
			}
		}
		if n := r.f.Calls("", "/") - requests; n != 0 {
			t.Errorf("bad names made %d requests", n)
		}
		if _, err := r.put(t, strings.Repeat("n", 251)+".ics", newEvent("long@x", "Long"), service.Precondition{}); err != nil {
			t.Errorf("a 255-byte name: %v", err)
		}
	})
}

// The preconditions, the gate and the refusals send nothing to Proton; an
// unchanged body sends nothing either and succeeds.
func TestPreconditionsAndRefusalsSendNothing(t *testing.T) {
	t.Parallel()
	var readOnly string
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
		f.AddEvent(cal, protontest.Event{UID: "broken@x", Start: base, End: base + 3600, BrokenPart: protontest.SharedEncryptedPart,
			Card: card("broken@x", "Secret", base)})
		readOnly = f.AddCalendar(f.Username, protontest.Calendar{Name: "Shared", Permissions: 1})
		f.AddEvent(readOnly, protontest.Event{UID: "ro@x", Start: base, End: base + 3600, Card: card("ro@x", "Theirs", base)})
	}).start(t)
	obj := r.object(t, "a@x")
	body := edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner")
	none := service.Precondition{}
	// Written by pcb: its parts are as pcb writes them, so the same body
	// again changes nothing (a seeded row's parts carry VERSION and PRODID,
	// which a write drops).
	if _, err := r.put(t, "clean@x.ics", newEvent("clean@x", "Clean"), service.Precondition{IfNoneMatchAny: true}); err != nil {
		t.Fatal(err)
	}
	clean := r.object(t, "clean@x")
	for _, c := range []struct {
		name string
		run  func() error
		want error
		code string
	}{
		{"StaleIfMatch", func() error {
			_, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: `"v1-stale"`})
			return err
		}, service.ErrPreconditionFailed, ""},
		{"IfNoneMatchOnAnObject", func() error {
			_, err := r.put(t, "a@x.ics", body, service.Precondition{IfNoneMatchAny: true})
			return err
		}, service.ErrPreconditionFailed, ""},
		{"IfMatchAnyOnNothing", func() error {
			_, err := r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfMatch: "*"})
			return err
		}, service.ErrPreconditionFailed, ""},
		{"DeleteStale", func() error {
			return r.svc.DeleteObject(t.Context(), r.cal, "a@x.ics", service.Precondition{IfMatch: `"v1-stale"`})
		}, service.ErrPreconditionFailed, ""},
		{"DeleteMissing", func() error { return r.svc.DeleteObject(t.Context(), r.cal, "n@x.ics", none) }, service.ErrNoSuchObject, ""},
		{"ReadOnlyCalendar", func() error {
			_, err := r.svc.PutObject(t.Context(), readOnly, "ro@x.ics", newEvent("ro@x", "Mine"), none)
			return err
		}, service.ErrNotWritable, ""},
		{"DeleteInReadOnlyCalendar", func() error { return r.svc.DeleteObject(t.Context(), readOnly, "ro@x.ics", none) }, service.ErrNotWritable, ""},
		{"Placeholder", func() error {
			_, err := r.put(t, "broken@x.ics", newEvent("broken@x", "Overwrite"), none)
			return err
		}, nil, service.RefusePlaceholder},
		{"BodyRefused", func() error {
			_, err := r.put(t, "a@x.ics", edit(t, body, "SUMMARY:Dinner", "SUMMARY:Dinner\r\nRDATE:20270120T080000Z"), none)
			return err
		}, nil, "pcb:rdate"},
		{"Unchanged", func() error {
			_, err := r.put(t, "clean@x.ics", clean.Data, service.Precondition{IfMatch: clean.ETag})
			return err
		}, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			readBacks := r.f.Calls(http.MethodGet, "/calendar/v1/"+readOnly+"/events")
			sends := r.sendCalls()
			err := c.run()
			switch {
			case c.code != "":
				wantRefused(t, err, c.code)
			case c.want != nil:
				_ = wantExit(t, err, exitcode.Usage)
				wantIs(t, err, c.want)
			case err != nil:
				t.Fatalf("error %v", err)
			}
			if n := r.sendCalls() - sends; n != 0 {
				t.Errorf("%d requests to the sync endpoint", n)
			}
			if n := r.f.Calls(http.MethodGet, "/calendar/v1/"+readOnly+"/events") - readBacks; n != 0 {
				t.Errorf("the read-only calendar was read %d times", n)
			}
		})
	}
	if _, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("the exact If-Match: %v", err)
	}
	contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:Dinner")
}

// Writes run only during Serve: before it and after it they are refused
// with ErrNotServing and make no request.
func TestWritesNeedServe(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	})
	r.login(t, r.req())
	r.sync(t, false)
	notServing := func(t *testing.T) {
		t.Helper()
		requests := r.f.Calls("", "/")
		_, err := r.put(t, "a@x.ics", newEvent("a@x", "Dinner"), service.Precondition{})
		_ = wantExit(t, err, exitcode.Blocked)
		wantIs(t, err, service.ErrNotServing)
		err = r.svc.DeleteObject(t.Context(), r.cal, "a@x.ics", service.Precondition{})
		_ = wantExit(t, err, exitcode.Blocked)
		wantIs(t, err, service.ErrNotServing)
		if n := r.f.Calls("", "/") - requests; n != 0 {
			t.Errorf("%d requests", n)
		}
	}
	notServing(t)

	ctx, cancel := context.WithCancel(t.Context())
	passed := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		done <- r.svc.Serve(ctx, service.ServeOptions{OnPass: func(service.WatchPass) { once.Do(func() { close(passed) }) }})
	}()
	<-passed
	if err := r.svc.DeleteObject(t.Context(), r.cal, "a@x.ics", service.Precondition{}); err != nil {
		t.Fatalf("DeleteObject during Serve: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	notServing(t)
}

// A delete removes every row of the UID in one call, is read back, and the
// UID is deleted in the next Changes, with nothing left for the next pass.
func TestDeleteReadsBack(t *testing.T) {
	t.Parallel()
	rid := base + day
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY",
			Card: card("series@x", "Standup", base, "RRULE:FREQ=DAILY")})
		f.AddEvent(cal, protontest.Event{UID: "series@x", Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid,
			Card: card("series@x", "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid))})
	}).start(t)
	etags, tok := r.listing(t, r.cal)
	if err := r.svc.DeleteObject(t.Context(), r.cal, "series@x.ics", service.Precondition{IfMatch: etags["series@x"]}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	calls := r.f.SyncCalls()
	if len(calls) != 1 || len(calls[0].Body.Events) != 2 {
		t.Fatalf("sync calls %+v, want one call deleting both rows", calls)
	}
	if n := len(r.proton(t, "series@x")); n != 0 {
		t.Errorf("Proton still holds %d rows", n)
	}
	if !r.object(t, "series@x").Missing {
		t.Error("the cache still serves the object")
	}
	if cs := r.changes(t, tok); !slices.Equal(cs.Deleted, []string{"series@x"}) || len(cs.Changed) != 0 {
		t.Errorf("changes %+v", cs)
	}
	if p := r.s.next(t, false); calSync(t, p.Result, r.cal).Deleted != 0 {
		t.Errorf("the pass deleted again: %+v", calSync(t, p.Result, r.cal))
	}
}
