package service_test

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
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

// waitLimit bounds a test's wait for another goroutine: past it the test
// fails rather than hangs.
const waitLimit = 10 * time.Second

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

// webPut is an edit made in Proton's web app through the write endpoint:
// body planned over uid's rows as Proton holds them, sealed with the
// account's keys; the rows keep their key packets, as Proton keeps them
// (research B.5).
func (r *rig) webPut(t *testing.T, uid string, body []byte) {
	t.Helper()
	g := cache.Group{UID: uid}
	for _, row := range r.proton(t, uid) {
		g.Rows = append(g.Rows, cache.StoredRow{ID: row.ID, UID: row.UID, Start: row.Start, End: row.End, FullDay: row.FullDay,
			RecurrenceID: row.RecurrenceID, RRule: row.RRule, LastEdit: row.LastEditTime, Raw: row.Raw})
	}
	addr := r.f.AddressKeyRing()
	keys := service.GroupKeys{Calendar: r.f.CalendarKeyRing(r.cal), Own: func(string) *crypto.KeyRing { return addr }}
	in, err := ical.ParseWrite(body)
	if err != nil {
		t.Fatal(err)
	}
	p, err := service.Plan(service.OpenGroup(g, keys), in, service.Defaults{PartDay: []ical.Notification{{Type: 1, Trigger: "-PT1H"}},
		Signer: addr, CalendarPrimary: r.f.CalendarKeyRing(r.cal), Now: t0})
	if err != nil || len(p.Ops) == 0 {
		t.Fatalf("web edit plan: %d ops, %v", len(p.Ops), err)
	}
	res, err := events.Sync(context.Background(), r.pc, r.cal, r.f.MemberID(r.cal), p.Ops)
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
	t.Run("CreateNotAppliedIs503", func(t *testing.T) {
		r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
			if r.isSync(req) {
				return nil, errReset
			}
			return send()
		})
		defer r.tr.set(nil)
		_, err := r.put(t, "c@x.ics", newEvent("c@x", "Lost"), service.Precondition{IfNoneMatchAny: true})
		wantUnconfirmed(t, err)
		if !r.object(t, "c@x").Missing || len(r.proton(t, "c@x")) != 0 {
			t.Error("c@x exists")
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

// A refused entry is read back, and the cache holds what Proton has (ruling
// M4-R34): a refusal met by a concurrent change of the event is a 412 (a
// duplicate UID: another client created the event in the window between
// the refresh and the call; a 2001: the web app bumped the master's
// SEQUENCE in that window), one without is a 403.
func TestEntryRefusalIsReadBack(t *testing.T) {
	t.Parallel()
	rid := base + day
	r := newRig(t, protontest.Options{Write: protontest.WriteOptions{RejectDuplicateUID: true}}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Color: green,
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
		_ = wantExit(t, err, exitcode.Usage)
		wantIs(t, err, service.ErrPreconditionFailed)
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
		// Written by pcb, so the body's master is unchanged: the call holds
		// the exception's create alone.
		series := vcal([]string{"UID:pcbseries@x", "DTSTAMP:20270101T000000Z", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600),
			"RRULE:FREQ=DAILY", "SUMMARY:Standup"})
		if _, err := r.put(t, "pcbseries@x.ics", series, service.Precondition{IfNoneMatchAny: true}); err != nil {
			t.Fatal(err)
		}
		obj := r.object(t, "pcbseries@x")
		exception := []string{"UID:pcbseries@x", "DTSTAMP:20270101T000000Z", "RECURRENCE-ID:" + stamp(rid),
			"DTSTART:" + stamp(rid+1800), "DTEND:" + stamp(rid+5400), "SUMMARY:Standup moved"}
		body := edit(t, obj.Data, "END:VCALENDAR", "BEGIN:VEVENT\r\n"+strings.Join(exception, "\r\n")+"\r\nEND:VEVENT\r\nEND:VCALENDAR")
		// The web app bumps the master's SEQUENCE in the window (through the
		// write endpoint: its key packets stay).
		bumped := edit(t, obj.Data, "RRULE:FREQ=DAILY", "RRULE:FREQ=DAILY\r\nSEQUENCE:5")
		once(func() { r.webPut(t, "pcbseries@x", bumped) })
		defer r.tr.set(nil)
		calls := len(r.f.SyncCalls())
		_, err := r.put(t, "pcbseries@x.ics", body, service.Precondition{IfMatch: obj.ETag})
		_ = wantExit(t, err, exitcode.Usage)
		wantIs(t, err, service.ErrPreconditionFailed)
		if n := len(r.f.SyncCalls()) - calls; n != 2 {
			t.Errorf("%d sync calls, want 2: the web edit and ours, never retried", n)
		}
		fresh := r.object(t, "pcbseries@x")
		contains(t, "the refreshed object", fresh.Data, "SEQUENCE:5")
		lacks(t, "the refreshed object", fresh.Data, "RECURRENCE-ID")

		// The client reads the object again and retries: the exception now
		// takes the master's SEQUENCE.
		retry := edit(t, fresh.Data, "END:VCALENDAR", "BEGIN:VEVENT\r\n"+strings.Join(exception, "\r\n")+"\r\nEND:VEVENT\r\nEND:VCALENDAR")
		if _, err := r.put(t, "pcbseries@x.ics", retry, service.Precondition{IfMatch: fresh.ETag}); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		contains(t, "the object", r.object(t, "pcbseries@x").Data, "RECURRENCE-ID:"+stamp(rid), "SUMMARY:Standup moved")
	})
	t.Run("RefusalWithoutARaceIs403", func(t *testing.T) {
		// A colour off Proton's palette, re-sent with any update: Proton
		// refuses the entry (2011) and nothing else changed.
		r.f.AddEvent(r.cal, protontest.Event{UID: "odd@x", Start: base, End: base + 3600, Color: "#123456", Card: card("odd@x", "Odd", base)})
		r.s.next(t, false)
		obj := r.object(t, "odd@x")
		calls := len(r.f.SyncCalls())
		_, err := r.put(t, "odd@x.ics", edit(t, obj.Data, "SUMMARY:Odd", "SUMMARY:Even"), service.Precondition{IfMatch: obj.ETag})
		wantRefused(t, err, service.RefuseProtonRefused)
		if n := len(r.f.SyncCalls()) - calls; n != 1 {
			t.Errorf("%d sync calls, want 1", n)
		}
		if got := r.object(t, "odd@x"); got.ETag != obj.ETag {
			t.Error("the refused write changed the cache")
		}
	})
	t.Run("CreateBesideARefusalIs403", func(t *testing.T) {
		// One call: the edit of an exception whose colour is off the
		// palette (refused, 2011) and the create of another exception
		// (applied). The new row is the create's, and the rows read back,
		// spelled otherwise, are the same by meaning: no race, a 403.
		rid1, rid2 := base+day, base+2*day
		r.f.AddEvent(r.cal, protontest.Event{UID: "mixed@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Color: green,
			Card: card("mixed@x", "Standup", base, "RRULE:FREQ=DAILY")})
		r.f.AddEvent(r.cal, protontest.Event{UID: "mixed@x", Start: rid1 + 1800, End: rid1 + 5400, RecurrenceID: &rid1, Color: "#123456",
			Card: card("mixed@x", "Moved", rid1+1800, "RECURRENCE-ID:"+stamp(rid1))})
		r.s.next(t, false)
		obj := r.object(t, "mixed@x")
		later := []string{"UID:mixed@x", "DTSTAMP:20270101T000000Z", "RECURRENCE-ID:" + stamp(rid2),
			"DTSTART:" + stamp(rid2+1800), "DTEND:" + stamp(rid2+5400), "SUMMARY:Standup later"}
		body := edit(t, edit(t, obj.Data, "SUMMARY:Moved", "SUMMARY:Moved again"),
			"END:VCALENDAR", "BEGIN:VEVENT\r\n"+strings.Join(later, "\r\n")+"\r\nEND:VEVENT\r\nEND:VCALENDAR")
		r.respellReadBack(t)
		defer r.tr.set(nil)
		_, err := r.put(t, "mixed@x.ics", body, service.Precondition{IfMatch: obj.ETag})
		wantRefused(t, err, service.RefuseProtonRefused)
		if n := len(r.proton(t, "mixed@x")); n != 3 {
			t.Errorf("Proton holds %d rows of mixed@x, want 3: the create applied", n)
		}
		data := r.object(t, "mixed@x").Data
		contains(t, "the refreshed object", data, "SUMMARY:Standup later", "SUMMARY:Moved")
		lacks(t, "the refreshed object", data, "SUMMARY:Moved again")
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
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo) // a failure below must not leave the pass held
	select {         // the second pass starts and is held in the calendar
	case r.s.resume <- struct{}{}:
	case <-time.After(waitLimit):
		t.Fatal("the sync loop never took the resume")
	}
	select {
	case <-inPass:
	case <-time.After(waitLimit):
		t.Fatal("the second pass never reached the calendar")
	}

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
	case <-time.After(waitLimit):
		t.Fatal("the write neither waited for the calendar nor ended")
	}
	letGo()
	r.tr.set(nil)
	select {
	case p := <-r.s.passes:
		if p.Err != nil {
			t.Fatalf("pass: %v", p.Err)
		}
	case err := <-r.s.done:
		t.Fatalf("Serve ended: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("the pass never ended")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the write after the pass: %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("the write never ended after the pass")
	}
	contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:Dinner")
}

// DropUnbumpedSequence on: Proton answers success but keeps the row for an
// edit that does not bump SEQUENCE (research A.4). The read-back shows the
// row unchanged, so the answer is a 503, never a false success; an edit
// that bumps it is stored and confirmed.
func TestDroppedEditIsNeverASuccess(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{Write: protontest.WriteOptions{DropUnbumpedSequence: true}}, nil).start(t)
	// Written by pcb (a create is never dropped): a title edit then changes
	// only the shared encrypted part, an alarm edit only Notifications.
	alarmed := vcal([]string{"UID:a@x", "DTSTAMP:20270101T000000Z", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600), "SUMMARY:Lunch",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT1H", "DESCRIPTION:Reminder", "END:VALARM"})
	if _, err := r.put(t, "a@x.ics", alarmed, service.Precondition{IfNoneMatchAny: true}); err != nil {
		t.Fatal(err)
	}
	before := raws(r.proton(t, "a@x"))
	obj := r.object(t, "a@x")
	_, tok := r.listing(t, r.cal)
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"title", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner")},
		{"alarm", edit(t, obj.Data, "TRIGGER:-PT1H", "TRIGGER:-PT10M")},
	} {
		_, err := r.put(t, "a@x.ics", c.body, service.Precondition{IfMatch: obj.ETag})
		if !errors.Is(err, service.ErrWriteNotConfirmed) {
			t.Errorf("%s edit dropped by Proton: %v, want ErrWriteNotConfirmed", c.name, err)
		}
	}
	if n := len(r.f.SyncCalls()); n != 3 {
		t.Fatalf("%d sync calls, want 3", n)
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
// exception row, one call) when the web app deleted that row in the window
// (ruling M4-R34): entries standing alone apply the master and refuse the
// delete, and Proton then holds what was asked, a success; with
// AllOrNothing nothing applies (a whole-call 422) and the exception is gone
// all the same, a concurrent change: 412. Either way the cache holds what
// Proton has.
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
			if allOrNothing {
				_ = wantExit(t, err, exitcode.Usage)
				wantIs(t, err, service.ErrPreconditionFailed)
			} else if err != nil {
				t.Fatalf("PutObject = %v, want a success: Proton holds what was asked", err)
			}
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

	// A sync pass stores the swapped row: the pin is still the first
	// packets (ruling M4-R30), so the write stays refused.
	if p := r.s.next(t, false); calSync(t, p.Result, r.cal).Updated != 1 {
		t.Fatalf("the pass did not store the swapped row: %+v", calSync(t, p.Result, r.cal))
	}
	synced := r.object(t, "a@x")
	if synced.ETag == obj.ETag {
		t.Fatal("the cache still holds the first row")
	}
	_, err = r.put(t, "a@x.ics", edit(t, synced.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: synced.ETag})
	wantRefused(t, err, service.RefuseUntrustedKey)
	if n := len(r.f.SyncCalls()); n != 0 {
		t.Errorf("%d sync calls after the pass", n)
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
		// Both: the UID lives elsewhere and the name holds another UID; the
		// answer names where the UID lives.
		_, err = r.put(t, "1234-abcd.ics", newEvent("a@x", "Moved"), service.Precondition{})
		conflict(t, err, "a@x.ics")
		reads := r.f.Calls(http.MethodGet, "/calendar/v1/"+r.cal+"/events")
		if err := r.svc.DeleteObject(t.Context(), r.cal, "u1@x.ics", service.Precondition{}); !errors.Is(err, service.ErrNoSuchObject) {
			t.Errorf("DeleteObject at the aliased UID's canonical name = %v, want ErrNoSuchObject", err)
		}
		if n := r.f.Calls(http.MethodGet, "/calendar/v1/"+r.cal+"/events") - reads; n != 0 {
			t.Errorf("a name serving nothing was looked up in Proton (%d requests)", n)
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
	if cs := r.changes(t, tok); !slices.Equal(deletedUIDs(cs), []string{"series@x"}) || len(cs.Changed) != 0 {
		t.Errorf("changes %+v", cs)
	}
	if p := r.s.next(t, false); calSync(t, p.Result, r.cal).Deleted != 0 {
		t.Errorf("the pass deleted again: %+v", calSync(t, p.Result, r.cal))
	}
}

// rewriteReadBack makes the rig's transport hand every row of a read-back
// (a UID listing after the write left) through fn, as if Proton served it
// so.
func (r *rig) rewriteReadBack(t *testing.T, fn func(row map[string]any)) {
	t.Helper()
	r.rewriteUIDPages(t, true, func(page map[string]any) {
		rows, _ := page["Events"].([]any)
		for _, row := range rows {
			fn(row.(map[string]any))
		}
	})
}

// rewriteRefresh makes the rig's transport hand every row of a write's
// refresh (a UID listing before any Sync call left) through fn.
func (r *rig) rewriteRefresh(t *testing.T, fn func(row map[string]any)) {
	t.Helper()
	r.rewriteUIDPages(t, false, func(page map[string]any) {
		rows, _ := page["Events"].([]any)
		for _, row := range rows {
			fn(row.(map[string]any))
		}
	})
}

// rewriteUIDPages makes the rig's transport hand each page of a UID
// listing through fn, as if Proton served it so: the read-backs (UID
// listings once a Sync call left) when readBack, else the refreshes before
// it.
func (r *rig) rewriteUIDPages(t *testing.T, readBack bool, fn func(page map[string]any)) {
	t.Helper()
	r.rewriteUIDPagesAs(t, readBack, fn, json.Marshal)
}

// respellReadBack makes the rig's transport serve every read-back indented:
// each row the same by meaning (and ETag), in other bytes.
func (r *rig) respellReadBack(t *testing.T) {
	t.Helper()
	r.rewriteUIDPagesAs(t, true, func(map[string]any) {}, func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") })
}

// rewriteUIDPagesAs is rewriteUIDPages with the page encoded by marshal.
func (r *rig) rewriteUIDPagesAs(t *testing.T, readBack bool, fn func(page map[string]any), marshal func(any) ([]byte, error)) {
	t.Helper()
	var mu sync.Mutex
	sent := false
	r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
		mu.Lock()
		if r.isSync(req) {
			sent = true
		}
		hit := sent == readBack && r.isReadBack(req)
		mu.Unlock()
		resp, err := send()
		if err != nil || !hit {
			return resp, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var page map[string]any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&page); err != nil {
			t.Errorf("UID listing page: %v", err)
			return nil, err
		}
		fn(page)
		out, err := marshal(page)
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Del("Content-Length")
		return resp, nil
	})
}

// pins reads the pins of the rows ids from the cache file, as Serve wrote
// them.
func (r *rig) pins(t *testing.T, ids ...string) map[string]cache.Pin {
	t.Helper()
	db, err := cache.OpenReadOnly(cache.Path(r.dirs.State), r.f.UserID(r.f.Username))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var out map[string]cache.Pin
	if err := db.Snapshot(t.Context(), r.cal, func(v cache.View) error {
		var err error
		out, err = v.Pins(ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// wrapped is s cut into CRLF-ended lines of 40 characters: the same base64
// in another spelling.
func wrapped(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 40 {
		b.WriteString(s[i:min(i+40, len(s))] + "\r\n")
	}
	return b.String()
}

// cardParts are a row's card parts, every list.
func cardParts(row map[string]any) []map[string]any {
	var out []map[string]any
	for _, list := range []string{"SharedEvents", "CalendarEvents", "AttendeesEvents"} {
		parts, _ := row[list].([]any)
		for _, p := range parts {
			out = append(out, p.(map[string]any))
		}
	}
	return out
}

// encrypted reports whether a part, as decoded with UseNumber, is
// encrypted.
func encrypted(part map[string]any) bool {
	n, _ := part["Type"].(json.Number).Int64()
	return n&1 != 0
}

// The read-back is compared by meaning (ruling M4-R32): Proton serving the
// same parts in another armour or base64 spelling confirms the write; a
// part with other plaintext, or one whose signature does not verify, does
// not.
func TestReadBackComparesPlaintext(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil).start(t)
	if _, err := r.put(t, "a@x.ics", newEvent("a@x", "Lunch"), service.Precondition{IfNoneMatchAny: true}); err != nil {
		t.Fatal(err)
	}
	stored := r.proton(t, "a@x")[0]
	var old struct{ SharedEvents []map[string]any }
	if err := json.Unmarshal(stored.Raw, &old); err != nil || len(old.SharedEvents) != 2 {
		t.Fatalf("the stored row's shared parts: %v", err)
	}
	// put retitles the event. No If-Match, and the body is built afresh: a
	// read-back rewritten below leaves the cache with what Proton did not
	// store.
	put := func(t *testing.T, to string) error {
		t.Helper()
		_, err := r.put(t, "a@x.ics", newEvent("a@x", to), service.Precondition{})
		return err
	}

	t.Run("ReEncodedIsConfirmed", func(t *testing.T) {
		defer r.tr.set(nil)
		r.rewriteReadBack(t, func(row map[string]any) {
			for _, p := range cardParts(row) {
				if encrypted(p) {
					p["Data"] = wrapped(p["Data"].(string))
				}
				var kept []string
				for _, line := range strings.Split(p["Signature"].(string), "\n") {
					if !strings.HasPrefix(line, "Comment:") && !strings.HasPrefix(line, "Version:") {
						kept = append(kept, line)
					}
				}
				p["Signature"] = strings.Join(kept, "\r\n")
			}
		})
		if err := put(t, "Dinner"); err != nil {
			t.Fatalf("a write Proton serves re-encoded: %v", err)
		}
		contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:Dinner")
	})
	t.Run("OtherPlaintextIsNotConfirmed", func(t *testing.T) {
		defer r.tr.set(nil)
		// The encrypted shared part read back is the first one, Lunch: a
		// part of ours, signed, under the row's key, but not what was sent.
		r.rewriteReadBack(t, func(row map[string]any) {
			parts := row["SharedEvents"].([]any)
			for i, p := range parts {
				if encrypted(p.(map[string]any)) {
					parts[i] = old.SharedEvents[i]
				}
			}
		})
		wantUnconfirmed(t, put(t, "Supper"))
	})
	t.Run("UnverifiedIsNotConfirmed", func(t *testing.T) {
		defer r.tr.set(nil)
		// The encrypted part's signature is the signed part's: well formed,
		// ours, over other bytes.
		r.rewriteReadBack(t, func(row map[string]any) {
			var clear string
			for _, p := range cardParts(row) {
				if !encrypted(p) {
					clear = p["Signature"].(string)
					break
				}
			}
			for _, p := range cardParts(row) {
				if encrypted(p) {
					p["Signature"] = clear
				}
			}
		})
		wantUnconfirmed(t, put(t, "Brunch"))
	})
}

// A create is pinned with the key packets pcb sent (ruling M4-R36). A
// read-back spelling them otherwise confirms it. One under other packets
// (the server re-encrypted the event under a session key of its own) is a
// 503, logged without secrets, and the row's pin stays what was sent: the
// server's key is never written under, while the event as Proton really
// holds it stays writable.
func TestCreatePinsTheSentPackets(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil).start(t)
	sentPin := func(t *testing.T) cache.Pin {
		t.Helper()
		calls := r.f.SyncCalls()
		ev := calls[len(calls)-1].Body.Events[0].Event
		if ev == nil || ev.SharedKeyPacket == nil {
			t.Fatalf("the last call is not a create with key packets: %+v", ev)
		}
		p := cache.Pin{Shared: *ev.SharedKeyPacket}
		if ev.CalendarKeyPacket != nil {
			p.Calendar = *ev.CalendarKeyPacket
		}
		return p
	}

	t.Run("RespelledIsConfirmed", func(t *testing.T) {
		defer r.tr.set(nil)
		r.rewriteReadBack(t, func(row map[string]any) {
			for _, k := range []string{"SharedKeyPacket", "CalendarKeyPacket"} {
				if v, ok := row[k].(string); ok {
					row[k] = wrapped(v)
				}
			}
		})
		res, err := r.put(t, "sp@x.ics", newEvent("sp@x", "Spelled"), service.Precondition{IfNoneMatchAny: true})
		if err != nil || !res.Created {
			t.Fatalf("a create read back in another spelling: %+v, %v", res, err)
		}
		id := r.proton(t, "sp@x")[0].ID
		if got, want := r.pins(t, id)[id], sentPin(t); got != want {
			t.Errorf("pin %+v, want the packets as sent %+v", got, want)
		}
	})
	// rekey re-encrypts a row's shared parts under a session key of the
	// server's own: the same plaintext and signatures under another
	// SharedKeyPacket, which it returns.
	kr := r.f.CalendarKeyRing(r.cal)
	rekey := func(t *testing.T, row map[string]any) string {
		packet, err := base64.StdEncoding.DecodeString(row["SharedKeyPacket"].(string))
		if err != nil {
			t.Errorf("packet: %v", err)
			return ""
		}
		old, err := kr.DecryptSessionKey(packet)
		if err != nil {
			t.Errorf("session key: %v", err)
			return ""
		}
		mine, err := crypto.GenerateSessionKeyAlgo(old.Algo)
		if err != nil {
			t.Errorf("new session key: %v", err)
			return ""
		}
		for _, p := range row["SharedEvents"].([]any) {
			part := p.(map[string]any)
			if !encrypted(part) {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(part["Data"].(string))
			if err != nil {
				t.Errorf("data: %v", err)
				return ""
			}
			plain, err := old.Decrypt(data)
			if err != nil {
				t.Errorf("decrypt: %v", err)
				return ""
			}
			again, err := mine.Encrypt(plain)
			if err != nil {
				t.Errorf("encrypt: %v", err)
				return ""
			}
			part["Data"] = base64.StdEncoding.EncodeToString(again)
		}
		packet, err = kr.EncryptSessionKey(mine)
		if err != nil {
			t.Errorf("packet: %v", err)
			return ""
		}
		row["SharedKeyPacket"] = base64.StdEncoding.EncodeToString(packet)
		return row["SharedKeyPacket"].(string)
	}
	t.Run("ReKeyedUpdateIsNotConfirmed", func(t *testing.T) {
		// sp@x, created above: its read-back under another packet than
		// its pin.
		defer r.tr.set(nil)
		r.rewriteReadBack(t, func(row map[string]any) { rekey(t, row) })
		_, err := r.put(t, "sp@x.ics", newEvent("sp@x", "Respelled"), service.Precondition{})
		wantUnconfirmed(t, err)
	})
	t.Run("ReKeyedIsNotConfirmed", func(t *testing.T) {
		var swapped string
		// The server also changes an unsigned field: the row is still the
		// create's, by its signed content (ruling M4-R37).
		r.rewriteReadBack(t, func(row map[string]any) {
			swapped = rekey(t, row)
			row["Notifications"] = []any{map[string]any{"Type": 1, "Trigger": "-PT9M"}}
		})
		_, err := r.put(t, "rk@x.ics", newEvent("rk@x", "Rekeyed"), service.Precondition{IfNoneMatchAny: true})
		r.tr.set(nil)
		wantUnconfirmed(t, err)
		id := r.proton(t, "rk@x")[0].ID
		if got, want := r.pins(t, id)[id], sentPin(t); got != want {
			t.Errorf("pin %+v, want the packets as sent %+v", got, want)
		}
		logs := r.logs.String()
		if !strings.Contains(logs, "other key packets") || !strings.Contains(logs, id) || !strings.Contains(logs, "SharedKeyPacket") {
			t.Errorf("no warning naming the row and the packet:\n%s", logs)
		}
		for _, secret := range []string{swapped, "Rekeyed"} {
			if secret == "" || strings.Contains(logs, secret) {
				t.Errorf("the log holds %q:\n%s", secret, logs)
			}
		}
		// Proton really holds the packets sent: the event stays writable.
		if _, err := r.put(t, "rk@x.ics", newEvent("rk@x", "Renamed"), service.Precondition{}); err != nil {
			t.Fatalf("a write of the event as Proton holds it: %v", err)
		}
		contains(t, "the object", r.object(t, "rk@x").Data, "SUMMARY:Renamed")
	})
	t.Run("OtherFieldsAreNotConfirmed", func(t *testing.T) {
		// The create's own row, under the packets sent, with other alarms:
		// not what was asked (503), yet pinned as the create's.
		defer r.tr.set(nil)
		r.rewriteReadBack(t, func(row map[string]any) {
			row["Notifications"] = []any{map[string]any{"Type": 1, "Trigger": "-PT9M"}}
		})
		_, err := r.put(t, "nf@x.ics", newEvent("nf@x", "Fields"), service.Precondition{IfNoneMatchAny: true})
		wantUnconfirmed(t, err)
		id := r.proton(t, "nf@x")[0].ID
		if got, want := r.pins(t, id)[id], sentPin(t); got != want {
			t.Errorf("pin %+v, want the packets as sent %+v", got, want)
		}
	})
	t.Run("ReadBackFailedIsPinnedByTheAnswer", func(t *testing.T) {
		// The read-back fails: the row Proton answered is pinned with the
		// packets sent all the same (ruling M4-R37), so a pass that then
		// first stores it under the server's packets does not move the
		// pin, and the next write is refused.
		var mu sync.Mutex
		sent := false
		r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			if r.isSync(req) {
				sent = true
			} else if sent && r.isReadBack(req) {
				return nil, errReset
			}
			return send()
		})
		_, err := r.put(t, "rb@x.ics", newEvent("rb@x", "Lost"), service.Precondition{IfNoneMatchAny: true})
		r.tr.set(nil)
		wantUnconfirmed(t, err)
		want := sentPin(t)
		rows := r.proton(t, "rb@x")
		if len(rows) != 1 {
			t.Fatalf("Proton holds %d rows of rb@x", len(rows))
		}
		id := rows[0].ID
		if got := r.pins(t, id)[id]; got != want {
			t.Errorf("pin after a failed read-back %+v, want the packets as sent %+v", got, want)
		}
		r.f.UpdateEvent(r.cal, id, protontest.Event{UID: "rb@x", Start: base, End: base + 3600, Card: card("rb@x", "Lost", base)})
		if p := r.s.next(t, false); calSync(t, p.Result, r.cal).Added != 1 {
			t.Fatalf("the pass did not store the row: %+v", calSync(t, p.Result, r.cal))
		}
		if got := r.pins(t, id)[id]; got != want {
			t.Errorf("pin after the pass %+v, want the packets as sent %+v", got, want)
		}
		_, err = r.put(t, "rb@x.ics", newEvent("rb@x", "Found"), service.Precondition{})
		wantRefused(t, err, service.RefuseUntrustedKey)
	})
}

// A pin outlives its row (ruling M4-R35): a refresh that hides the row once
// does not reset it, so the row served again under other packets is still
// refused.
func TestHiddenRowKeepsItsPin(t *testing.T) {
	t.Parallel()
	var id string
	seed := protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)}
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) { id = f.AddEvent(cal, seed) }).start(t)
	obj := r.object(t, "a@x")
	body := edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner")

	r.rewriteUIDPages(t, false, func(page map[string]any) { page["Events"] = []any{} })
	_, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag})
	r.tr.set(nil)
	wantIs(t, err, service.ErrPreconditionFailed)
	if !r.object(t, "a@x").Missing {
		t.Fatal("the hidden row is still cached")
	}

	r.f.UpdateEvent(r.cal, id, seed) // served again under other packets
	_, err = r.put(t, "a@x.ics", body, service.Precondition{})
	wantRefused(t, err, service.RefuseUntrustedKey)
	if n := len(r.f.SyncCalls()); n != 0 {
		t.Errorf("%d sync calls", n)
	}
}

