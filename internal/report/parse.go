package report

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ModulePath is pcb's Go module: a function under it is pcb's own code.
const ModulePath = "github.com/Barben360/proton-calendar-bridge"

// removed stands for a text pcb does not send: a panic's or an error's,
// which may hold anything the code put in it.
const removed = "(text removed)"

// Frame is one call of a stack, as a report holds it: the function, its
// file relative to the module or GOROOT (or the last two elements of a
// path that is neither), the line, and whether it is pcb's code. Module,
// the function's package, is not sent.
type Frame struct {
	Function string `json:"function"`
	Module   string `json:"-"`
	Filename string `json:"filename"`
	Lineno   int    `json:"lineno"`
	InApp    bool   `json:"in_app"`
}

// Crash is what a fatal crash's traceback says: its kind ("panic",
// "runtime error", "fatal error", "signal SIGSEGV"…), the runtime's line
// when it is one (else "(text removed)") and the crashing goroutine's
// frames, outermost first.
type Crash struct {
	Kind, Message string
	Frames        []Frame
}

var errNotTraceback = errors.New("not a Go traceback")

var (
	goroutineHeader = regexp.MustCompile(`^goroutine \d+ .*\]:$`)
	signalLine      = regexp.MustCompile(`^(SIG[A-Z0-9]+): `)
	signalName      = regexp.MustCompile(`^\[signal (SIG[A-Z0-9]+)`)
	fileLine        = regexp.MustCompile(`^(.*):(\d+)(?: .*)?$`)
)

// ParseCrash reads the text Go prints when a process dies (a panic nobody
// recovered, a fatal error): the first runtime line before the first
// goroutine block gives the kind, and the first goroutine block (the
// crashing one) the frames. Argument words are dropped. A text with no
// such line is an error.
func ParseCrash(text []byte) (Crash, error) {
	lines := splitLines(text)
	var c Crash
	found := false
	i := 0
	for ; i < len(lines) && !goroutineHeader.MatchString(lines[i]); i++ {
		if !found {
			c.Kind, c.Message, found = kindOf(lines[i])
		}
	}
	if !found {
		return Crash{}, errNotTraceback
	}
	if i < len(lines) {
		c.Frames = parseFrames(lines[i+1:])
	}
	return c, nil
}

// ParseStack reads a goroutine's stack as runtime/debug.Stack prints it,
// by the same rules as ParseCrash.
func ParseStack(stack []byte) []Frame {
	lines := splitLines(stack)
	for i, l := range lines {
		if goroutineHeader.MatchString(l) {
			return parseFrames(lines[i+1:])
		}
	}
	return parseFrames(lines)
}

func splitLines(b []byte) []string {
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// kindOf reads one line before the goroutines. Only the runtime's own lines
// keep their text; a panic's text, which may span lines (Go indents the
// next ones), never does.
func kindOf(line string) (kind, message string, ok bool) {
	switch {
	case strings.HasPrefix(line, "panic: runtime error: "):
		return "runtime error", strings.TrimPrefix(line, "panic: "), true
	case strings.HasPrefix(line, "panic: "):
		return "panic", removed, true
	case strings.HasPrefix(line, "fatal error: "):
		return "fatal error", line, true
	}
	if m := signalName.FindStringSubmatch(line); m != nil {
		return "signal " + m[1], line, true
	}
	if m := signalLine.FindStringSubmatch(line); m != nil {
		return "signal " + m[1], removed, true
	}
	return "", "", false
}

// runtimeLine reports whether msg is a line of the runtime's own, as
// kindOf keeps them.
func runtimeLine(msg string) bool {
	for _, p := range []string{"runtime error: ", "fatal error: ", "[signal "} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// parseFrames reads one goroutine block, up to its blank line: a function
// line, then its tab-indented file line. Go prints the innermost call
// first; frames are returned outermost first.
func parseFrames(lines []string) []Frame {
	var frames []Frame
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if l == "" || goroutineHeader.MatchString(l) {
			break
		}
		if strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "...") {
			continue // a file line without its function, or "...frames elided..."
		}
		fn := functionName(l)
		var file string
		var line int
		if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\t") {
			i++
			file, line = fileAndLine(strings.TrimPrefix(lines[i], "\t"))
		}
		frames = append(frames, newFrame(fn, file, line))
	}
	slices.Reverse(frames)
	return frames
}

// functionName drops a function line's arguments ("(0xc000012345, …)") and
// a goroutine creator's wording ("created by f in goroutine 1").
func functionName(l string) string {
	if rest, ok := strings.CutPrefix(l, "created by "); ok {
		if i := strings.Index(rest, " in goroutine "); i >= 0 {
			rest = rest[:i]
		}
		return rest
	}
	if strings.HasSuffix(l, ")") {
		if i := strings.LastIndex(l, "("); i > 0 {
			return l[:i]
		}
	}
	return l
}

// fileAndLine reads "path/file.go:12 +0x1d" (and any fp=/sp=/pc= words).
func fileAndLine(s string) (string, int) {
	m := fileLine.FindStringSubmatch(s)
	if m == nil {
		return s, 0
	}
	n, _ := strconv.Atoi(m[2])
	return m[1], n
}

// newFrame applies the report's rules to one call: the file made relative
// (shortPath), the package read from the function, in_app for pcb's code.
func newFrame(function, file string, line int) Frame {
	pkg := packageOf(function)
	return Frame{Function: function, Module: pkg, Filename: shortPath(file), Lineno: line, InApp: inApp(pkg, file)}
}

// packageOf is the import path of a function's package: up to the first dot
// after the last slash ("a/b.(*T).M" → "a/b"); "" when there is none.
func packageOf(fn string) string {
	slash := strings.LastIndex(fn, "/")
	dot := strings.Index(fn[slash+1:], ".")
	if dot < 0 {
		return ""
	}
	return fn[:slash+1+dot]
}

// inApp reports whether a call is pcb's own code: a package of the module,
// or a main package whose file is in the module (-trimpath builds), but
// not this package, whose frames are the report's own making.
func inApp(pkg, file string) bool {
	switch {
	case pkg == ModulePath+"/internal/report":
		return false
	case pkg == ModulePath || strings.HasPrefix(pkg, ModulePath+"/"):
		return true
	default:
		return pkg == "main" && strings.HasPrefix(file, ModulePath+"/")
	}
}

// shortPath keeps a relative path (what -trimpath records: module or
// GOROOT relative), without the module's prefix; an absolute path, which
// names the user's machine, is cut to its last two elements.
func shortPath(p string) string {
	if !isAbs(p) {
		return strings.TrimPrefix(p, ModulePath+"/")
	}
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

func isAbs(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || len(p) >= 2 && p[1] == ':'
}
