package mcp

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The error classes of a tool error, exactly these (design §5.7).
const (
	classMustAct  = "must-act"  // a person must do something (log in, open the web app)
	classRetry    = "retry"     // transient: try again, after retry_after_s when given
	classRefused  = "refused"   // the call is not acceptable as it is
	classConflict = "conflict"  // the event changed since it was read: read it again
	classNotFound = "not-found" // no such account, calendar or event (or not visible)
)

// toolError is a tool's error as the agent reads it: the text of the
// result is {"error": {...}}. Message and Hint come from an
// *exitcode.Error, secret-free by its rule; Reason is a refusal's code.
type toolError struct {
	Class       string `json:"class"`
	Message     string `json:"message"`
	Hint        string `json:"hint,omitempty"`
	Reason      string `json:"reason,omitempty"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
}

func (e *toolError) Error() string {
	b, _ := json.Marshal(struct {
		Error *toolError `json:"error"`
	}{e})
	return string(b)
}

func notFound(msg, hint string) *toolError {
	return &toolError{Class: classNotFound, Message: msg, Hint: hint}
}

func refusedErr(msg, hint string) *toolError {
	return &toolError{Class: classRefused, Message: msg, Hint: hint}
}

// retryAfterOf is how long Proton asked to wait, as err carries it (a
// variable so a test can stand for Proton's 429).
var retryAfterOf = service.ProtonRetryAfter

// toToolError classifies err: a stale ETag is a conflict; a selector that
// names nothing is not-found; a refused change is refused with its code;
// then by exit code: MustAct is must-act, Usage refused, anything else
// retry (Blocked with Proton's Retry-After in seconds). The text of an
// error that is not an *exitcode.Error is dropped: nothing vouches that it
// is secret-free.
func toToolError(err error) *toolError {
	var (
		te *toolError
		ee *exitcode.Error
		wr *service.WriteRefusal
	)
	if errors.As(err, &te) {
		return te
	}
	if !errors.As(err, &ee) {
		return &toolError{Class: classRetry, Message: "the call failed", Hint: "retry; if it fails again, see the pcb log"}
	}
	out := &toolError{Message: ee.Error(), Hint: ee.Hint}
	switch {
	case errors.Is(err, service.ErrPreconditionFailed):
		out.Class = classConflict
	case errors.Is(err, service.ErrNoAccount), errors.Is(err, service.ErrNoSuchCalendar), errors.Is(err, service.ErrNoSuchObject):
		out.Class = classNotFound
	case errors.As(err, &wr):
		out.Class, out.Reason = classRefused, wr.Code
	case ee.Code == exitcode.MustAct:
		out.Class = classMustAct
	case ee.Code == exitcode.Usage:
		out.Class = classRefused
	default:
		out.Class = classRetry
		if d := retryAfterOf(err); d > 0 {
			out.RetryAfterS = int(math.Ceil(d.Seconds()))
		}
	}
	return out
}
