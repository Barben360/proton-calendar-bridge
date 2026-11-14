package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
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
		{"not ours: text dropped", errors.New("password=hunter2"),
			toolError{Class: "retry", Message: "the call failed", Hint: "retry; if it fails again, see the pcb log"}},
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
