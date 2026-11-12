package caldav_test

// The E08 contract (spec/experiments/E08-thunderbird-caldav.md, "Conclusion")
// as tests, one per MUST/SHOULD/AVOID item that applies to a read-only
// server, named after it. They run the handler on a stub service.API, so
// every answer of the service (errors included) is the test's choice; the
// real service is behind report_test.go and webdav_client_test.go.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const (
	password  = "the-test-client-app-password"
	davHeader = "1, 3, calendar-access"
	challenge = `Basic realm="pcb", charset="UTF-8"`

	nsD  = "DAV:"
	nsC  = "urn:ietf:params:xml:ns:caldav"
	nsCS = "http://calendarserver.org/ns/"
	nsA  = "http://apple.com/ns/ical/"

	principal = "/dav/principals/me/"
	home      = "/dav/calendars/me/"

	validSyncTokenBody  = `<D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`
	supportedReportBody = `<D:error xmlns:D="DAV:"><D:supported-report/></D:error>`
	finiteDepthBody     = `<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`
	// rateLimitBody with a 403 makes Thunderbird skip the refresh and keep
	// its token (E08 §3.3): never sent.
	rateLimitBody = `<D:error xmlns:D="DAV:"/>`

	// secretText is an error text of the service: it must never reach a body.
	secretText = "proton refused session 0123abcd of user 42"
)

// Thunderbird's request bodies, byte for byte as E08 quotes them.
const (
	tbDetect = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D='DAV:' xmlns:A='http://apple.com/ns/ical/' xmlns:C='urn:ietf:params:xml:ns:caldav'><D:prop>
 <D:resourcetype/><D:owner/><D:displayname/><D:current-user-principal/><D:current-user-privilege-set/>
 <A:calendar-color/><C:calendar-home-set/></D:prop></D:propfind>`
	tbHomeSet = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D='DAV:' xmlns:A='http://apple.com/ns/ical/'><D:prop>
 <D:resourcetype/><D:displayname/><D:current-user-privilege-set/><A:calendar-color/></D:prop></D:propfind>`
	tbServerInfo = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D='DAV:' xmlns:CS='http://calendarserver.org/ns/' xmlns:C='urn:ietf:params:xml:ns:caldav'><D:prop>
 <D:resourcetype/><D:owner/><D:current-user-principal/><D:current-user-privilege-set/><D:supported-report-set/>
 <C:supported-calendar-component-set/><CS:getctag/></D:prop></D:propfind>`
	tbEtags = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D='DAV:'><D:prop><D:getcontenttype/><D:resourcetype/><D:getetag/></D:prop></D:propfind>`
)

func tbSync(token string) string {
	tok := "<sync-token/>"
	if token != "" {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(token))
		tok = "<sync-token>" + b.String() + "</sync-token>"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<sync-collection xmlns="DAV:">` + tok + `
<sync-level>1</sync-level><prop><getcontenttype/><getetag/></prop></sync-collection>`
}

func tbMultiget(hrefs ...string) string {
	var b strings.Builder
	b.WriteString(`<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop>` + "\n")
	for _, h := range hrefs {
		b.WriteString("<D:href>")
		_ = xml.EscapeText(&b, []byte(h))
		b.WriteString("</D:href>")
	}
	b.WriteString("</C:calendar-multiget>")
	return b.String()
}

// tbEncode is JavaScript's encodeURIComponent, which Thunderbird applies to
// each segment of a multiget href (E08 §3.4).
func tbEncode(seg string) string {
	var b strings.Builder
	for _, c := range []byte(seg) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// calPath is a calendar's collection URL: the first 16 hex digits of
// sha256 of its ID under the home set.
func calPath(id string) string {
	sum := sha256.Sum256([]byte(id))
	return home + hex.EncodeToString(sum[:])[:16] + "/"
}

var plainName = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,200}$`)

// objName is the resource name of a UID: the UID when it is plain, else
// "~" + its unpadded base64url, then ".ics" (ruling M3-R13).
func objName(uid string) string {
	if plainName.MatchString(uid) {
		return uid + ".ics"
	}
	return "~" + base64.RawURLEncoding.EncodeToString([]byte(uid)) + ".ics"
}

func objPath(calID, uid string) string { return calPath(calID) + objName(uid) }

// tbHref is objPath as Thunderbird sends it: each segment encodeURIComponent'ed.
func tbHref(calID, uid string) string {
	segs := strings.Split(objPath(calID, uid), "/")
	for i, s := range segs {
		segs[i] = tbEncode(s)
	}
	return strings.Join(segs, "/")
}

// The stub's data.
const (
	workID   = "Work+calendar/ID=="
	holID    = "Holidays-ID=="
	plainUID = "plain-1.2_3@example.org"
	oddUID   = "odd uid/é?#%~"
	lastUID  = "last@x"
)

func ics(uid, summary string, extra ...string) []byte {
	lines := append([]string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//pcb//proton-calendar-bridge//EN", "CALSCALE:GREGORIAN",
		"BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20270115T080000Z", "DTSTART:20270115T080000Z", "SUMMARY:" + summary}, extra...)
	lines = append(lines, "END:VEVENT", "END:VCALENDAR", "")
	return []byte(strings.Join(lines, "\r\n"))
}

// richData has what must survive byte for byte: CRLF, UTF-8, XML
// specials, Thunderbird's X- properties, a folded line.
var richData = ics(oddUID, "Lunch <with> Zoé & \"co\" ]]>", "X-MOZ-LASTACK:20270115T070000Z", "X-MOZ-SNOOZE-TIME:20270115T075000Z",
	"DESCRIPTION:a long line that is folded at seventy-five octets for the test of the f", " olding\tand a tab",
	"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT1H", "X-MOZ-LASTACK:20270115T070000Z", "END:VALARM")

// stub is a service.API whose read and auth methods answer from fields;
// any other method panics (nil embedded interface).
type stub struct {
	service.API

	mu       sync.Mutex
	colls    []service.Collection
	objs     map[string][]service.Object     // calID → objects, listing order
	deltas   map[string]service.ChangeSet    // token → answer, any calendar
	gone     map[string]bool                 // calIDs Collections lists but reads call unknown
	pending  map[string]bool                 // calIDs whose reads say ErrCalendarPending
	readErr  error                           // every read fails with it
	wait     time.Duration                   // CheckClient's retryAfter on a refusal
	checks   int                             // CheckClient calls
	reads    int                             // read calls
	uidReqs  [][]string                      // ObjectsByUID's uids, per call
	nameReqs [][]string                      // ObjectsByName's names, per call
	before   func(ctx context.Context) error // runs first in each read, when set
	// aliases[calID][uid] is the name uid is served at instead of its
	// canonical one.
	aliases map[string]map[string]string
	// The writes: each call recorded, answered with putRes or writeErr.
	writes   []write
	putRes   service.PutResult
	writeErr error
	// slow, when set, runs in each write before it answers, outside the
	// lock; its error is the write's (a write that outlasts the listener's
	// timeouts, or whose context ends meanwhile).
	slow func(ctx context.Context) error
	// misnamed makes ObjectsByName answer each object under another name
	// (a service that broke its contract).
	misnamed bool
}

// write is a PutObject or DeleteObject call the stub received.
type write struct {
	method, calID, name string
	body                []byte
	cond                service.Precondition
}

// served is the name the stub serves uid at in calID.
func (s *stub) served(calID, uid string) string {
	if a, ok := s.aliases[calID][uid]; ok {
		return a
	}
	return service.ObjectName(uid)
}

// dels lists deletions for a ChangeSet (the stub names them).
func dels(uids ...string) []service.ObjectInfo {
	out := []service.ObjectInfo{}
	for _, u := range uids {
		out = append(out, service.ObjectInfo{UID: u})
	}
	return out
}

func newStub() *stub {
	return &stub{
		colls: []service.Collection{
			{ID: workID, Name: "Work", Color: "#3CBB3A", Description: "Team", CTag: "e1:7:hash1", SyncToken: "urn:pcb:sync:1:e1:7:gen1"},
			{ID: holID, Name: "Holidays", ReadOnly: true, CTag: "e1:3:hash2", SyncToken: "urn:pcb:sync:1:e1:3:gen2"},
		},
		objs: map[string][]service.Object{
			workID: {
				{UID: plainUID, ETag: `"v1-plain"`, Data: ics(plainUID, "Standup")},
				{UID: oddUID, ETag: `"v1-odd"`, Data: richData},
				{UID: lastUID, ETag: `"v1-last"`, Data: ics(lastUID, "Last")},
			},
			holID: {{UID: "newyear@x", ETag: `"v1-ny"`, Data: ics("newyear@x", "New year")}},
		},
		deltas:  map[string]service.ChangeSet{},
		gone:    map[string]bool{},
		pending: map[string]bool{},
		aliases: map[string]map[string]string{},
	}
}

func (s *stub) CheckClient(_ context.Context, pw, _ string) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	if pw == password {
		return true, 0
	}
	return false, s.wait
}

func noSuchCalendar() error {
	return &exitcode.Error{Code: exitcode.Usage, Err: service.ErrNoSuchCalendar, Hint: "x"}
}

func (s *stub) read(ctx context.Context, calID string) error {
	s.mu.Lock()
	s.reads++
	before, err := s.before, s.readErr
	gone, pending := s.gone[calID], s.pending[calID]
	s.mu.Unlock()
	if before != nil {
		if err := before(ctx); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	if pending {
		return &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrCalendarPending, Hint: "x"}
	}
	if _, ok := s.objs[calID]; calID != "" && (!ok || gone) {
		return noSuchCalendar()
	}
	return nil
}

func (s *stub) Collections(ctx context.Context) ([]service.Collection, error) {
	if err := s.read(ctx, ""); err != nil {
		return nil, err
	}
	return slices.Clone(s.colls), nil
}

func (s *stub) Objects(ctx context.Context, calID string) ([]service.ObjectInfo, string, error) {
	if err := s.read(ctx, calID); err != nil {
		return nil, "", err
	}
	var out []service.ObjectInfo
	for _, o := range s.objs[calID] {
		out = append(out, service.ObjectInfo{UID: o.UID, Name: s.served(calID, o.UID), ETag: o.ETag})
	}
	for _, c := range s.colls {
		if c.ID == calID {
			return out, c.SyncToken, nil
		}
	}
	return out, "", nil
}