// Each key packet of a row is pinned on its own: a refresh serving any one
// of them changed is refused (403 untrusted-key), naming that packet,
// before anything is sent.
func TestEachKeyPacketIsPinned(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}).start(t)
	obj := r.object(t, "a@x")
	body := edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner")
	for _, field := range []string{"SharedKeyPacket", "CalendarKeyPacket", "AddressKeyPacket"} {
		t.Run(field, func(t *testing.T) {
			r.rewriteRefresh(t, func(row map[string]any) { row[field] = "AAECAwQFBgc=" })
			defer r.tr.set(nil)
			_, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag})
			wantRefused(t, err, service.RefuseUntrustedKey)
			if n := len(r.f.SyncCalls()); n != 0 {
				t.Errorf("%d sync calls", n)
			}
			if logs := r.logs.String(); !strings.Contains(logs, "field="+field) {
				t.Errorf("no warning naming %s:\n%s", field, logs)
			}
		})
	}
	if _, err := r.put(t, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("the write with the pinned packets: %v", err)
	}
}

// A DELETE refreshes the object from Proton first (as a PUT does): a stale
// If-Match fails (412, nothing sent) on an edit made in the web app, and an
// exception the web app added is deleted with the rest.
func TestDeleteRefreshesFirst(t *testing.T) {
	t.Parallel()
	rid := base + day
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Color: green,
			Card: card("series@x", "Standup", base, "RRULE:FREQ=DAILY")})
	}).start(t)

	if _, err := r.put(t, "a@x.ics", newEvent("a@x", "Lunch"), service.Precondition{IfNoneMatchAny: true}); err != nil {
		t.Fatal(err)
	}
	obj := r.object(t, "a@x")
	r.webEdit(t, r.proton(t, "a@x")[0], events.NotificationList{{Type: 1, Trigger: "-PT5M"}})
	err := r.svc.DeleteObject(t.Context(), r.cal, "a@x.ics", service.Precondition{IfMatch: obj.ETag})
	_ = wantExit(t, err, exitcode.Usage)
	wantIs(t, err, service.ErrPreconditionFailed)
	if n := len(r.f.SyncCalls()); n != 2 {
		t.Errorf("%d sync calls, want 2 (the create and the web edit)", n)
	}
	if len(r.proton(t, "a@x")) != 1 {
		t.Error("Proton lost the event")
	}
	contains(t, "the refreshed object", r.object(t, "a@x").Data, "TRIGGER:-PT5M")

	r.f.AddEvent(r.cal, protontest.Event{UID: "series@x", Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid, Color: green,
		Card: card("series@x", "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid))})
	if err := r.svc.DeleteObject(t.Context(), r.cal, "series@x.ics", service.Precondition{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if n := len(r.proton(t, "series@x")); n != 0 {
		t.Errorf("Proton holds %d rows of the deleted series", n)
	}
	if !r.object(t, "series@x").Missing {
		t.Error("the deleted series is still cached")
	}
}

// A DELETE is confirmed only when no row of the UID is left (ruling
// M4-R34): an exception another client created in the window between the
// refresh and the call survives it, a concurrent change: 412, and the cache
// holds what is left.
func TestDeleteWithARowLeftIs412(t *testing.T) {
	t.Parallel()
	rid := base + day
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY", Color: green,
			Card: card("series@x", "Standup", base, "RRULE:FREQ=DAILY")})
	}).start(t)
	var once sync.Once
	r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
		if r.isSync(req) {
			once.Do(func() {
				r.f.AddEvent(r.cal, protontest.Event{UID: "series@x", Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid, Color: green,
					Card: card("series@x", "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid))})
			})
		}
		return send()
	})
	defer r.tr.set(nil)
	err := r.svc.DeleteObject(t.Context(), r.cal, "series@x.ics", service.Precondition{})
	_ = wantExit(t, err, exitcode.Usage)
	wantIs(t, err, service.ErrPreconditionFailed)
	if n := len(r.f.SyncCalls()); n != 1 {
		t.Errorf("%d sync calls, want 1", n)
	}
	if n := len(r.proton(t, "series@x")); n != 1 {
		t.Fatalf("Proton holds %d rows, want the new exception", n)
	}
	contains(t, "the cached object", r.object(t, "series@x").Data, "RECURRENCE-ID:"+stamp(rid))
}

// A write waiting for its calendar's lock gives up when its context ends,
// at once, with the unavailable class, and sends nothing.
func TestWriteWaitingForTheLockEndsWithItsContext(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.svc.OnWriteWait(func(string) { cancel() })
	r.start(t)
	obj := r.object(t, "a@x")
	unlock, err := r.svc.LockCalendar(t.Context(), r.cal)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	done := make(chan error, 1)
	go func() {
		_, err := r.svc.PutObject(ctx, r.cal, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: obj.ETag})
		done <- err
	}()
	select {
	case err := <-done:
		_ = wantExit(t, err, exitcode.Blocked)
	case <-time.After(waitLimit):
		t.Fatal("the write still waits for the lock after its context ended")
	}
	if n := r.sendCalls(); n != 0 {
		t.Errorf("%d requests reached the sync endpoint", n)
	}
}

