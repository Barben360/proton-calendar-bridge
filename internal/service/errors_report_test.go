package service

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// oddError is an error type mapError knows nothing of.
type oddError struct{ detail string }

func (e *oddError) Error() string { return "odd: " + e.detail }

// installRecorder makes a recorder keep its reports in a fresh state
// directory until the test ends, and returns its store.
func installRecorder(t *testing.T) report.Store {
	t.Helper()
	st := report.Store{StateDir: t.TempDir(), Env: func(string) string { return "" }}
	report.Install(&report.Process{Store: st, Meta: report.Meta{Binary: "pcb", Command: "pcb sync"}, DSN: "https://key@reports.example/7"})
	t.Cleanup(report.Uninstall)
	return st
}

// Not parallel: the recorder is the process's.
func TestMapErrorUnexpectedRecords(t *testing.T) {
	st := installRecorder(t)
	err := fmt.Errorf("syncing MARKER-7f3a: %w", &oddError{detail: "MARKER-7f3a"})
	_ = mapError(err, "/k/secrets.key")
	pend, perr := st.Pending()
	if perr != nil {
		t.Fatal(perr)
	}
	if len(pend) != 1 {
		t.Fatalf("%d pending reports after an unexpected error, want 1", len(pend))
	}
	body := string(pend[0].JSON)
	if pend[0].Level != "error" || pend[0].Source != "error" {
		t.Errorf("level %q, source %q, want error, error", pend[0].Level, pend[0].Source)
	}
	if !strings.Contains(body, `"value":"*fmt.wrapError < *service.oddError"`) {
		t.Errorf("report lacks the type chain: %s", body)
	}
	if !strings.Contains(body, "service.mapError") {
		t.Errorf("report lacks the frame of mapError: %s", body)
	}
	if strings.Contains(body, "MARKER-7f3a") {
		t.Errorf("the error's text leaked into the report: %s", body)
	}
}

func TestMapErrorClassifiedRecordsNothing(t *testing.T) {
	st := installRecorder(t)
	for name, err := range map[string]error{
		"Proton 503": &api.Error{Status: 503, Message: "down"},
		"network":    &net.OpError{Op: "dial", Err: errors.New("refused")},
		"nil":        nil,
	} {
		_ = mapError(err, "/k/secrets.key")
		pend, perr := st.Pending()
		if perr != nil {
			t.Fatal(perr)
		}
		if len(pend) != 0 {
			t.Fatalf("%s: %d pending reports, want none", name, len(pend))
		}
	}
}