func (s *stub) ObjectsByUID(ctx context.Context, calID string, uids []string) ([]service.Object, error) {
	if err := s.read(ctx, calID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.uidReqs = append(s.uidReqs, slices.Clone(uids))
	s.mu.Unlock()
	out := make([]service.Object, len(uids))
	for i, uid := range uids {
		out[i] = service.Object{UID: uid, Name: s.served(calID, uid), Missing: true}
		for _, o := range s.objs[calID] {
			if o.UID == uid {
				out[i] = o
				out[i].Name = s.served(calID, uid)
			}
		}
	}
	return out, nil
}

// ObjectsByName serves each object at its served name only.
func (s *stub) ObjectsByName(ctx context.Context, calID string, names []string) ([]service.Object, error) {
	if err := s.read(ctx, calID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.nameReqs = append(s.nameReqs, slices.Clone(names))
	s.mu.Unlock()
	out := make([]service.Object, len(names))
	for i, name := range names {
		out[i] = service.Object{Name: name, Missing: true}
		for _, o := range s.objs[calID] {
			if s.served(calID, o.UID) == name {
				out[i] = o
				out[i].Name = name
				if s.misnamed {
					out[i].Name = "not-" + name
				}
			}
		}
	}
	return out, nil
}

func (s *stub) PutObject(ctx context.Context, calID, name string, body []byte, cond service.Precondition) (service.PutResult, error) {
	if s.slow != nil {
		if err := s.slow(ctx); err != nil {
			return service.PutResult{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, write{method: http.MethodPut, calID: calID, name: name, body: slices.Clone(body), cond: cond})
	if s.writeErr != nil {
		return service.PutResult{}, s.writeErr
	}
	return s.putRes, nil
}

func (s *stub) DeleteObject(ctx context.Context, calID, name string, cond service.Precondition) error {
	if s.slow != nil {
		if err := s.slow(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, write{method: http.MethodDelete, calID: calID, name: name, cond: cond})
	return s.writeErr
}

func (s *stub) writeCalls() []write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.writes)
}

func (s *stub) Changes(ctx context.Context, calID, token string) (service.ChangeSet, error) {
	if err := s.read(ctx, calID); err != nil {
		return service.ChangeSet{}, err
	}
	if token == "" {
		infos, tok, _ := s.Objects(ctx, calID)
		return service.ChangeSet{Changed: infos, Deleted: []service.ObjectInfo{}, Token: tok}, nil
	}
	if cs, ok := s.deltas[token]; ok {
		out := service.ChangeSet{Changed: slices.Clone(cs.Changed), Deleted: slices.Clone(cs.Deleted), Token: cs.Token}
		for i, o := range out.Changed {
			out.Changed[i].Name = s.served(calID, o.UID)
		}
		for i, o := range out.Deleted {
			out.Deleted[i].Name = s.served(calID, o.UID)
		}
		return out, nil
	}
	return service.ChangeSet{}, &exitcode.Error{Code: exitcode.Usage, Err: service.ErrInvalidSyncToken, Hint: "x"}
}

func (s *stub) counts() (checks, reads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks, s.reads
}

// call builds a request; hdr is name, value pairs.
func call(method, target, body string, hdr ...string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return r
}

// authed adds the client's credentials (any username works).
func authed(r *http.Request) *http.Request {
	r.SetBasicAuth("someone@example.org", password)
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// do serves an authenticated request.
func do(h http.Handler, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	return serve(h, authed(call(method, target, body, hdr...)))
}

// el is any XML element of a response, namespaces resolved.
type el struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Text    string     `xml:",chardata"`
	Kids    []el       `xml:",any"`
}

func (e el) kid(name xml.Name) (el, bool) {
	for _, k := range e.Kids {
		if k.XMLName == name {
			return k, true
		}
	}
	return el{}, false
}

func (e el) kidNames() []xml.Name {
	var out []xml.Name
	for _, k := range e.Kids {
		out = append(out, k.XMLName)
	}
	return out
}

// hrefOf is the text of e's DAV:href child.
func (e el) hrefOf(t *testing.T) string {
	t.Helper()
	h, ok := e.kid(n(nsD, "href"))
	if !ok {
		t.Fatalf("%v has no DAV:href", e.XMLName)
	}
	return h.Text
}

type response struct {
	href      string
	status    string            // response-level status, "" if none
	props     map[xml.Name]el   // in a 200 propstat
	missing   map[xml.Name]bool // in a 404 propstat
	propstats int
}

type multistatus struct {
	responses []response
	byPath    map[string]response // by decoded href path
	token     *string             // DAV:sync-token, nil if absent
	raw       string
}

func n(space, local string) xml.Name { return xml.Name{Space: space, Local: local} }

// parseMS asserts a 207 with an XML multistatus body and decodes it.
func parseMS(t *testing.T, rec *httptest.ResponseRecorder) multistatus {
	t.Helper()
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status %d, want 207; body:\n%s", rec.Code, rec.Body)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/xml") && !strings.HasPrefix(ct, "text/xml") || !strings.Contains(strings.ToLower(ct), "charset=utf-8") {
		t.Errorf("Content-Type %q: want XML with charset=utf-8 (E08 SHOULD)", ct)
	}
	var root el
	if err := xml.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatalf("multistatus does not parse: %v\n%s", err, rec.Body)
	}
	if root.XMLName != n(nsD, "multistatus") {
		t.Fatalf("root is %v", root.XMLName)
	}
	ms := multistatus{byPath: map[string]response{}, raw: rec.Body.String()}
	for _, k := range root.Kids {
		switch k.XMLName {
		case n(nsD, "sync-token"):
			tok := k.Text
			ms.token = &tok
		case n(nsD, "response"):
			r := response{props: map[xml.Name]el{}, missing: map[xml.Name]bool{}}
			hrefs := 0
			for _, p := range k.Kids {
				switch p.XMLName {
				case n(nsD, "href"):
					r.href = p.Text
					hrefs++
				case n(nsD, "status"):
					r.status = p.Text
				case n(nsD, "propstat"):
					r.propstats++
					st, _ := p.kid(n(nsD, "status"))
					prop, ok := p.kid(n(nsD, "prop"))
					if !ok {
						t.Fatalf("propstat without prop in %s", rec.Body)
					}
					for _, v := range prop.Kids {
						switch st.Text {
						case "HTTP/1.1 200 OK":
							r.props[v.XMLName] = v
						case "HTTP/1.1 404 Not Found":
							r.missing[v.XMLName] = true
						default:
							t.Errorf("propstat status %q", st.Text)
						}
					}
				}
			}
			if hrefs != 1 {
				t.Fatalf("a response has %d hrefs:\n%s", hrefs, rec.Body)
			}
			u, err := url.Parse(r.href)
			if err != nil {
				t.Fatalf("href %q: %v", r.href, err)
			}
			if _, dup := ms.byPath[u.Path]; dup {
				t.Errorf("href %s listed twice", u.Path)
			}
			ms.byPath[u.Path] = r
			ms.responses = append(ms.responses, r)
		default:
			t.Errorf("unexpected %v in multistatus", k.XMLName)
		}
	}
	return ms
}

func (ms multistatus) get(t *testing.T, path string) response {
	t.Helper()
	r, ok := ms.byPath[path]
	if !ok {
		t.Fatalf("no response for %s in:\n%s", path, ms.raw)
	}
	return r
}

func (r response) prop(t *testing.T, space, local string) el {
	t.Helper()
	v, ok := r.props[n(space, local)]
	if !ok {
		t.Fatalf("%s: no 200 property %s %s (missing: %v)", r.href, space, local, r.missing)
	}
	return v
}

func wantNames(t *testing.T, what string, got []xml.Name, want ...xml.Name) {
	t.Helper()
	sortNames := func(s []xml.Name) []xml.Name {
		s = slices.Clone(s)
		slices.SortFunc(s, func(a, b xml.Name) int { return strings.Compare(a.Space+" "+a.Local, b.Space+" "+b.Local) })
		return s
	}
	if !slices.Equal(sortNames(got), sortNames(want)) {
		t.Errorf("%s = %v, want exactly %v", what, got, want)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status %d, want %d; body:\n%s", rec.Code, code, rec.Body)
	}
}

func wantBody(t *testing.T, rec *httptest.ResponseRecorder, body string) {
	t.Helper()
	if got := rec.Body.String(); got != body {
		t.Errorf("body %q, want %q", got, body)
	}
}

// privileges lists the privilege names of a current-user-privilege-set.
func privileges(t *testing.T, set el) []xml.Name {
	t.Helper()
	var out []xml.Name
	for _, p := range set.Kids {
		if p.XMLName != n(nsD, "privilege") {
			t.Fatalf("privilege set holds %v", p.XMLName)
		}
		out = append(out, p.kidNames()...)
	}
	return out
}

// --- MUST 1 ---------------------------------------------------------------

// Every method on every path needs credentials and asks for them with a
// Basic challenge, never a 403, before any other work.
func TestMust01UnauthenticatedRequestsGet401WithTheBasicChallenge(t *testing.T) {
	t.Parallel()
	targets := []struct{ method, path, body string }{
		{"PROPFIND", "/", tbDetect},
		{"PROPFIND", "/.well-known/caldav", tbDetect},
		{"GET", "/.well-known/caldav", ""},
		{"PROPFIND", principal, tbDetect},
		{"PROPFIND", home, tbHomeSet},
		{"PROPFIND", calPath(workID), tbServerInfo},
		{"REPORT", calPath(workID), tbSync("")},
		{"GET", objPath(workID, plainUID), ""},
		{"HEAD", objPath(workID, plainUID), ""},
		{"PUT", objPath(workID, plainUID), "BEGIN:VCALENDAR"},
		{"DELETE", objPath(workID, plainUID), ""},
		{"PROPPATCH", calPath(workID), ""},
		{"MKCALENDAR", home + "new/", ""},
		{"PROPFIND", "/nowhere", ""},
	}
	for _, tc := range targets {
		for _, mode := range []string{"anonymous", "wrong password", "empty password"} {
			s := newStub()
			h := caldav.Handler(s, caldav.Options{})
			r := call(tc.method, tc.path, tc.body, "Depth", "0")
			switch mode {
			case "wrong password":
				r.SetBasicAuth("someone@example.org", "not-"+password)
			case "empty password":
				r.SetBasicAuth("someone@example.org", "")
			}
			rec := serve(h, r)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s (%s): status %d, want 401", tc.method, tc.path, mode, rec.Code)
				continue
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != challenge {
				t.Errorf("%s %s (%s): WWW-Authenticate %q, want %q", tc.method, tc.path, mode, got, challenge)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("%s %s (%s): body %q, want none", tc.method, tc.path, mode, rec.Body)
			}
			checks, reads := s.counts()
			if reads != 0 {
				t.Errorf("%s %s (%s): %d service reads before authentication", tc.method, tc.path, mode, reads)
			}
			// A request without credentials is Thunderbird's normal first
			// try (necko answers the challenge): it must not count as a
			// failed attempt against the client's IP.
			if wantChecks := map[bool]int{true: 0, false: 1}[mode == "anonymous"]; checks != wantChecks {
				t.Errorf("%s %s (%s): %d password checks, want %d", tc.method, tc.path, mode, checks, wantChecks)
			}
		}
	}
}

// A blocked IP gets 503 (not 429: a 4xx on the first PROPFIND disables the
// calendar, ruling M3-R26) with Retry-After in whole seconds, rounded up, at
// least 1 (ruling M3-R24), and no challenge.
func TestMust01BlockedClientsGet503WithWholeSecondsRetryAfter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wait time.Duration
		want string
	}{{time.Nanosecond, "1"}, {time.Second, "1"}, {1500 * time.Millisecond, "2"}, {59*time.Second + time.Millisecond, "60"}} {
		s := newStub()
		s.wait = tc.wait
		r := call("PROPFIND", "/", tbDetect, "Depth", "0")
		r.SetBasicAuth("x", "wrong")
		rec := serve(caldav.Handler(s, caldav.Options{}), r)
		wantStatus(t, rec, http.StatusServiceUnavailable)
		if got := rec.Header().Get("Retry-After"); got != tc.want {
			t.Errorf("retryAfter %v: Retry-After %q, want %q", tc.wait, got, tc.want)
		}
		if rec.Body.Len() != 0 || rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("blocked: body %q, WWW-Authenticate %q", rec.Body, rec.Header().Get("WWW-Authenticate"))
		}
	}
}

// --- MUST 2 ---------------------------------------------------------------

func TestMust02WellKnownRedirectsToThePrincipal(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	for _, tc := range []struct{ method, path string }{
		{"PROPFIND", "/.well-known/caldav"}, {"PROPFIND", "/.well-known/caldav/"}, {"GET", "/.well-known/caldav"},
	} {
		rec := do(h, tc.method, tc.path, tbDetect, "Depth", "0")
		wantStatus(t, rec, http.StatusPermanentRedirect)
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || loc.Host != "" && loc.Host != "example.com" || loc.Path != principal {
			t.Fatalf("%s %s: Location %q, want the principal on the same origin", tc.method, tc.path, rec.Header().Get("Location"))
		}
	}
	// Following it (Thunderbird re-sends the PROPFIND) finds the home set.
	ms := parseMS(t, do(h, "PROPFIND", principal, tbDetect, "Depth", "0"))
	p := ms.get(t, principal)
	if got := p.prop(t, nsC, "calendar-home-set").hrefOf(t); got != home {
		t.Errorf("calendar-home-set %q, want %q", got, home)
	}
}

// --- MUST 3 ---------------------------------------------------------------

// The discovery chain from the bare URL: / names the principal, the
// principal names the home set; resourcetype is always there; property
// names match by namespace, whatever the prefix.
func TestMust03DiscoveryFromTheRootFindsPrincipalAndHomeSet(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	otherPrefixes := `<propfind xmlns="DAV:" xmlns:cal="urn:ietf:params:xml:ns:caldav"><prop><resourcetype/><current-user-principal/><cal:calendar-home-set/></prop></propfind>`
	for _, body := range []string{tbDetect, otherPrefixes} {
		root := parseMS(t, do(h, "PROPFIND", "/", body, "Depth", "0", "Content-Type", "text/xml; charset=utf-8"))
		if len(root.responses) != 1 {
			t.Fatalf("depth 0 on / gave %d responses", len(root.responses))
		}
		r := root.responses[0]
		wantNames(t, "resourcetype of /", r.prop(t, nsD, "resourcetype").kidNames(), n(nsD, "collection"))
		if got := r.prop(t, nsD, "current-user-principal").hrefOf(t); got != principal {
			t.Fatalf("current-user-principal %q, want %q", got, principal)
		}
		pr := parseMS(t, do(h, "PROPFIND", principal, body, "Depth", "0")).get(t, principal)
		wantNames(t, "resourcetype of the principal", pr.prop(t, nsD, "resourcetype").kidNames(), n(nsD, "collection"), n(nsD, "principal"))
		if got := pr.prop(t, nsC, "calendar-home-set").hrefOf(t); got != home {
			t.Fatalf("calendar-home-set %q, want %q", got, home)
		}
	}
	// What a resource lacks comes back in a 404 propstat, not dropped.
	r := parseMS(t, do(h, "PROPFIND", "/", tbDetect, "Depth", "0")).responses[0]
	for _, p := range []xml.Name{n(nsA, "calendar-color"), n(nsC, "calendar-home-set")} {
		if !r.missing[p] {
			t.Errorf("/ does not report %v as 404 (missing %v)", p, r.missing)
		}
	}
	// The calendar URL entered directly is recognised as a calendar
	// (Thunderbird's attemptLocation), with or without its trailing slash.
	for _, p := range []string{calPath(workID), strings.TrimSuffix(calPath(workID), "/")} {
		c := parseMS(t, do(h, "PROPFIND", p, tbDetect, "Depth", "0")).get(t, calPath(workID))
		wantNames(t, "resourcetype of the calendar", c.prop(t, nsD, "resourcetype").kidNames(), n(nsD, "collection"), n(nsC, "calendar"))
	}
}

// --- MUST 4 ---------------------------------------------------------------

func TestMust04HomeSetDepth1ListsEveryCalendar(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	ms := parseMS(t, do(h, "PROPFIND", home, tbHomeSet, "Depth", "1"))
	if len(ms.responses) != 3 {
		t.Fatalf("%d responses, want the home set and its 2 calendars:\n%s", len(ms.responses), ms.raw)
	}
	for _, r := range ms.responses {
		if _, ok := r.props[n(nsD, "resourcetype")]; !ok {
			t.Errorf("%s has no resourcetype (Thunderbird throws)", r.href)
		}
	}
	wantNames(t, "resourcetype of the home set", ms.get(t, home).prop(t, nsD, "resourcetype").kidNames(), n(nsD, "collection"))
	for id, want := range map[string]struct{ name, color string }{workID: {"Work", "#3CBB3A"}, holID: {"Holidays", ""}} {
		c := ms.get(t, calPath(id))
		wantNames(t, "resourcetype of "+want.name, c.prop(t, nsD, "resourcetype").kidNames(), n(nsD, "collection"), n(nsC, "calendar"))
		if got := c.prop(t, nsD, "displayname").Text; got != want.name {
			t.Errorf("displayname %q, want %q", got, want.name)
		}
		if want.color != "" {
			if got := c.prop(t, nsA, "calendar-color").Text; got != want.color {
				t.Errorf("calendar-color %q, want %q", got, want.color)
			}
		} else if !c.missing[n(nsA, "calendar-color")] {
			t.Errorf("%s: an absent colour must be a 404 property", want.name)
		}
		c.prop(t, nsD, "current-user-privilege-set")
	}
}

// --- MUST 5 ---------------------------------------------------------------

