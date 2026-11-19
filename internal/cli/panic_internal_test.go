package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
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
