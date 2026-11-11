package caldav_test

// The reports against the real service (protontest fake, real encrypted
// cards, the real cache): what the stub cannot show. The tests of this file
// share one logged-in rig, built by the first of them and torn down by
// TestMain; each works in a calendar of its own and starts from an
// empty-token sync, so any of them runs alone, in any order, any number of
// times.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// base is 2027-01-15 08:00 UTC, the start of the test events.
const (
	base = int64(1_800_000_000)
	day  = int64(24 * 3600)
)

func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format("20060102T150405Z") }

func card(uid, summary string, start int64, extra ...string) string {
	lines := []string{"UID:" + uid, "DTSTAMP:20270115T080000Z", "DTSTART:" + stamp(start), "DTEND:" + stamp(start+3600), "SUMMARY:" + summary}
	return strings.Join(append(lines, extra...), "\r\n")
}

// rig is a logged-in service on a fake Proton, its handler and a client's
// app password.
type rig struct {
	f    *protontest.Fake
	svc  *service.Local
	dirs paths.Dirs
	h    http.Handler
	pass string
}

func noSleep(context.Context, time.Duration) error { return nil }

// buildRig builds a rig in root whose fake and service live as long as tb;
// setup adds the account's calendars and events before the login, and the
// first sync runs when firstSync.
func buildRig(ctx context.Context, tb testing.TB, root string, setup func(f *protontest.Fake), firstSync bool) (*rig, error) {
	r := &rig{f: protontest.New(tb, protontest.Options{})}
	r.dirs = paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(r.dirs)
	cfg.Proton.BaseURL = r.f.URL
	svc, err := service.NewLocal(service.Options{Dirs: r.dirs, Config: cfg, Sleep: noSleep})
	if err != nil {
		return nil, err
	}
	tb.Cleanup(func() { _ = svc.Close() })
	r.svc = svc
	setup(r.f)
	if _, err := svc.Login(ctx, service.LoginRequest{Username: r.f.Username, Password: r.f.Password}); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if firstSync {
		if _, err := svc.Sync(ctx, service.SyncRequest{}); err != nil {
			return nil, fmt.Errorf("sync: %w", err)
		}
	}
	sec, err := svc.AddClient(ctx, "Thunderbird")
	if err != nil {
		return nil, err
	}
	r.pass = sec.Password
	r.h = caldav.Handler(svc, caldav.Options{})
	return r, nil
}

