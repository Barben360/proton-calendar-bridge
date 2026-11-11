// Package caldav is A1: the bridge's own read-only CalDAV server on top of
// service.API (ruling M3-R5), built to Thunderbird's contract
// (spec/experiments/E08-thunderbird-caldav.md). It serves:
//
//	/                                       PROPFIND: current-user-principal
//	/.well-known/caldav                     308 to the principal
//	/dav/principals/me/                     the principal: calendar-home-set
//	/dav/calendars/me/                      the home set
//	/dav/calendars/me/<calseg>/             a calendar: PROPFIND, REPORT
//	/dav/calendars/me/<calseg>/<name>.ics   an object: PROPFIND, GET, HEAD
//
// calseg is the first 16 hex digits of sha256 of the calendar's ID; name is
// the UID when it is made of [A-Za-z0-9._@-] (1 to 200 bytes), else "~" and
// the UID in unpadded base64url (ruling M3-R13). Writes come with M4: PUT
// and DELETE are refused (403 need-privileges), PROPPATCH and MKCALENDAR are
// 405.
//
// Every method but OPTIONS needs a client's app password (Basic, any
// username). A service that cannot answer (logged out, no cache, Proton or
// session failure) is a 503 with Retry-After: 30, never a 4xx (ruling
// M3-R22: a 4xx disables the calendar in Thunderbird, a 5xx is temporary).
// No body carries error text.
package caldav

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const (
	wellKnownPath = "/.well-known/caldav"
	principalPath = "/dav/principals/me/"
	homePath      = "/dav/calendars/me/"

	// davClasses never names calendar-schedule or calendar-auto-schedule:
	// Thunderbird would stop sending invitations itself (E08 §5.1).
	davClasses = "1, 3, calendar-access"
	allowed    = "OPTIONS, GET, HEAD, PROPFIND, REPORT"

	maxBody = 8 << 20 // request bodies, larger: 413
	// maxHrefs bounds a calendar-multiget (Thunderbird sends 100); more is
	// 413, the request being too large rather than forbidden.
	maxHrefs = 1000

	unavailableRetry = 30 * time.Second

	objectType    = "text/calendar; charset=utf-8"
	componentType = "text/calendar; charset=utf-8; component=vevent"
)

// Options configure the handler.
type Options struct {
	// Logger gets a warning for each request the service could not answer
	// (503); nil discards them.
	Logger *slog.Logger
}

// Handler serves api's calendars over CalDAV. It holds no state: every
// request reads the service.
func Handler(api service.API, o Options) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &server{api: api, log: o.Logger}
}

type server struct {
	api service.API
	log *slog.Logger
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions { // no credentials needed (ruling M3-R23)
		w.Header().Set("DAV", davClasses)
		w.Header().Set("Allow", allowed)
		empty(w, http.StatusOK)
		return
	}
	if !s.authenticate(w, r) {
		return
	}
	t := resolve(r.URL.Path)
	switch {
	case t.kind == kindWellKnown:
		w.Header().Set("Location", principalPath)
		empty(w, http.StatusPermanentRedirect)
	case r.Method == http.MethodPut || r.Method == http.MethodDelete:
		refuseWrite(w, r)
	case r.Method != "PROPFIND" && r.Method != "REPORT" && r.Method != http.MethodGet && r.Method != http.MethodHead:
		w.Header().Set("Allow", allowed)
		empty(w, http.StatusMethodNotAllowed)
	case t.kind == kindNone:
		empty(w, http.StatusNotFound)
	case r.Method == "PROPFIND":
		s.propfind(w, r, t)
	case r.Method == "REPORT":
		s.report(w, r, t)
	case t.kind == kindObject:
		s.get(w, r, t)
	default: // GET or HEAD on a collection
		w.Header().Set("Allow", allowed)
		empty(w, http.StatusMethodNotAllowed)
	}
}

// fail answers a service error: ErrInvalidSyncToken is 403 valid-sync-token,
// ErrNoSuchCalendar 404, anything else 503 with Retry-After (ruling M3-R22)
// and a warning; a request whose client went away gets nothing.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case r.Context().Err() != nil:
	case errors.Is(err, service.ErrInvalidSyncToken):
		forbidden(w, errValidSyncToken)
	case errors.Is(err, service.ErrNoSuchCalendar):
		empty(w, http.StatusNotFound)
	default:
		// The service's errors are secret-free by contract (service.API).
		s.log.Warn("caldav: the service could not answer", "method", r.Method, "error", err.Error())
		w.Header().Set("Retry-After", retryAfter(unavailableRetry))
		empty(w, http.StatusServiceUnavailable)
	}
}

// readBody reads the request body, at most maxBody bytes; it answers 413
// (larger) or 400 itself.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			empty(w, http.StatusRequestEntityTooLarge)
		} else {
			empty(w, http.StatusBadRequest)
		}
		return nil, false
	}
	return body, true
}