// Ruling M4-R39: a write is refused (ErrNotWritable, CalDAV 403) only when
// the gate is decided from good data; when it cannot be decided (a
// passphrase signature that cannot be checked, keys that did not unlock)
// the write is unavailable (ErrGateUndecided, Blocked: CalDAV 503). Nothing
// is sent either way.
func TestWriteGateDecidedOrUndecided(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		fo        protontest.Options
		bootstrap int // the first bootstrap read's failure status, 0: none
		cause     error
		code      exitcode.Code
	}{
		{"signature checked and bad", protontest.Options{BadPassphraseSignature: true}, 0, service.ErrNotWritable, exitcode.Usage},
		{"no signature to check", protontest.Options{EmptyPassphraseSignature: true}, 0, service.ErrGateUndecided, exitcode.Blocked},
		{"bootstrap refused", protontest.Options{}, http.StatusUnprocessableEntity, service.ErrGateUndecided, exitcode.Blocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.fo, func(f *protontest.Fake, cal string) {
				f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
				if tc.bootstrap != 0 {
					f.FailNext(http.MethodGet, "/calendar/v2/"+cal+"/bootstrap", tc.bootstrap, 2501, 0)
				}
			}).start(t)
			_, err := r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true})
			_ = wantExit(t, err, tc.code)
			wantIs(t, err, tc.cause)
			err = r.svc.DeleteObject(t.Context(), r.cal, "a@x.ics", service.Precondition{})
			_ = wantExit(t, err, tc.code)
			wantIs(t, err, tc.cause)
			if n := r.sendCalls(); n != 0 {
				t.Errorf("%d requests reached the sync endpoint", n)
			}
			c := r.collection(t, r.cal)
			undecided := errors.Is(tc.cause, service.ErrGateUndecided)
			if c.PrivilegesUnknown != undecided { // never decided (ruling M4-R40)
				t.Errorf("collection PrivilegesUnknown %v, want %v", c.PrivilegesUnknown, undecided)
			}
			if !c.ReadOnly || c.WriteGateUndecided != errors.Is(tc.cause, service.ErrGateUndecided) {
				t.Errorf("collection ReadOnly %v, WriteGateUndecided %v; want read only, undecided %v",
					c.ReadOnly, c.WriteGateUndecided, errors.Is(tc.cause, service.ErrGateUndecided))
			}
		})
	}
}

