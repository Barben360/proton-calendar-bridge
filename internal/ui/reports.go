package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// reportLockWait bounds how long start-up waits for the reports' locks: it
// must not hold the window up.
const reportLockWait = time.Second

// Reports is what pcb-ui asks of the crash reports it keeps: the waiting
// ones (the dialog and the panel), the answers (Send, Always send, Don't
// send) and the Settings switch.
type Reports struct {
	Store  report.Store
	Sender report.Sender
}

// StartReports keeps the crash reports of this pcb-ui process, for a build
// with an address (dsn): a crash file for a fatal crash, the crash files of
// dead processes turned into reports, and the recorder of unexpected
// errors. It returns nil for a build without an address; a failure to keep
// reports leaves them off, silently. stop is for a clean exit only, so that
// a crash leaves its file behind.
func StartReports(state, dsn string, m report.Meta, env func(string) string) (r *Reports, stop func()) {
	stop = func() {}
	if dsn == "" {
		return nil, stop
	}
	store := report.Store{StateDir: state, Env: env}
	stopCrash, err := report.StartCrashFile(state, m)
	if err != nil {
		return nil, stop
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportLockWait)
	_, _ = store.Collect(ctx)
	cancel()
	report.Install(&report.Process{Store: store, Meta: m, DSN: dsn})
	return &Reports{Store: store, Sender: report.Sender{DSN: dsn, Store: store}}, func() {
		report.Uninstall()
		stopCrash()
	}
}

// Pending is how many reports wait, and whether one of them is a fatal
// crash (the dialog's title).
func (r *Reports) Pending() (n int, crashed bool) {
	pend, err := r.Store.Pending()
	if err != nil {
		return 0, false
	}
	for _, p := range pend {
		if p.Source == "crash" {
			crashed = true
		}
	}
	return len(pend), crashed
}

// Asking reports whether the mode is ask: only then pcb-ui asks.
func (r *Reports) Asking() bool { return r.Store.Mode() == report.Ask }

// Text is the waiting reports, exactly as they would be sent, indented.
func (r *Reports) Text() string {
	pend, err := r.Store.Pending()
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(pend))
	for _, p := range pend {
		var b bytes.Buffer
		if json.Indent(&b, p.JSON, "", "  ") != nil {
			b.Reset()
			b.Write(p.JSON)
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, "\n\n")
}

// Send posts the waiting reports now, whatever the mode (the person asked).
// It fails when a report was refused or the request failed.
func (r *Reports) Send(ctx context.Context) error {
	results, err := r.Sender.SendPending(ctx)
	if err != nil {
		return err
	}
	for _, res := range results {
		if !res.Sent {
			return res.Err
		}
	}
	return nil
}

// AlwaysSend sends from now on without asking, and sends the waiting
// reports.
func (r *Reports) AlwaysSend(ctx context.Context) error {
	if err := r.Store.SetMode(ctx, report.Always); err != nil {
		return err
	}
	return r.Send(ctx)
}

// DontSend deletes the waiting reports.
func (r *Reports) DontSend(ctx context.Context) error { return r.Store.Discard(ctx) }

// Enabled is the Settings switch: on while reports are sent without asking.
func (r *Reports) Enabled() bool { return r.Store.Mode() == report.Always }

// SetEnabled flips the switch: on is always send, off is ask.
func (r *Reports) SetEnabled(ctx context.Context, on bool) error {
	if on {
		return r.Store.SetMode(ctx, report.Always)
	}
	return r.Store.SetMode(ctx, report.Ask)
}

// Watch calls show each time a report is saved while the mode is ask and
// reports wait, until ctx ends. It does nothing without an installed
// recorder.
func (r *Reports) Watch(ctx context.Context, show func()) {
	p := report.Current()
	if p == nil {
		return
	}
	poke, unsubscribe := p.Subscribe()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poke:
			if n, _ := r.Pending(); n > 0 && r.Asking() {
				show()
			}
		}
	}
}