func TestMust05CalendarDepth0CarriesTheServerInfo(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	c := parseMS(t, do(h, "PROPFIND", calPath(workID), tbServerInfo, "Depth", "0")).get(t, calPath(workID))
	wantNames(t, "resourcetype", c.prop(t, nsD, "resourcetype").kidNames(), n(nsD, "collection"), n(nsC, "calendar"))
	var reports []xml.Name
	for _, sr := range c.prop(t, nsD, "supported-report-set").Kids {
		rep, ok := sr.kid(n(nsD, "report"))
		if sr.XMLName != n(nsD, "supported-report") || !ok {
			t.Fatalf("supported-report-set holds %v", sr.XMLName)
		}
		reports = append(reports, rep.kidNames()...)
	}
	wantNames(t, "supported reports", reports, n(nsD, "sync-collection"), n(nsC, "calendar-multiget"))
	comps := c.prop(t, nsC, "supported-calendar-component-set")
	if len(comps.Kids) != 1 || comps.Kids[0].XMLName != n(nsC, "comp") || len(comps.Kids[0].Attrs) != 1 ||
		comps.Kids[0].Attrs[0].Name.Local != "name" || comps.Kids[0].Attrs[0].Value != "VEVENT" {
		t.Errorf("supported-calendar-component-set = %+v, want one comp name=VEVENT", comps.Kids)
	}
	c.prop(t, nsD, "current-user-privilege-set")
	if got := c.prop(t, nsCS, "getctag").Text; got != "e1:7:hash1" {
		t.Errorf("getctag %q", got)
	}
	for _, p := range []string{"owner", "current-user-principal"} {
		if got := c.prop(t, nsD, p).hrefOf(t); got != principal {
			t.Errorf("%s %q, want %q", p, got, principal)
		}
	}
	// The other properties of the calendar, asked for by name.
	more := `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:sync-token/><D:displayname/><C:calendar-description/></D:prop></D:propfind>`
	c = parseMS(t, do(h, "PROPFIND", calPath(workID), more, "Depth", "0")).get(t, calPath(workID))
	if c.prop(t, nsD, "sync-token").Text != "urn:pcb:sync:1:e1:7:gen1" || c.prop(t, nsD, "displayname").Text != "Work" ||
		c.prop(t, nsC, "calendar-description").Text != "Team" {
		t.Errorf("calendar properties = %+v", c.props)
	}
}

// --- SHOULD: privileges (rulings M3-R1, M3-R15) ---------------------------

func TestShouldPrivilegesFollowReadOnly(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	ms := parseMS(t, do(h, "PROPFIND", home, tbHomeSet, "Depth", "1"))
	wantNames(t, "privileges of a writable calendar", privileges(t, ms.get(t, calPath(workID)).prop(t, nsD, "current-user-privilege-set")),
		n(nsD, "read"), n(nsD, "write"), n(nsD, "write-content"), n(nsD, "bind"), n(nsD, "unbind"))
	wantNames(t, "privileges of a read-only calendar", privileges(t, ms.get(t, calPath(holID)).prop(t, nsD, "current-user-privilege-set")),
		n(nsD, "read"))
}

// --- MUST 6 / AVOID scheduling --------------------------------------------

// OPTIONS answers 200 with exactly the DAV classes, on any path, without
// credentials and without asking the service (ruling M3-R23); never a
// scheduling class (AVOID 1).
func TestMust06OptionsAnswers200WithTheDAVHeaderWithoutAuth(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	for _, p := range []string{"/", "/.well-known/caldav", principal, home, calPath(workID), objPath(workID, plainUID), "/nowhere/at/all"} {
		rec := serve(h, call("OPTIONS", p, ""))
		wantStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("DAV"); got != davHeader {
			t.Errorf("OPTIONS %s: DAV %q, want %q", p, got, davHeader)
		}
		if strings.Contains(rec.Header().Get("DAV"), "schedule") {
			t.Errorf("OPTIONS %s advertises scheduling", p)
		}
	}
	if checks, reads := s.counts(); checks != 0 || reads != 0 {
		t.Errorf("OPTIONS made %d password checks and %d service reads", checks, reads)
	}
}

// --- MUST 7 ---------------------------------------------------------------

func TestMust07SyncCollectionWithAnEmptyTokenListsEverything(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	ms := parseMS(t, do(h, "REPORT", calPath(workID), tbSync(""), "Depth", "1", "Content-Type", "text/xml; charset=utf-8"))
	if len(ms.responses) != len(s.objs[workID]) {
		t.Fatalf("%d responses, want every object (%d):\n%s", len(ms.responses), len(s.objs[workID]), ms.raw)
	}
	for _, o := range s.objs[workID] {
		r := ms.get(t, objPath(workID, o.UID))
		if got := r.prop(t, nsD, "getetag").Text; got != o.ETag {
			t.Errorf("%s: getetag %q, want %q", o.UID, got, o.ETag)
		}
		if got := r.prop(t, nsD, "getcontenttype").Text; !strings.HasPrefix(got, "text/calendar") {
			t.Errorf("%s: getcontenttype %q", o.UID, got)
		}
		if r.status != "" {
			t.Errorf("%s: response-level status %q on a listed object", o.UID, r.status)
		}
	}
	if ms.token == nil || *ms.token != "urn:pcb:sync:1:e1:7:gen1" {
		t.Errorf("sync-token = %v, want the calendar's", ms.token)
	}
	// A listing never decrypts: only the service's ETags are read.
	if len(s.uidReqs) != 0 {
		t.Errorf("the listing fetched objects %v", s.uidReqs)
	}
}

// A delta passes through as the service computed it (ruling M3-R11): changed
// objects with their ETags, deletions as a response-level 404 with no
// propstat, and a new token even when nothing changed.
func TestMust07SyncCollectionServesTheDeltaAndDeletionsAs404(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.deltas["tok-1"] = service.ChangeSet{
		Changed: []service.ObjectInfo{{UID: plainUID, ETag: `"v1-plain"`}},
		Deleted: dels("gone@x", "gone too/ü"),
		Token:   "tok-2",
	}
	s.deltas["tok-2"] = service.ChangeSet{Changed: []service.ObjectInfo{}, Deleted: dels(), Token: "tok-2"}
	h := caldav.Handler(s, caldav.Options{})
	ms := parseMS(t, do(h, "REPORT", calPath(workID), tbSync("tok-1"), "Depth", "1"))
	if len(ms.responses) != 3 {
		t.Fatalf("%d responses:\n%s", len(ms.responses), ms.raw)
	}
	if got := ms.get(t, objPath(workID, plainUID)).prop(t, nsD, "getetag").Text; got != `"v1-plain"` {
		t.Errorf("changed getetag %q", got)
	}
	for _, uid := range []string{"gone@x", "gone too/ü"} {
		r := ms.get(t, objPath(workID, uid))
		if r.status != "HTTP/1.1 404 Not Found" || r.propstats != 0 {
			t.Errorf("deleted %s: status %q, %d propstats; want a response-level 404 alone", uid, r.status, r.propstats)
		}
	}
	if ms.token == nil || *ms.token != "tok-2" {
		t.Errorf("sync-token %v, want tok-2", ms.token)
	}
	quiet := parseMS(t, do(h, "REPORT", calPath(workID), tbSync("tok-2"), "Depth", "1"))
	if len(quiet.responses) != 0 || quiet.token == nil || *quiet.token != "tok-2" {
		t.Errorf("nothing changed: %d responses, token %v; want none and a token", len(quiet.responses), quiet.token)
	}
}

// A token the service cannot answer from is a 403 with the valid-sync-token
// precondition, a non-empty body that is not the rate-limit one (E08 §3.3).
func TestMust07InvalidSyncTokenIs403ValidSyncToken(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	for _, tok := range []string{"urn:pcb:sync:1:other-epoch:7:gen1", "garbage"} {
		rec := do(h, "REPORT", calPath(workID), tbSync(tok), "Depth", "1")
		wantStatus(t, rec, http.StatusForbidden)
		wantBody(t, rec, validSyncTokenBody)
		if rec.Body.String() == rateLimitBody {
			t.Error("the rate-limit body")
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
			t.Errorf("Content-Type %q", ct)
		}
	}
}

// --- MUST 8 ---------------------------------------------------------------

func TestMust08MultigetAnswersEveryHrefAndMissingOnesWith404(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	hrefs := []string{
		tbHref(workID, plainUID),                        // %40 for @
		tbHref(workID, oddUID),                          // ~base64url name
		"http://example.com" + objPath(workID, lastUID), // absolute form
		tbHref(workID, "nobody@x"),                      // no such object
		objPath(holID, "newyear@x"),                     // another calendar's
		calPath(workID) + "~!!!.ics",                    // a name that does not decode
		calPath(workID) + "~" + base64.RawURLEncoding.EncodeToString([]byte(plainUID)) + ".ics", // not the canonical name
	}
	ms := parseMS(t, do(h, "REPORT", calPath(workID), tbMultiget(hrefs...), "Depth", "1"))
	if len(ms.responses) != len(hrefs) {
		t.Fatalf("%d responses for %d hrefs:\n%s", len(ms.responses), len(hrefs), ms.raw)
	}
	for _, o := range s.objs[workID] {
		r := ms.get(t, objPath(workID, o.UID))
		if r.status != "" {
			t.Errorf("%s: response-level status %q", o.UID, r.status)
		}
		if got := r.prop(t, nsD, "getetag").Text; got != o.ETag {
			t.Errorf("%s: getetag %q, want %q", o.UID, got, o.ETag)
		}
		if got := r.prop(t, nsC, "calendar-data").Text; got != string(o.Data) {
			t.Errorf("%s: calendar-data\n%q\nwant\n%q", o.UID, got, o.Data)
		}
	}
	for _, p := range []string{objPath(workID, "nobody@x"), objPath(holID, "newyear@x"), calPath(workID) + "~!!!.ics", hrefs[6]} {
		r := ms.get(t, p)
		if r.status != "HTTP/1.1 404 Not Found" || r.propstats != 0 {
			t.Errorf("%s: status %q with %d propstats; want a response-level 404 alone", p, r.status, r.propstats)
		}
	}
	// One fetch for the batch, of the six names in this calendar only (the
	// service says which of them serve an object).
	if len(s.nameReqs) != 1 || len(s.nameReqs[0]) != 6 || !slices.Contains(s.nameReqs[0], "nobody@x.ics") ||
		slices.Contains(s.nameReqs[0], "newyear@x.ics") {
		t.Errorf("ObjectsByName calls %q", s.nameReqs)
	}
}

// An aliased object deleted between the change list and the render of its
// calendar-data is a deletion at its alias, the only href a client knows.
func TestSyncWithCalendarDataReportsAVanishedAliasAtItsAlias(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.aliases[workID] = map[string]string{"vanished@x": "my event.ics"}
	s.deltas["tok-1"] = service.ChangeSet{Changed: []service.ObjectInfo{{UID: "vanished@x", ETag: `"v1-vanished"`}}, Deleted: dels(), Token: "tok-2"}
	h := caldav.Handler(s, caldav.Options{})
	body := `<sync-collection xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><sync-token>tok-1</sync-token><sync-level>1</sync-level><prop><getetag/><C:calendar-data/></prop></sync-collection>`
	ms := parseMS(t, do(h, "REPORT", calPath(workID), body, "Depth", "1"))
	if len(ms.responses) != 1 {
		t.Fatalf("%d responses:\n%s", len(ms.responses), ms.raw)
	}
	if r := ms.get(t, calPath(workID)+"my event.ics"); r.status != "HTTP/1.1 404 Not Found" || r.propstats != 0 {
		t.Errorf("the vanished alias: %+v", r)
	}
}

// An object the service answers under another name than the href asked is
// never served at that href: the href is a 404.
func TestMultigetServesAnObjectOnlyAtItsOwnName(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.misnamed = true
	h := caldav.Handler(s, caldav.Options{})
	ms := parseMS(t, do(h, "REPORT", calPath(workID), tbMultiget(tbHref(workID, plainUID)), "Depth", "1"))
	if r := ms.get(t, objPath(workID, plainUID)); r.status != "HTTP/1.1 404 Not Found" || r.propstats != 0 {
		t.Errorf("a misnamed object: status %q, %d propstats; want a response-level 404", r.status, r.propstats)
	}
}

// At most 1000 hrefs per multiget; more is 413 (Thunderbird batches 100).
func TestMust08MultigetRefusesMoreThan1000Hrefs(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	hrefs := make([]string, 1001)
	for i := range hrefs {
		hrefs[i] = objPath(workID, fmt.Sprintf("u%d@x", i))
	}
	wantStatus(t, do(h, "REPORT", calPath(workID), tbMultiget(hrefs...), "Depth", "1"), http.StatusRequestEntityTooLarge)
	if _, reads := s.counts(); reads != 0 {
		t.Errorf("a refused multiget made %d reads", reads)
	}
	if ms := parseMS(t, do(h, "REPORT", calPath(workID), tbMultiget(hrefs[:1000]...), "Depth", "1")); len(ms.responses) != 1000 {
		t.Errorf("1000 hrefs: %d responses", len(ms.responses))
	}
}

// --- MUST 9 / AVOID nested collections ------------------------------------

func TestMust09ObjectsAreICSResourcesDirectlyUnderTheirCalendar(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	ms := parseMS(t, do(h, "PROPFIND", calPath(workID), tbEtags, "Depth", "1"))
	if len(ms.responses) != 1+len(s.objs[workID]) {
		t.Fatalf("%d responses:\n%s", len(ms.responses), ms.raw)
	}
	for _, o := range s.objs[workID] {
		p := objPath(workID, o.UID)
		r := ms.get(t, p)
		if rest := strings.TrimPrefix(p, calPath(workID)); strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".ics") {
			t.Errorf("%s is not an .ics directly under the calendar", p)
		}
		if kids := r.prop(t, nsD, "resourcetype").Kids; len(kids) != 0 {
			t.Errorf("%s: resourcetype %v, want empty", p, kids)
		}
		// GET serves the one VCALENDAR of the UID.
		rec := do(h, "GET", p, "")
		wantStatus(t, rec, http.StatusOK)
		if !bytes.Equal(rec.Body.Bytes(), o.Data) {
			t.Errorf("GET %s:\n%q\nwant\n%q", p, rec.Body, o.Data)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
			t.Errorf("GET %s: Content-Type %q", p, ct)
		}
	}
}