// Ruling M4-R39: a calendar whose gate cannot be decided now keeps
// advertising the privileges last decided for it (read and write here),
// while its writes are unavailable; one never decided is read only.
func TestUndecidedGateKeepsTheLastDecision(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	fresh := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Fresh"})
	e.f.FailNext(http.MethodGet, "/calendar/v2/"+fresh+"/bootstrap", http.StatusUnprocessableEntity, 2501, 0)
	e.login(t, e.req())
	e.sync(t, false)
	if c := e.collection(t, work); c.ReadOnly || c.WriteGateUndecided || c.PrivilegesUnknown {
		t.Fatalf("precondition: Work is decided writable, got %+v", c)
	}
	if c := e.collection(t, fresh); !c.ReadOnly || !c.WriteGateUndecided || !c.PrivilegesUnknown {
		t.Errorf("a calendar never decided: ReadOnly %v, undecided %v, unknown %v; want all", c.ReadOnly, c.WriteGateUndecided, c.PrivilegesUnknown)
	}
	// A new login drops the keys; Work's bootstrap then fails.
	e.clk.advance(2 * time.Minute)
	e.login(t, e.req())
	e.f.FailNext(http.MethodGet, "/calendar/v2/"+work+"/bootstrap", http.StatusUnprocessableEntity, 2501, 0)
	if c := e.collection(t, work); c.ReadOnly || !c.WriteGateUndecided || c.PrivilegesUnknown {
		t.Errorf("Work undecided after a decision: ReadOnly %v, undecided %v; want the last decision (writable), undecided", c.ReadOnly, c.WriteGateUndecided)
	}
}

