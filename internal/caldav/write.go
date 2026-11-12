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
		empty(w, http.StatusPreconditionFailed)
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
		needPrivileges(w, r)
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
		empty(w, http.StatusPreconditionFailed)
		return
	}
	extendDeadline(w)
	c, ok := s.collection(w, r, t.calseg)
	if !ok {
		return
	}
	if c.ReadOnly && !c.WriteGateUndecided {
		needPrivileges(w, r)
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

// precondition reads If-Match (an ETag compared as an exact string, or
// "*") and If-None-Match: "*", or the ETags it names (the service compares
// them weakly with the object's, and a missing object passes, RFC 9110
// §13.1.2). An If-None-Match with no ETag in it is false: the request is
// answered 412 without being tried.
func precondition(r *http.Request) (service.Precondition, bool) {
	cond := service.Precondition{IfMatch: strings.TrimSpace(r.Header.Get("If-Match"))}
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
		empty(w, http.StatusPreconditionFailed)
	case errors.As(err, &conflict):
		xmlError(w, http.StatusConflict, `<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><C:no-uid-conflict>`+
			hrefXML(objectHref(c.ID, conflict.Href))+"</C:no-uid-conflict></D:error>")
	case errors.Is(err, service.ErrNotWritable):
		needPrivileges(w, r)
	case errors.As(err, &refusal):
		refuse(w, refusal)
	case errors.Is(err, service.ErrNoSuchObject), errors.Is(err, service.ErrNoSuchCalendar):
		empty(w, http.StatusNotFound)
	default:
		s.unavailable(w, r, err)
	}
}

// needPrivileges answers a write to a read-only calendar: 403 with the
// privilege the request lacks (RFC 3744 need-privileges): write-content on
// the object for a PUT, unbind on the calendar for a DELETE.
func needPrivileges(w http.ResponseWriter, r *http.Request) {
	href, privilege := r.URL.EscapedPath(), "<D:write-content/>"
	if r.Method == http.MethodDelete {
		href, privilege = path.Dir(strings.TrimSuffix(href, "/"))+"/", "<D:unbind/>"
	}
	forbidden(w, `<D:error xmlns:D="DAV:"><D:need-privileges><D:resource>`+hrefXML(href)+
		"<D:privilege>"+privilege+"</D:privilege></D:resource></D:need-privileges></D:error>")
}

// pcbNamespace holds the bridge's own preconditions.
const pcbNamespace = "urn:pcb"

// refuse answers a change the bridge refuses: 403 with its pcb precondition
// (the code without its "pcb:" prefix, in the urn:pcb namespace) and its
// reason, which is ours, short and fixed (service.WriteRefusal).
func refuse(w http.ResponseWriter, e *service.WriteRefusal) {
	local, ok := strings.CutPrefix(e.Code, "pcb:")
	if !ok || !ncName(local) {
		local = "refused"
	}
	forbidden(w, `<D:error xmlns:D="DAV:"><pcb:`+local+` xmlns:pcb="`+pcbNamespace+`"/><pcb:reason xmlns:pcb="`+pcbNamespace+`">`+
		text(e.Reason)+"</pcb:reason></D:error>")
}