// Names parse back exactly (ruling M3-R13): only the canonical name of a
// UID reaches it, anything else is 404.
func TestHrefNamesDecodeExactly(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.objs[workID] = append(s.objs[workID], service.Object{UID: "ABC", ETag: `"v1-abc"`, Data: ics("ABC", "abc")},
		service.Object{UID: strings.Repeat("a", 200), ETag: `"v1-200"`, Data: ics("a200", "long")},
		service.Object{UID: strings.Repeat("b", 201), ETag: `"v1-201"`, Data: ics("b201", "longer")})
	h := caldav.Handler(s, caldav.Options{})
	for _, uid := range []string{plainUID, oddUID, "ABC", strings.Repeat("a", 200), strings.Repeat("b", 201)} {
		if rec := do(h, "GET", objPath(workID, uid), ""); rec.Code != http.StatusOK {
			t.Errorf("GET of %q's canonical name: %d", uid, rec.Code)
		}
	}
	if !strings.HasPrefix(objName(strings.Repeat("b", 201)), "~") {
		t.Fatal("test bug: a 201-byte UID must be encoded")
	}
	cal := calPath(workID)
	for _, p := range []string{
		cal + "~QUJD.ics",                       // "ABC" encoded: not its canonical name
		cal + "~QUJD" + "=.ics",                 // padded
		cal + "ABC",                             // no .ics
		cal + "ABC.ICS",                         // another suffix
		cal + "~.ics",                           // empty UID
		cal + ".ics",                            // empty UID
		cal + strings.Repeat("b", 201) + ".ics", // plain but too long
		cal + "odd%20uid.ics",                   // not plain, not encoded
		cal + "~" + base64.URLEncoding.EncodeToString([]byte(oddUID)) + ".ics",     // padded form
		cal + "~" + base64.StdEncoding.EncodeToString([]byte(oddUID+"?")) + ".ics", // standard alphabet
		cal + "ABC.ics/",                  // a collection below an object
		cal + "x/ABC.ics",                 // nested
		home + "0123456789abcdef/ABC.ics", // no such calendar
	} {
		if rec := do(h, "GET", p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, rec.Code)
		}
	}
}

// --- MUST 10/11: writes ---------------------------------------------------

// svcErr is err as the service returns it: in an *exitcode.Error.
func svcErr(code exitcode.Code, err error) error {
	return &exitcode.Error{Code: code, Err: err, Hint: "x"}
}

// MUST 10: a PUT with Thunderbird's headers reaches the service with the
// object's name, the body byte for byte and the precondition as sent; a
// create is 201, a replace 204, neither with an ETag or a body (E08 §4.1).
func TestMust10PutIs201Or204WithoutAnETag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		created bool
		hdr     []string
		cond    service.Precondition
		code    int
	}{
		{"create", true, []string{"If-None-Match", "*"}, service.Precondition{IfNoneMatchAny: true}, http.StatusCreated},
		{"update", false, []string{"If-Match", `"v1-plain"`}, service.Precondition{IfMatch: `"v1-plain"`}, http.StatusNoContent},
		{"forced", false, nil, service.Precondition{}, http.StatusNoContent},
		{"If-Match *", false, []string{"If-Match", "*"}, service.Precondition{IfMatch: "*"}, http.StatusNoContent},
	} {
		s := newStub()
		s.putRes = service.PutResult{Created: tc.created}
		h := caldav.Handler(s, caldav.Options{})
		body := string(richData)
		rec := do(h, http.MethodPut, calPath(workID)+"3f2a1c9e-new%20one.ics", body,
			append([]string{"Content-Type", "text/calendar; charset=utf-8", "Accept", "text/xml"}, tc.hdr...)...)
		wantStatus(t, rec, tc.code)
		if rec.Header().Get("ETag") != "" || rec.Body.Len() != 0 {
			t.Errorf("%s: ETag %q, body %q; want neither", tc.name, rec.Header().Get("ETag"), rec.Body)
		}
		w := s.writeCalls()
		if len(w) != 1 || w[0].method != http.MethodPut || w[0].calID != workID || w[0].name != "3f2a1c9e-new one.ics" ||
			string(w[0].body) != body || !reflect.DeepEqual(w[0].cond, tc.cond) {
			t.Errorf("%s: the service got %+v", tc.name, w)
		}
	}
}

// An If-None-Match naming ETags reaches the service as the list of them
// (RFC 9110 §13.1.2: the service compares them with the object's, and a
// missing object passes), from one header or several.
func TestIfNoneMatchETagsReachTheService(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	req := authed(call(http.MethodPut, objPath(workID, "new@x"), "x", "Content-Type", "text/calendar"))
	req.Header.Add("If-None-Match", `"v1-a", W/"v1-b"`)
	req.Header.Add("If-None-Match", `"v1-c"`)
	wantStatus(t, serve(h, req), http.StatusNoContent)
	wantStatus(t, do(h, http.MethodDelete, objPath(workID, plainUID), "", "If-None-Match", ` "v1-d" `), http.StatusNoContent)
	w := s.writeCalls()
	if len(w) != 2 || !slices.Equal(w[0].cond.IfNoneMatch, []string{`"v1-a"`, `W/"v1-b"`, `"v1-c"`}) || w[0].cond.IfNoneMatchAny ||
		!slices.Equal(w[1].cond.IfNoneMatch, []string{`"v1-d"`}) {
		t.Errorf("the service got %+v", w)
	}
}

// MUST 10: a failed precondition is a 412 with no body; a UID living
// elsewhere is a 409 whose no-uid-conflict names that object's href (E08
// §4.2, RFC 4791 §5.3.2.1), percent-encoded.
func TestMust10PreconditionsAre412AndUIDConflictsAre409(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	put := func() *httptest.ResponseRecorder {
		return do(h, http.MethodPut, objPath(workID, plainUID), "x", "Content-Type", "text/calendar", "If-Match", `"v1-old"`)
	}
	s.writeErr = svcErr(exitcode.Usage, service.ErrPreconditionFailed)
	rec := put()
	wantStatus(t, rec, http.StatusPreconditionFailed)
	wantBody(t, rec, "")

	s.writeErr = svcErr(exitcode.Usage, &service.UIDConflictError{Href: "other one.ics"})
	rec = put()
	wantStatus(t, rec, http.StatusConflict)
	wantBody(t, rec, `<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><C:no-uid-conflict><D:href>`+
		calPath(workID)+`other%20one.ics</D:href></C:no-uid-conflict></D:error>`)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("409 Content-Type %q", ct)
	}
}

// MUST 11: DELETE reaches the service with the name and If-Match, and is a
// 204; a missing object is 404 (Thunderbird: success), a mismatch 412.
// HEAD answers 200 with the ETag or 404, which ends Thunderbird's delete
// flow after a 412 (E08 §4.3).
func TestMust11DeleteIs204Or404Or412AndHeadAnswers(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	rec := do(h, http.MethodDelete, objPath(workID, plainUID), "", "If-Match", `"v1-plain"`)
	wantStatus(t, rec, http.StatusNoContent)
	wantBody(t, rec, "")
	if w := s.writeCalls(); len(w) != 1 || w[0].method != http.MethodDelete || w[0].name != objName(plainUID) ||
		!reflect.DeepEqual(w[0].cond, service.Precondition{IfMatch: `"v1-plain"`}) {
		t.Errorf("the service got %+v", w)
	}
	for _, tc := range []struct {
		err  error
		code int
	}{
		{svcErr(exitcode.Usage, service.ErrNoSuchObject), http.StatusNotFound},
		{svcErr(exitcode.Usage, service.ErrPreconditionFailed), http.StatusPreconditionFailed},
	} {
		s.writeErr = tc.err
		rec := do(h, http.MethodDelete, objPath(workID, plainUID), "", "If-Match", `"v1-plain"`)
		wantStatus(t, rec, tc.code)
		wantBody(t, rec, "")
	}

	o := s.objs[workID][0]
	rec = do(h, "HEAD", objPath(workID, o.UID), "")
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("ETag") != o.ETag || rec.Header().Get("Content-Length") != fmt.Sprint(len(o.Data)) || rec.Body.Len() != 0 {
		t.Errorf("HEAD: ETag %q, Content-Length %q, %d body bytes", rec.Header().Get("ETag"), rec.Header().Get("Content-Length"), rec.Body.Len())
	}
	wantStatus(t, do(h, "HEAD", objPath(workID, "nobody@x"), ""), http.StatusNotFound)
	wantStatus(t, do(h, "GET", objPath(workID, "nobody@x"), ""), http.StatusNotFound)
}

// A write to a read-only calendar is 403 need-privileges naming the
// privilege it lacks, without asking the service to write; so is the
// service's own ErrNotWritable (the gate changed since the listing).
func TestWritesToReadOnlyCalendarsAre403NeedPrivileges(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	const putBody = `<D:error xmlns:D="DAV:"><D:need-privileges><D:resource><D:href>%s</D:href><D:privilege><D:write-content/></D:privilege></D:resource></D:need-privileges></D:error>`
	const delBody = `<D:error xmlns:D="DAV:"><D:need-privileges><D:resource><D:href>%s</D:href><D:privilege><D:unbind/></D:privilege></D:resource></D:need-privileges></D:error>`
	rec := do(h, http.MethodPut, objPath(holID, "newyear@x"), "x", "Content-Type", "text/calendar")
	wantStatus(t, rec, http.StatusForbidden)
	wantBody(t, rec, fmt.Sprintf(putBody, objPath(holID, "newyear@x")))
	rec = do(h, http.MethodDelete, objPath(holID, "newyear@x"), "")
	wantStatus(t, rec, http.StatusForbidden)
	wantBody(t, rec, fmt.Sprintf(delBody, calPath(holID)))
	if w := s.writeCalls(); len(w) != 0 {
		t.Errorf("writes to a read-only calendar reached the service: %+v", w)
	}

	s.writeErr = svcErr(exitcode.Usage, service.ErrNotWritable)
	rec = do(h, http.MethodPut, objPath(workID, plainUID), "x", "Content-Type", "text/calendar")
	wantStatus(t, rec, http.StatusForbidden)
	wantBody(t, rec, fmt.Sprintf(putBody, objPath(workID, plainUID)))
	rec = do(h, http.MethodDelete, objPath(workID, plainUID), "")
	wantStatus(t, rec, http.StatusForbidden)
	wantBody(t, rec, fmt.Sprintf(delBody, calPath(workID)))
}

// Ruling M4-R40: a calendar whose gate was never decided advertises no
// privileges: current-user-privilege-set is in a 404 propstat (E08: no
// decision; Thunderbird keeps a read-only flag for good), in a Depth 0 and
// a Depth 1 PROPFIND, and allprop leaves it out.
func TestUndecidedPrivilegesAreA404Propstat(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.colls[1].ReadOnly, s.colls[1].PrivilegesUnknown, s.colls[1].WriteGateUndecided = true, true, true
	h := caldav.Handler(s, caldav.Options{})
	hs := parseMS(t, do(h, "PROPFIND", home, tbHomeSet, "Depth", "1"))
	one := parseMS(t, do(h, "PROPFIND", calPath(holID), tbServerInfo, "Depth", "0"))
	for _, r := range []response{hs.get(t, calPath(holID)), one.get(t, calPath(holID))} {
		if _, ok := r.props[n(nsD, "current-user-privilege-set")]; ok || !r.missing[n(nsD, "current-user-privilege-set")] {
			t.Errorf("an undecided calendar: privileges %v, missing %v; want a 404 propstat", r.props[n(nsD, "current-user-privilege-set")], r.missing)
		}
	}
	wantNames(t, "a decided calendar", privileges(t, hs.get(t, calPath(workID)).prop(t, nsD, "current-user-privilege-set")),
		n(nsD, "read"), n(nsD, "write"), n(nsD, "write-content"), n(nsD, "bind"), n(nsD, "unbind"))
	all := parseMS(t, do(h, "PROPFIND", calPath(holID), "", "Depth", "0")).get(t, calPath(holID))
	if _, ok := all.props[n(nsD, "current-user-privilege-set")]; ok || len(all.missing) != 0 {
		t.Errorf("allprop of an undecided calendar: %+v", all)
	}
}

