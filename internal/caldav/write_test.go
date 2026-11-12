package caldav_test

// PUT and DELETE against the real service (protontest fake, real sealing,
// the real cache) during Serve, the only time the service writes. Each test
// has its own rig whose sync loop is paused after each pass (next lets one
// more run), so a write is what changes the cache unless a test lets a pass
// run. Every outcome is read over HTTP, as Thunderbird reads it, and from
// the fake: what reached Proton's write endpoint.

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	webdavcaldav "github.com/emersion/go-webdav/caldav"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// waitLimit bounds a wait for Serve: past it the test fails, never hangs.
const waitLimit = 10 * time.Second

// green is a Proton palette colour: an update re-sends a seeded row's
// colour, and the fake refuses one off the palette, as Proton does.
const green = "#3CBB3A"

// servingRig is a rig whose service runs Serve, its sync loop held after
// each pass until next lets one more run.
type servingRig struct {
	*rig
	passes chan service.WatchPass
	resume chan struct{}
	done   chan error
}

// newServingRig builds a rig, starts Serve and waits for its first pass,
// which syncs every calendar setup added.
func newServingRig(t *testing.T, setup func(f *protontest.Fake)) *servingRig {
	t.Helper()
	s := &servingRig{rig: newUnsyncedRig(t, setup), passes: make(chan service.WatchPass), resume: make(chan struct{}), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		s.done <- s.svc.Serve(ctx, service.ServeOptions{OnPass: func(p service.WatchPass) {
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
			t.Errorf("Serve = %v, want nil after the test cancelled it", err)
		}
	})
	s.take(t)
	return s
}

// take waits for the pass that is running to end.
func (s *servingRig) take(t *testing.T) {
	t.Helper()
	select {
	case p := <-s.passes:
		if p.Err != nil {
			t.Fatalf("pass: %v", p.Err)
		}
	case err := <-s.done:
		t.Fatalf("Serve returned %v", err)
	case <-time.After(waitLimit):
		t.Fatal("no pass within the wait limit")
	}
}

// next lets one more pass run and waits for it.
func (s *servingRig) next(t *testing.T) {
	t.Helper()
	select {
	case s.resume <- struct{}{}:
	case <-time.After(waitLimit):
		t.Fatal("Serve is not waiting for a pass")
	}
	s.take(t)
}

const textCalendar = "text/calendar; charset=utf-8"

// put sends PUT as Thunderbird does: its Content-Type and Accept, hdr added.
func (r *rig) put(target, body string, hdr ...string) *httptest.ResponseRecorder {
	return r.do(http.MethodPut, target, body, append([]string{"Content-Type", textCalendar, "Accept", "text/xml"}, hdr...)...)
}

// sent is how many requests reached Proton's write endpoint, any calendar.
func (r *rig) sent() int { return r.f.Calls(http.MethodPut, "/calendar/v1/") }

// etag is the getetag a Depth 0 PROPFIND serves at target.
func (r *rig) etag(t *testing.T, target string) string {
	t.Helper()
	u := strings.TrimPrefix(target, "http://example.com")
	return parseMS(t, r.do("PROPFIND", target, tbEtags, "Depth", "0")).get(t, u).prop(t, nsD, "getetag").Text
}

// body is GET target's body; it fails the test unless the GET is a 200.
func (r *rig) body(t *testing.T, target string) string {
	t.Helper()
	rec := r.do(http.MethodGet, target, "")
	wantStatus(t, rec, http.StatusOK)
	return rec.Body.String()
}

// thunderbird is a VCALENDAR as Thunderbird serialises a new event (E08
// §6.1): its PRODID and VERSION, a VTIMEZONE-free UTC event, extra lines
// added to the VEVENT.
func thunderbird(uid, summary string, extra ...string) string {
	lines := []string{"BEGIN:VCALENDAR", "PRODID:-//Mozilla.org/NONSGML Mozilla Calendar V1.1//EN", "VERSION:2.0",
		"BEGIN:VEVENT", "CREATED:20270101T090000Z", "LAST-MODIFIED:20270101T090000Z", "DTSTAMP:20270101T090000Z",
		"UID:" + uid, "SUMMARY:" + summary, "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600)}
	lines = append(lines, extra...)
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n") + "\r\n"
}

// urlPath is href's path, decoded.
func urlPath(href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	return u.Path, nil
}

// altName is the alternative name of a UID whose canonical name another
// object's alias holds (ruling M4-R38): "~~", its unpadded base64url, ".ics".
func altName(uid string) string {
	return "~~" + base64.RawURLEncoding.EncodeToString([]byte(uid)) + ".ics"
}

// wantNoBody: an answer with no body and no ETag (E08 §4.1, §4.5).
func wantNoBody(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Body.Len() != 0 || rec.Header().Get("ETag") != "" {
		t.Errorf("%s: ETag %q, body %q; want neither", what, rec.Header().Get("ETag"), rec.Body)
	}
}

// conflictHref is the DAV:href of a 409's no-uid-conflict.
func conflictHref(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	wantStatus(t, rec, http.StatusConflict)
	var e el
	if err := xml.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.XMLName != n(nsD, "error") {
		t.Fatalf("409 body %q is not a DAV:error", rec.Body)
	}
	c, ok := e.kid(n(nsC, "no-uid-conflict"))
	if !ok {
		t.Fatalf("409 body %q lacks C:no-uid-conflict", rec.Body)
	}
	return c.hrefOf(t)
}

// precondition403 is the pcb precondition of a 403 refusal, "" if none.
func precondition403(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	wantStatus(t, rec, http.StatusForbidden)
	var e el
	if err := xml.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.XMLName != n(nsD, "error") {
		t.Fatalf("403 body %q is not a DAV:error", rec.Body)
	}
	for _, k := range e.Kids {
		if k.XMLName.Space == "urn:pcb" && k.XMLName.Local != "reason" {
			return k.XMLName.Local
		}
	}
	return ""
}

// wantNeedPrivileges: a 403 need-privileges.
func wantNeedPrivileges(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	wantStatus(t, rec, http.StatusForbidden)
	var e el
	if err := xml.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("%s: body %q: %v", what, rec.Body, err)
	}
	if _, ok := e.kid(n(nsD, "need-privileges")); !ok {
		t.Errorf("%s: body %q lacks need-privileges", what, rec.Body)
	}
}

// A PUT under an If-Match that no longer holds (another client changed the
// object) is a 412 with no body, and nothing reaches Proton: the object
// keeps the other client's change. So is If-None-Match: * on an existing
// object, and a DELETE under the stale ETag.
func TestStaleIfMatchIs412AndWritesNothing(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	href := calPath(cal) + "stale@x.ics"
	wantStatus(t, r.put(href, thunderbird("stale@x", "First"), "If-None-Match", "*"), http.StatusCreated)
	first := r.etag(t, href)
	wantStatus(t, r.put(href, thunderbird("stale@x", "Second"), "If-Match", first), http.StatusNoContent)
	if now := r.etag(t, href); now == first {
		t.Fatalf("the update left the ETag at %s", now)
	}
	sent := r.sent()

	for _, tc := range []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"PUT If-Match stale", r.put(href, thunderbird("stale@x", "Third"), "If-Match", first)},
		{"PUT If-None-Match on an object", r.put(href, thunderbird("stale@x", "Third"), "If-None-Match", "*")},
		{"DELETE If-Match stale", r.do(http.MethodDelete, href, "", "If-Match", first)},
	} {
		wantStatus(t, tc.rec, http.StatusPreconditionFailed)
		wantNoBody(t, tc.name, tc.rec)
	}
	if got := r.sent(); got != sent {
		t.Errorf("%d requests reached Proton's write endpoint after the 412s, want none", got-sent)
	}
	if b := r.body(t, href); !strings.Contains(b, "SUMMARY:Second\r\n") || strings.Contains(b, "Third") {
		t.Errorf("the object after the 412s:\n%s", b)
	}
}