// Ruling M4-R39: the last decision is the one the settings refresh made
// too: a calendar downgraded to a viewer under Serve, whose gate later
// cannot be decided, keeps advertising read only.
func TestRefreshedGateIsTheLastDecision(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	id := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req())
	s := e.serve(t)
	s.next(t, true)
	if c := e.collection(t, id); c.ReadOnly {
		t.Fatal("precondition: the calendar starts writable")
	}
	e.f.SetPermissions(id, 2)
	e.clk.advance(10*time.Minute + time.Second)
	s.next(t, false)
	if c := e.collection(t, id); !c.ReadOnly || c.WriteGateUndecided {
		t.Fatalf("precondition: the refresh decided read only, got %+v", c)
	}
	s.stop(t)
	e.clk.advance(2 * time.Minute)
	e.login(t, e.req())
	e.f.FailNext(http.MethodGet, "/calendar/v2/"+id+"/bootstrap", http.StatusUnprocessableEntity, 2501, 0)
	if c := e.collection(t, id); !c.ReadOnly || !c.WriteGateUndecided || c.PrivilegesUnknown {
		t.Errorf("undecided after the refresh's decision: ReadOnly %v, undecided %v; want read only, undecided", c.ReadOnly, c.WriteGateUndecided)
	}
}

// Ruling M4-R42: a write whose preparation (the wait for the calendar's
// lock, the refresh from Proton) runs past its budget answers unavailable
// (ErrWriteNotSent: CalDAV 503) while its caller still waits, logged, and
// sends nothing: a Sync call sent so late could land after the client gave
// up on the answer.
func TestSlowPreparationSendsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		slow func(t *testing.T, r *rig)
	}{
		{"the calendar's lock held", func(t *testing.T, r *rig) {
			unlock, err := r.svc.LockCalendar(t.Context(), r.cal)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(unlock)
		}},
		{"the refresh unanswered", func(_ *testing.T, r *rig) {
			r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
				if r.isReadBack(req) {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return send()
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
				f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
			})
			r.svc.SetPreSendBudget(50 * time.Millisecond)
			r.start(t)
			obj := r.object(t, "a@x")
			body := edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner")
			tc.slow(t, r)
			for _, write := range []struct {
				method string
				run    func() error
			}{
				{http.MethodPut, func() error {
					_, err := r.svc.PutObject(t.Context(), r.cal, "a@x.ics", body, service.Precondition{IfMatch: obj.ETag})
					return err
				}},
				{http.MethodDelete, func() error {
					return r.svc.DeleteObject(t.Context(), r.cal, "a@x.ics", service.Precondition{IfMatch: obj.ETag})
				}},
			} {
				done := make(chan error, 1)
				go func() { done <- write.run() }()
				select {
				case err := <-done:
					_ = wantExit(t, err, exitcode.Blocked)
					wantIs(t, err, service.ErrWriteNotSent)
				case <-time.After(waitLimit):
					t.Fatalf("%s still waits past its budget", write.method)
				}
			}
			if n := r.sendCalls(); n != 0 {
				t.Errorf("%d requests reached the sync endpoint", n)
			}
			if !strings.Contains(r.logs.String(), "a write was not sent") {
				t.Errorf("the log does not name the late preparation:\n%s", r.logs)
			}
		})
	}
}