// Ruling M4-R39: a calendar advertised read only whose gate is undecided
// is not refused by the adapter: the write goes to the service, whose
// ErrGateUndecided is a 503 with Retry-After.
func TestUndecidedReadOnlyCalendarsAreNotRefusedByTheAdapter(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.colls[1].WriteGateUndecided = true // Holidays: read only, undecided
	s.writeErr = svcErr(exitcode.Blocked, service.ErrGateUndecided)
	h := caldav.Handler(s, caldav.Options{})
	for _, rec := range []*httptest.ResponseRecorder{
		do(h, http.MethodPut, objPath(holID, "newyear@x"), "x", "Content-Type", "text/calendar"),
		do(h, http.MethodDelete, objPath(holID, "newyear@x"), ""),
	} {
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
			t.Errorf("%d, Retry-After %q; want 503, 30", rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	if w := s.writeCalls(); len(w) != 2 {
		t.Errorf("the service got %d writes, want both", len(w))
	}
}

// A change the bridge refuses is a 403 whose DAV:error holds its pcb
// precondition in the urn:pcb namespace and its reason (ours: the service
// keeps Proton's text out of it), XML-escaped; a code that is not one of
// ours is pcb:refused.
func TestWriteRefusalsAre403WithTheirPCBPrecondition(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	for _, tc := range []struct {
		refusal *service.WriteRefusal
		body    string
	}{
		{&service.WriteRefusal{Code: "pcb:unsupported-recurrence", Reason: "RRULE BYDAY=2MO & <more>"},
			`<D:error xmlns:D="DAV:"><pcb:unsupported-recurrence xmlns:pcb="urn:pcb"/><pcb:reason xmlns:pcb="urn:pcb">RRULE BYDAY=2MO &amp; &lt;more&gt;</pcb:reason></D:error>`},
		{&service.WriteRefusal{Code: "pcb:proton-refused", Reason: "Proton refused the write (code 2001)"},
			`<D:error xmlns:D="DAV:"><pcb:proton-refused xmlns:pcb="urn:pcb"/><pcb:reason xmlns:pcb="urn:pcb">Proton refused the write (code 2001)</pcb:reason></D:error>`},
		{&service.WriteRefusal{Code: "pcb:<bad>", Reason: "r"},
			`<D:error xmlns:D="DAV:"><pcb:refused xmlns:pcb="urn:pcb"/><pcb:reason xmlns:pcb="urn:pcb">r</pcb:reason></D:error>`},
		{&service.WriteRefusal{Code: "other:x", Reason: "r"},
			`<D:error xmlns:D="DAV:"><pcb:refused xmlns:pcb="urn:pcb"/><pcb:reason xmlns:pcb="urn:pcb">r</pcb:reason></D:error>`},
	} {
		s.writeErr = svcErr(exitcode.Usage, tc.refusal)
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			rec := do(h, method, objPath(workID, plainUID), "x", "Content-Type", "text/calendar")
			wantStatus(t, rec, http.StatusForbidden)
			wantBody(t, rec, tc.body)
			if rec.Body.String() == rateLimitBody {
				t.Error("the rate-limit body")
			}
		}
	}
}

// The adapter's own refusals, before the service: a PUT body that is not
// text/calendar is 415, over 8 MiB 413, an If-None-Match or an If-Match
// that names no ETag 412; PUT or DELETE on a
// collection 405, on no resource 404. None reaches the service.
func TestPutRequestChecksComeFirst(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	obj := objPath(workID, plainUID)
	for _, tc := range []struct {
		name, method, target, body string
		hdr                        []string
		code                       int
	}{
		{"no Content-Type", http.MethodPut, obj, "x", nil, http.StatusUnsupportedMediaType},
		{"text/plain", http.MethodPut, obj, "x", []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType},
		{"application/xml", http.MethodPut, obj, "x", []string{"Content-Type", "application/xml"}, http.StatusUnsupportedMediaType},
		{"broken Content-Type", http.MethodPut, obj, "x", []string{"Content-Type", "text/calendar; ="}, http.StatusUnsupportedMediaType},
		{"over 8 MiB", http.MethodPut, obj, strings.Repeat("x", 8<<20+1), []string{"Content-Type", "text/calendar"}, http.StatusRequestEntityTooLarge},
		{"If-None-Match without an ETag", http.MethodPut, obj, "x", []string{"Content-Type", "text/calendar", "If-None-Match", ","}, http.StatusPreconditionFailed},
		{"DELETE If-None-Match without an ETag", http.MethodDelete, obj, "", []string{"If-None-Match", " , "}, http.StatusPreconditionFailed},
		// A blank If-Match fails closed (ruling A9): never an unconditional write.
		{"empty If-Match", http.MethodPut, obj, "x", []string{"Content-Type", "text/calendar", "If-Match", ""}, http.StatusPreconditionFailed},
		{"blank If-Match", http.MethodPut, obj, "x", []string{"Content-Type", "text/calendar", "If-Match", " \t "}, http.StatusPreconditionFailed},
		{"DELETE If-Match of commas", http.MethodDelete, obj, "", []string{"If-Match", " , ,"}, http.StatusPreconditionFailed},
		{"PUT a calendar", http.MethodPut, calPath(workID), "x", []string{"Content-Type", "text/calendar"}, http.StatusMethodNotAllowed},
		{"DELETE a calendar", http.MethodDelete, calPath(workID), "", nil, http.StatusMethodNotAllowed},
		{"PUT the home set", http.MethodPut, home, "x", []string{"Content-Type", "text/calendar"}, http.StatusMethodNotAllowed},
		{"DELETE the principal", http.MethodDelete, principal, "", nil, http.StatusMethodNotAllowed},
		{"PUT nested", http.MethodPut, calPath(workID) + "x/y.ics", "x", []string{"Content-Type", "text/calendar"}, http.StatusNotFound},
		{"DELETE elsewhere", http.MethodDelete, "/nowhere.ics", "", nil, http.StatusNotFound},
	} {
		rec := do(h, tc.method, tc.target, tc.body, tc.hdr...)
		if rec.Code != tc.code || rec.Body.Len() != 0 {
			t.Errorf("%s: %d with body %q, want %d and none", tc.name, rec.Code, rec.Body, tc.code)
		}
	}
	if _, reads := s.counts(); reads != 0 {
		t.Errorf("the refused requests made %d service reads", reads)
	}
	if w := s.writeCalls(); len(w) != 0 {
		t.Errorf("the refused requests reached the service: %+v", w)
	}
	// Exactly 8 MiB, and iCalendar spelt in capitals, pass.
	wantStatus(t, do(h, http.MethodPut, obj, strings.Repeat("x", 8<<20), "Content-Type", "TEXT/Calendar; Charset=UTF-8"), http.StatusNoContent)
	if w := s.writeCalls(); len(w) != 1 || len(w[0].body) != 8<<20 {
		t.Errorf("an 8 MiB PUT: %d writes", len(w))
	}
}

// A write the service cannot answer is a 503 with Retry-After: 30 and no
// body, never a 4xx (ruling M3-R22): not serving, the calendar pending, a
// write not confirmed, the keys dropped, a Usage error outside the
// contract, an unknown error; logged without the body, except a pending
// calendar or a client gone.
func TestWritesAre503WhenTheServiceCannotAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		warn bool
	}{
		{"not serving", svcErr(exitcode.Blocked, service.ErrNotServing), true},
		{"pending", svcErr(exitcode.Blocked, service.ErrCalendarPending), false},
		{"not confirmed", svcErr(exitcode.Blocked, service.ErrWriteNotConfirmed), true},
		{"gate undecided", svcErr(exitcode.Blocked, service.ErrGateUndecided), true},
		{"blocked", svcErr(exitcode.Blocked, errors.New(secretText)), true},
		{"must act", svcErr(exitcode.MustAct, errors.New(secretText)), true},
		{"a Usage error outside the contract", svcErr(exitcode.Usage, service.ErrInvalidSyncToken), true},
		{"a bare error", errors.New(secretText), true},
	} {
		var logs bytes.Buffer
		s := newStub()
		s.writeErr = tc.err
		h := caldav.Handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		for _, rec := range []*httptest.ResponseRecorder{
			do(h, http.MethodPut, objPath(workID, plainUID), "x", "Content-Type", "text/calendar"),
			do(h, http.MethodDelete, objPath(workID, plainUID), ""),
		} {
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" || rec.Body.Len() != 0 {
				t.Errorf("%s: %d, Retry-After %q, body %q; want 503, 30, none", tc.name, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
			}
		}
		if got := strings.Contains(logs.String(), "level=WARN"); got != tc.warn {
			t.Errorf("%s: warned %v, want %v:\n%s", tc.name, got, tc.warn, logs.String())
		}
	}
}

// --- MUST 12 --------------------------------------------------------------

func TestMust12ETagsAgreeAcrossListingsAndReads(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	synced := parseMS(t, do(h, "REPORT", calPath(workID), tbSync(""), "Depth", "1"))
	listed := parseMS(t, do(h, "PROPFIND", calPath(workID), tbEtags, "Depth", "1"))
	var hrefs []string
	for _, o := range s.objs[workID] {
		hrefs = append(hrefs, tbHref(workID, o.UID))
	}
	got := parseMS(t, do(h, "REPORT", calPath(workID), tbMultiget(hrefs...), "Depth", "1"))
	for _, o := range s.objs[workID] {
		p := objPath(workID, o.UID)
		one := parseMS(t, do(h, "PROPFIND", p, tbEtags, "Depth", "0"))
		head := do(h, "HEAD", p, "")
		etags := []string{
			synced.get(t, p).prop(t, nsD, "getetag").Text, listed.get(t, p).prop(t, nsD, "getetag").Text,
			got.get(t, p).prop(t, nsD, "getetag").Text, one.get(t, p).prop(t, nsD, "getetag").Text, head.Header().Get("ETag"),
		}
		for _, e := range etags {
			if e != o.ETag {
				t.Errorf("%s: ETags %q, want all %q", o.UID, etags, o.ETag)
				break
			}
		}
	}
}

// --- MUST 13 / AVOID normalising ------------------------------------------

func TestMust13CalendarDataIsServedByteForByte(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	ms := parseMS(t, do(h, "REPORT", calPath(workID), tbMultiget(tbHref(workID, oddUID)), "Depth", "1"))
	if got := ms.get(t, objPath(workID, oddUID)).prop(t, nsC, "calendar-data").Text; got != string(richData) {
		t.Errorf("calendar-data after XML decoding:\n%q\nwant\n%q", got, richData)
	}
	if rec := do(h, "GET", objPath(workID, oddUID), ""); !bytes.Equal(rec.Body.Bytes(), richData) {
		t.Errorf("GET:\n%q\nwant\n%q", rec.Body, richData)
	}
}

// --- MUST 14 --------------------------------------------------------------

func TestMust14AcceptsTextXMLBodiesAndThunderbirdHeaders(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	for _, ct := range []string{"text/xml; charset=utf-8", "application/xml", ""} {
		rec := do(h, "PROPFIND", calPath(workID), tbServerInfo, "Depth", "0", "Content-Type", ct,
			"Accept", "text/xml", "Accept-Charset", "utf-8,*;q=0.1", "Cache-Control", "no-cache")
		parseMS(t, rec)
	}
}

// --- SHOULD ---------------------------------------------------------------

// The PROPFIND Depth 1 fallback lists every object with getcontenttype and
// getetag; the calendar carries its CTag.
func TestShouldDepth1ListingCarriesContentTypeAndETagOnEveryObject(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	ms := parseMS(t, do(h, "PROPFIND", calPath(workID), tbEtags, "Depth", "1"))
	objects := 0
	for _, r := range ms.responses {
		if r.href == calPath(workID) {
			continue
		}
		objects++
		if ct := r.prop(t, nsD, "getcontenttype").Text; !strings.HasPrefix(ct, "text/calendar") {
			t.Errorf("%s: getcontenttype %q", r.href, ct)
		}
		if r.prop(t, nsD, "getetag").Text == "" {
			t.Errorf("%s: empty getetag", r.href)
		}
	}
	if objects != len(s.objs[workID]) {
		t.Errorf("%d objects listed, want %d", objects, len(s.objs[workID]))
	}
	if len(s.uidReqs) != 0 {
		t.Errorf("an ETag listing fetched objects %v", s.uidReqs)
	}
}

// A Depth 1 PROPFIND that asks for calendar-data gets it for every object.
func TestDepth1ListingServesCalendarDataWhenAsked(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	body := `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop></D:propfind>`
	ms := parseMS(t, do(h, "PROPFIND", calPath(workID), body, "Depth", "1"))
	for _, o := range s.objs[workID] {
		if got := ms.get(t, objPath(workID, o.UID)).prop(t, nsC, "calendar-data").Text; got != string(o.Data) {
			t.Errorf("%s: calendar-data %q", o.UID, got)
		}
	}
	// allprop leaves calendar-data out (RFC 4791 §9.6).
	all := parseMS(t, do(h, "PROPFIND", calPath(workID), `<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`, "Depth", "1"))
	r := all.get(t, objPath(workID, plainUID))
	if _, ok := r.props[n(nsC, "calendar-data")]; ok {
		t.Error("allprop returned calendar-data")
	}
	r.prop(t, nsD, "getetag")
	all.get(t, calPath(workID)).prop(t, nsD, "resourcetype")
	// An empty body is allprop.
	parseMS(t, do(h, "PROPFIND", calPath(workID), "", "Depth", "0")).get(t, calPath(workID)).prop(t, nsCS, "getctag")
}

// Ruling M3-R35 (final review, minor 2): a PROPFIND Depth 1 or a
// sync-collection asking for calendar-data renders at most the cap of
// objects (1 000; 2 here), in listing order; every other object keeps its
// getetag and gets calendar-data in a 404 propstat, so the client fetches it
// with a multiget.
func TestCalendarDataInListingsIsCapped(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.HandlerWithDataCap(s, caldav.Options{}, 2)
	objs := s.objs[workID]
	check := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		ms := parseMS(t, rec)
		for i, o := range objs {
			r := ms.get(t, objPath(workID, o.UID))
			if e := r.prop(t, nsD, "getetag").Text; e != o.ETag {
				t.Errorf("%s, %s: getetag %q, want %q", what, o.UID, e, o.ETag)
			}
			data, has := r.props[n(nsC, "calendar-data")]
			switch {
			case i < 2 && (!has || data.Text != string(o.Data)):
				t.Errorf("%s, %s (within the cap): calendar-data %v %q", what, o.UID, has, data.Text)
			case i >= 2 && (has || !r.missing[n(nsC, "calendar-data")]):
				t.Errorf("%s, %s (past the cap): calendar-data served %v, in a 404 propstat %v", what, o.UID, has, r.missing[n(nsC, "calendar-data")])
			}
		}
		s.mu.Lock()
		last := s.uidReqs[len(s.uidReqs)-1]
		s.mu.Unlock()
		if len(last) != 2 {
			t.Errorf("%s rendered %d objects, want the cap (2)", what, len(last))
		}
	}
	body := `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop></D:propfind>`
	check("PROPFIND Depth 1", do(h, "PROPFIND", calPath(workID), body, "Depth", "1"))
	sync := `<D:sync-collection xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:sync-token/><D:sync-level>1</D:sync-level>` +
		`<D:prop><D:getetag/><C:calendar-data/></D:prop></D:sync-collection>`
	check("sync-collection", do(h, "REPORT", calPath(workID), sync, "Depth", "1"))
}

// --- AVOID ----------------------------------------------------------------

