package service_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const marker = "MARKER-7f3a"

//go:noinline
func panicWithMarker() { panic(marker) }

// A panic serveGuarded recovers never reaches the Go runtime, so it never
// reaches the crash file: the guard keeps a report of it itself, ending at
// the panicking function and without the panic's text, and the account is
// still parked while the other serves. Not parallel: the recorder is
// process-wide.
func TestRecoveredAccountLoopPanicIsReported(t *testing.T) {
	store := report.Store{StateDir: t.TempDir(), Env: func(string) string { return "" }}
	report.Install(&report.Process{Store: store, Meta: report.Meta{Binary: "pcb", Command: "pcb serve", Install: "test", Version: "0"}, DSN: "http://k@127.0.0.1:1/1"})
	t.Cleanup(report.Uninstall)
	e := newServeEnv(t)
	h := e.open(t)
	a, b := e.loginBoth(t, h)
	r := newServeRig(h)
	o := r.options(true)
	record := o.OnPass
	o.OnPass = func(id string, p service.WatchPass) {
		if id == a {
			panicWithMarker()
		}
		record(id, p)
	}
	_, stop := r.serve(t, o)
	defer func() { _ = stop() }()
	info := r.waitStopped(t, a)
	r.waitPassed(t, b, 0)
	if s := info.Stopped; s.Code != int(exitcode.Blocked) || !strings.Contains(s.Message, "internal error") {
		t.Errorf("a.Stopped = %+v, want Blocked naming an internal error", s)
	}
	if hl := health(t, h, b); hl.Serve != service.ServeServing {
		t.Errorf("Health(b).Serve = %q, want serving", hl.Serve)
	}

	pend, err := store.Pending()
	if err != nil || len(pend) != 1 {
		t.Fatalf("pending = %d, %v; want exactly one report", len(pend), err)
	}
	if strings.Contains(string(pend[0].JSON), marker) {
		t.Errorf("the report holds the panic's text:\n%s", pend[0].JSON)
	}
	var ev struct {
		Extra struct {
			Count int `json:"count"`
		} `json:"extra"`
		Tags struct {
			Source string `json:"source"`
		} `json:"tags"`
		Exception struct {
			Values []struct {
				Mechanism struct {
					Type    string `json:"type"`
					Handled bool   `json:"handled"`
				} `json:"mechanism"`
				Stacktrace struct {
					Frames []report.Frame `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		} `json:"exception"`
	}
	if err := json.Unmarshal(pend[0].JSON, &ev); err != nil || len(ev.Exception.Values) != 1 {
		t.Fatalf("report JSON: %v\n%s", err, pend[0].JSON)
	}
	if ev.Extra.Count != 1 {
		t.Errorf("extra.count = %d, want 1: the panic was recorded more than once", ev.Extra.Count)
	}
	frames := ev.Exception.Values[0].Stacktrace.Frames

	if m := ev.Exception.Values[0].Mechanism; m.Type != "go.recovered" || m.Handled != true || ev.Tags.Source != "recovered" {
		t.Errorf("mechanism %+v, source %q; want go.recovered (handled true), recovered", m, ev.Tags.Source)
	}
	if len(frames) == 0 || !strings.HasSuffix(frames[len(frames)-1].Function, "internal/service_test.panicWithMarker") || !frames[len(frames)-1].InApp {
		t.Errorf("the last frame is not the in_app panicking function:\n%v", frames)
	}
	for _, f := range frames {
		if strings.HasSuffix(f.Function, "service.serveGuarded.func1") || strings.Contains(f.Function, "runtime/debug") || f.Function == "panic" {
			t.Errorf("recovery frame %q in the report", f.Function)
		}
	}
}

// serveGuarded logs a recovered panic's Go type and, for a runtime error,
// its message, never another panic's text.
func TestServeGuardedLogsNoPanicText(t *testing.T) {
	for _, tc := range []struct {
		name, typ, msg string
		f              func() error
	}{
		{"string", "string", "(text removed)", func() error { panicWithMarker(); return nil }},
		{"runtime error", "runtime.boundsError", "runtime error: index out of range [5] with length 0", func() error {
			_ = indexAt(nil, 5)
			return nil
		}},
	} {
		var buf bytes.Buffer
		err := service.ServeGuarded(slog.New(slog.NewJSONHandler(&buf, nil)), tc.f)
		if exitcode.From(err) != exitcode.Blocked {
			t.Fatalf("%s: err = %v, want Blocked", tc.name, err)
		}
		var line map[string]any
		if e := json.Unmarshal(buf.Bytes(), &line); e != nil {
			t.Fatalf("%s: log is not one JSON line: %v\n%s", tc.name, e, buf.String())
		}
		if line["panic_type"] != tc.typ || line["panic"] != tc.msg {
			t.Errorf("%s: panic_type = %v, panic = %v; want %q, %q", tc.name, line["panic_type"], line["panic"], tc.typ, tc.msg)
		}
		if strings.Contains(buf.String(), marker) {
			t.Errorf("the log holds the panic's text:\n%s", buf.String())
		}
	}
}

//go:noinline
func indexAt(s []int, i int) int { return s[i] }
