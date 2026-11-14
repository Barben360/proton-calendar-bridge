// Package output writes the JSON envelope every pcb command prints with
// --json (SPEC §13).
package output

import (
	"encoding/json"
	"io"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// SchemaVersion of the envelope; bumped on any breaking change.
const SchemaVersion = 2

// Envelope is the one JSON object a command prints: Data on success, Error
// on failure.
type Envelope struct {
	SchemaVersion int        `json:"schema_version"`
	Command       string     `json:"command"`
	OK            bool       `json:"ok"`
	Data          any        `json:"data,omitempty"`
	Error         *ErrorBody `json:"error,omitempty"`
}

// ErrorBody describes a failure: its exit code, message and next step.
type ErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Write prints one envelope for command: data when err is nil, else err.
func Write(w io.Writer, command string, data any, err error) error {
	env := Envelope{SchemaVersion: SchemaVersion, Command: command, OK: err == nil, Data: data}
	if err != nil {
		env.Error = &ErrorBody{Code: int(exitcode.From(err)), Message: err.Error(), Hint: exitcode.HintOf(err)}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}
