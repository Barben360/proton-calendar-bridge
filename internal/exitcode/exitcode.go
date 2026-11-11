// Package exitcode defines the process exit codes of pcb (SPEC §13).
// Agents and scripts branch on them, so the values never change.
package exitcode

import (
	"errors"
	"strconv"
)

// Code is a process exit code.
type Code int

// The exit codes.
const (
	OK         Code = 0  // done
	Usage      Code = 2  // bad invocation: unknown command, flag or argument
	MustAct    Code = 10 // the user must answer, type a secret or run something
	Blocked    Code = 20 // the machine or the environment prevents the operation
	RolledBack Code = 30 // failed and rolled back
	Partial    Code = 40 // partly done and resumable
)

// Error is a command failure with its exit code and an optional hint
// telling the user what to do next.
type Error struct {
	Code Code
	Err  error
	Hint string
}

func (e *Error) Error() string {
	if e.Err == nil {
		return "failed with exit code " + strconv.Itoa(int(e.Code))
	}
	return e.Err.Error()
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// From returns the exit code for err: OK for nil, the code of the first
// *Error in its chain, else Blocked.
func From(err error) Code {
	if err == nil {
		return OK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Blocked
}

// HintOf returns the hint of the first *Error in err's chain, or "".
func HintOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Hint
	}
	return ""
}
