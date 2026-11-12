package caldav

// PUT and DELETE on an object (M4), on the service's write API: one
// VCALENDAR per object, created or replaced whole (RFC 4791 §4.1), at the
// name the client chose (an alias when it is not the UID's canonical name,
// ruling M4-R6), under the client's If-Match or If-None-Match: *. The
// answers follow E08 MUST 10 and 11:
//
//	201 created, 204 replaced or deleted, never with an ETag (Thunderbird
//	    reads the object back with a multiget, E08 §4.1)
//	412 a precondition that does not hold (ErrPreconditionFailed: also a
//	    concurrent change Proton showed, ruling M4-R34)
//	409 the UID lives at another name, or the name holds another UID:
//	    <C:no-uid-conflict> naming that object (RFC 4791 §5.3.2.1)
//	403 the calendar is read only, decided from good data (need-privileges,
//	    rulings M4-R1, M4-R39; an undecided gate is a 503), or the
//	    bridge refuses the change: <pcb:CODE xmlns:pcb="urn:pcb"/> and a
//	    short reason of ours, never Proton's text
//	404 DELETE of a name that serves no object (Thunderbird: success)
//	415 a body that is not text/calendar; 413 one over 8 MiB
//	503 + Retry-After for anything else: the service is not ready, Proton is
//	    unavailable or rate limiting (its own Retry-After), or the write is
//	    not confirmed (ruling M3-R22: never a 4xx for a state that passes)
//
// Each 412, 409 and 403 above is a refusal, logged once at warning level
// (refuse, audit write-I1).
//
// A write may run longer than the listener's timeouts allow a read
// (ruling M4-R42): PUT and DELETE extend their connection's write deadline
// to writeBudget, so a write that landed delivers its answer.

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// writeBudget is how long a PUT or DELETE keeps its connection usable: the
// service's bound on a write (lock wait, refresh and plan, Sync call,
// read-back) and a margin for sending the answer (ruling M4-R42: at least
// 150 s).
const writeBudget = service.MaxWriteDuration + 40*time.Second

// extendDeadline gives a write's answer writeBudget from now, past the
// listener's write timeout, so a write that landed still delivers its
// answer. The read timeout needs nothing: net/http clears the read
// deadline once the body is read, so it never ends the request's context.
// A writer without deadlines (a test recorder) is left as it is.
func extendDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeBudget))
}

// put answers PUT on an object.
func (s *server) put(w http.ResponseWriter, r *http.Request, t target) {
	if !calendarBody(r.Header.Get("Content-Type")) {
		empty(w, http.StatusUnsupportedMediaType)
		return
	}
	cond, ok := precondition(r)
	if !ok {
		s.refuse(w, r, "", preconditionRefusal())
		return
	}
	body, ok := readBody(w, r, maxBody)
	if !ok {
		return
	}
	extendDeadline(w)
	c, ok := s.collection(w, r, t.calseg)
	if !ok {
		return
	}
	if c.ReadOnly && !c.WriteGateUndecided {
		s.refuse(w, r, c.ID, privilegesRefusal(r))
		return
	}
	res, err := s.api.PutObject(r.Context(), c.ID, t.name, body, cond)
	if err != nil {
		s.writeFail(w, r, c, err)
		return
	}
	if res.Created {
		empty(w, http.StatusCreated)
		return
	}
	empty(w, http.StatusNoContent)
}

// delete answers DELETE on an object: every row of its UID.
func (s *server) delete(w http.ResponseWriter, r *http.Request, t target) {
	cond, ok := precondition(r)
	if !ok {
		s.refuse(w, r, "", preconditionRefusal())
		return
	}
	extendDeadline(w)
	c, ok := s.collection(w, r, t.calseg)
	if !ok {
		return
	}
	if c.ReadOnly && !c.WriteGateUndecided {
		s.refuse(w, r, c.ID, privilegesRefusal(r))
		return
	}
	if err := s.api.DeleteObject(r.Context(), c.ID, t.name, cond); err != nil {
		s.writeFail(w, r, c, err)
		return
	}
	empty(w, http.StatusNoContent)
}

// calendarBody reports whether a Content-Type names iCalendar (parameters
// such as charset aside).
func calendarBody(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/calendar"
}

// precondition reads If-Match (the service compares its list of ETags
// strongly, or "*"), from one header or several, and If-None-Match: "*", or
// the ETags it names (the service compares them weakly with the object's,
// and a missing object passes, RFC 9110 §13.1.2). An If-None-Match with no
// ETag in it is false: the request is answered 412 without being tried.
func precondition(r *http.Request) (service.Precondition, bool) {
	cond := service.Precondition{IfMatch: strings.TrimSpace(strings.Join(r.Header.Values("If-Match"), ","))}
	inm := strings.TrimSpace(strings.Join(r.Header.Values("If-None-Match"), ","))
	if inm == "" {
		return cond, true
	}
	for _, e := range strings.Split(inm, ",") {
		switch e = strings.TrimSpace(e); e {
		case "":
		case "*": // anywhere in the list, repeated or beside ETags: the object must not exist
			cond.IfNoneMatchAny = true
		default:
			cond.IfNoneMatch = append(cond.IfNoneMatch, e)
		}
	}
	if cond.IfNoneMatchAny {
		cond.IfNoneMatch = nil
		return cond, true
	}
	return cond, len(cond.IfNoneMatch) > 0
}