// collection finds the calendar of segment seg; it answers the failure
// itself (404 when no synced calendar has that segment).
func (s *server) collection(w http.ResponseWriter, r *http.Request, seg string) (service.Collection, bool) {
	colls, err := s.api.Collections(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return service.Collection{}, false
	}
	for _, c := range colls {
		if calendarSegment(c.ID) == seg {
			return c, true
		}
	}
	empty(w, http.StatusNotFound)
	return service.Collection{}, false
}

// object reads one object; it answers the failure itself (404 when the UID
// has no live row).
func (s *server) object(w http.ResponseWriter, r *http.Request, t target) (service.Collection, service.Object, bool) {
	c, ok := s.collection(w, r, t.calseg)
	if !ok {
		return c, service.Object{}, false
	}
	objs, err := s.api.ObjectsByUID(r.Context(), c.ID, []string{t.uid})
	if err != nil {
		s.fail(w, r, err)
		return c, service.Object{}, false
	}
	if len(objs) != 1 || objs[0].Missing {
		empty(w, http.StatusNotFound)
		return c, service.Object{}, false
	}
	return c, objs[0], true
}

// get answers GET and HEAD on an object: its VCALENDAR as the service
// rendered it, byte for byte.
func (s *server) get(w http.ResponseWriter, r *http.Request, t target) {
	_, o, ok := s.object(w, r, t)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", objectType)
	w.Header().Set("ETag", o.ETag)
	w.Header().Set("Content-Length", strconv.Itoa(len(o.Data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(o.Data)
	}
}

// refuseWrite answers PUT and DELETE until M4: 403 with the privilege the
// request lacks (RFC 3744 need-privileges).
func refuseWrite(w http.ResponseWriter, r *http.Request) {
	href, privilege := r.URL.EscapedPath(), "<D:write-content/>"
	if r.Method == http.MethodDelete {
		href, privilege = path.Dir(strings.TrimSuffix(href, "/"))+"/", "<D:unbind/>"
	}
	forbidden(w, `<D:error xmlns:D="DAV:"><D:need-privileges><D:resource>`+hrefXML(href)+
		"<D:privilege>"+privilege+"</D:privilege></D:resource></D:need-privileges></D:error>")
}

// kind is what a path names.
type kind int

const (
	kindNone kind = iota
	kindRoot
	kindWellKnown
	kindPrincipal
	kindHome
	kindCalendar
	kindObject
)

type target struct {
	kind   kind
	calseg string // kindCalendar, kindObject
	uid    string // kindObject
}

// resolve maps a request path (decoded) to what it names. Collections
// answer with or without their trailing slash.
func resolve(p string) target {
	switch {
	case p == "" || p == "/":
		return target{kind: kindRoot}
	case p == wellKnownPath || p == wellKnownPath+"/":
		return target{kind: kindWellKnown}
	case p == principalPath || p+"/" == principalPath:
		return target{kind: kindPrincipal}
	case p == homePath || p+"/" == homePath:
		return target{kind: kindHome}
	}
	rest, ok := strings.CutPrefix(p, homePath)
	if !ok {
		return target{}
	}
	seg, name, _ := strings.Cut(rest, "/")
	if !isSegment(seg) {
		return target{}
	}
	if name == "" {
		return target{kind: kindCalendar, calseg: seg}
	}
	uid, ok := parseName(name)
	if !ok {
		return target{}
	}
	return target{kind: kindObject, calseg: seg, uid: uid}
}

// calendarSegment is the path segment of calendar id: the first 16 hex
// digits of its sha256.
func calendarSegment(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func isSegment(s string) bool {
	return len(s) == 16 && strings.Trim(s, "0123456789abcdef") == ""
}

func calendarHref(id string) string { return homePath + calendarSegment(id) + "/" }

func objectHref(calID, uid string) string { return calendarHref(calID) + objectName(uid) }

// plainUID reports whether uid is its own resource name: 1 to 200 bytes of
// [A-Za-z0-9._@-], the set Thunderbird names its own events with (E08 §4.4).
func plainUID(uid string) bool {
	if uid == "" || len(uid) > 200 {
		return false
	}
	for i := range len(uid) {
		switch c := uid[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// objectName is uid's resource name: the UID, or "~" + its unpadded
// base64url, then ".ics".
func objectName(uid string) string {
	if plainUID(uid) {
		return uid + ".ics"
	}
	return "~" + base64.RawURLEncoding.EncodeToString([]byte(uid)) + ".ics"
}

// parseName is objectName's inverse, exact: a name that is not the
// canonical name of its UID does not decode.
func parseName(name string) (string, bool) {
	stem, ok := strings.CutSuffix(name, ".ics")
	if !ok {
		return "", false
	}
	uid := stem
	if enc, encoded := strings.CutPrefix(stem, "~"); encoded {
		b, err := base64.RawURLEncoding.Strict().DecodeString(enc)
		if err != nil {
			return "", false
		}
		uid = string(b)
	}
	if uid == "" || objectName(uid) != name {
		return "", false
	}
	return uid, true
}
