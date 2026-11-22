package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

func panickingRoot() *cobra.Command {
	root := &cobra.Command{Use: "pcb", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(&cobra.Command{Use: "boom", RunE: func(*cobra.Command, []string) error {
		panic("unexpected payload")
	}})
	return root
}

// A command that panics is a bug, not a bad invocation: exit 20, one
// envelope on stdout with --json (the stack never goes there), else the
// message on stderr.
func TestPanickingCommandIsExit20WithOneEnvelope(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	code := Run(panickingRoot(), []string{"boom", "--json"}, &out, &errs)
	if code != exitcode.Blocked {
		t.Fatalf("exit = %d, want %d", code, exitcode.Blocked)
	}
	dec := json.NewDecoder(&out)
	var env struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not an envelope: %v\n%s", err, out.String())
	}
	if dec.More() {
		t.Errorf("more than one value on stdout")
	}
	if env.OK || env.Command != "boom" || env.Error == nil || env.Error.Code != 20 || !strings.Contains(env.Error.Message, "internal error") {
		t.Errorf("envelope = %+v, want boom, not ok, error 20 naming an internal error", env)
	}
	if strings.Contains(out.String(), "goroutine") || strings.Contains(out.String(), "unexpected payload") {
		t.Errorf("stdout leaks the panic or its stack: %s", out.String())
	}
}

func TestPanickingCommandInTextModeKeepsStdoutEmpty(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	code := Run(panickingRoot(), []string{"boom"}, &out, &errs)
	if code != exitcode.Blocked || out.Len() != 0 {
		t.Fatalf("exit = %d, stdout = %q; want 20 and nothing on stdout", code, out.String())
	}
	if !strings.Contains(errs.String(), "internal error") || !strings.Contains(errs.String(), "goroutine") {
		t.Errorf("stderr = %q, want the internal error and the stack", errs.String())
	}
}

//go:noinline
func panicWithMarker() { panic("MARKER-7f3a") }

// A command panic RunContext recovers never reaches the Go runtime, so it
// never reaches the crash file: RunContext keeps a report of it itself,
// ending at the panicking function and without the panic's text. Not
// parallel: the recorder is process-wide.
func TestRecoveredCommandPanicIsReported(t *testing.T) {
	store := report.Store{StateDir: t.TempDir(), Env: func(string) string { return "" }}
	report.Install(&report.Process{Store: store, Meta: report.Meta{Binary: "pcb", Command: "pcb boom", Install: "test", Version: "0"}, DSN: "http://k@127.0.0.1:1/1"})
	t.Cleanup(report.Uninstall)
	root := &cobra.Command{Use: "pcb", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(&cobra.Command{Use: "boom", RunE: func(*cobra.Command, []string) error {
		panicWithMarker()
		return nil
	}})

	var out, errs bytes.Buffer
	code := Run(root, []string{"boom", "--json"}, &out, &errs)
	if code != exitcode.Blocked {
		t.Fatalf("exit = %d, want %d", code, exitcode.Blocked)
	}
	dec := json.NewDecoder(&out)
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := dec.Decode(&env); err != nil || dec.More() || env.OK || env.Error == nil || env.Error.Code != 20 {
		t.Errorf("stdout = %q (%v), want one envelope with error 20", out.String(), err)
	}

	pend, err := store.Pending()
	if err != nil || len(pend) != 1 {
		t.Fatalf("pending = %d, %v; want exactly one report", len(pend), err)
	}
	if strings.Contains(string(pend[0].JSON), "MARKER-7f3a") {
		t.Errorf("the report holds the panic's text:\n%s", pend[0].JSON)
	}
	var ev struct {
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
	frames := ev.Exception.Values[0].Stacktrace.Frames

	if m := ev.Exception.Values[0].Mechanism; m.Type != "go.recovered" || m.Handled != true || ev.Tags.Source != "recovered" {
		t.Errorf("mechanism %+v, source %q; want go.recovered (handled true), recovered", m, ev.Tags.Source)
	}
	if len(frames) == 0 || !strings.HasSuffix(frames[len(frames)-1].Function, "internal/cli.panicWithMarker") || !frames[len(frames)-1].InApp {
		t.Errorf("the last frame is not the in_app panicking function:\n%v", frames)
	}
	for _, f := range frames {
		if strings.HasSuffix(f.Function, "cli.RunContext.func1") || strings.Contains(f.Function, "runtime/debug") || f.Function == "panic" {
			t.Errorf("recovery frame %q in the report", f.Function)
		}
	}
}
