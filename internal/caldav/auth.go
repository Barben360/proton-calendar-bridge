package caldav

import (
	"net/http"
	"strconv"
	"time"
)

// challenge is the 401's WWW-Authenticate: Basic, so Thunderbird needs no
// Digest OPTIONS probe (E08 §2.1).
const challenge = `Basic realm="pcb", charset="UTF-8"`

// authenticate checks the request's Basic credentials, any username, the
// password a client's app password. It answers the refusal itself: 401 with
// the challenge (never 403, E08 MUST 1), 503 with Retry-After while the
// remote is blocked (ruling M3-R26: not 429, a 4xx on a calendar's first
// PROPFIND disables it in Thunderbird, a 5xx is temporary).
// A request without credentials is not a failed attempt: it is how
// Thunderbird asks for the challenge, so it is answered without a check.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	_, password, ok := r.BasicAuth()
	if ok {
		allowed, wait := s.api.CheckClient(r.Context(), password, r.RemoteAddr)
		if allowed {
			return true
		}
		if wait > 0 {
			w.Header().Set("Retry-After", retryAfter(wait))
			empty(w, http.StatusServiceUnavailable)
			return false
		}
	}
	w.Header().Set("WWW-Authenticate", challenge)
	empty(w, http.StatusUnauthorized)
	return false
}

// retryAfter is d in whole seconds, rounded up, at least 1 (ruling M3-R24).
func retryAfter(d time.Duration) string {
	secs := int64((d + time.Second - 1) / time.Second)
	return strconv.FormatInt(max(secs, 1), 10)
}
