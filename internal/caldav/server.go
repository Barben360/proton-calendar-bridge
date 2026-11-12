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
// calseg is the first 16 hex digits of sha256 of the calendar's ID. name is
// the one name the service serves the object at (service.ObjectsByName):
// the name a client created it at (an alias, ruling M4-R6), else the UID
// when it is made of [A-Za-z0-9._@-] (1 to 200 bytes), else "~" and the UID
// in unpadded base64url (ruling M3-R13), or "~~" and the UID in unpadded
// base64url when another object's alias holds that name (ruling M4-R38).
// An object also answers PUT and DELETE (write.go, M4); PROPPATCH and
// MKCALENDAR are 405.
//
// Every method but OPTIONS needs a client's app password (Basic, any
// username). A service that cannot answer (logged out, waiting for a login,
// no cache, Proton or session failure) is a 503 with Retry-After: 30, never a 4xx (ruling
// M3-R22: a 4xx disables the calendar in Thunderbird, a 5xx is temporary).
// No body carries error text.
package caldav

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	allowed    = "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, REPORT"

	maxBody = 8 << 20 // request bodies (a PUT's iCalendar too), larger: 413
	// maxHrefs bounds a calendar-multiget (Thunderbird sends 100); more is
	// 413, the request being too large rather than forbidden.
	maxHrefs = 1000
	// maxCalendarData bounds the objects whose calendar-data one PROPFIND
	// Depth 1 or sync-collection renders (ruling M3-R35): past it, an object
	// keeps its getetag and gets calendar-data in a 404 propstat, and the
	// client multigets it. Thunderbird never asks for calendar-data there.
	maxCalendarData = 1000

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
	return &server{api: api, log: o.Logger, dataCap: maxCalendarData}
}

type server struct {
	api     service.API
	log     *slog.Logger
	dataCap int // maxCalendarData (tests lower it)
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
	case r.Method != "PROPFIND" && r.Method != "REPORT" && r.Method != http.MethodGet && r.Method != http.MethodHead &&
		r.Method != http.MethodPut && r.Method != http.MethodDelete:
		w.Header().Set("Allow", allowed)
		empty(w, http.StatusMethodNotAllowed)
	case t.kind == kindNone:
		empty(w, http.StatusNotFound)
	case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && t.kind != kindObject:
		// A collection is not written to.
		w.Header().Set("Allow", allowed)
		empty(w, http.StatusMethodNotAllowed)
	case r.Method == http.MethodPut:
		s.put(w, r, t)
	case r.Method == http.MethodDelete:
		s.delete(w, r, t)
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
// ErrNoSuchCalendar 404, anything else 503 with Retry-After (ruling M3-R22),
// with a warning unless the calendar is pending (ruling M3-R25), pcb serve
// waits for a login (ruling M5-P8) or the request's context ended (its client is gone, or the server is stopping:
// still a 503, never an empty 200).
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidSyncToken):
		forbidden(w, errValidSyncToken)
	case errors.Is(err, service.ErrNoSuchCalendar):
		empty(w, http.StatusNotFound)
	default:
		s.unavailable(w, r, err)
	}
}

// unavailable answers err, a failure of the service, 503 with Retry-After:
// Proton's own wait when it asked for one (a 429), else unavailableRetry;
// with a warning unless it is expected (the calendar is pending, or pcb
// serve waits for a login: ruling M5-P8) or the request's context ended.
func (s *server) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil && !errors.Is(err, service.ErrCalendarPending) && !errors.Is(err, service.ErrWaitingForLogin) {
		// The service's errors are secret-free by contract (service.API).
		s.log.Warn("caldav: the service could not answer", "method", r.Method, "error", err.Error())
	}
	wait := service.ProtonRetryAfter(err)
	if wait <= 0 {
		wait = unavailableRetry
	}
	w.Header().Set("Retry-After", retryAfter(wait))
	empty(w, http.StatusServiceUnavailable)
}

// unavailable answers 503 with the Retry-After a client should wait.
func unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", retryAfter(unavailableRetry))
	empty(w, http.StatusServiceUnavailable)
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
// itself: 503 while it is pending (ruling M3-R25); for a segment no calendar
// has, 503 too while any calendar is pending or none is synced (a first pass
// records the calendars one by one, and a 404 would disable one not
// recorded yet in Thunderbird, E08 §2.2), else 404 (ruling M3-R35).
func (s *server) collection(w http.ResponseWriter, r *http.Request, seg string) (service.Collection, bool) {
	colls, err := s.api.Collections(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return service.Collection{}, false
	}
	pending := false
	for _, c := range colls {
		if calendarSegment(c.ID) == seg {
			if c.Pending {
				unavailable(w)
				return c, false
			}
			return c, true
		}
		pending = pending || c.Pending
	}
	if pending || len(colls) == 0 { // calendars are being recorded
		unavailable(w)
		return service.Collection{}, false
	}
	empty(w, http.StatusNotFound)
	return service.Collection{}, false
}

// withData writes the objects of infos into ms with the properties q asks;
// calendar-data only for the first dataCap of them, rendered in one
// ObjectsByUID (the others answer it in a 404 propstat). It returns the
// names of the objects rendered as missing (deleted since infos was read);
// false when it answered a service failure itself.
func (s *server) withData(w http.ResponseWriter, r *http.Request, ms *multistatus, calID string, infos []service.ObjectInfo, q propRequest) ([]string, bool) {
	n := min(len(infos), s.dataCap)
	uids := make([]string, n)
	for i, o := range infos[:n] {
		uids[i] = o.UID
	}
	var gone []string
	if n > 0 {
		objs, err := s.api.ObjectsByUID(r.Context(), calID, uids)
		if err != nil {
			s.fail(w, r, err)
			return nil, false
		}
		for _, o := range objs {
			if o.Missing {
				gone = append(gone, o.Name)
				continue
			}
			objectResource(calID, o.Name, o.ETag, o.Data, true).write(ms, q)
		}
	}
	for _, o := range infos[n:] {
		objectResource(calID, o.Name, o.ETag, nil, false).write(ms, q)
	}
	return gone, true
}

// object reads the object served at t's name; it answers the failure
// itself (404 when the name serves no live object).
func (s *server) object(w http.ResponseWriter, r *http.Request, t target) (service.Collection, service.Object, bool) {
	c, ok := s.collection(w, r, t.calseg)
	if !ok {
		return c, service.Object{}, false
	}
	objs, err := s.api.ObjectsByName(r.Context(), c.ID, []string{t.name})
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
	name   string // kindObject: the resource name, decoded
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
	switch {
	case name == "":
		return target{kind: kindCalendar, calseg: seg}
	case strings.Contains(name, "/"): // nothing is nested in a calendar
		return target{}
	}
	// Which object the name serves, if any, is the service's to say: a
	// client may have chosen it (ruling M4-R6).
	return target{kind: kindObject, calseg: seg, name: name}
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

// objectHref is the href of the object served at name in calendar calID,
// the name percent-encoded (a canonical name needs none).
func objectHref(calID, name string) string { return calendarHref(calID) + url.PathEscape(name) }