// Ruling M4-R43: once the read-back shows the write landed, a cache that
// cannot store the rows read back does not change the answer: success,
// logged; the next sync pass stores what Proton holds.
func TestLandedWriteSurvivesACacheFailure(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	})
	var failing sync.Map
	r.svc.SetWriteFault(func(stage string) error {
		if _, ok := failing.Load(stage); ok {
			return errors.New("disk I/O error")
		}
		return nil
	})
	r.start(t)
	obj := r.object(t, "a@x")
	failing.Store("store", true)
	if _, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("PutObject = %v, want success: Proton holds the write", err)
	}
	if n := r.sendCalls(); n != 1 {
		t.Errorf("%d requests reached the sync endpoint, want 1", n)
	}
	if !strings.Contains(r.logs.String(), "a write landed but the cache could not store it") {
		t.Errorf("the log does not name the cache failure:\n%s", r.logs)
	}
	failing.Delete("store")
	r.s.next(t, false)
	contains(t, "the object after the next pass", r.object(t, "a@x").Data, "SUMMARY:Dinner")
}

// Ruling M4-R43: a create whose rows cannot be pinned when Proton answers
// still reads back, and the answer follows the read-back: 201, logged, the
// row pinned with the packets sent when the read-back stores it.
func TestPinFailureStillReadsBack(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil)
	r.svc.SetWriteFault(func(stage string) error {
		if stage == "pin" {
			return errors.New("database is locked")
		}
		return nil
	})
	r.start(t)
	res, err := r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true})
	if err != nil || !res.Created {
		t.Fatalf("PutObject = %+v, %v; want created", res, err)
	}
	if !strings.Contains(r.logs.String(), "could not be pinned before its read-back") {
		t.Errorf("the log does not name the missing pin:\n%s", r.logs)
	}
	rows := r.proton(t, "n@x")
	if len(rows) != 1 {
		t.Fatalf("Proton holds %d rows, want 1", len(rows))
	}
	if pins := r.pins(t, rows[0].ID); len(pins) != 1 || pins[rows[0].ID] != cache.PinOf(rows[0].Raw) {
		t.Errorf("pins %+v, want the created row's packets", pins)
	}
	contains(t, "the created object", r.object(t, "n@x").Data, "SUMMARY:New")
}