// Every refusal the adapter or the service decides alone is answered
// without a request to Proton's write endpoint, and those decided before
// the service writes without any Proton request at all: a body that is not
// iCalendar (415), too large (413), an If-None-Match the service does not
// evaluate (412), a read-only calendar (403 need-privileges), a body the
// bridge refuses (403 with its precondition), a collection (405). The
// calendars stay as they were.
func TestRefusedWritesNeverReachProton(t *testing.T) {
	t.Parallel()
	var work, hol, shared string
	r := newServingRig(t, func(f *protontest.Fake) {
		work = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"})
		hol = f.AddCalendar(f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
		shared = f.AddCalendar(f.Username, protontest.Calendar{Name: "Shared", Permissions: 127 &^ 8})
		f.AddEvent(hol, protontest.Event{UID: "ny@x", Start: base, End: base + 3600, Card: card("ny@x", "New year", base)})
		f.AddEvent(shared, protontest.Event{UID: "s@x", Start: base, End: base + 3600, Color: green, Card: card("s@x", "Shared", base)})
	})
	parseMS(t, r.do("PROPFIND", home, tbHomeSet, "Depth", "1")) // loads the keys
	calls := r.f.Calls("", "/")

	ok := thunderbird("new@x", "New")
	for _, tc := range []struct {
		name string
		rec  *httptest.ResponseRecorder
		code int
		pcb  string // the 403's pcb precondition; "need-privileges" for that one
	}{
		{"no Content-Type", r.do(http.MethodPut, calPath(work)+"new@x.ics", ok), http.StatusUnsupportedMediaType, ""},
		{"text/plain", r.do(http.MethodPut, calPath(work)+"new@x.ics", ok, "Content-Type", "text/plain"), http.StatusUnsupportedMediaType, ""},
		{"over 8 MiB", r.put(calPath(work)+"new@x.ics", ok+strings.Repeat("X", 8<<20)), http.StatusRequestEntityTooLarge, ""},
		{"If-None-Match an ETag", r.put(calPath(work)+"new@x.ics", ok, "If-None-Match", `"v1-x"`), http.StatusPreconditionFailed, ""},
		{"holidays PUT", r.put(calPath(hol)+"new@x.ics", ok), http.StatusForbidden, "need-privileges"},
		{"holidays DELETE", r.do(http.MethodDelete, calPath(hol)+"ny@x.ics", ""), http.StatusForbidden, "need-privileges"},
		{"no write permission PUT", r.put(calPath(shared)+"s@x.ics", thunderbird("s@x", "Mine now")), http.StatusForbidden, "need-privileges"},
		{"no write permission DELETE", r.do(http.MethodDelete, calPath(shared)+"s@x.ics", ""), http.StatusForbidden, "need-privileges"},
		{"RDATE", r.put(calPath(work)+"rd@x.ics", thunderbird("rd@x", "Odd", "RDATE:20270120T080000Z")), http.StatusForbidden, "rdate"},
		{"two UIDs", r.put(calPath(work)+"two@x.ics", strings.Replace(ok, "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:other@x\r\nDTSTAMP:20270101T090000Z\r\nDTSTART:20270101T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)), http.StatusForbidden, "not-one-uid"},
		{"not iCalendar", r.put(calPath(work)+"junk.ics", "hello"), http.StatusForbidden, "bad-data"},
		{"floating time", r.put(calPath(work)+"fl@x.ics", strings.ReplaceAll(thunderbird("fl@x", "Floating"), "00Z\r\n", "00\r\n")), http.StatusForbidden, "floating-time"},
		{"PUT on the calendar", r.put(calPath(work), ok), http.StatusMethodNotAllowed, ""},
		{"DELETE the calendar", r.do(http.MethodDelete, calPath(work), ""), http.StatusMethodNotAllowed, ""},
	} {
		switch {
		case tc.pcb == "need-privileges":
			wantNeedPrivileges(t, tc.name, tc.rec)
		case tc.pcb != "":
			if got := precondition403(t, tc.rec); got != tc.pcb {
				t.Errorf("%s: pcb precondition %q, want %q", tc.name, got, tc.pcb)
			}
		default:
			if tc.rec.Code != tc.code {
				t.Errorf("%s: status %d, want %d", tc.name, tc.rec.Code, tc.code)
			}
		}
	}
	if got := r.f.Calls("", "/"); got != calls {
		t.Errorf("the refused writes made %d Proton requests, want none", got-calls)
	}
	// The name of another UID is reserved (ruling M4-R38): refused after the
	// refresh, never sent.
	if got := precondition403(t, r.put(calPath(work)+altName("ghost@x"), ok, "If-None-Match", "*")); got != "bad-resource-name" {
		t.Errorf("a create at another UID's alternative name: pcb precondition %q", got)
	}
	if n := r.sent(); n != 0 {
		t.Errorf("%d requests reached Proton's write endpoint, want none", n)
	}
	for _, c := range []string{work, hol, shared} {
		changed, deleted := listed(t, r.syncReport(t, c, ""))
		if want := map[string]int{work: 0, hol: 1, shared: 1}[c]; len(changed) != want || len(deleted) != 0 {
			t.Errorf("calendar %s lists %v, deleted %v; want its %d seeded events alone", c, changed, deleted, want)
		}
	}
}

// The privileges a calendar advertises follow the write gate (ruling
// M4-R1), and so do the writes: a normal calendar with the Write bit is
// read and write and takes a PUT; one without the bit, and a holidays
// calendar, are read only and refuse it.
func TestPrivilegesFollowTheWriteGate(t *testing.T) {
	t.Parallel()
	var work, hol, shared string
	r := newServingRig(t, func(f *protontest.Fake) {
		work = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"})
		hol = f.AddCalendar(f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
		shared = f.AddCalendar(f.Username, protontest.Calendar{Name: "Shared", Permissions: 127 &^ 8})
	})
	ms := parseMS(t, r.do("PROPFIND", home, tbHomeSet, "Depth", "1"))
	wantNames(t, "Work", privileges(t, ms.get(t, calPath(work)).prop(t, nsD, "current-user-privilege-set")),
		n(nsD, "read"), n(nsD, "write"), n(nsD, "write-content"), n(nsD, "bind"), n(nsD, "unbind"))
	for _, c := range []string{hol, shared} {
		wantNames(t, "a read-only calendar", privileges(t, ms.get(t, calPath(c)).prop(t, nsD, "current-user-privilege-set")), n(nsD, "read"))
		wantNeedPrivileges(t, "PUT", r.put(calPath(c)+"g@x.ics", thunderbird("g@x", "Gate"), "If-None-Match", "*"))
	}
	wantStatus(t, r.put(calPath(work)+"g@x.ics", thunderbird("g@x", "Gate"), "If-None-Match", "*"), http.StatusCreated)
	if n := r.sent(); n != 1 {
		t.Errorf("%d requests reached Proton's write endpoint, want the one create", n)
	}
}

// Thunderbird names an imported event after its UID with every character
// outside [a-zA-Z0-9_.-] replaced by "_" (E08 §4.4): the server must take
// that name and serve the event there, in every read, and only there (one
// href per UID, ruling M4-R6).
func TestCreateAtASanitisedNameIsServedThere(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	const uid = "import:42@example.org"
	alias, canonical := calPath(cal)+"import_42_example.org.ics", objPath(cal, uid)
	if alias == canonical {
		t.Fatal("test bug: the sanitised name is the canonical one")
	}
	rec := r.put(alias, thunderbird(uid, "Imported"), "If-None-Match", "*")
	wantStatus(t, rec, http.StatusCreated)
	wantNoBody(t, "create", rec)

	// Every read serves the object at the alias, with one ETag.
	etag := r.etag(t, alias)
	synced, _ := listed(t, r.syncReport(t, cal, ""))
	if len(synced) != 1 || synced[alias] != etag {
		t.Errorf("sync-collection lists %v, want %s alone with %s", synced, alias, etag)
	}
	listing := parseMS(t, r.do("PROPFIND", calPath(cal), tbEtags, "Depth", "1"))
	if len(listing.responses) != 2 || listing.get(t, alias).prop(t, nsD, "getetag").Text != etag {
		t.Errorf("the Depth 1 listing:\n%s", listing.raw)
	}
	// The same with calendar-data: rendered at the alias too.
	withData := `<sync-collection xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><sync-token/><sync-level>1</sync-level>` +
		`<prop><getetag/><C:calendar-data/></prop></sync-collection>`
	full := parseMS(t, r.do("REPORT", calPath(cal), withData, "Depth", "1"))
	if len(full.responses) != 1 || !strings.Contains(full.get(t, alias).prop(t, nsC, "calendar-data").Text, "UID:"+uid+"\r\n") {
		t.Errorf("sync-collection with calendar-data:\n%s", full.raw)
	}
	listing = parseMS(t, r.do("PROPFIND", calPath(cal), `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop></D:propfind>`, "Depth", "1"))
	if len(listing.responses) != 2 || listing.get(t, alias).prop(t, nsD, "getetag").Text != etag {
		t.Errorf("the Depth 1 listing with calendar-data:\n%s", listing.raw)
	}
	got := parseMS(t, r.do("REPORT", calPath(cal), tbMultiget(alias), "Depth", "1")).get(t, alias)
	if d := got.prop(t, nsC, "calendar-data").Text; !strings.Contains(d, "UID:"+uid+"\r\n") || got.prop(t, nsD, "getetag").Text != etag {
		t.Errorf("multiget of the alias: ETag %q, data:\n%s", got.prop(t, nsD, "getetag").Text, d)
	}
	if b := r.body(t, alias); !strings.Contains(b, "SUMMARY:Imported\r\n") {
		t.Errorf("GET of the alias:\n%s", b)
	}
	if head := r.do(http.MethodHead, alias, ""); head.Code != http.StatusOK || head.Header().Get("ETag") != etag {
		t.Errorf("HEAD of the alias: %d, ETag %q", head.Code, head.Header().Get("ETag"))
	}

	// The UID's canonical name serves nothing.
	wantStatus(t, r.do(http.MethodGet, canonical, ""), http.StatusNotFound)
	wantStatus(t, r.do("PROPFIND", canonical, tbEtags, "Depth", "0"), http.StatusNotFound)
	if m := parseMS(t, r.do("REPORT", calPath(cal), tbMultiget(canonical), "Depth", "1")).get(t, canonical); m.status != "HTTP/1.1 404 Not Found" {
		t.Errorf("multiget of the canonical name: %+v", m)
	}
	wantStatus(t, r.do(http.MethodDelete, canonical, ""), http.StatusNotFound)
	if r.body(t, alias) == "" {
		t.Error("the DELETE at the canonical name removed the object")
	}
}

// An alias is the object's only href for writes too: an update and a
// DELETE at it work, a sync-collection reports both there, never at the
// canonical name.
func TestAnAliasIsWrittenAndDeletedThere(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	const uid = "meet@example.org"
	alias := calPath(cal) + "meet_example.org.ics"
	wantStatus(t, r.put(alias, thunderbird(uid, "Meet"), "If-None-Match", "*"), http.StatusCreated)
	tok := *r.syncReport(t, cal, "").token

	wantStatus(t, r.put(alias, thunderbird(uid, "Meet again"), "If-Match", r.etag(t, alias)), http.StatusNoContent)
	delta := r.syncReport(t, cal, tok)
	changed, deleted := listed(t, delta)
	if len(changed) != 1 || changed[alias] == "" || len(deleted) != 0 {
		t.Errorf("after the update: changed %v, deleted %v; want the alias alone", changed, deleted)
	}
	if b := r.body(t, alias); !strings.Contains(b, "SUMMARY:Meet again\r\n") {
		t.Errorf("the alias after the update:\n%s", b)
	}

	rec := r.do(http.MethodDelete, alias, "", "If-Match", r.etag(t, alias))
	wantStatus(t, rec, http.StatusNoContent)
	wantNoBody(t, "DELETE", rec)
	changed, deleted = listed(t, r.syncReport(t, cal, *delta.token))
	if len(changed) != 0 || len(deleted) != 1 || deleted[0] != alias {
		t.Errorf("after the DELETE: changed %v, deleted %v; want the alias deleted", changed, deleted)
	}
	wantStatus(t, r.do(http.MethodHead, alias, ""), http.StatusNotFound)
	if n := r.sent(); n != 3 {
		t.Errorf("%d requests reached Proton's write endpoint, want 3", n)
	}
}

// A UID that lives at another name is a 409 naming that name, the href the
// client must use (E08 MUST 10, RFC 4791 §5.3.2.1); so is a name that holds
// another UID. Nothing is sent.
func TestUIDConflictNamesTheOtherHref(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	first := calPath(cal) + "My Event.ics"
	wantStatus(t, r.put(calPath(cal)+"My%20Event.ics", thunderbird("u@x", "First"), "If-None-Match", "*"), http.StatusCreated)
	sent := r.sent()

	for _, tc := range []struct{ name, target, body string }{
		{"the UID at another name", calPath(cal) + "second.ics", thunderbird("u@x", "Again")},
		{"the UID at its canonical name", objPath(cal, "u@x"), thunderbird("u@x", "Again")},
		{"another UID at the name", calPath(cal) + "My%20Event.ics", thunderbird("v@x", "Other")},
	} {
		h := conflictHref(t, r.put(tc.target, tc.body, "If-None-Match", "*"))
		u, err := urlPath(h)
		if err != nil || u != first || !strings.HasPrefix(h, "/") {
			t.Errorf("%s: no-uid-conflict href %q, want the path %q", tc.name, h, first)
		}
	}
	if got := r.sent(); got != sent {
		t.Errorf("%d requests reached Proton's write endpoint after the conflicts, want none", got-sent)
	}
	wantStatus(t, r.do(http.MethodGet, calPath(cal)+"second.ics", ""), http.StatusNotFound)
	if b := r.body(t, calPath(cal)+"My%20Event.ics"); !strings.Contains(b, "SUMMARY:First\r\n") || !strings.Contains(b, "UID:u@x\r\n") {
		t.Errorf("the object after the conflicts:\n%s", b)
	}
}

// DELETE of a name that serves no object is a 404 (Thunderbird: already
// gone, a success, E08 §4.3), whatever its If-Match, and nothing is sent;
// so is a second DELETE, and a HEAD after it.
func TestDeleteMissingIs404(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	for _, hdr := range [][]string{nil, {"If-Match", `"v1-gone"`}} {
		rec := r.do(http.MethodDelete, objPath(cal, "nobody@x"), "", hdr...)
		wantStatus(t, rec, http.StatusNotFound)
		wantNoBody(t, "DELETE of nothing", rec)
	}
	if n := r.sent(); n != 0 {
		t.Errorf("%d requests reached Proton's write endpoint for a missing object", n)
	}
	href := objPath(cal, "d@x")
	wantStatus(t, r.put(href, thunderbird("d@x", "Doomed"), "If-None-Match", "*"), http.StatusCreated)
	wantStatus(t, r.do(http.MethodDelete, href, ""), http.StatusNoContent)
	wantStatus(t, r.do(http.MethodDelete, href, ""), http.StatusNotFound)
	wantStatus(t, r.do(http.MethodHead, href, ""), http.StatusNotFound)
	if n := r.sent(); n != 2 {
		t.Errorf("%d requests reached Proton's write endpoint, want the create and one delete", n)
	}
}

// After a PUT, Thunderbird ignores the answer's ETag and multigets the
// object (E08 §4.1): what it gets is what Proton holds, not the body it
// sent: the change Proton refuses is gone (STATUS:TENTATIVE, constraints),
// Thunderbird's own properties are kept (E08 MUST 13), and a sync pass from
// Proton serves the same bytes under the same ETag.
func TestWriteThenMultigetSeesProtonsBytes(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	href := objPath(cal, "tb-1234-abcd")
	body := thunderbird("tb-1234-abcd", "Lunch <with> Zoé", "STATUS:TENTATIVE", "X-MOZ-GENERATION:1",
		"X-MOZ-LASTACK:20270114T070000Z", "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15M", "DESCRIPTION:Default Mozilla Description", "END:VALARM")
	rec := r.put(href, body, "If-None-Match", "*")
	wantStatus(t, rec, http.StatusCreated)
	wantNoBody(t, "create", rec)

	got := parseMS(t, r.do("REPORT", calPath(cal), tbMultiget(tbHref(cal, "tb-1234-abcd")), "Depth", "1")).get(t, href)
	data, etag := got.prop(t, nsC, "calendar-data").Text, got.prop(t, nsD, "getetag").Text
	for _, want := range []string{"UID:tb-1234-abcd\r\n", "SUMMARY:Lunch <with> Zoé\r\n", "X-MOZ-GENERATION:1\r\n", "X-MOZ-LASTACK:20270114T070000Z\r\n", "TRIGGER:-PT15M\r\n"} {
		if !strings.Contains(data, want) {
			t.Errorf("the multiget lacks %q:\n%s", want, data)
		}
	}
	if strings.Contains(data, "TENTATIVE") || data == body {
		t.Errorf("the multiget serves the body sent, not what Proton holds:\n%s", data)
	}
	tok := *r.syncReport(t, cal, "").token

	r.next(t) // a pass reads the calendar from Proton
	again := parseMS(t, r.do("REPORT", calPath(cal), tbMultiget(tbHref(cal, "tb-1234-abcd")), "Depth", "1")).get(t, href)
	if a := again.prop(t, nsC, "calendar-data").Text; a != data || again.prop(t, nsD, "getetag").Text != etag {
		t.Errorf("after a pass from Proton: ETag %q (was %q), data:\n%s\nwas:\n%s", again.prop(t, nsD, "getetag").Text, etag, a, data)
	}
	if changed, deleted := listed(t, r.syncReport(t, cal, tok)); len(changed)+len(deleted) != 0 {
		t.Errorf("the pass after the write changed %v, deleted %v", changed, deleted)
	}
}

// E08 MUST 12 across writes: each write changes the object's ETag, and the
// new one is the same in sync-collection, the Depth 1 listing, multiget,
// PROPFIND and HEAD.
func TestMust12ETagsChangeWithWritesAndAgree(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	href := objPath(cal, "e@x")
	seen := map[string]bool{}
	for i, summary := range []string{"One", "Two", "Three"} {
		hdr := []string{"If-None-Match", "*"}
		if i > 0 {
			hdr = []string{"If-Match", r.etag(t, href)}
		}
		wantStatus(t, r.put(href, thunderbird("e@x", summary), hdr...), map[bool]int{true: http.StatusCreated, false: http.StatusNoContent}[i == 0])
		synced, _ := listed(t, r.syncReport(t, cal, ""))
		etags := []string{
			synced[href],
			parseMS(t, r.do("PROPFIND", calPath(cal), tbEtags, "Depth", "1")).get(t, href).prop(t, nsD, "getetag").Text,
			parseMS(t, r.do("REPORT", calPath(cal), tbMultiget(href), "Depth", "1")).get(t, href).prop(t, nsD, "getetag").Text,
			r.etag(t, href),
			r.do(http.MethodHead, href, "").Header().Get("ETag"),
		}
		for _, e := range etags[1:] {
			if e != etags[0] {
				t.Errorf("after %s: ETags %q differ", summary, etags)
				break
			}
		}
		if seen[etags[0]] || etags[0] == "" {
			t.Errorf("after %s: ETag %q seen before", summary, etags[0])
		}
		seen[etags[0]] = true
	}
}

// Ruling M4-R38: an alias made before a UID existed keeps its name when an
// object of that UID, whose canonical name it is, appears later (here from
// the web app); that object is served at its alternative name, in every
// read and for every write, and never two objects share an href.
func TestALaterUIDMovesAsideForAnAlias(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	held, alt := objPath(cal, "x@x"), calPath(cal)+altName("x@x")
	wantStatus(t, r.put(held, thunderbird("y@x", "Mine"), "If-None-Match", "*"), http.StatusCreated)
	r.f.AddEvent(cal, protontest.Event{UID: "x@x", Start: base, End: base + 3600, Color: green, Card: card("x@x", "From the web", base)})
	r.next(t)

	synced, _ := listed(t, r.syncReport(t, cal, ""))
	if len(synced) != 2 || synced[held] == "" || synced[alt] == "" || synced[held] == synced[alt] {
		t.Fatalf("sync-collection lists %v, want %s and %s", synced, held, alt)
	}
	ms := parseMS(t, r.do("REPORT", calPath(cal), tbMultiget(held, alt), "Depth", "1"))
	if d := ms.get(t, held).prop(t, nsC, "calendar-data").Text; !strings.Contains(d, "UID:y@x\r\n") {
		t.Errorf("the alias serves:\n%s", d)
	}
	if d := ms.get(t, alt).prop(t, nsC, "calendar-data").Text; !strings.Contains(d, "UID:x@x\r\n") {
		t.Errorf("the alternative name serves:\n%s", d)
	}
	if listing := parseMS(t, r.do("PROPFIND", calPath(cal), tbEtags, "Depth", "1")); len(listing.responses) != 3 ||
		listing.get(t, alt).prop(t, nsD, "getetag").Text != synced[alt] {
		t.Errorf("the Depth 1 listing:\n%s", listing.raw)
	}
	tok := *r.syncReport(t, cal, "").token

	// Writes reach x@x at its alternative name only.
	if h := conflictHref(t, r.put(held, thunderbird("x@x", "Mine too"), "If-Match", synced[alt])); h != alt {
		t.Errorf("x@x at its canonical name: no-uid-conflict %q, want %q", h, alt)
	}
	wantStatus(t, r.put(alt, thunderbird("x@x", "Edited"), "If-Match", synced[alt]), http.StatusNoContent)
	if b := r.body(t, alt); !strings.Contains(b, "SUMMARY:Edited\r\n") {
		t.Errorf("x@x after the edit:\n%s", b)
	}
	if b := r.body(t, held); !strings.Contains(b, "UID:y@x\r\n") || !strings.Contains(b, "SUMMARY:Mine\r\n") {
		t.Errorf("the alias after x@x's edit:\n%s", b)
	}
	wantStatus(t, r.do(http.MethodDelete, alt, ""), http.StatusNoContent)
	changed, deleted := listed(t, r.syncReport(t, cal, tok))
	if len(changed) != 0 || len(deleted) != 1 || deleted[0] != alt {
		t.Errorf("after x@x's edit and deletion: changed %v, deleted %v; want %s deleted", changed, deleted, alt)
	}
	if b := r.body(t, held); !strings.Contains(b, "UID:y@x\r\n") {
		t.Errorf("the alias after x@x's deletion:\n%s", b)
	}
}

// Proton unavailable or rate limiting is a 503 with Retry-After (Proton's
// own wait when it gave one), never a 4xx, and the write is never sent
// again; a service that does not serve (no pcb serve) is a 503 too.
func TestUnavailableWritesAre503WithRetryAfter(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	syncPath := "/calendar/v1/" + cal + "/events/sync"
	for _, tc := range []struct {
		status, retry int
		want          string
	}{
		{http.StatusServiceUnavailable, 0, "30"},
		{http.StatusTooManyRequests, 7, "7"},
	} {
		before := r.sent()
		r.f.FailNext(http.MethodPut, syncPath, tc.status, 0, tc.retry)
		rec := r.put(objPath(cal, "busy@x"), thunderbird("busy@x", "Busy"), "If-None-Match", "*")
		wantStatus(t, rec, http.StatusServiceUnavailable)
		if got := rec.Header().Get("Retry-After"); got != tc.want {
			t.Errorf("Proton %d: Retry-After %q, want %q", tc.status, got, tc.want)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("Proton %d: body %q", tc.status, rec.Body)
		}
		if n := r.sent() - before; n != 1 {
			t.Errorf("Proton %d: the write reached Proton %d times, want once", tc.status, n)
		}
	}
	wantStatus(t, r.do(http.MethodGet, objPath(cal, "busy@x"), ""), http.StatusNotFound)

	var idleCal string
	idle := newRig(t, func(f *protontest.Fake) { idleCal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	for _, rec := range []*httptest.ResponseRecorder{
		idle.put(objPath(idleCal, "a@x"), thunderbird("a@x", "A"), "If-None-Match", "*"),
		idle.do(http.MethodDelete, objPath(idleCal, "a@x"), ""),
	} {
		wantStatus(t, rec, http.StatusServiceUnavailable)
		if rec.Header().Get("Retry-After") != "30" {
			t.Errorf("outside Serve: Retry-After %q", rec.Header().Get("Retry-After"))
		}
	}
	if n := idle.sent(); n != 0 {
		t.Errorf("outside Serve %d writes reached Proton", n)
	}
}

// An independent client writes over a real listener: go-webdav's
// PutCalendarObject (no precondition: a forced write) creates and replaces
// an object, MultiGetCalendar reads it back, RemoveAll deletes it.
func TestGoWebdavClientWritesAndReadsBack(t *testing.T) {
	t.Parallel()
	var cal string
	r := newServingRig(t, func(f *protontest.Fake) { cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"}) })
	srv := httptest.NewServer(r.h)
	t.Cleanup(srv.Close)
	c, err := webdavcaldav.NewClient(webdav.HTTPClientWithBasicAuth(srv.Client(), "user@proton.me", r.pass), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	p := calPath(cal) + "gowebdav.ics"
	event := func(summary string) *ical.Calendar {
		ev := ical.NewEvent()
		ev.Props.SetText(ical.PropUID, "gowebdav@x")
		ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Unix(base, 0).UTC())
		ev.Props.SetDateTime(ical.PropDateTimeStart, time.Unix(base, 0).UTC())
		ev.Props.SetDateTime(ical.PropDateTimeEnd, time.Unix(base+3600, 0).UTC())
		ev.Props.SetText(ical.PropSummary, summary)
		cal := ical.NewCalendar()
		cal.Props.SetText(ical.PropVersion, "2.0")
		cal.Props.SetText(ical.PropProductID, "-//go-webdav test//EN")
		cal.Children = append(cal.Children, ev.Component)
		return cal
	}
	read := func() webdavcaldav.CalendarObject {
		t.Helper()
		objs, err := c.MultiGetCalendar(ctx, calPath(cal), &webdavcaldav.CalendarMultiGet{
			Paths:       []string{p},
			CompRequest: webdavcaldav.CalendarCompRequest{Name: "VCALENDAR", AllProps: true, AllComps: true},
		})
		if err != nil || len(objs) != 1 || objs[0].Path != p || objs[0].ETag == "" {
			t.Fatalf("MultiGetCalendar = %+v, %v", objs, err)
		}
		return objs[0]
	}

	put, err := c.PutCalendarObject(ctx, p, event("Written by go-webdav"))
	if err != nil {
		t.Fatalf("PutCalendarObject (create): %v", err)
	}
	if put.ETag != "" {
		t.Errorf("the PUT answered ETag %q, want none", put.ETag)
	}
	first := read()
	if got := summary(t, first.Data); got != "Written by go-webdav" {
		t.Errorf("summary %q after the create", got)
	}
	if _, err := c.PutCalendarObject(ctx, p, event("Rewritten")); err != nil {
		t.Fatalf("PutCalendarObject (replace): %v", err)
	}
	second := read()
	if got := summary(t, second.Data); got != "Rewritten" || second.ETag == first.ETag {
		t.Errorf("after the replace: summary %q, ETag %q (was %q)", got, second.ETag, first.ETag)
	}
	if err := c.RemoveAll(ctx, p); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := c.GetCalendarObject(ctx, p); err == nil {
		t.Error("GetCalendarObject after the delete found the object")
	}
	if n := r.sent(); n != 3 {
		t.Errorf("%d requests reached Proton's write endpoint, want 3", n)
	}
}

// Ruling M4-R39: a write is 403 need-privileges only when the gate is
// decided from good data (here the Write bit, turned off with
// SetPermissions); when it cannot be decided (a bootstrap Proton refused,
// whatever its status, or a passphrase that opens no key) the write is a
// 503 with Retry-After, which Thunderbird retries, and nothing is sent.
func TestUndecidedWriteGateIs503(t *testing.T) {
	t.Parallel()
	statuses := []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity}
	undecided := map[string]string{}
	var decided, holidays string
	r := newServingRig(t, func(f *protontest.Fake) {
		for _, st := range statuses {
			id := f.AddCalendar(f.Username, protontest.Calendar{Name: fmt.Sprint("Bootstrap ", st)})
			f.AddEvent(id, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "A", base)})
			f.FailNext(http.MethodGet, "/calendar/v2/"+id+"/bootstrap", st, 2501, 0)
			undecided[id] = fmt.Sprint("bootstrap ", st)
		}
		locked := f.AddCalendar(f.Username, protontest.Calendar{Name: "Locked", BrokenPassphrase: true})
		f.AddEvent(locked, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "A", base)})
		undecided[locked] = "keys locked"
		// A holidays calendar is decided by its cached type, keys or not.
		hol := f.AddCalendar(f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
		f.AddEvent(hol, protontest.Event{UID: "h@x", Start: base, End: base + 3600, Card: card("h@x", "H", base)})
		f.FailNext(http.MethodGet, "/calendar/v2/"+hol+"/bootstrap", http.StatusUnprocessableEntity, 2501, 0)
		holidays = hol
		decided = f.AddCalendar(f.Username, protontest.Calendar{Name: "Viewer"})
		f.AddEvent(decided, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "A", base)})
		f.SetPermissions(decided, 127&^8)
	})
	ms := parseMS(t, r.do("PROPFIND", home, tbHomeSet, "Depth", "1"))
	for id, what := range undecided {
		// Never decided: advertised read only, but a write is retried, not refused.
		wantNames(t, what, privileges(t, ms.get(t, calPath(id)).prop(t, nsD, "current-user-privilege-set")), n(nsD, "read"))
		for _, rec := range []*httptest.ResponseRecorder{
			r.put(objPath(id, "n@x"), thunderbird("n@x", "New"), "If-None-Match", "*"),
			r.put(objPath(id, "a@x"), thunderbird("a@x", "Edited")),
			r.do(http.MethodDelete, objPath(id, "a@x"), ""),
		} {
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" || rec.Body.Len() != 0 {
				t.Errorf("%s: %d, Retry-After %q, body %q; want 503, 30, none", what, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
			}
		}
	}
	wantNeedPrivileges(t, "PUT, Write bit off", r.put(objPath(decided, "n@x"), thunderbird("n@x", "New"), "If-None-Match", "*"))
	wantNeedPrivileges(t, "DELETE, Write bit off", r.do(http.MethodDelete, objPath(decided, "a@x"), ""))
	wantNeedPrivileges(t, "PUT, holidays without keys", r.put(objPath(holidays, "n@x"), thunderbird("n@x", "New"), "If-None-Match", "*"))
	if n := r.sent(); n != 0 {
		t.Errorf("%d requests reached Proton's write endpoint", n)
	}
}
