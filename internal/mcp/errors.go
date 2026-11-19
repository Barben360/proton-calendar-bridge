package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

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

// refusedInvalid is the middleware that gives the SDK's refusal of a call's
// arguments (missing or mistyped, or outside the input schema's values and
// bounds: it answers before a handler runs, in plain text) the envelope of
// every other tool error: class refused, the message and a hint.
func refusedInvalid(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
		res, err := next(ctx, method, req)
		r, ok := res.(*sdkmcp.CallToolResult)
		if method != "tools/call" || err != nil || !ok || r == nil || !r.IsError {
			return res, err
		}
		var te *toolError
		cause := r.GetError()
		if cause == nil || errors.As(cause, &te) {
			return res, err
		}
		detail, isArgs := strings.CutPrefix(cause.Error(), `validating "arguments": `)
		if !isArgs { // not the SDK's refusal of the arguments: its text may carry anything
			var out sdkmcp.CallToolResult
			out.SetError(toToolError(cause))
			return &out, nil
		}
		te = refusedErr("the arguments do not fit the tool: "+cutUTF8(detail, 300),
			"tools/list gives the tool's input schema (required arguments, types, allowed values and bounds): fix the arguments and call again")
		var out sdkmcp.CallToolResult
		out.SetError(te)
		return &out, nil
	}
}

// retryAfterOf is how long Proton asked to wait, as err carries it (a
// variable so a test can stand for Proton's 429).
var retryAfterOf = service.ProtonRetryAfter

// toToolError classifies err: a stale ETag is a conflict; a selector that
// names nothing is not-found; a refused change is refused with its code;
// then by exit code: MustAct is must-act, Usage refused; a Blocked error
// no retry fixes (service.NeedsPerson) is must-act, any other retry (with
// Proton's Retry-After in seconds). An error that is not an
// *exitcode.Error is retry only when it is a time-out or a cancelled
// context; else it is must-act, lest an agent loop on a failure that never
// clears: the person reads the pcb log. Its text is dropped: nothing
// vouches that it is secret-free.
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
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return &toolError{Class: classRetry, Message: "the call did not finish in time", Hint: "retry once; if it fails again, see the pcb log"}
		}
		return &toolError{Class: classMustAct, Message: "the call failed", Hint: "do not retry: the failure is unexplained and no retry is known to fix it; a person can read the pcb log"}
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
	case service.NeedsPerson(err):
		out.Class = classMustAct
	default:
		out.Class = classRetry
		if d := retryAfterOf(err); d > 0 {
			out.RetryAfterS = int(math.Ceil(d.Seconds()))
		}
	}
	return out
}