// Ruling M4-R44: Proton asking for a human verification (422, code 9001) on
// a write's calls is unavailable (CalDAV 503), logged as needing a person,
// never a refusal: on the Sync call (nothing applied) and on the refresh
// (nothing sent).
func TestHumanVerificationIsUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method string
		path         func(r *rig) string
		sent         int
	}{
		{"on the Sync call", http.MethodPut, func(r *rig) string { return r.syncPath() }, 1},
		{"on the refresh", http.MethodGet, func(r *rig) string { return "/calendar/v1/" + r.cal + "/events" }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, protontest.Options{}, nil).start(t)
			r.f.FailNext(tc.method, tc.path(r), http.StatusUnprocessableEntity, api.CodeHumanVerification, 0)
			_, err := r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true})
			_ = wantExit(t, err, exitcode.Blocked)
			wantIs(t, err, service.ErrHumanVerification)
			var wr *service.WriteRefusal
			if errors.As(err, &wr) {
				t.Errorf("a refusal %s: a human verification is never a 403", wr.Code)
			}
			if n := r.sendCalls(); n != tc.sent {
				t.Errorf("%d requests reached the sync endpoint, want %d", n, tc.sent)
			}
			if n := len(r.proton(t, "n@x")); n != 0 {
				t.Errorf("Proton holds %d rows", n)
			}
			if !strings.Contains(r.logs.String(), "Proton asks for a human verification") {
				t.Errorf("the log does not say a person is needed:\n%s", r.logs)
			}
		})
	}
}

// Ruling M4-R45: a UID listing that holds rows of another UID (Proton's
// filter not exact) does not block the write: those rows are ignored,
// logged by ID without content, and neither stored under the UID written
// nor sent anything.
func TestUIDListingKeepsOnlyThatUID(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
		f.AddEvent(cal, protontest.Event{UID: "b@x", Start: base, End: base + 3600, Color: green, Card: card("b@x", "Secret topic", base)})
	}).start(t)
	other := r.proton(t, "b@x")[0]
	var stranger map[string]any
	dec := json.NewDecoder(bytes.NewReader(other.Raw))
	dec.UseNumber()
	if err := dec.Decode(&stranger); err != nil {
		t.Fatal(err)
	}
	r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
		resp, err := send()
		if err != nil || !r.isReadBack(req) || req.URL.Query().Get("UID") != "a@x" {
			return resp, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var page map[string]any
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		if err := d.Decode(&page); err != nil {
			return nil, err
		}
		rows, _ := page["Events"].([]any)
		page["Events"] = append(rows, stranger)
		out, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Del("Content-Length")
		return resp, nil
	})
	defer r.tr.set(nil)
	objB := r.object(t, "b@x")
	obj := r.object(t, "a@x")
	if _, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("PutObject = %v, want success: the other UID's row is not the object", err)
	}
	if calls := r.f.SyncCalls(); len(calls) != 1 {
		t.Fatalf("%d sync calls, want 1", len(calls))
	}
	contains(t, "the written object", r.object(t, "a@x").Data, "SUMMARY:Dinner")
	lacks(t, "the written object", r.object(t, "a@x").Data, "Secret topic")
	if got := r.object(t, "b@x"); got.ETag != objB.ETag || got.Missing {
		t.Errorf("the other UID's object changed: %+v", got)
	}
	log := r.logs.String()
	if !strings.Contains(log, "rows of another UID") || !strings.Contains(log, other.ID) {
		t.Errorf("the log does not name the ignored row:\n%s", log)
	}
	if strings.Contains(log, "Secret topic") || strings.Contains(log, "b@x") {
		t.Errorf("the log carries event content:\n%s", log)
	}
}

// The settings refresh picks up a gate that moves between undecided and
// closed although the settings stay the same (both render read only): a
// passphrase signature now served and bad closes the calendar (writes
// 403), and one that cannot be checked any more makes it undecided again
// (writes 503, ruling M4-R39).
func TestRefreshSeesTheGateMoveBetweenUndecidedAndClosed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{EmptyPassphraseSignature: true})
	id := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req())
	s := e.serve(t)
	s.next(t, true)
	wantGate := func(step string, cause error, code exitcode.Code) {
		t.Helper()
		c := e.collection(t, id)
		if undecided := errors.Is(cause, service.ErrGateUndecided); !c.ReadOnly || c.WriteGateUndecided != undecided {
			t.Fatalf("%s: ReadOnly %v, WriteGateUndecided %v; want read only, undecided %v", step, c.ReadOnly, c.WriteGateUndecided, undecided)
		}
		_, err := e.svc.PutObject(t.Context(), id, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true})
		_ = wantExit(t, err, code)
		wantIs(t, err, cause)
	}
	wantGate("no signature to check", service.ErrGateUndecided, exitcode.Blocked)

	e.f.SetPassphraseSignature(id, protontest.PassphraseSignedByStranger)
	e.clk.advance(10*time.Minute + time.Second)
	s.next(t, false)
	wantGate("a bad signature after the refresh", service.ErrNotWritable, exitcode.Usage)

	e.f.SetPassphraseSignature(id, protontest.PassphraseUnsigned)
	e.clk.advance(10*time.Minute + time.Second)
	s.next(t, false)
	wantGate("no signature again after the refresh", service.ErrGateUndecided, exitcode.Blocked)
	if n := e.f.Calls(http.MethodPut, "/calendar/v1/"+id+"/events/sync"); n != 0 {
		t.Errorf("%d requests reached the sync endpoint", n)
	}
	s.stop(t)
}

