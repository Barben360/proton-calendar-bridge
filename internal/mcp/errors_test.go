package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestErrorClasses(t *testing.T) {
	limited := errors.New("rate limited")
	old := retryAfterOf
	retryAfterOf = func(err error) time.Duration {
		if errors.Is(err, limited) {
			return 2500 * time.Millisecond
		}
		return 0
	}
	t.Cleanup(func() { retryAfterOf = old })

	for _, tc := range []struct {
		name string
		err  error
		want toolError
	}{
		{"must act", &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the event cannot be read"), Hint: "open it in the Proton web app"},
			toolError{Class: "must-act", Message: "the event cannot be read", Hint: "open it in the Proton web app"}},
		{"retry with seconds", &exitcode.Error{Code: exitcode.Blocked, Err: limited, Hint: "wait"},
			toolError{Class: "retry", Message: "rate limited", Hint: "wait", RetryAfterS: 3}},
		{"blocked", &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrCalendarPending, Hint: "wait for the sync"},
			toolError{Class: "retry", Message: service.ErrCalendarPending.Error(), Hint: "wait for the sync"}},
		{"stale etag", &exitcode.Error{Code: exitcode.Usage, Err: service.ErrPreconditionFailed, Hint: "read again"},
			toolError{Class: "conflict", Message: service.ErrPreconditionFailed.Error(), Hint: "read again"}},
		{"usage", &exitcode.Error{Code: exitcode.Usage, Err: errors.New("the range ends before it starts"), Hint: "give an end after the start"},
			toolError{Class: "refused", Message: "the range ends before it starts", Hint: "give an end after the start"}},
		{"write refusal", &exitcode.Error{Code: exitcode.MustAct, Err: &service.WriteRefusal{Code: "pcb:series-has-exceptions", Reason: "2 exceptions"}, Hint: "make it in the web app"},
			toolError{Class: "refused", Message: "write refused (pcb:series-has-exceptions): 2 exceptions", Hint: "make it in the web app", Reason: "pcb:series-has-exceptions"}},
		{"no account", &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("%w %s", service.ErrNoAccount, "x"), Hint: "h"},
			toolError{Class: "not-found", Message: "no account x", Hint: "h"}},
		{"no calendar", &exitcode.Error{Code: exitcode.Usage, Err: service.ErrNoSuchCalendar, Hint: "h"},
			toolError{Class: "not-found", Message: service.ErrNoSuchCalendar.Error(), Hint: "h"}},
		{"no object", &exitcode.Error{Code: exitcode.Usage, Err: service.ErrNoSuchObject, Hint: "h"},
			toolError{Class: "not-found", Message: service.ErrNoSuchObject.Error(), Hint: "h"}},
		{"rolled back", &exitcode.Error{Code: exitcode.RolledBack, Err: errors.New("undone"), Hint: "h"},
			toolError{Class: "retry", Message: "undone", Hint: "h"}},
		{"not ours: text dropped, and no retry to loop on", errors.New("password=hunter2"),
			toolError{Class: "must-act", Message: "the call failed", Hint: "do not retry: the failure is unexplained and no retry is known to fix it; a person can read the pcb log"}},
		{"a time-out is worth one retry", fmt.Errorf("agenda: %w", context.DeadlineExceeded),
			toolError{Class: "retry", Message: "the call did not finish in time", Hint: "retry once; if it fails again, see the pcb log"}},
		{"blocked until a person completes the verification", &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrHumanVerification, Hint: "open the web app"},
			toolError{Class: "must-act", Message: service.ErrHumanVerification.Error(), Hint: "open the web app"}},
		{"blocked on an event stored by another client", &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrUIDStoredOtherwise, Hint: "edit it in the web app"},
			toolError{Class: "must-act", Message: service.ErrUIDStoredOtherwise.Error(), Hint: "edit it in the web app"}},
		{"blocked while pcb serve is not serving", &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrNotServing, Hint: "run pcb serve"},
			toolError{Class: "must-act", Message: service.ErrNotServing.Error(), Hint: "run pcb serve"}},
		{"a write not sent in time is retried", &exitcode.Error{Code: exitcode.Blocked, Err: service.ErrWriteNotSent, Hint: "retry the change"},
			toolError{Class: "retry", Message: service.ErrWriteNotSent.Error(), Hint: "retry the change"}},
		{"already a tool error", notFound("no account y", "z"), toolError{Class: "not-found", Message: "no account y", Hint: "z"}},
	} {
		got := toToolError(tc.err)
		if *got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, *got, tc.want)
		}
		var env struct {
			Error toolError `json:"error"`
		}
		if err := json.Unmarshal([]byte(got.Error()), &env); err != nil || env.Error != tc.want {
			t.Errorf("%s: text %q does not carry the error (%v)", tc.name, got.Error(), err)
		}
	}
	// The five classes, exactly.
	seen := map[string]bool{}
	for _, c := range []string{classMustAct, classRetry, classRefused, classConflict, classNotFound} {
		seen[c] = true
	}
	if len(seen) != 5 || !seen["must-act"] || !seen["retry"] || !seen["refused"] || !seen["conflict"] || !seen["not-found"] {
		t.Errorf("classes %v", seen)
	}
}

// A relogin the login guard refused as too soon is a retry whose
// retry_after_s is the guard's wait, through the real retryAfterOf (the
// service's), not a stub and not a default.
func TestToolErrorCarriesTheLoginGuardsWait(t *testing.T) {
	err := &exitcode.Error{Code: exitcode.Blocked, Err: &loginguard.TooSoon{Wait: 137 * time.Second}, Hint: "retry in 2m17s"}
	got := toToolError(err)
	if got.Class != classRetry || got.RetryAfterS != 137 {
		t.Errorf("a login refused as too soon: %+v, want a retry after 137 s", *got)
	}
}

// Only the SDK's refusal of the arguments is rewritten as one; any other
// error result keeps its own text.
func TestRefusedInvalidRewritesOnlyArgumentFailures(t *testing.T) {
	result := func(err error) sdkmcp.MethodHandler {
		return func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) {
			var r sdkmcp.CallToolResult
			r.SetError(err)
			return &r, nil
		}
	}
	run := func(err error) *toolError {
		res, _ := refusedInvalid(result(err))(context.Background(), "tools/call", nil)
		var te *toolError
		if !errors.As(res.(*sdkmcp.CallToolResult).GetError(), &te) {
			return nil
		}
		return te
	}
	te := run(errors.New(`validating "arguments": validating root: required: missing properties: ["account"]`))
	if te == nil || te.Class != "refused" || !strings.Contains(te.Message, "account") || te.Hint == "" {
		t.Errorf("argument failure became %+v", te)
	}
	te = run(errors.New("validating tool output: boom"))
	if te == nil || te.Class != "must-act" || strings.Contains(te.Message, "arguments") {
		t.Errorf("an output failure became %+v, want a generic failure", te)
	}
}