// writeFail answers a failed write per the service's error contract
// (service.API); anything outside it is unavailable (503), never a 4xx.
func (s *server) writeFail(w http.ResponseWriter, r *http.Request, c service.Collection, err error) {
	var (
		conflict *service.UIDConflictError
		refusal  *service.WriteRefusal
	)
	switch {
	case errors.Is(err, service.ErrPreconditionFailed):
		s.refuse(w, r, c.ID, preconditionRefusal())
	case errors.As(err, &conflict):
		s.refuse(w, r, c.ID, conflictRefusal(c.ID, conflict.Href))
	case errors.Is(err, service.ErrNotWritable):
		s.refuse(w, r, c.ID, privilegesRefusal(r))
	case errors.As(err, &refusal):
		s.refuse(w, r, c.ID, pcbRefusal(refusal))
	case errors.Is(err, service.ErrNoSuchObject), errors.Is(err, service.ErrNoSuchCalendar):
		empty(w, http.StatusNotFound)
	default:
		s.unavailable(w, r, err)
	}
}

// writeRefusal is a write the bridge refuses, answered with a 4xx the docs
// list (412, 409, 403): its status and XML body ("" for none), and what
// the log says of it, a code and a reason that are fixed text of ours.
type writeRefusal struct {
	status       int
	body         string
	code, reason string
	conflict     string // 409: the href of the object holding the UID or the name
}

// refuse answers a refused write and logs it, the one place that does
// (write-I1): a client may never show the answer (Thunderbird with offline
// support retries it silently, E08 §4.2), so each refusal is one warning
// naming the code, the calendar (calID; "" for a request refused before
// any calendar is looked up), the object's href as requested and the
// reason. Never the body, the event's content or the credentials.
func (s *server) refuse(w http.ResponseWriter, r *http.Request, calID string, e writeRefusal) {
	attrs := []any{"method", r.Method, "status", e.status, "code", e.code, "calendar", calID, "href", r.URL.EscapedPath(), "reason", e.reason}
	if e.conflict != "" {
		attrs = append(attrs, "conflict", e.conflict)
	}
	s.log.Warn("refused a CalDAV write", attrs...)
	if e.body == "" {
		empty(w, e.status)
		return
	}
	xmlError(w, e.status, e.body)
}

// preconditionRefusal: If-Match or If-None-Match does not hold (412, no
// body).
func preconditionRefusal() writeRefusal {
	return writeRefusal{status: http.StatusPreconditionFailed, code: "precondition-failed",
		reason: "the If-Match or If-None-Match precondition does not hold on the object"}
}

// conflictRefusal: the UID lives at another name of calendar calID, or the
// name holds another UID: 409 with the no-uid-conflict naming href, the
// object that holds it (RFC 4791 §5.3.2.1).
func conflictRefusal(calID, href string) writeRefusal {
	return writeRefusal{status: http.StatusConflict,
		body: `<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><C:no-uid-conflict>` +
			hrefXML(objectHref(calID, href)) + "</C:no-uid-conflict></D:error>",
		code: "no-uid-conflict", reason: "the UID or the name belongs to another object of the calendar", conflict: objectHref(calID, href)}
}

// privilegesRefusal answers a write to a read-only calendar: 403 with the
// privilege the request lacks (RFC 3744 need-privileges): write-content on
// the object for a PUT, unbind on the calendar for a DELETE.
func privilegesRefusal(r *http.Request) writeRefusal {
	href, privilege := r.URL.EscapedPath(), "<D:write-content/>"
	if r.Method == http.MethodDelete {
		href, privilege = path.Dir(strings.TrimSuffix(href, "/"))+"/", "<D:unbind/>"
	}
	return writeRefusal{status: http.StatusForbidden,
		body: `<D:error xmlns:D="DAV:"><D:need-privileges><D:resource>` + hrefXML(href) +
			"<D:privilege>" + privilege + "</D:privilege></D:resource></D:need-privileges></D:error>",
		code: "need-privileges", reason: "the calendar is read only"}
}

// pcbNamespace holds the bridge's own preconditions.
const pcbNamespace = "urn:pcb"

// pcbRefusal answers a change the bridge refuses: 403 with its pcb
// precondition (the code without its "pcb:" prefix, in the urn:pcb
// namespace) and its reason, which is ours, short and fixed
// (service.WriteRefusal).
func pcbRefusal(e *service.WriteRefusal) writeRefusal {
	local, ok := strings.CutPrefix(e.Code, "pcb:")
	if !ok || !ncName(local) {
		local = "refused"
	}
	return writeRefusal{status: http.StatusForbidden,
		body: `<D:error xmlns:D="DAV:"><pcb:` + local + ` xmlns:pcb="` + pcbNamespace + `"/><pcb:reason xmlns:pcb="` + pcbNamespace + `">` +
			text(e.Reason) + "</pcb:reason></D:error>",
		code: "pcb:" + local, reason: e.Reason}
}
