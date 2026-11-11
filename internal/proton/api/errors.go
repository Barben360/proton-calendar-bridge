// Package api is the single authenticated JSON transport for every logged-in
// Proton call: identity headers, one in-process token refresh, rate limiting
// and a small, explicit retry policy.
package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// Proton response codes the bridge reacts to.
const (
	CodeOK                = 1000
	CodeMulti             = 1001
	CodeNotFound          = 2501
	CodeHumanVerification = 9001
	CodeInsufficientScope = 9101
	CodeRefreshInvalid    = 10013
)

// ErrSessionEnded means the tokens can no longer authenticate: the user must
// log in again. It is wrapped, so test it with errors.Is.
var ErrSessionEnded = errors.New("proton session ended")

// Error is a Proton API failure. It holds nothing from the request.
type Error struct {
	Method, Path string // for diagnostics; never the query or the body
	Status, Code int
	Message      string
	RetryAfter   time.Duration // from the Retry-After header, 0 when absent
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("Proton: %s (code %d, HTTP %d)", e.Message, e.Code, e.Status)
}

// IsCode reports whether err is (or wraps) an *Error with the given Proton code.
func IsCode(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// SessionWide reports whether err stops a whole multi-calendar operation
// rather than one calendar: the session ended, the caller gave up, the caller
// already decided (an *exitcode.Error, e.g. a store failure during a
// refresh), or Proton is unreachable, unavailable or rate limiting. Only a
// deterministic failure of one calendar (another 4xx, keys that do not
// unlock) is reported on it.
func SessionWide(ctx context.Context, err error) bool {
	var (
		ee *exitcode.Error
		ae *Error
		ne net.Error
		ue *url.Error
	)
	switch {
	case ctx.Err() != nil, errors.Is(err, ErrSessionEnded), errors.As(err, &ee):
		return true
	case errors.As(err, &ae):
		return ae.Status == http.StatusTooManyRequests || ae.Status >= 500
	}
	return errors.As(err, &ne) || errors.As(err, &ue)
}