// UIDs are compared unescaped (write-I2; SPEC §5.3, the parked
// UID-escaping item): an event whose UID column holds its text unescaped
// (`pcb-a,b` for `UID:pcb-a\,b`) is the same object as a body carrying the
// text, never a 409 naming its own name. pcb cannot rewrite it while the
// two forms diverge, so its edit is unavailable (503), nothing sent; the
// same UID at another name is a 409 naming the object's name, as for any
// UID. The other direction too (ruling A9): a column holding the escaped
// text (`pcb-c\,d`) and a client sending the UID unescaped (`UID:pcb-c,d`).
func TestAnEscapedUIDIsNeverItsOwnConflict(t *testing.T) {
	t.Parallel()
	const column, text = "pcb-a,b", `pcb-a\,b`
	const escapedColumn = `pcb-c\,d`
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: column, Start: base, End: base + 3600, Color: green, Card: card(text, "Escaped", base)})
		f.AddEvent(cal, protontest.Event{UID: escapedColumn, Start: base, End: base + 3600, Color: green, Card: card(escapedColumn, "Kept", base)})
	}).start(t)
	t.Run("StoredEscapedSentUnescaped", func(t *testing.T) {
		obj := r.object(t, escapedColumn)
		body := edit(t, edit(t, obj.Data, "UID:"+escapedColumn+"\r\n", "UID:pcb-c,d\r\n"), "SUMMARY:Kept", "SUMMARY:Edited")
		_, err := r.put(t, service.ObjectName(escapedColumn), body, service.Precondition{IfMatch: obj.ETag})
		_ = wantExit(t, err, exitcode.Blocked)
		wantIs(t, err, service.ErrUIDStoredOtherwise)
		contains(t, "the object after the refusal", r.object(t, escapedColumn).Data, "SUMMARY:Kept")
	})
	obj := r.object(t, column)
	name := service.ObjectName(column)
	contains(t, "the object", obj.Data, "UID:"+text+"\r\n")

	_, err := r.put(t, name, edit(t, obj.Data, "SUMMARY:Escaped", "SUMMARY:Edited"), service.Precondition{IfMatch: obj.ETag})
	e := wantExit(t, err, exitcode.Blocked)
	wantIs(t, err, service.ErrUIDStoredOtherwise)
	if got, want := e.Err.Error(), "the event's UID is stored by Proton in another form than its text, so pcb cannot rewrite it yet"; got != want {
		t.Errorf("error %q, want %q", got, want)
	}
	if got, want := e.Hint, "edit the event in the Proton web app"; got != want {
		t.Errorf("hint %q, want %q", got, want)
	}
	var ce *service.UIDConflictError
	if errors.As(err, &ce) {
		t.Errorf("the object's own edit is a UID conflict naming %q", ce.Href)
	}

	_, err = r.put(t, "elsewhere.ics", obj.Data, service.Precondition{IfNoneMatchAny: true})
	_ = wantExit(t, err, exitcode.Usage)
	if !errors.As(err, &ce) || ce.Href != name {
		t.Errorf("the same UID at another name: %v, want a UID conflict naming %q", err, name)
	}
	if n := r.sendCalls(); n != 0 {
		t.Errorf("%d requests reached the sync endpoint, want none", n)
	}
	contains(t, "the object after the refusals", r.object(t, column).Data, "SUMMARY:Escaped")
}

// A row holding a personal part (alarms from before they moved to
// Notifications) is never updated (write-I3): pcb does not re-send that
// part, and what Proton does to a part left out is unknown until live check
// L.4 #10. The PUT is refused before anything is sent, whatever it changes;
// the event can still be deleted (a delete rewrites no part).
func TestARowWithAPersonalPartIsNotUpdated(t *testing.T) {
	t.Parallel()
	alarm := []string{"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "DESCRIPTION:x", "END:VALARM"}
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "p@x", Start: base, End: base + 3600, Color: green, PersonalAlarms: true,
			Card: card("p@x", "Personal", base, alarm...)})
		f.AddEvent(cal, protontest.Event{UID: "q@x", Start: base, End: base + 3600, Color: green, Card: card("q@x", "Plain", base)})
	}).start(t)
	if len(r.proton(t, "p@x")) != 1 || !strings.Contains(string(r.proton(t, "p@x")[0].Raw), `"PersonalEvents":[{`) {
		t.Fatal("precondition: the seeded row has no personal part")
	}
	obj := r.object(t, "p@x")
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"a title edit", edit(t, obj.Data, "SUMMARY:Personal", "SUMMARY:Renamed")},
		{"the object as served", obj.Data},
	} {
		_, err := r.put(t, "p@x.ics", c.body, service.Precondition{IfMatch: obj.ETag})
		wantRefused(t, err, service.RefusePersonalPart)
		var wr *service.WriteRefusal
		if errors.As(err, &wr) && wr.Reason != "this event has per-user data pcb cannot rewrite yet" {
			t.Errorf("%s: reason %q", c.name, wr.Reason)
		}
	}
	if n := r.sendCalls(); n != 0 {
		t.Errorf("%d requests reached the sync endpoint, want none", n)
	}
	// The control: a row without one takes the same edit.
	plain := r.object(t, "q@x")
	if _, err := r.put(t, "q@x.ics", edit(t, plain.Data, "SUMMARY:Plain", "SUMMARY:Renamed"), service.Precondition{IfMatch: plain.ETag}); err != nil {
		t.Fatalf("the control edit: %v", err)
	}
	if err := r.svc.DeleteObject(t.Context(), r.cal, "p@x.ics", service.Precondition{IfMatch: obj.ETag}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if len(r.proton(t, "p@x")) != 0 || !r.object(t, "p@x").Missing {
		t.Error("the delete did not happen")
	}
}

// If-Match is a list of entity tags or * (RFC 9110 §13.1.1, write-M4): the
// object's ETag anywhere in the list holds; * holds on any existing object;
// a weak tag never holds (strong comparison), not even the object's own
// marked weak. A failed one sends nothing.
func TestIfMatchListsCompareStrongly(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	}).start(t)
	summary := "Lunch"
	for _, c := range []struct {
		name, ifMatch string
		holds         bool
	}{
		{"the ETag weak", "W/%s", false},
		{"a list of others and the ETag weak", `"v1-other", W/%s`, false},
		{"a list of others", `"v1-other", "v1-another"`, false},
		{"the ETag last in a list", `"v1-other",%s`, true},
		{"the ETag first, spaces around", ` %s , "v1-other" `, true},
		{"* in a list", `"v1-other", *`, true},
	} {
		obj := r.object(t, "a@x")
		sent := r.sendCalls()
		next := summary + "+"
		_, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:"+summary, "SUMMARY:"+next),
			service.Precondition{IfMatch: strings.ReplaceAll(c.ifMatch, "%s", obj.ETag)})
		switch {
		case c.holds && err != nil:
			t.Errorf("%s: %v, want the edit written", c.name, err)
		case c.holds:
			summary = next
		case !errors.Is(err, service.ErrPreconditionFailed):
			t.Errorf("%s: %v, want ErrPreconditionFailed", c.name, err)
		case r.sendCalls() != sent:
			t.Errorf("%s: a failed precondition reached the sync endpoint", c.name)
		}
	}
	contains(t, "the object", r.object(t, "a@x").Data, "SUMMARY:"+summary+"\r\n")
}