// 403 only for a genuine refusal: anonymous or wrong credentials and the
// well-known URL never get one, and the rate-limit body is never sent.
func TestAvoid403OutsideGenuineRefusals(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	anon := serve(h, call("PROPFIND", "/.well-known/caldav", tbDetect, "Depth", "0"))
	wrong := call("PROPFIND", "/.well-known/caldav", tbDetect, "Depth", "0")
	wrong.SetBasicAuth("x", "nope")
	for _, rec := range []*httptest.ResponseRecorder{anon, serve(h, wrong), do(h, "PROPFIND", "/.well-known/caldav", tbDetect, "Depth", "0")} {
		if rec.Code == http.StatusForbidden {
			t.Errorf("a 403 on the well-known URL")
		}
	}
	for _, rec := range []*httptest.ResponseRecorder{
		do(h, "REPORT", calPath(workID), tbSync("bad"), "Depth", "1"),
		do(h, "PUT", objPath(holID, "newyear@x"), "x", "Content-Type", "text/calendar"),
		do(h, "DELETE", objPath(holID, "newyear@x"), ""),
		do(h, "PROPFIND", calPath(workID), tbEtags, "Depth", "infinity"),
		do(h, "REPORT", calPath(workID), `<C:calendar-query xmlns:C="urn:ietf:params:xml:ns:caldav"/>`, "Depth", "1"),
	} {
		if rec.Code != http.StatusForbidden || rec.Body.Len() == 0 || rec.Body.String() == rateLimitBody {
			t.Errorf("refusal %d with body %q: want a 403 with a precondition", rec.Code, rec.Body)
		}
	}
}

// A service that cannot answer (logged out, no cache yet, Proton or session
// failure) is a 503 with Retry-After: 30 and no text, never a 4xx, which
// would disable the calendar in Thunderbird (ruling M3-R22, E08 AVOID 3).
// Discovery that needs no service still answers.
func TestAvoid4xxWhenTheServiceIsUnavailable(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newStub()
	s.readErr = &exitcode.Error{Code: exitcode.MustAct, Err: errors.New(secretText), Hint: "run pcb login"}
	h := caldav.Handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	for _, tc := range []struct{ method, path, body, depth string }{
		{"PROPFIND", calPath(workID), tbServerInfo, "0"},
		{"PROPFIND", home, tbHomeSet, "1"},
		{"PROPFIND", objPath(workID, plainUID), tbEtags, "0"},
		{"REPORT", calPath(workID), tbSync(""), "1"},
		{"REPORT", calPath(workID), tbSync("urn:pcb:sync:1:e1:7:gen1"), "1"},
		{"REPORT", calPath(workID), tbMultiget(tbHref(workID, plainUID)), "1"},
		{"GET", objPath(workID, plainUID), "", ""},
		{"PUT", objPath(workID, plainUID), "x", ""},
		{"PUT", objPath(holID, "newyear@x"), "x", ""},
		{"DELETE", objPath(workID, plainUID), "", ""},
	} {
		rec := do(h, tc.method, tc.path, tc.body, "Depth", tc.depth, "Content-Type", "text/calendar")
		wantStatus(t, rec, http.StatusServiceUnavailable)
		if got := rec.Header().Get("Retry-After"); got != "30" {
			t.Errorf("%s %s: Retry-After %q, want 30", tc.method, tc.path, got)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s %s: body %q, want none", tc.method, tc.path, rec.Body)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("no warning logged for the 503s:\n%s", logs.String())
	}
	if w := s.writeCalls(); len(w) != 0 {
		t.Errorf("writes reached a service that cannot answer: %+v", w)
	}
	wantStatus(t, serve(h, call("OPTIONS", calPath(workID), "")), http.StatusOK)
	parseMS(t, do(h, "PROPFIND", "/", tbDetect, "Depth", "0"))
	parseMS(t, do(h, "PROPFIND", principal, tbDetect, "Depth", "0"))
}

// A calendar the service does not hold (gone since the listing) is a 404;
// so is one no listing names.
func TestUnknownCalendarsAre404(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.gone[workID] = true
	h := caldav.Handler(s, caldav.Options{})
	for _, tc := range []struct{ method, path, body, depth string }{
		{"REPORT", calPath(workID), tbSync(""), "1"},
		{"REPORT", calPath(workID), tbMultiget(tbHref(workID, plainUID)), "1"},
		{"PROPFIND", calPath(workID), tbEtags, "1"},
		{"GET", objPath(workID, plainUID), "", ""},
		{"PROPFIND", calPath("never-listed"), tbServerInfo, "0"},
		{"REPORT", calPath("never-listed"), tbSync(""), "1"},
	} {
		rec := do(h, tc.method, tc.path, tc.body, "Depth", tc.depth)
		wantStatus(t, rec, http.StatusNotFound)
		if rec.Body.Len() != 0 {
			t.Errorf("404 body %q", rec.Body)
		}
	}
}

// Ruling M3-R35 (final review, minor 4): while a calendar is pending (or
// none is synced), a segment that names no listed calendar may be one the
// pass has not recorded yet: 503 + Retry-After 30, which Thunderbird retries,
// never the 404 that disables the calendar for good (E08 §2.2). Once every
// calendar is synced, it is 404.
func TestUnknownCalendarIs503WhileACalendarIsPending(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.colls[1].Pending = true
	h := caldav.Handler(s, caldav.Options{})
	for _, tc := range []struct{ method, path, body, depth string }{
		{"PROPFIND", calPath("not-recorded-yet"), tbServerInfo, "0"},
		{"REPORT", calPath("not-recorded-yet"), tbSync(""), "1"},
		{"REPORT", calPath("not-recorded-yet"), tbMultiget(tbHref("not-recorded-yet", plainUID)), "1"},
		{"GET", objPath("not-recorded-yet", plainUID), "", ""},
	} {
		rec := do(h, tc.method, tc.path, tc.body, "Depth", tc.depth)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
			t.Errorf("%s %s with a pending calendar: %d, Retry-After %q; want 503, 30", tc.method, tc.path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	s.colls[1].Pending = false
	wantStatus(t, do(h, "PROPFIND", calPath("not-recorded-yet"), tbServerInfo, "Depth", "0"), http.StatusNotFound)
}

// A request whose context ended still gets its 503 (a live client must
// never see an empty 200), but it is not logged as a service failure.
func TestCancelledRequestsAreNotReportedAsFailures(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newStub()
	s.before = func(ctx context.Context) error {
		<-ctx.Done()
		return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("the operation was cancelled"), Hint: "x"}
	}
	h := caldav.Handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := serve(h, authed(call("REPORT", calPath(workID), tbSync(""), "Depth", "1")).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("a cancelled request got %d, Retry-After %q; want 503, 30", rec.Code, rec.Header().Get("Retry-After"))
	}
	if logs.Len() != 0 {
		t.Errorf("logged:\n%s", logs.String())
	}
}

var spaceBetweenTags = regexp.MustCompile(`>\s+<`)

// No whitespace around leaf values (href, getetag, sync-token, status...):
// Thunderbird does not trim them.
func TestAvoidWhitespaceInLeafElements(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.deltas["tok-1"] = service.ChangeSet{Changed: []service.ObjectInfo{{UID: plainUID, ETag: `"v1-plain"`}}, Deleted: dels("gone@x"), Token: "tok-2"}
	h := caldav.Handler(s, caldav.Options{})
	recs := []*httptest.ResponseRecorder{
		do(h, "PROPFIND", "/", tbDetect, "Depth", "0"),
		do(h, "PROPFIND", principal, tbDetect, "Depth", "0"),
		do(h, "PROPFIND", home, tbHomeSet, "Depth", "1"),
		do(h, "PROPFIND", calPath(workID), tbServerInfo, "Depth", "0"),
		do(h, "PROPFIND", calPath(workID), tbEtags, "Depth", "1"),
		do(h, "REPORT", calPath(workID), tbSync(""), "Depth", "1"),
		do(h, "REPORT", calPath(workID), tbSync("tok-1"), "Depth", "1"),
		do(h, "REPORT", calPath(workID), tbMultiget(tbHref(workID, plainUID), tbHref(workID, "nobody@x")), "Depth", "1"),
	}
	var walk func(e el)
	walk = func(e el) {
		if len(e.Kids) == 0 && e.XMLName != n(nsC, "calendar-data") && e.Text != strings.TrimSpace(e.Text) {
			t.Errorf("leaf %v = %q", e.XMLName, e.Text)
		}
		for _, k := range e.Kids {
			walk(k)
		}
	}
	for _, rec := range recs {
		parseMS(t, rec)
		if spaceBetweenTags.MatchString(rec.Body.String()) {
			t.Errorf("whitespace between tags:\n%s", rec.Body)
		}
		var root el
		_ = xml.Unmarshal(rec.Body.Bytes(), &root)
		walk(root)
	}
}

// Every sync-collection response is classifiable by Thunderbird: a listed
// object (200 propstat with a non-empty getetag and a text/calendar type)
// or a deletion (response-level 404, nothing else).
func TestAvoidUnclassifiableSyncResponses(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.deltas["tok-1"] = service.ChangeSet{Changed: []service.ObjectInfo{{UID: oddUID, ETag: `"v1-odd"`}}, Deleted: dels("gone@x"), Token: "tok-2"}
	h := caldav.Handler(s, caldav.Options{})
	for _, tok := range []string{"", "tok-1"} {
		for _, r := range parseMS(t, do(h, "REPORT", calPath(workID), tbSync(tok), "Depth", "1")).responses {
			deleted := r.status == "HTTP/1.1 404 Not Found" && r.propstats == 0
			listed := r.status == "" && r.props[n(nsD, "getetag")].Text != "" && strings.HasPrefix(r.props[n(nsD, "getcontenttype")].Text, "text/calendar")
			if !deleted && !listed {
				t.Errorf("token %q: unclassifiable response %+v", tok, r)
			}
		}
	}
}

// Error bodies carry no text: neither the service's nor Go's.
func TestAvoidErrorTextInBodies(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"PROPFIND", calPath(workID), "<D:propfind xmlns:D='DAV:'><D:prop>", http.StatusBadRequest},
		{"PROPFIND", calPath(workID), "<notdav/>", http.StatusBadRequest},
		{"REPORT", calPath(workID), "", http.StatusBadRequest},
		{"REPORT", calPath(workID), "<sync-collection xmlns='DAV:'><prop/></sync-collection>", http.StatusBadRequest},
		{"REPORT", calPath(workID), `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop/></C:calendar-multiget>`, http.StatusBadRequest},
		{"PROPFIND", "/dav/", tbDetect, http.StatusNotFound},
		{"GET", calPath(workID), "", http.StatusMethodNotAllowed},
	} {
		rec := do(h, tc.method, tc.path, tc.body, "Depth", "1")
		wantStatus(t, rec, tc.code)
		if rec.Body.Len() != 0 {
			t.Errorf("%s %s: body %q, want none", tc.method, tc.path, rec.Body)
		}
	}
}

// Thunderbird never sends calendar-query or free-busy-query; any report but
// ours is 403 supported-report (ruling M3-R23), as are ours where they do
// not apply.
func TestAvoidUnsupportedReportsAre403SupportedReport(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	for _, tc := range []struct{ path, body string }{
		{calPath(workID), `<C:calendar-query xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:"><D:prop><D:getetag/></D:prop><C:filter><C:comp-filter name="VCALENDAR"/></C:filter></C:calendar-query>`},
		{calPath(workID), `<C:free-busy-query xmlns:C="urn:ietf:params:xml:ns:caldav"/>`},
		{calPath(workID), `<D:expand-property xmlns:D="DAV:"/>`},
		{calPath(workID), `<sync-collection xmlns="urn:other"/>`},
		{home, tbSync("")},
		{principal, tbMultiget(tbHref(workID, plainUID))},
		{objPath(workID, plainUID), tbSync("")},
	} {
		rec := do(h, "REPORT", tc.path, tc.body, "Depth", "1")
		wantStatus(t, rec, http.StatusForbidden)
		wantBody(t, rec, supportedReportBody)
	}
	if _, reads := s.counts(); reads != 0 {
		t.Errorf("unsupported reports made %d reads", reads)
	}
}

func TestAvoidProppatchAndMkcalendarAre405(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	for _, tc := range []struct{ method, path string }{
		{"PROPPATCH", calPath(workID)}, {"MKCALENDAR", home + "new/"}, {"MKCOL", home + "new/"}, {"MOVE", objPath(workID, plainUID)},
		{"POST", calPath(workID)}, {"GET", home}, {"HEAD", calPath(workID)},
	} {
		rec := do(h, tc.method, tc.path, `<D:propertyupdate xmlns:D="DAV:"/>`)
		wantStatus(t, rec, http.StatusMethodNotAllowed)
		if rec.Header().Get("Allow") == "" {
			t.Errorf("%s: no Allow header", tc.method)
		}
	}
}

// --- Robustness -----------------------------------------------------------

func TestPropfindDepthInfinityIs403FiniteDepth(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	for _, depth := range []string{"infinity", ""} {
		rec := do(h, "PROPFIND", home, tbHomeSet, "Depth", depth)
		wantStatus(t, rec, http.StatusForbidden)
		wantBody(t, rec, finiteDepthBody)
	}
	wantStatus(t, do(h, "PROPFIND", home, tbHomeSet, "Depth", "2"), http.StatusBadRequest)
}

func TestPropfindAndReportBodiesOver1MiBAre413(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	const limit = 1 << 20
	valid := `<D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:propfind>`
	pad := func(size int) string { return valid + "<!--" + strings.Repeat("x", size-len(valid)-7) + "-->" }
	if len(pad(limit)) != limit {
		t.Fatal("test bug: padding")
	}
	parseMS(t, do(h, "PROPFIND", calPath(workID), pad(limit), "Depth", "0"))
	wantStatus(t, do(h, "PROPFIND", calPath(workID), pad(limit+1), "Depth", "0"), http.StatusRequestEntityTooLarge)
	// Without a Content-Length (chunked) too.
	r := authed(call("REPORT", calPath(workID), "", "Depth", "1"))
	r.Body = io.NopCloser(io.MultiReader(strings.NewReader(pad(limit + 1))))
	r.ContentLength = -1
	wantStatus(t, serve(h, r), http.StatusRequestEntityTooLarge)
}

