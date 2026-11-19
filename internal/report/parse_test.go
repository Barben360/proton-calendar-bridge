package report_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

const (
	mod     = report.ModulePath
	daemon  = mod + "/internal/daemon"
	helpers = mod + "/internal/report_test"
)

// inApp lists the functions of c's in-app frames, outermost first.
func inApp(frames []report.Frame) []string {
	var fns []string
	for _, f := range frames {
		if f.InApp {
			fns = append(fns, f.Function)
		}
	}
	return fns
}

func frame(fn, file string, line int, app bool) report.Frame {
	return report.Frame{Function: fn, Filename: file, Lineno: line, InApp: app}
}

// withoutModule drops Module, which never leaves the machine, so the
// expected frames below say only what the JSON holds.
func withoutModule(frames []report.Frame) []report.Frame {
	out := make([]report.Frame, len(frames))
	for i, f := range frames {
		f.Module = ""
		out[i] = f
	}
	return out
}

// The golden tracebacks were printed by real panics of a -trimpath build
// (how releases are built) of a program under pcb's module path: paths are
// module- or GOROOT-relative.
func TestParseCrashGolden(t *testing.T) {
	cases := []struct {
		file          string
		kind, message string
		frames        []report.Frame
	}{
		{
			file: "goroutine-panic.txt", kind: "panic", message: "(text removed)",
			frames: []report.Frame{
				frame(daemon+".(*Server).Serve", "internal/daemon/d.go", 13, true),
				frame(daemon+".(*Server).handle", "internal/daemon/d.go", 19, true),
			},
		},
		{
			file: "runtime-error.txt", kind: "runtime error", message: "runtime error: index out of range [3] with length 0",
			frames: []report.Frame{
				frame("main.main", "cmd/pcb/main.go", 14, true),
				frame(daemon+".(*Server).Index", "internal/daemon/d.go", 24, true),
				frame(daemon+".first[...]", "internal/daemon/d.go", 22, true),
			},
		},
		{
			file: "nil-deref.txt", kind: "runtime error", message: "runtime error: invalid memory address or nil pointer dereference",
			frames: []report.Frame{
				frame("main.main", "cmd/pcb/main.go", 16, true),
				frame(daemon+".Walk", "internal/daemon/d.go", 30, true),
				frame(daemon+".(*node).depth", "internal/daemon/d.go", 28, true),
			},
		},
		{
			// GOTRACEBACK=all: six goroutine blocks, the first one kept.
			file: "map-fatal.txt", kind: "fatal error", message: "fatal error: concurrent map writes",
			frames: []report.Frame{
				frame(daemon+".Race", "internal/daemon/d.go", 38, true),
				frame(daemon+".Race.func1", "internal/daemon/d.go", 40, true),
				frame("internal/runtime/maps.fatal", "runtime/panic.go", 1197, false),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join("..", "..", "testdata", "report", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			c, err := report.ParseCrash(text)
			if err != nil {
				t.Fatal(err)
			}
			if c.Kind != tc.kind || c.Message != tc.message {
				t.Errorf("Kind, Message = %q, %q; want %q, %q", c.Kind, c.Message, tc.kind, tc.message)
			}
			if got := withoutModule(c.Frames); !reflect.DeepEqual(got, tc.frames) {
				t.Errorf("frames:\n got %+v\nwant %+v", got, tc.frames)
			}
		})
	}
}

func TestParseCrashSetsModule(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("..", "..", "testdata", "report", "map-fatal.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := report.ParseCrash(text)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{c.Frames[0].Module, c.Frames[2].Module}; !reflect.DeepEqual(got, []string{daemon, "internal/runtime/maps"}) {
		t.Errorf("modules %q", got)
	}
}

// Real panics of the test binary (no -trimpath: absolute paths).
func TestParseCrashFromRealPanics(t *testing.T) {
	t.Run("goroutine-panic", func(t *testing.T) {
		text := crashOutput(t, "goroutine-panic")
		c, err := report.ParseCrash(text)
		if err != nil {
			t.Fatalf("%v\n%s", err, text)
		}
		if c.Kind != "panic" || c.Message != "(text removed)" {
			t.Errorf("Kind, Message = %q, %q", c.Kind, c.Message)
		}
		want := []string{helpers + ".helperGoroutinePanic", helpers + ".helperPanicky"}
		if got := inApp(c.Frames); !reflect.DeepEqual(got, want) {
			t.Errorf("in-app frames %q, want %q\n%s", got, want, text)
		}
		last := c.Frames[len(c.Frames)-1]
		if last.Filename != "report/main_test.go" || last.Lineno == 0 {
			t.Errorf("crashing frame %+v, want report/main_test.go and a line", last)
		}
		for _, f := range c.Frames {
			if strings.Contains(f.Function+f.Filename, "MARKER") || strings.HasPrefix(f.Filename, "/") || strings.Contains(f.Filename, ":") {
				t.Errorf("frame %+v keeps an argument or a local path", f)
			}
		}
	})
	t.Run("runtime-error", func(t *testing.T) {
		text := crashOutput(t, "runtime-error")
		c, err := report.ParseCrash(text)
		if err != nil {
			t.Fatalf("%v\n%s", err, text)
		}
		if c.Kind != "runtime error" || c.Message != "runtime error: index out of range [5] with length 3" {
			t.Errorf("Kind, Message = %q, %q", c.Kind, c.Message)
		}
		want := []string{helpers + ".TestMain", helpers + ".runHelper", helpers + ".helperRuntimeError"}
		if got := inApp(c.Frames); !reflect.DeepEqual(got, want) {
			t.Errorf("in-app frames %q, want %q\n%s", got, want, text)
		}
	})
	t.Run("map-fatal", func(t *testing.T) {
		text := crashOutput(t, "map-fatal", "GOTRACEBACK=all")
		if n := len(regexp.MustCompile(`(?m)^goroutine \d+ `).FindAll(text, -1)); n < 2 {
			t.Fatalf("%d goroutine blocks printed, want several for the check to mean anything\n%s", n, text)
		}
		c, err := report.ParseCrash(text)
		if err != nil {
			t.Fatalf("%v\n%s", err, text)
		}
		if c.Kind != "fatal error" || c.Message != "fatal error: concurrent map writes" {
			t.Errorf("Kind, Message = %q, %q", c.Kind, c.Message)
		}
		want := []string{helpers + ".helperMapFatal", helpers + ".helperMapWriter"}
		if got := inApp(c.Frames); !reflect.DeepEqual(got, want) {
			t.Errorf("in-app frames %q, want %q (one goroutine)\n%s", got, want, text)
		}
	})
}

func TestParseCrashKinds(t *testing.T) {
	cases := []struct{ first, kind, message string }{
		{"panic: MARKER-7f3a", "panic", "(text removed)"},
		{"panic: runtime error: slice bounds out of range [:9] with capacity 2", "runtime error", "runtime error: slice bounds out of range [:9] with capacity 2"},
		{"fatal error: all goroutines are asleep - deadlock!", "fatal error", "fatal error: all goroutines are asleep - deadlock!"},
		{"[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x1]", "signal SIGSEGV", "[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x1]"},
		{"SIGABRT: abort", "signal SIGABRT", "(text removed)"},
		// A multi-line panic text: Go indents its next lines, which are
		// never taken for a runtime line nor for a goroutine.
		{"panic: MARKER-7f3a\n\tfatal error: MARKER-7f3a\n\t\n\tgoroutine 9 [running]:\n\tMARKER-7f3a()\n\t\t/MARKER-7f3a/x.go:1", "panic", "(text removed)"},
		// The first runtime line wins.
		{"unexpected fault address 0x1\nfatal error: fault\n[signal SIGBUS: bus error code=0x2 addr=0x1 pc=0x2]", "fatal error", "fatal error: fault"},
	}
	for _, tc := range cases {
		text := tc.first + "\n\ngoroutine 1 [running]:\n" + mod + "/internal/cli.Execute()\n\tgithub.com/Barben360/proton-calendar-bridge/internal/cli/root.go:7 +0x1\n"
		c, err := report.ParseCrash([]byte(text))
		if err != nil {
			t.Fatalf("%q: %v", tc.first, err)
		}
		if c.Kind != tc.kind || c.Message != tc.message {
			t.Errorf("%q: Kind, Message = %q, %q; want %q, %q", tc.first, c.Kind, c.Message, tc.kind, tc.message)
		}
		if got := inApp(c.Frames); !reflect.DeepEqual(got, []string{mod + "/internal/cli.Execute"}) {
			t.Errorf("%q: in-app frames %q", tc.first, got)
		}
	}
}

func TestParseCrashRefusesOtherText(t *testing.T) {
	for _, text := range []string{"", "hello\nworld\n", "goroutine 1 [running]:\nmain.main()\n\t/x/main.go:1\n"} {
		if c, err := report.ParseCrash([]byte(text)); err == nil {
			t.Errorf("%q: parsed as %+v, want an error", text, c)
		}
	}
}

func TestParseStackPaths(t *testing.T) {
	stack := "goroutine 12 gp=0xc000102000 m=4 mp=0xc000100008 [running]:\n" +
		"runtime/debug.Stack()\n\truntime/debug/stack.go:26 +0x5e\n" +
		mod + "/internal/report.RecordPanic({0x1, 0x2})\n\tgithub.com/Barben360/proton-calendar-bridge/internal/report/process.go:40 +0x2\n" +
		daemon + ".(*Daemon).serveHTTP(0xc0000a, {0xMARKER-7f3a, 0x3})\n\t/home/someone/MARKER-7f3a/src/pcb/internal/daemon/daemon.go:88 +0x1c fp=0xc0 sp=0xc1 pc=0xc2\n" +
		"...additional frames elided...\n" +
		"github.com/spf13/cobra.(*Command).execute(0xc1)\n\tgithub.com/spf13/cobra@v1.10.1/command.go:1015 +0x2\n" +
		mod + "/internal/ui.run(...)\n\tC:/Users/MARKER-7f3a/pcb/internal/ui/run.go:5\n" +
		"created by " + mod + "/internal/daemon.Start in goroutine 1\n\t" + mod + "/internal/daemon/start.go:3 +0x9\n" +
		"\n" +
		"goroutine 13 [select]:\n" + mod + "/internal/other.Wait()\n\t" + mod + "/internal/other/w.go:1 +0x1\n"
	want := []report.Frame{
		frame(daemon+".Start", "internal/daemon/start.go", 3, true),
		frame(mod+"/internal/ui.run", "ui/run.go", 5, true),
		frame("github.com/spf13/cobra.(*Command).execute", "github.com/spf13/cobra@v1.10.1/command.go", 1015, false),
		frame(daemon+".(*Daemon).serveHTTP", "daemon/daemon.go", 88, true),
		frame(mod+"/internal/report.RecordPanic", "internal/report/process.go", 40, false),
		frame("runtime/debug.Stack", "runtime/debug/stack.go", 26, false),
	}
	if got := withoutModule(report.ParseStack([]byte(stack))); !reflect.DeepEqual(got, want) {
		t.Errorf("frames:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseStackOfDebugStack(t *testing.T) {
	frames := report.ParseStack(stackHere())
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	if got := frames[len(frames)-1].Function; got != "runtime/debug.Stack" {
		t.Errorf("innermost frame %q, want runtime/debug.Stack", got)
	}
	if got := inApp(frames); len(got) == 0 || got[len(got)-1] != helpers+".stackHere" {
		t.Errorf("in-app frames %q, want stackHere innermost", got)
	}
}

//go:noinline
func stackHere() []byte { return debug.Stack() }