// newRig builds a synced rig of the test t.
func newRig(t *testing.T, setup func(f *protontest.Fake)) *rig {
	t.Helper()
	r, err := buildRig(t.Context(), t, t.TempDir(), setup, true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newUnsyncedRig builds a rig of the test t that never synced.
func newUnsyncedRig(t *testing.T, setup func(f *protontest.Fake)) *rig {
	t.Helper()
	r, err := buildRig(t.Context(), t, t.TempDir(), setup, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// processTB is a testing.TB that lives as long as the test binary, for the
// shared rig: its cleanups run in TestMain after every test. It implements
// what protontest calls (Helper, Cleanup, Fatal, Fatalf, Errorf); every
// other method panics, naming itself. The embedded TB stays nil: it only
// supplies testing.TB's unexported method.
type processTB struct {
	testing.TB
	mu       sync.Mutex
	cleanups []func()
	failed   bool
}

const sharedRigName = "shared caldav test rig"

func (*processTB) Helper() {}

func (p *processTB) Cleanup(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanups = append(p.cleanups, f)
}

func (*processTB) Fatal(args ...any) { panic(sharedRigName + ": Fatal: " + fmt.Sprint(args...)) }

func (*processTB) Fatalf(format string, args ...any) {
	panic(sharedRigName + ": Fatalf: " + fmt.Sprintf(format, args...))
}

func (p *processTB) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, sharedRigName+": "+format+"\n", args...)
	p.mu.Lock()
	p.failed = true
	p.mu.Unlock()
}

// unsupported panics for a testing.TB method processTB does not implement.
func unsupported(method string) {
	panic(sharedRigName + ": testing.TB." + method + " is not implemented by processTB (report_test.go); implement it there")
}

func (*processTB) ArtifactDir() string      { unsupported("ArtifactDir"); return "" }
func (*processTB) Attr(string, string)      { unsupported("Attr") }
func (*processTB) Error(...any)             { unsupported("Error") }
func (*processTB) Fail()                    { unsupported("Fail") }
func (*processTB) FailNow()                 { unsupported("FailNow") }
func (*processTB) Failed() bool             { unsupported("Failed"); return false }
func (*processTB) Log(...any)               { unsupported("Log") }
func (*processTB) Logf(string, ...any)      { unsupported("Logf") }
func (*processTB) Name() string             { unsupported("Name"); return "" }
func (*processTB) Setenv(string, string)    { unsupported("Setenv") }
func (*processTB) Chdir(string)             { unsupported("Chdir") }
func (*processTB) Skip(...any)              { unsupported("Skip") }
func (*processTB) SkipNow()                 { unsupported("SkipNow") }
func (*processTB) Skipf(string, ...any)     { unsupported("Skipf") }
func (*processTB) Skipped() bool            { unsupported("Skipped"); return false }
func (*processTB) TempDir() string          { unsupported("TempDir"); return "" }
func (*processTB) Context() context.Context { unsupported("Context"); return nil }
func (*processTB) Output() io.Writer        { unsupported("Output"); return nil }

// The shared rig's TB implements every testing.TB method itself, so none
// reaches the nil embedded TB (a bare nil-pointer panic): each names itself.
func TestSharedRigTBNamesUnimplementedMethods(t *testing.T) {
	t.Parallel()
	var tb processTB
	calls := map[string]func(){
		"ArtifactDir": func() { tb.ArtifactDir() }, "Attr": func() { tb.Attr("k", "v") }, "Error": func() { tb.Error("x") },
		"Fail": tb.Fail, "FailNow": tb.FailNow, "Failed": func() { tb.Failed() }, "Log": func() { tb.Log("x") },
		"Logf": func() { tb.Logf("x") }, "Name": func() { tb.Name() }, "Setenv": func() { tb.Setenv("K", "v") },
		"Chdir": func() { tb.Chdir(".") }, "Skip": func() { tb.Skip() }, "SkipNow": tb.SkipNow, "Skipf": func() { tb.Skipf("x") },
		"Skipped": func() { tb.Skipped() }, "TempDir": func() { tb.TempDir() }, "Context": func() { tb.Context() },
		"Output": func() { tb.Output() }, "Fatal": func() { tb.Fatal("boom") }, "Fatalf": func() { tb.Fatalf("boom %d", 1) },
	}
	tbType, want := reflect.TypeFor[testing.TB](), 0
	for i := range tbType.NumMethod() {
		if tbType.Method(i).IsExported() {
			want++
		}
	}
	if len(calls)+3 != want { // + Helper, Cleanup, Errorf
		t.Fatalf("testing.TB has %d exported methods, the test covers %d: add the new ones to processTB", want, len(calls)+3)
	}
	for method, call := range calls {
		func() {
			defer func() {
				msg, _ := recover().(string)
				if !strings.Contains(msg, sharedRigName) || !strings.Contains(msg, method) {
					t.Errorf("%s panics with %q, want a message naming the method and the %s", method, msg, sharedRigName)
				}
			}()
			call()
		}()
	}
}

// teardown runs the cleanups, last first, and reports whether all went well.
func (p *processTB) teardown() bool {
	p.mu.Lock()
	cleanups := p.cleanups
	p.cleanups = nil
	p.mu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.failed
}

// The shared rig's calendars, one per test, and their fixed events.
type reportCalendars struct{ placeholders, edits, deletions, epoch string }

const (
	keepUID   = "keep@x"
	seriesUID = "series@x"
	brokenUID = "broken@x"
	oddRigUID = "odd uid/é"
)

var shared struct {
	once sync.Once
	tb   processTB
	r    *rig
	cals reportCalendars
	err  error
	runs atomic.Int64 // makes each run's UIDs unique (-count=N)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if !shared.tb.teardown() && code == 0 {
		code = 1
	}
	os.Exit(code)
}

// sharedRig returns the file's rig, built on first use.
func sharedRig(t *testing.T) (*rig, reportCalendars) {
	t.Helper()
	shared.once.Do(func() {
		defer func() {
			if p := recover(); p != nil {
				shared.err = fmt.Errorf("%v", p)
			}
		}()
		root, err := os.MkdirTemp("", "caldav-report-rig-")
		if err != nil {
			shared.err = err
			return
		}
		shared.tb.Cleanup(func() { _ = os.RemoveAll(root) })
		c := &shared.cals
		rid := base + day
		shared.r, shared.err = buildRig(context.Background(), &shared.tb, root, func(f *protontest.Fake) {
			c.placeholders = f.AddCalendar(f.Username, protontest.Calendar{Name: "Placeholders"})
			c.edits = f.AddCalendar(f.Username, protontest.Calendar{Name: "Edits"})
			c.deletions = f.AddCalendar(f.Username, protontest.Calendar{Name: "Deletions"})
			c.epoch = f.AddCalendar(f.Username, protontest.Calendar{Name: "Epoch"})
			p := c.placeholders
			f.AddEvent(p, protontest.Event{UID: keepUID, Start: base, End: base + 3600, Card: card(keepUID, "Keep me", base)})
			f.AddEvent(p, protontest.Event{UID: seriesUID, Start: base, End: base + 3600, RRule: "FREQ=DAILY",
				Card: card(seriesUID, "Standup", base, "RRULE:FREQ=DAILY")})
			f.AddEvent(p, protontest.Event{UID: seriesUID, Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid,
				Card: card(seriesUID, "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid))})
			f.AddEvent(p, protontest.Event{UID: brokenUID, Start: base, End: base + 3600, BrokenPart: protontest.SharedEncryptedPart,
				Card: card(brokenUID, "Secret plan", base)})
			f.AddEvent(p, protontest.Event{UID: oddRigUID, Start: base, End: base + 3600, Card: card(oddRigUID, "Odd one", base)})
			for _, uid := range []string{"e1@x", "e2 /é"} {
				f.AddEvent(c.epoch, protontest.Event{UID: uid, Start: base, End: base + 3600, Card: card(uid, "Epoch "+uid, base)})
			}
		}, true)
	})
	if shared.err != nil {
		t.Fatalf("shared rig: %v", shared.err)
	}
	return shared.r, shared.cals
}

func (r *rig) sync(t *testing.T) {
	t.Helper()
	if _, err := r.svc.Sync(t.Context(), service.SyncRequest{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func (r *rig) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	req := call(method, target, body, hdr...)
	req.SetBasicAuth("user@proton.me", r.pass)
	return serve(r.h, req)
}

// syncReport runs Thunderbird's sync-collection with token.
func (r *rig) syncReport(t *testing.T, calID, token string) multistatus {
	t.Helper()
	ms := parseMS(t, r.do("REPORT", calPath(calID), tbSync(token), "Depth", "1", "Content-Type", "text/xml; charset=utf-8"))
	if ms.token == nil || *ms.token == "" {
		t.Fatalf("no sync-token in:\n%s", ms.raw)
	}
	return ms
}

// multiget fetches uids the way Thunderbird does.
func (r *rig) multiget(t *testing.T, calID string, uids ...string) multistatus {
	t.Helper()
	hrefs := make([]string, len(uids))
	for i, u := range uids {
		hrefs[i] = tbHref(calID, u)
	}
	return parseMS(t, r.do("REPORT", calPath(calID), tbMultiget(hrefs...), "Depth", "1"))
}

// listed returns the hrefs a sync listed (200) and deleted (404).
func listed(t *testing.T, ms multistatus) (changed map[string]string, deleted []string) {
	t.Helper()
	changed = map[string]string{}
	for path, resp := range ms.byPath {
		switch {
		case resp.status == "HTTP/1.1 404 Not Found" && resp.propstats == 0:
			deleted = append(deleted, path)
		case resp.status == "":
			changed[path] = resp.prop(t, nsD, "getetag").Text
		default:
			t.Errorf("%s: status %q", path, resp.status)
		}
	}
	return changed, deleted
}

// Review focus 3: one event with a part that does not decrypt; every other
// event of the calendar still syncs, that one is the placeholder, counted.
func TestUndecryptableEventDoesNotFailTheReport(t *testing.T) {
	r, c := sharedRig(t)
	all := []string{keepUID, seriesUID, brokenUID, oddRigUID}
	changed, deleted := listed(t, r.syncReport(t, c.placeholders, ""))
	if len(changed) != len(all) || len(deleted) != 0 {
		t.Fatalf("empty-token sync listed %v, deleted %v; want the %d UIDs", changed, deleted, len(all))
	}
	got := r.multiget(t, c.placeholders, all...)
	for _, uid := range all {
		resp := got.get(t, objPath(c.placeholders, uid))
		if resp.status != "" {
			t.Fatalf("%s: status %q", uid, resp.status)
		}
		if e := resp.prop(t, nsD, "getetag").Text; e != changed[objPath(c.placeholders, uid)] {
			t.Errorf("%s: multiget ETag %q, listing %q", uid, e, changed[objPath(c.placeholders, uid)])
		}
	}
	data := func(uid string) string {
		return got.get(t, objPath(c.placeholders, uid)).prop(t, nsC, "calendar-data").Text
	}
	if d := data(brokenUID); !strings.Contains(d, "X-PCB-PLACEHOLDER:1\r\n") || !strings.Contains(d, "SUMMARY:(pcb could not decrypt this event)\r\n") ||
		strings.Contains(d, "Secret plan") {
		t.Errorf("the undecryptable event:\n%q", d)
	}
	for uid, summary := range map[string]string{keepUID: "Keep me", oddRigUID: "Odd one", seriesUID: "Standup moved"} {
		if d := data(uid); !strings.Contains(d, "SUMMARY:"+summary+"\r\n") || strings.Contains(d, "X-PCB-PLACEHOLDER") {
			t.Errorf("%s:\n%q", uid, d)
		}
	}
	if n := r.svc.RenderCounts().Undecryptable; n == 0 {
		t.Errorf("the placeholder is not counted: Undecryptable = %d", n)
	}
}

// E08 MUST 12: an edit changes the object's ETag, and the delta, the
// multiget, PROPFIND and GET agree on the new one.
func TestMust12ETagsChangeWithContentAndAgree(t *testing.T) {
	r, c := sharedRig(t)
	uid := fmt.Sprintf("edit-%d@x", shared.runs.Add(1))
	id := r.f.AddEvent(c.edits, protontest.Event{UID: uid, Start: base, End: base + 3600, Card: card(uid, "Before", base)})
	r.sync(t)
	first := r.syncReport(t, c.edits, "")
	p := objPath(c.edits, uid)
	before, _ := listed(t, first)
	if before[p] == "" {
		t.Fatalf("%s not listed", uid)
	}
	r.f.UpdateEvent(c.edits, id, protontest.Event{UID: uid, Start: base, End: base + 3600, Card: card(uid, "Kept and edited", base)})
	r.sync(t)
	changed, deleted := listed(t, r.syncReport(t, c.edits, *first.token))
	if len(changed) != 1 || changed[p] == "" || len(deleted) != 0 {
		t.Fatalf("delta after one edit: changed %v, deleted %v", changed, deleted)
	}
	if changed[p] == before[p] {
		t.Errorf("the ETag did not change with the content: %s", changed[p])
	}
	got := r.multiget(t, c.edits, uid).get(t, p)
	one := parseMS(t, r.do("PROPFIND", p, tbEtags, "Depth", "0")).get(t, p)
	head := r.do("GET", p, "")
	for what, e := range map[string]string{"multiget": got.prop(t, nsD, "getetag").Text, "PROPFIND": one.prop(t, nsD, "getetag").Text, "GET": head.Header().Get("ETag")} {
		if e != changed[p] {
			t.Errorf("%s ETag %q, delta %q", what, e, changed[p])
		}
	}
	if !strings.Contains(head.Body.String(), "SUMMARY:Kept and edited\r\n") {
		t.Errorf("GET:\n%s", head.Body)
	}
}

// Review focus 2: an event deleted in Proton is its href's response-level
// 404, exactly once; a deleted exception changes its UID's object.
func TestSyncCollectionReportsDeletionsPerUID(t *testing.T) {
	r, c := sharedRig(t)
	n := shared.runs.Add(1)
	gone, series := fmt.Sprintf("gone-%d@x", n), fmt.Sprintf("series-%d@x", n)
	rid := base + day
	goneID := r.f.AddEvent(c.deletions, protontest.Event{UID: gone, Start: base, End: base + 3600, Card: card(gone, "Gone soon", base)})
	r.f.AddEvent(c.deletions, protontest.Event{UID: series, Start: base, End: base + 3600, RRule: "FREQ=DAILY",
		Card: card(series, "Standup", base, "RRULE:FREQ=DAILY")})
	excID := r.f.AddEvent(c.deletions, protontest.Event{UID: series, Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid,
		Card: card(series, "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid))})
	r.sync(t)
	first := r.syncReport(t, c.deletions, "")
	before, _ := listed(t, first)
	r.f.DeleteEvent(c.deletions, goneID)
	r.f.DeleteEvent(c.deletions, excID)
	r.sync(t)
	ms := r.syncReport(t, c.deletions, *first.token)
	changed, deleted := listed(t, ms)
	if len(deleted) != 1 || deleted[0] != objPath(c.deletions, gone) {
		t.Errorf("deleted %v, want only %s", deleted, objPath(c.deletions, gone))
	}
	sp := objPath(c.deletions, series)
	if len(changed) != 1 || changed[sp] == "" || changed[sp] == before[sp] {
		t.Errorf("changed %v, want %s with a new ETag", changed, series)
	}
	if d := r.multiget(t, c.deletions, series).get(t, sp).prop(t, nsC, "calendar-data").Text; strings.Contains(d, "RECURRENCE-ID") {
		t.Errorf("the exception is still served:\n%s", d)
	}
	if rec := r.do("GET", objPath(c.deletions, gone), ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET of the deleted object: %d", rec.Code)
	}
	again, gone2 := listed(t, r.syncReport(t, c.deletions, *ms.token))
	if len(again) != 0 || len(gone2) != 0 {
		t.Errorf("the next sync repeats: changed %v, deleted %v", again, gone2)
	}
}

// Review focus 1: Thunderbird holds a token of a cache since recreated
// (another epoch): 403 valid-sync-token, then its empty-token sync lists
// every live object once.
func TestSyncTokenOfAnotherEpochIsRefused(t *testing.T) {
	r, c := sharedRig(t)
	old := *r.syncReport(t, c.epoch, "").token
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(cache.Path(r.dirs.State) + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	r.sync(t)
	rec := r.do("REPORT", calPath(c.epoch), tbSync(old), "Depth", "1")
	wantStatus(t, rec, http.StatusForbidden)
	wantBody(t, rec, validSyncTokenBody)
	ms := r.syncReport(t, c.epoch, "")
	changed, deleted := listed(t, ms)
	want := []string{"e1@x", "e2 /é"}
	if len(changed) != len(want) || len(deleted) != 0 || len(ms.responses) != len(want) {
		t.Fatalf("full sync: %d responses, changed %v, deleted %v; want %v once each", len(ms.responses), changed, deleted, want)
	}
	for _, uid := range want {
		if changed[objPath(c.epoch, uid)] == "" {
			t.Errorf("%s not listed", uid)
		}
	}
	if *ms.token == old {
		t.Error("the new life kept the old token")
	}
}
