package report_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

var (
	t0   = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	meta = report.Meta{Binary: "pcb", Command: "pcb serve", Install: "container", Version: "1.2.3"}
)

// someFrames is a stack as the parser returns it: outermost first.
func someFrames() []report.Frame {
	return []report.Frame{
		frame("runtime.goexit", "runtime/asm_amd64.s", 1700, false),
		frame(daemon+".serveLoop", "internal/daemon/serve.go", 88, true),
		frame(daemon+".handleRequest", "internal/daemon/handler.go", 142, true),
	}
}

// eventJSON is e as a pending file holds it: JSON without HTML escapes
// ("<", not "\u003c").
func eventJSON(t *testing.T, e report.Event) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// keyPaths lists every key of v as a dotted path, "[]" for an array's
// elements: the shape of a JSON document, without its values.
func keyPaths(v any, prefix string, into map[string]bool) {
	switch v := v.(type) {
	case map[string]any:
		for k, sub := range v {
			into[prefix+k] = true
			keyPaths(sub, prefix+k+".", into)
		}
	case []any:
		for _, sub := range v {
			keyPaths(sub, strings.TrimSuffix(prefix, ".")+"[].", into)
		}
	}
}

func shape(t *testing.T, b []byte) []string {
	t.Helper()
	keys := map[string]bool{}
	keyPaths(decode(t, b), "", keys)
	var out []string
	for k := range keys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// The events pcb builds have exactly the keys of the design's allow list,
// as testdata/report/event.json holds them (the shape Bugsink 2.6.3 was
// shown to accept): no field more, none less.
func TestCrashEventAllowList(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("..", "..", "testdata", "report", "event.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := shape(t, golden)
	events := map[string]report.Event{
		"crash": report.CrashEvent(report.Crash{Kind: "runtime error", Message: "runtime error: x", Frames: someFrames()}, meta, t0),
		"error": report.ErrorEvent(errors.New("x"), someFrames(), meta, t0),
		"panic": report.PanicEvent("x", someFrames(), meta, t0),
	}
	for name, e := range events {
		if got := shape(t, eventJSON(t, e)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s event keys:\n got %q\nwant %q", name, got, want)
		}
	}
}

func TestEventValues(t *testing.T) {
	type values struct {
		level, mechanism, source, typ, value string
		handled                              bool
	}
	cases := map[string]struct {
		e    report.Event
		want values
	}{
		"crash": {
			report.CrashEvent(report.Crash{Kind: "fatal error", Message: "fatal error: concurrent map writes", Frames: someFrames()}, meta, t0),
			values{"fatal", "go.crash", "crash", "fatal error", "fatal error: concurrent map writes", false},
		},
		"error": {
			report.ErrorEvent(&fs.PathError{Op: "open", Path: "x", Err: syscall.ENOENT}, someFrames(), meta, t0),
			values{"error", "go.error", "error", "*fs.PathError", "*fs.PathError < syscall.Errno", true},
		},
		"panic": {
			report.PanicEvent(42, someFrames(), meta, t0),
			values{"error", "go.http", "http-panic", "int", "(text removed)", false},
		},
	}
	for name, tc := range cases {
		var got struct {
			EventID     string `json:"event_id"`
			Timestamp   string
			Platform    string
			Level       string
			Release     string
			Environment string
			Tags        map[string]string
			Fingerprint []string
			Contexts    struct {
				OS      struct{ Name string }
				Runtime struct{ Name, Version string }
				Device  struct{ Arch string }
			}
			Exception struct {
				Values []struct {
					Type, Value string
					Mechanism   struct {
						Type    string
						Handled bool
					}
					Stacktrace struct{ Frames []report.Frame }
				}
			}
			Extra struct{ Count int }
		}
		if err := json.Unmarshal(eventJSON(t, tc.e), &got); err != nil {
			t.Fatal(err)
		}
		ex := got.Exception.Values[0]
		v := values{got.Level, ex.Mechanism.Type, got.Tags["source"], ex.Type, ex.Value, ex.Mechanism.Handled}
		if v != tc.want {
			t.Errorf("%s: %+v, want %+v", name, v, tc.want)
		}
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(got.EventID) || got.EventID != tc.e.ID() {
			t.Errorf("%s: event_id %q, ID() %q", name, got.EventID, tc.e.ID())
		}
		if got.Timestamp != "2026-11-19T10:00:00Z" || got.Platform != "go" || got.Release != "pcb@1.2.3" || got.Environment != "production" {
			t.Errorf("%s: timestamp %q platform %q release %q environment %q", name, got.Timestamp, got.Platform, got.Release, got.Environment)
		}
		wantTags := map[string]string{"binary": "pcb", "command": "pcb serve", "install": "container", "source": tc.want.source}
		if !reflect.DeepEqual(got.Tags, wantTags) {
			t.Errorf("%s: tags %v", name, got.Tags)
		}
		c := got.Contexts
		if c.OS.Name != runtime.GOOS || c.Runtime.Name != "go" || c.Runtime.Version != runtime.Version() || c.Device.Arch != runtime.GOARCH {
			t.Errorf("%s: contexts %+v", name, c)
		}
		if !reflect.DeepEqual(got.Fingerprint, tc.e.Fingerprint()) || got.Extra.Count != 1 {
			t.Errorf("%s: fingerprint %q (Fingerprint() %q), count %d", name, got.Fingerprint, tc.e.Fingerprint(), got.Extra.Count)
		}
		if !reflect.DeepEqual(ex.Stacktrace.Frames, someFrames()) {
			t.Errorf("%s: frames %+v", name, ex.Stacktrace.Frames)
		}
	}
}

func TestEventIDsDiffer(t *testing.T) {
	a := report.CrashEvent(report.Crash{Kind: "panic"}, meta, t0)
	b := report.CrashEvent(report.Crash{Kind: "panic"}, meta, t0)
	if a.ID() == b.ID() {
		t.Errorf("two events share the id %s", a.ID())
	}
}

const marker = "MARKER-7f3a"

// A panic text, an argument word, a wrapped error's text and a path of the
// user's machine, each carrying a marker: none reaches the JSON.
func TestNoMarkerLeaks(t *testing.T) {
	crashText := "panic: " + marker + " boom\n\tsecond line " + marker + "\n\n" +
		"goroutine 7 [running]:\n" +
		daemon + ".handle(0xc000012345, {0x" + marker + ", 0x3})\n" +
		"\t/home/someone/" + marker + "/pcb/internal/daemon/handler.go:12 +0x1d\n" +
		"created by " + daemon + ".Serve in goroutine 1\n" +
		"\t/home/someone/" + marker + "/pcb/internal/daemon/serve.go:20 +0x30\n"
	c, err := report.ParseCrash([]byte(crashText))
	if err != nil {
		t.Fatal(err)
	}
	stack := []byte("goroutine 1 [running]:\n" + daemon + ".f({0x" + marker + "})\n\t/home/someone/" + marker + "/a/b.go:1 +0x1\n")
	wrapped := fmt.Errorf("sync %s: %w", marker, &fs.PathError{Op: "open", Path: "/home/someone/" + marker, Err: errors.New(marker)})
	// Each event also holds what it keeps instead, so an empty event
	// cannot pass.
	events := map[string]struct {
		e    report.Event
		kept string
	}{
		"crash":           {report.CrashEvent(c, meta, t0), `"filename":"daemon/handler.go"`},
		"error":           {report.ErrorEvent(wrapped, report.ParseStack(stack), meta, t0), `"value":"*fmt.wrapError < *fs.PathError < *errors.errorString"`},
		"joined":          {report.ErrorEvent(errors.Join(errors.New(marker), wrapped), nil, meta, t0), `"type":"*errors.joinError"`},
		"panic string":    {report.PanicEvent(marker, report.ParseStack(stack), meta, t0), `"filename":"a/b.go"`},
		"panic error":     {report.PanicEvent(wrapped, nil, meta, t0), `"value":"(text removed)"`},
		"panic lookalike": {report.PanicEvent(errors.New("runtime error: "+marker), nil, meta, t0), `"value":"(text removed)"`},
		"crash by hand":   {report.CrashEvent(report.Crash{Kind: "panic", Message: marker}, meta, t0), `"value":"(text removed)"`},
	}
	for name, tc := range events {
		b := string(eventJSON(t, tc.e))
		if strings.Contains(b, marker) || strings.Contains(b, "/home/") {
			t.Errorf("%s: the JSON leaks a marker or a path: %s", name, b)
		}
		if !strings.Contains(b, tc.kept) {
			t.Errorf("%s: no %s in %s", name, tc.kept, b)
		}
	}
}

func TestRuntimeMessageKept(t *testing.T) {
	c, err := report.ParseCrash([]byte("panic: runtime error: index out of range [5] with length 3\n\ngoroutine 1 [running]:\nmain.main()\n\tmain.go:3 +0x1\n"))
	if err != nil {
		t.Fatal(err)
	}
	idx := make([]int, 3)
	func() {
		defer func() {
			e := report.PanicEvent(recover(), nil, meta, t0)
			b := eventJSON(t, e)
			if !strings.Contains(string(b), `"value":"runtime error: index out of range [5] with length 3"`) || !strings.Contains(string(b), `"type":"runtime error"`) {
				t.Errorf("panic event: %s", b)
			}
		}()
		_ = idx[5]
	}()
	b := eventJSON(t, report.CrashEvent(c, meta, t0))
	if !strings.Contains(string(b), `"value":"runtime error: index out of range [5] with length 3"`) {
		t.Errorf("crash event: %s", b)
	}
}

type wrapper struct{ err error }

func (w wrapper) Error() string { return "wrapper: " + w.err.Error() }
func (w wrapper) Unwrap() error { return w.err }

func exceptionValue(t *testing.T, e report.Event) (typ, value string) {
	t.Helper()
	var got struct {
		Exception struct {
			Values []struct{ Type, Value string }
		}
	}
	if err := json.Unmarshal(eventJSON(t, e), &got); err != nil {
		t.Fatal(err)
	}
	return got.Exception.Values[0].Type, got.Exception.Values[0].Value
}

func TestErrorEventTypeChain(t *testing.T) {
	var deep error = syscall.ENOENT
	for range 10 {
		deep = wrapper{deep}
	}
	cases := []struct {
		name      string
		err       error
		typ, want string
	}{
		{"path error", fmt.Errorf("read: %w", &fs.PathError{Op: "open", Path: "p", Err: syscall.ENOENT}), "*fmt.wrapError", "*fmt.wrapError < *fs.PathError < syscall.Errno"},
		{"joined", errors.Join(&fs.PathError{Op: "open", Path: "p", Err: syscall.ENOENT}, errors.New("b")), "*errors.joinError", "*errors.joinError < *fs.PathError < syscall.Errno < *errors.errorString"},
		{"eight levels", deep, "report_test.wrapper", strings.Repeat("report_test.wrapper < ", 7) + "report_test.wrapper"},
	}
	for _, tc := range cases {
		typ, value := exceptionValue(t, report.ErrorEvent(tc.err, nil, meta, t0))
		if typ != tc.typ || value != tc.want {
			t.Errorf("%s: type %q value %q; want %q, %q", tc.name, typ, value, tc.typ, tc.want)
		}
	}
}

func TestFingerprintStable(t *testing.T) {
	base := report.CrashEvent(report.Crash{Kind: "panic", Frames: someFrames()}, meta, t0).Fingerprint()
	want := []string{"go.crash", "panic", "internal/daemon.serveLoop", "internal/daemon.handleRequest"}
	if !reflect.DeepEqual(base, want) {
		t.Errorf("fingerprint %q, want %q", base, want)
	}

	moved := someFrames()
	for i := range moved {
		moved[i].Lineno += 7
	}
	if got := report.CrashEvent(report.Crash{Kind: "panic", Frames: moved}, meta, t0.Add(time.Hour)).Fingerprint(); !reflect.DeepEqual(got, base) {
		t.Errorf("other lines and time: %q, want %q", got, base)
	}

	other := someFrames()
	other[2].Function = daemon + ".handleOther"
	if got := report.CrashEvent(report.Crash{Kind: "panic", Frames: other}, meta, t0).Fingerprint(); reflect.DeepEqual(got, base) {
		t.Errorf("another crashing function kept the fingerprint %q", got)
	}

	// Only the top five in-app frames count.
	var long []report.Frame
	for i := range 8 {
		long = append(long, frame(fmt.Sprintf("%s.f%d", daemon, i), "internal/daemon/f.go", i, true))
	}
	fp := report.CrashEvent(report.Crash{Kind: "panic", Frames: long}, meta, t0).Fingerprint()
	long[0].Function = daemon + ".elsewhere"
	if got := report.CrashEvent(report.Crash{Kind: "panic", Frames: long}, meta, t0).Fingerprint(); !reflect.DeepEqual(got, fp) || len(fp) != 7 {
		t.Errorf("outer frames changed the fingerprint: %q vs %q", got, fp)
	}
}
