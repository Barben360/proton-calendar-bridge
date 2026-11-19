// Package report keeps pcb's crash and error reports: it reads a crash's
// traceback, builds the Sentry event that may be sent (an allow list: only
// the fields written here exist), keeps it on disk until the user says yes
// or no, and holds that answer.
package report

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Meta is what a report says of the process: the binary (pcb, pcb-ui), the
// command path (never its arguments), the install kind and pcb's version.
// The OS, the architecture and the Go version come from runtime.
type Meta struct{ Binary, Command, Install, Version string }

// Event is one report, the exact JSON that is shown and sent
// (spec/designs/2026-11-19-crash-reports-design.md §5.4). Nothing else is
// ever set: no host name, user, request, breadcrumb, module list, OS
// version or environment variable.
type Event struct {
	EventID     string    `json:"event_id"`
	Timestamp   string    `json:"timestamp"`
	Platform    string    `json:"platform"`
	Level       string    `json:"level"`
	Release     string    `json:"release"`
	Environment string    `json:"environment"`
	Tags        tags      `json:"tags"`
	Grouping    []string  `json:"fingerprint"`
	Contexts    contexts  `json:"contexts"`
	Exception   exception `json:"exception"`
	Extra       extra     `json:"extra"`
}

type tags struct {
	Binary  string `json:"binary"`
	Command string `json:"command"`
	Install string `json:"install"`
	Source  string `json:"source"`
}

type contexts struct {
	OS      osContext      `json:"os"`
	Runtime runtimeContext `json:"runtime"`
	Device  deviceContext  `json:"device"`
}

type osContext struct {
	Name string `json:"name"`
}

type runtimeContext struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type deviceContext struct {
	Arch string `json:"arch"`
}

type exception struct {
	Values []exceptionValue `json:"values"`
}

type exceptionValue struct {
	Type       string     `json:"type"`
	Value      string     `json:"value"`
	Mechanism  mechanism  `json:"mechanism"`
	Stacktrace stacktrace `json:"stacktrace"`
}

type mechanism struct {
	Type    string `json:"type"`
	Handled bool   `json:"handled"`
}

type stacktrace struct {
	Frames []Frame `json:"frames"`
}

type extra struct {
	Count int `json:"count"` // occurrences folded into this report, itself included
}

// Fingerprint is the event's grouping key, in Bugsink and for folding
// repeats: [mechanism, exception type, the top five in-app functions].
func (e Event) Fingerprint() []string { return slices.Clone(e.Grouping) }

// ID is the event's id, 32 hex digits.
func (e Event) ID() string { return e.EventID }

const (
	// The mechanisms of the three kinds of report.
	mechCrash = "go.crash"
	mechError = "go.error"
	mechHTTP  = "go.http"

	// maxChain is how many types of an error's chain a report keeps.
	maxChain = 8
	// fingerprintFrames is how many in-app functions the fingerprint holds.
	fingerprintFrames = 5
)

// crashKinds are the only exception types a crash report sends.
var crashKinds = regexp.MustCompile(`^(panic|runtime error|fatal error|signal SIG[A-Z0-9]+)$`)

// CrashEvent is the report of a fatal crash, read by ParseCrash. Its
// message is kept only when it is one of the runtime's own, whoever built
// c.
func CrashEvent(c Crash, m Meta, now time.Time) Event {
	kind := c.Kind
	if !crashKinds.MatchString(kind) {
		kind = "panic" // a hand-built Kind is no way to send text
	}
	msg := keptOrRemoved(c.Message)
	return newEvent("fatal", "crash", exceptionValue{Type: kind, Value: msg, Mechanism: mechanism{Type: mechCrash}}, c.Frames, m, now)
}

// ErrorEvent is the report of an error that points to a bug: its type is
// the error's Go type, its value the chain of types it wraps ("*fs.PathError
// < syscall.Errno"), never its text; frames is where it was seen.
func ErrorEvent(err error, frames []Frame, m Meta, now time.Time) Event {
	ex := exceptionValue{
		Type:      fmt.Sprintf("%T", err),
		Value:     strings.Join(typeChain(err), " < "),
		Mechanism: mechanism{Type: mechError, Handled: true},
	}
	return newEvent("error", "error", ex, frames, m, now)
}

// PanicEvent is the report of a panic an HTTP handler raised: its type is
// the panic value's Go type, or "runtime error" for a runtime.Error, whose
// message is kept only when it is one of the runtime's own; frames is the
// panicking goroutine's stack.
func PanicEvent(v any, frames []Frame, m Meta, now time.Time) Event {
	ex := exceptionValue{Type: fmt.Sprintf("%T", v), Value: removed, Mechanism: mechanism{Type: mechHTTP}}
	if re, ok := v.(runtime.Error); ok {
		ex.Type = "runtime error"
		if msg, ok := panicMessage(re.Error()); ok {
			ex.Value = msg
		}
	}
	return newEvent("error", "http-panic", ex, frames, m, now)
}

func newEvent(level, source string, ex exceptionValue, frames []Frame, m Meta, now time.Time) Event {
	kept := make([]Frame, len(frames))
	for i, f := range frames {
		f.Filename = shortPath(f.Filename) // a caller's frames obey the path rule too
		kept[i] = f
	}
	ex.Stacktrace.Frames = kept
	return Event{
		EventID:     newID(),
		Timestamp:   now.UTC().Format(time.RFC3339),
		Platform:    "go",
		Level:       level,
		Release:     "pcb@" + m.Version,
		Environment: "production",
		Tags:        tags{Binary: m.Binary, Command: m.Command, Install: m.Install, Source: source},
		Grouping:    fingerprint(ex.Mechanism.Type, ex.Type, kept),
		Contexts: contexts{
			OS:      osContext{Name: runtime.GOOS},
			Runtime: runtimeContext{Name: "go", Version: runtime.Version()},
			Device:  deviceContext{Arch: runtime.GOARCH},
		},
		Exception: exception{Values: []exceptionValue{ex}},
		Extra:     extra{Count: 1},
	}
}

// fingerprint is [mechanism, type, functions of the innermost five in-app
// frames], in frame order and without the module's prefix.
func fingerprint(mech, typ string, frames []Frame) []string {
	var fns []string
	for i := len(frames) - 1; i >= 0 && len(fns) < fingerprintFrames; i-- {
		if frames[i].InApp {
			fns = append(fns, strings.TrimPrefix(frames[i].Function, ModulePath+"/"))
		}
	}
	slices.Reverse(fns)
	return append([]string{mech, typ}, fns...)
}

// typeChain lists the Go type of err and of each error it wraps, depth
// first (Unwrap() error and Unwrap() []error), at most maxChain of them.
func typeChain(err error) []string {
	var chain []string
	var walk func(error)
	walk = func(e error) {
		if e == nil || len(chain) >= maxChain {
			return
		}
		chain = append(chain, fmt.Sprintf("%T", e))
		switch u := e.(type) { //nolint:errorlint // walking the chain by hand
		case interface{ Unwrap() []error }:
			for _, sub := range u.Unwrap() {
				walk(sub)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	if len(chain) == 0 {
		chain = []string{fmt.Sprintf("%T", err)}
	}
	return chain
}

// marshal is v's JSON without HTML escapes, so that a type chain reads
// "a < b" in the file the user is shown, not "a \u003c b".
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// newID is a random version 4 UUID in 32 hex digits, as Sentry wants.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}
