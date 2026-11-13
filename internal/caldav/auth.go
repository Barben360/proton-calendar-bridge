package caldav

import (
	"net/http"
	"strconv"
	"time"
)

// challenge is the 401's WWW-Authenticate on the namespace of account id
// ("" the compatibility one): Basic, so Thunderbird needs no Digest
// OPTIONS probe (E08 §2.1); realm "pcb <local-id>" on an account's URLs,
// pcb on the compatibility ones, so a client holding two accounts on one
// origin never sends one's password on the other's URLs (design §4.4).
func challenge(id string) string {
	realm := "pcb"
	if id != "" {
		realm += " " + id
	}
	return `Basic realm="` + realm + `", charset="UTF-8"`
}

// authenticate checks the request's Basic credentials, any username, the
// password a client's app password, on the namespace of account id: the
// Router chooses the account, which must be id on its URLs. It answers the
// refusal itself: 401 with id's challenge (never 403, E08 MUST 1), 503
// with Retry-After while the remote is blocked or the account cannot
// answer (ruling M3-R26: not 429, a 4xx on a calendar's first PROPFIND
// disables it in Thunderbird, a 5xx is temporary). Authenticated, it
// returns the copy of s that serves the request.
// A request without credentials is not a failed attempt: it is how
// Thunderbird asks for the challenge, so it is answered without a check.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request, id string) (*server, bool) {
	_, password, ok := r.BasicAuth()
	if ok {
		api, localID, wait, err := s.router.Route(r.Context(), id, password, r.RemoteAddr)
		switch {
		case err != nil:
			s.unavailable(w, r, err)
			return nil, false
		case api != nil && (id == "" || localID == id):
			rs := *s
			rs.api, rs.ns, rs.own = api, accountSpace(id), accountSpace(localID)
			if localID != "" {
				rs.log = s.log.With("account", localID) // its warnings name it (ruling R30)
			}
			return &rs, true
		case api != nil:
			// Never another account's API on an account's URLs.
			s.log.Error("caldav: refused a request the router sent to another account", "account", id, "routed", localID)
		case wait > 0:
			w.Header().Set("Retry-After", retryAfter(wait))
			empty(w, http.StatusServiceUnavailable)
			return nil, false
		}
	}
	w.Header().Set("WWW-Authenticate", challenge(id))
	empty(w, http.StatusUnauthorized)
	return nil, false
}

// retryAfter is d in whole seconds, rounded up, at least 1 (ruling M3-R24).
func retryAfter(d time.Duration) string {
	secs := int64((d + time.Second - 1) / time.Second)
	return strconv.FormatInt(max(secs, 1), 10)
}