// A DOCTYPE is refused outright: no entity of any kind is expanded.
func TestXMLWithADoctypeIsRefused(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	for _, body := range []string{
		`<!DOCTYPE p [<!ENTITY e "boom">]><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:propfind>`,
		`<?xml version="1.0"?><!DOCTYPE p SYSTEM "file:///etc/passwd"><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`,
		`<!DOCTYPE p><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`,
	} {
		for _, m := range []string{"PROPFIND", "REPORT"} {
			rec := do(h, m, calPath(workID), body, "Depth", "0")
			wantStatus(t, rec, http.StatusBadRequest)
		}
	}
	rec := do(h, "REPORT", calPath(workID), `<!DOCTYPE s [<!ENTITY t "urn:pcb:sync:1:e1:7:gen1">]><sync-collection xmlns="DAV:"><sync-token>&t;</sync-token><prop/></sync-collection>`, "Depth", "1")
	wantStatus(t, rec, http.StatusBadRequest)
	if _, reads := s.counts(); reads != 0 {
		t.Errorf("refused bodies made %d reads", reads)
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	for _, p := range []string{"/dav/", "/dav/principals/", "/dav/principals/someone-else/", "/dav/calendars/", "/dav/calendars/other/", home + "nothex/", "/favicon.ico"} {
		wantStatus(t, do(h, "PROPFIND", p, tbDetect, "Depth", "0"), http.StatusNotFound)
	}
}

// --- Fix round 1 ----------------------------------------------------------

// propNames is a prop list of count distinct names (ruling M3-R27's cap is
// 64), each written twice: duplicates do not count.
func propNames(count int) string {
	var b strings.Builder
	for i := range count {
		fmt.Fprintf(&b, "<x:p%d/><x:p%d/>", i, i)
	}
	return b.String()
}

// More than 64 distinct property names in a prop or include is 400 (ruling
// M3-R27): each name is echoed on every resource, so the list bounds the
// response; the service is not asked.
func TestPropertyListsOver64NamesAre400(t *testing.T) {
	t.Parallel()
	const ns = `xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:x="urn:x"`
	bodies := map[string]func(names string) (method, body string){
		"PROPFIND prop": func(names string) (string, string) {
			return "PROPFIND", `<D:propfind ` + ns + `><D:prop>` + names + `</D:prop></D:propfind>`
		},
		"PROPFIND include": func(names string) (string, string) {
			return "PROPFIND", `<D:propfind ` + ns + `><D:allprop/><D:include>` + names + `</D:include></D:propfind>`
		},
		"sync-collection prop": func(names string) (string, string) {
			return "REPORT", `<D:sync-collection ` + ns + `><D:sync-token/><D:prop>` + names + `</D:prop></D:sync-collection>`
		},
		"multiget prop": func(names string) (string, string) {
			return "REPORT", `<C:calendar-multiget ` + ns + `><D:prop>` + names + `</D:prop><D:href>` + objPath(workID, plainUID) + `</D:href></C:calendar-multiget>`
		},
	}
	for what, body := range bodies {
		s := newStub()
		h := caldav.Handler(s, caldav.Options{})
		m, b := body(propNames(65))
		rec := do(h, m, calPath(workID), b, "Depth", "1")
		wantStatus(t, rec, http.StatusBadRequest)
		if _, reads := s.counts(); reads != 0 || rec.Body.Len() != 0 {
			t.Errorf("%s: %d reads, body %q", what, reads, rec.Body)
		}
		m, b = body(propNames(64))
		ms := parseMS(t, do(h, m, calPath(workID), b, "Depth", "1"))
		if r := ms.responses[len(ms.responses)-1]; len(r.missing) != 64 {
			t.Errorf("%s with 64 names: %d missing properties, want 64", what, len(r.missing))
		}
	}
}

// More than 64 attributes on one element (namespace declarations included)
// is 400 (ruling M3-R27).
func TestElementsWithOver64AttributesAre400(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	attrs := func(count int) string {
		var b strings.Builder
		for i := range count {
			fmt.Fprintf(&b, ` xmlns:p%d="urn:p%d"`, i, i)
		}
		return b.String()
	}
	// The root carries xmlns:D, so it holds count+1 attributes.
	body := func(count int) string {
		return `<D:propfind xmlns:D="DAV:"` + attrs(count-1) + `><D:prop><D:getetag/></D:prop></D:propfind>`
	}
	parseMS(t, do(h, "PROPFIND", calPath(workID), body(64), "Depth", "0"))
	wantStatus(t, do(h, "PROPFIND", calPath(workID), body(65), "Depth", "0"), http.StatusBadRequest)
	wantStatus(t, do(h, "REPORT", calPath(workID), `<sync-collection xmlns="DAV:"`+attrs(64)+`><sync-token/><prop/></sync-collection>`, "Depth", "1"),
		http.StatusBadRequest)
	// Plain attributes count the same way, on any element.
	var plain strings.Builder
	for i := range 65 {
		fmt.Fprintf(&plain, ` a%d="v"`, i)
	}
	wantStatus(t, do(h, "PROPFIND", calPath(workID), `<D:propfind xmlns:D="DAV:"><D:prop><D:getetag`+plain.String()+`/></D:prop></D:propfind>`, "Depth", "0"),
		http.StatusBadRequest)
}

// The parser's limits: at most 10 000 elements (else 413) and 32 levels of
// nesting (else 400).
func TestXMLElementAndDepthLimits(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	// propfind, prop, getetag and x are 4 elements; the rest are y.
	elements := func(count int) string {
		return `<D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop><x>` + strings.Repeat("<y/>", count-4) + `</x></D:propfind>`
	}
	parseMS(t, do(h, "PROPFIND", calPath(workID), elements(10_000), "Depth", "0"))
	wantStatus(t, do(h, "PROPFIND", calPath(workID), elements(10_001), "Depth", "0"), http.StatusRequestEntityTooLarge)
	// propfind is level 1; depth-1 nested z reach level depth.
	nested := func(depth int) string {
		return `<D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop>` + strings.Repeat("<z>", depth-1) + strings.Repeat("</z>", depth-1) + `</D:propfind>`
	}
	parseMS(t, do(h, "PROPFIND", calPath(workID), nested(32), "Depth", "0"))
	wantStatus(t, do(h, "PROPFIND", calPath(workID), nested(33), "Depth", "0"), http.StatusBadRequest)
}

// sync-level, when sent, is 1 or infinite (a calendar holds no collection,
// both list the same); anything else is 400.
func TestSyncLevelMustBe1OrInfinite(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	body := func(level string) string {
		return `<sync-collection xmlns="DAV:"><sync-token/>` + level + `<prop><getetag/></prop></sync-collection>`
	}
	for _, level := range []string{"<sync-level>1</sync-level>", "<sync-level>infinite</sync-level>", "<sync-level> 1 </sync-level>", ""} {
		if ms := parseMS(t, do(h, "REPORT", calPath(workID), body(level), "Depth", "1")); len(ms.responses) != 3 {
			t.Errorf("%q: %d responses", level, len(ms.responses))
		}
	}
	for _, level := range []string{"<sync-level>2</sync-level>", "<sync-level>0</sync-level>", "<sync-level/>"} {
		wantStatus(t, do(h, "REPORT", calPath(workID), body(level), "Depth", "1"), http.StatusBadRequest)
	}
}

// A sync-collection asking for calendar-data fetches the changed objects;
// one gone since the changes were read is a deletion (response-level 404).
func TestSyncCollectionWithCalendarDataTurnsMissingObjectsIntoDeletions(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.deltas["tok-1"] = service.ChangeSet{
		Changed: []service.ObjectInfo{{UID: plainUID, ETag: `"v1-plain"`}, {UID: "vanished@x", ETag: `"v1-vanished"`}},
		Deleted: dels(), Token: "tok-2",
	}
	h := caldav.Handler(s, caldav.Options{})
	body := `<sync-collection xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><sync-token>tok-1</sync-token><prop><getetag/><C:calendar-data/></prop></sync-collection>`
	ms := parseMS(t, do(h, "REPORT", calPath(workID), body, "Depth", "1"))
	if got := ms.get(t, objPath(workID, plainUID)).prop(t, nsC, "calendar-data").Text; got != string(s.objs[workID][0].Data) {
		t.Errorf("calendar-data %q", got)
	}
	if r := ms.get(t, objPath(workID, "vanished@x")); r.status != "HTTP/1.1 404 Not Found" || r.propstats != 0 {
		t.Errorf("vanished object: status %q, %d propstats; want a response-level 404", r.status, r.propstats)
	}
	if len(ms.responses) != 2 || ms.token == nil || *ms.token != "tok-2" {
		t.Errorf("%d responses, token %v", len(ms.responses), ms.token)
	}
}

// allprop leaves sync-token out (RFC 6578 §4) unless include names it; an
// included name the resource lacks is a 404 property; propname lists the
// names with empty values.
func TestAllpropIncludeAndPropname(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	c := parseMS(t, do(h, "PROPFIND", calPath(workID), `<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`, "Depth", "0")).get(t, calPath(workID))
	if _, ok := c.props[n(nsD, "sync-token")]; ok {
		t.Error("allprop returned sync-token")
	}
	c.prop(t, nsCS, "getctag")
	inc := `<D:propfind xmlns:D="DAV:"><D:allprop/><D:include><D:sync-token/><D:getctag-not/><D:displayname/></D:include></D:propfind>`
	c = parseMS(t, do(h, "PROPFIND", calPath(workID), inc, "Depth", "0")).get(t, calPath(workID))
	if c.prop(t, nsD, "sync-token").Text != "urn:pcb:sync:1:e1:7:gen1" || !c.missing[n(nsD, "getctag-not")] || c.prop(t, nsD, "displayname").Text != "Work" {
		t.Errorf("allprop with include: %+v, missing %v", c.props, c.missing)
	}
	if strings.Count(parseMS(t, do(h, "PROPFIND", calPath(workID), inc, "Depth", "0")).raw, "<D:displayname>") != 1 {
		t.Error("an included property already in allprop is written twice")
	}
	names := parseMS(t, do(h, "PROPFIND", calPath(workID), `<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`, "Depth", "0")).get(t, calPath(workID))
	for _, p := range []xml.Name{n(nsD, "resourcetype"), n(nsCS, "getctag"), n(nsD, "sync-token"), n(nsD, "current-user-privilege-set")} {
		v, ok := names.props[p]
		if !ok || len(v.Kids) != 0 || v.Text != "" {
			t.Errorf("propname: %v = %+v (listed %v), want an empty element", p, v, ok)
		}
	}
	if len(names.missing) != 0 {
		t.Errorf("propname reported missing %v", names.missing)
	}
}

// Path variants resolve exactly (rulings M3-R13): dot segments, encoded
// slashes and dots, double encoding, an uppercase calseg, empty segments and
// a trailing slash on an object name nothing; %40 and a query string are
// the object.
func TestPathVariantsResolveExactly(t *testing.T) {
	t.Parallel()
	h := caldav.Handler(newStub(), caldav.Options{})
	cal, holCal := calPath(workID), calPath(holID)
	name := objName(plainUID)
	seg := strings.TrimSuffix(strings.TrimPrefix(cal, home), "/")
	for _, tc := range []struct {
		target string
		code   int
	}{
		{cal + name, http.StatusOK},
		{cal + strings.ReplaceAll(name, "@", "%40"), http.StatusOK},
		{cal + name + "?x=1", http.StatusOK},
		{cal + name + "/", http.StatusNotFound},
		{cal + strings.ReplaceAll(name, "@", "%2540"), http.StatusNotFound},
		{cal + "../" + strings.TrimPrefix(holCal, home) + objName("newyear@x"), http.StatusNotFound},
		{cal + "%2E%2E/" + strings.TrimPrefix(holCal, home) + objName("newyear@x"), http.StatusNotFound},
		{cal + "x%2F..%2F" + name, http.StatusNotFound},
		{cal + "./" + name, http.StatusNotFound},
		{home + strings.ToUpper(seg) + "/" + name, http.StatusNotFound},
		{home + "/" + seg + "/" + name, http.StatusNotFound},
		{cal + "/" + name, http.StatusNotFound},
		{strings.Replace(cal, "/dav/", "//dav/", 1) + name, http.StatusNotFound},
		{"/dav/calendars/me/../me/" + seg + "/" + name, http.StatusNotFound},
	} {
		if rec := do(h, "GET", tc.target, ""); rec.Code != tc.code {
			t.Errorf("GET %s: %d, want %d", tc.target, rec.Code, tc.code)
		}
	}
}

// A pending calendar (known to the cache, not synced yet, ruling M3-R25) is
// left out of the home set and answers 503 + Retry-After: 30, never a 4xx
// (which would disable it in Thunderbird), without a warning; so does one
// whose read says it is pending.
func TestPendingCalendarsAre503AndLeftOutOfTheHomeSet(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newStub()
	s.colls = append(s.colls, service.Collection{ID: "pending-ID", Name: "Pending", Pending: true})
	s.pending[holID] = true // listed as synced, then a new cache life began
	h := caldav.Handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	ms := parseMS(t, do(h, "PROPFIND", home, tbHomeSet, "Depth", "1"))
	if _, ok := ms.byPath[calPath("pending-ID")]; ok || len(ms.responses) != 3 {
		t.Errorf("home set lists %d responses, the pending calendar included: %v", len(ms.responses), ok)
	}
	for _, id := range []string{"pending-ID", holID} {
		for _, tc := range []struct{ method, path, body, depth string }{
			{"PROPFIND", calPath(id), tbServerInfo, "0"},
			{"PROPFIND", calPath(id), tbEtags, "1"},
			{"REPORT", calPath(id), tbSync(""), "1"},
			{"REPORT", calPath(id), tbSync("urn:pcb:sync:1:e1:3:gen2"), "1"},
			{"REPORT", calPath(id), tbMultiget(tbHref(id, "newyear@x")), "1"},
			{"GET", objPath(id, "newyear@x"), "", ""},
			{"PUT", objPath(id, "newyear@x"), "x", ""},
			{"DELETE", objPath(id, "newyear@x"), "", ""},
		} {
			if id == holID && (tc.method == "PROPFIND" && tc.depth == "0" || tc.method == "PUT" || tc.method == "DELETE") {
				// Reads nothing but the listing, which says synced (and,
				// for a write, read only: 403, as it is in every cache life).
				continue
			}
			rec := do(h, tc.method, tc.path, tc.body, "Depth", tc.depth, "Content-Type", "text/calendar")
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" || rec.Body.Len() != 0 {
				t.Errorf("%s %s (%s): %d, Retry-After %q, body %q; want 503, 30, none", tc.method, tc.path, id, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
			}
		}
	}
	if logs.Len() != 0 {
		t.Errorf("a pending calendar logged:\n%s", logs.String())
	}
}

// --- Fix round 2 ----------------------------------------------------------

// While the cache knows calendars but has synced none (a first pass under
// way), the home set answers 503 + Retry-After: 30 at Depth 0 and 1, so
// Thunderbird's detection reports a temporary failure, not "no calendars"
// (ruling M3-R30). One synced calendar is enough to list it.
func TestHomeSetIs503WhileEveryCalendarIsPending(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newStub()
	for i := range s.colls {
		s.colls[i].Pending, s.colls[i].CTag, s.colls[i].SyncToken = true, "", ""
	}
	h := caldav.Handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	for _, depth := range []string{"0", "1"} {
		for _, p := range []string{home, strings.TrimSuffix(home, "/")} {
			rec := do(h, "PROPFIND", p, tbHomeSet, "Depth", depth)
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" || rec.Body.Len() != 0 {
				t.Errorf("PROPFIND %s Depth %s, all pending: %d, Retry-After %q, body %q; want 503, 30, none",
					p, depth, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
			}
		}
	}
	if logs.Len() != 0 {
		t.Errorf("all-pending logged:\n%s", logs.String())
	}
	// Discovery above the home set still answers.
	parseMS(t, do(h, "PROPFIND", principal, tbDetect, "Depth", "0"))

	s.colls[0].Pending = false
	for _, depth := range []string{"0", "1"} {
		ms := parseMS(t, do(h, "PROPFIND", home, tbHomeSet, "Depth", depth))
		want := map[string]int{"0": 1, "1": 2}[depth]
		if len(ms.responses) != want {
			t.Errorf("one synced, Depth %s: %d responses, want %d", depth, len(ms.responses), want)
		}
	}
}

// Ruling M4-R42: a write may outlast the listener's timeouts (Proton slow
// or rate limiting). PUT and DELETE extend their connection's write
// deadline, so a write that landed still delivers its 201 or 204; the read
// timeout does not cut the request's context meanwhile (a cut context is a
// 503 here, as the service would give).
func TestSlowWriteStillDeliversItsAnswer(t *testing.T) {
	t.Parallel()
	const timeout = 250 * time.Millisecond
	s := newStub()
	s.putRes = service.PutResult{Created: true}
	s.slow = func(ctx context.Context) error {
		select {
		case <-time.After(3 * timeout): // past both listener timeouts
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	srv := httptest.NewUnstartedServer(caldav.Handler(s, caldav.Options{}))
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = timeout, timeout
	srv.Start()
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		method string
		body   []byte
		hdr    []string
		want   int
	}{
		{http.MethodPut, ics("new@x", "New"), []string{"Content-Type", "text/calendar", "If-None-Match", "*"}, http.StatusCreated},
		{http.MethodDelete, nil, nil, http.StatusNoContent},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, srv.URL+objPath(workID, "new@x"), bytes.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(tc.hdr); i += 2 {
			req.Header.Set(tc.hdr[i], tc.hdr[i+1])
		}
		resp, err := srv.Client().Do(authed(req))
		if err != nil {
			t.Errorf("%s: %v: the answer of a write that took longer than the listener's timeouts was lost", tc.method, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.method, resp.StatusCode, tc.want)
		}
	}
	if n := len(s.writeCalls()); n != 2 {
		t.Errorf("%d writes reached the service, want 2", n)
	}
}

// Ruling M4-R42: the connection of a write stays usable for at least
// 150 s, longer than the service may take.
func TestWriteBudgetCoversTheService(t *testing.T) {
	t.Parallel()
	if caldav.WriteBudget < 150*time.Second || caldav.WriteBudget <= service.MaxWriteDuration {
		t.Errorf("write budget %v: want at least 150 s and more than the service's %v", caldav.WriteBudget, service.MaxWriteDuration)
	}
}

// A requested property name or namespace over 256 bytes is 400 in PROPFIND
// and in both reports (audit surface-I1: it is echoed on every resource); the
// service is not asked. 256 bytes still passes.
func TestPropertyNamesAndNamespacesOver256BytesAre400(t *testing.T) {
	t.Parallel()
	long, ok := strings.Repeat("a", 257), strings.Repeat("b", 256)
	prop := func(local, space string) string { return `<x:` + local + ` xmlns:x="` + space + `"/>` }
	bodies := map[string]func(props string) string{
		"PROPFIND": func(p string) string {
			return `<D:propfind xmlns:D="DAV:"><D:prop>` + p + `</D:prop></D:propfind>`
		},
		"multiget": func(p string) string {
			return `<C:calendar-multiget xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:"><D:prop>` + p + `</D:prop><D:href>` + calPath(workID) + `a.ics</D:href></C:calendar-multiget>`
		},
		"sync": func(p string) string {
			return `<D:sync-collection xmlns:D="DAV:"><D:sync-token/><D:sync-level>1</D:sync-level><D:prop>` + p + `</D:prop></D:sync-collection>`
		},
	}
	for name, build := range bodies {
		method := "REPORT"
		if name == "PROPFIND" {
			method = "PROPFIND"
		}
		for what, p := range map[string]string{"name": prop(long, "u:x"), "namespace": prop("n", "u:"+long)} {
			s := newStub()
			h := caldav.Handler(s, caldav.Options{})
			wantStatus(t, do(h, method, calPath(workID), build(p), "Depth", "0"), http.StatusBadRequest)
			if _, reads := s.counts(); reads != 0 {
				t.Errorf("%s with a long %s made %d reads", name, what, reads)
			}
		}
		rec := do(caldav.Handler(newStub(), caldav.Options{}), method, calPath(workID), build(prop("n", "u:"+ok[:253])), "Depth", "0")
		if rec.Code == http.StatusBadRequest {
			t.Errorf("%s: a 256-byte namespace was refused", name)
		}
	}
}

// Every refused write logs exactly one warning, "refused a CalDAV write",
// with its code, calendar, href and reason (write-I1: Thunderbird with
// offline support never shows the answer and retries silently, so the log
// is where a refusal reaches someone). It never carries the body, the
// event's text or the client's password. A write that succeeds, or one
// answered 404, logs no such line.
func TestEachRefusedWriteLogsOneWarning(t *testing.T) {
	t.Parallel()
	const summary = "Secret board meeting"
	body := string(ics(plainUID, summary, "ATTENDEE;CN=Ann Example:mailto:ann@example.org"))
	put := func(calID string, hdr ...string) func(h http.Handler) *httptest.ResponseRecorder {
		return func(h http.Handler) *httptest.ResponseRecorder {
			return do(h, http.MethodPut, objPath(calID, plainUID), body, append([]string{"Content-Type", "text/calendar"}, hdr...)...)
		}
	}
	del := func(calID string, hdr ...string) func(h http.Handler) *httptest.ResponseRecorder {
		return func(h http.Handler) *httptest.ResponseRecorder {
			return do(h, http.MethodDelete, objPath(calID, plainUID), "", hdr...)
		}
	}
	href := objPath(workID, plainUID)
	// The IDs hold "=", which slog's text handler quotes.
	for _, tc := range []struct {
		name    string
		err     error
		send    func(h http.Handler) *httptest.ResponseRecorder
		status  int
		attrs   []string // the line must hold each
		noLines bool
	}{
		{"pcb refusal", svcErr(exitcode.Usage, &service.WriteRefusal{Code: "pcb:attendee-change", Reason: "adding or removing attendees is not supported"}),
			put(workID), http.StatusForbidden,
			[]string{"method=PUT", "status=403", "code=pcb:attendee-change", "calendar=" + strconv.Quote(workID), "href=" + href, `reason="adding or removing attendees is not supported"`}, false},
		{"pcb refusal of a DELETE", svcErr(exitcode.Usage, &service.WriteRefusal{Code: "pcb:invitation", Reason: "the event is an invitation from someone else"}),
			del(workID), http.StatusForbidden,
			[]string{"method=DELETE", "status=403", "code=pcb:invitation", "calendar=" + strconv.Quote(workID), "href=" + href, `reason="the event is an invitation from someone else"`}, false},
		{"service precondition", svcErr(exitcode.Usage, service.ErrPreconditionFailed), put(workID, "If-Match", `"v1-old"`), http.StatusPreconditionFailed,
			[]string{"status=412", "code=precondition-failed", "calendar=" + strconv.Quote(workID), "href=" + href, `reason="the If-Match or If-None-Match precondition does not hold on the object"`}, false},
		{"If-None-Match naming no ETag", nil, put(workID, "If-None-Match", ","), http.StatusPreconditionFailed,
			[]string{"status=412", "code=precondition-failed", `calendar=""`, "href=" + href}, false},
		{"UID conflict", svcErr(exitcode.Usage, &service.UIDConflictError{Href: "other one.ics"}), put(workID), http.StatusConflict,
			[]string{"status=409", "code=no-uid-conflict", "calendar=" + strconv.Quote(workID), "href=" + href, "conflict=" + calPath(workID) + "other%20one.ics",
				`reason="the UID or the name belongs to another object of the calendar"`}, false},
		{"read-only calendar", nil, put(holID), http.StatusForbidden,
			[]string{"status=403", "code=need-privileges", "calendar=" + strconv.Quote(holID), "href=" + objPath(holID, plainUID), `reason="the calendar is read only"`}, false},
		{"service read only", svcErr(exitcode.Usage, service.ErrNotWritable), del(workID), http.StatusForbidden,
			[]string{"method=DELETE", "status=403", "code=need-privileges", "calendar=" + strconv.Quote(workID)}, false},
		{"success", nil, put(workID), http.StatusNoContent, nil, true},
		{"missing object", svcErr(exitcode.Usage, service.ErrNoSuchObject), del(workID), http.StatusNotFound, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			s := newStub()
			s.writeErr = tc.err
			h := caldav.Handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			wantStatus(t, tc.send(h), tc.status)
			var lines []string
			for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if strings.Contains(l, `msg="refused a CalDAV write"`) {
					lines = append(lines, l)
				}
			}
			if tc.noLines {
				if len(lines) != 0 {
					t.Fatalf("logged %q, want no refusal line", lines)
				}
				return
			}
			if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
				t.Fatalf("log:\n%s\nwant exactly one warning \"refused a CalDAV write\"", logs.String())
			}
			for _, a := range tc.attrs {
				if !strings.Contains(lines[0], a) {
					t.Errorf("the line %q lacks %q", lines[0], a)
				}
			}
			for _, bad := range []string{summary, "Ann Example", "ann@example.org", password, "BEGIN:VEVENT"} {
				if strings.Contains(logs.String(), bad) {
					t.Errorf("the log carries %q:\n%s", bad, logs.String())
				}
			}
		})
	}
}

// If-Match reaches the service as sent, a list included, and several
// If-Match headers as one list (RFC 9110 §5.3): the service compares them
// (write-M4).
func TestIfMatchListsReachTheService(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(s, caldav.Options{})
	req := authed(call(http.MethodPut, objPath(workID, plainUID), "x", "Content-Type", "text/calendar"))
	req.Header.Add("If-Match", `"v1-a", W/"v1-b"`)
	req.Header.Add("If-Match", `"v1-plain"`)
	wantStatus(t, serve(h, req), http.StatusNoContent)
	if w := s.writeCalls(); len(w) != 1 || w[0].cond.IfMatch != `"v1-a", W/"v1-b","v1-plain"` {
		t.Errorf("the service got %+v", w)
	}
}

// The same refusal (code and href) is logged once per 10 minutes; the next
// line after the window carries repeated=N, and the refusal itself is still
// answered every time. Another href or code is a new line (A12).
func TestRepeatedRefusalsAreLoggedOncePerWindow(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newStub()
	s.writeErr = svcErr(exitcode.Usage, service.ErrPreconditionFailed)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	h := caldav.Handler(s, caldav.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)), Now: func() time.Time { return now }})
	put := func(uid string) {
		t.Helper()
		wantStatus(t, do(h, http.MethodPut, objPath(workID, uid), string(ics(uid, "x")), "Content-Type", "text/calendar", "If-Match", `"v1"`), http.StatusPreconditionFailed)
	}
	lines := func() []string {
		return slices.DeleteFunc(strings.Split(logs.String(), "\n"), func(l string) bool { return l == "" })
	}
	put(plainUID)
	now = now.Add(time.Minute)
	put(plainUID)
	now = now.Add(8 * time.Minute) // 9 minutes in: still inside the window
	put(plainUID)
	if n := len(lines()); n != 1 || strings.Contains(lines()[0], "repeated=") {
		t.Fatalf("%d lines inside the window, want the first alone:\n%s", n, logs.String())
	}
	put("other-uid") // another href is its own refusal
	if n := len(lines()); n != 2 {
		t.Fatalf("%d lines after another href, want 2:\n%s", n, logs.String())
	}
	now = now.Add(2 * time.Minute) // 11 minutes after the first line
	put(plainUID)
	got := lines()
	if len(got) != 3 || !strings.Contains(got[2], "repeated=2") || !strings.Contains(got[2], "href="+objPath(workID, plainUID)) {
		t.Fatalf("after the window want a third line with repeated=2:\n%s", logs.String())
	}
	put(plainUID) // the window restarts
	if n := len(lines()); n != 3 {
		t.Errorf("%d lines, want the window to restart at the third", n)
	}
}

// The dedup memory is bounded: a flood of distinct hrefs never grows it past
// its cap, and the newest is always remembered (A12).
func TestRefusalLogMemoryIsBounded(t *testing.T) {
	t.Parallel()
	s := newStub()
	s.writeErr = svcErr(exitcode.Usage, service.ErrPreconditionFailed)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	h := caldav.Handler(s, caldav.Options{Now: func() time.Time { return now }})
	for i := range caldav.RefusalLogSize + 500 {
		now = now.Add(time.Millisecond) // all inside one window: only the cap can prune
		do(h, http.MethodPut, objPath(workID, "uid-"+strconv.Itoa(i)), string(ics(plainUID, "x")), "Content-Type", "text/calendar", "If-Match", `"v1"`)
	}
	if n := caldav.RefusalLogLen(h); n != caldav.RefusalLogSize {
		t.Errorf("dedup holds %d entries, want exactly %d", n, caldav.RefusalLogSize)
	}
}
