package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// Reports is what pcb-ui asks of the crash reports it keeps: the waiting
// ones (the dialog and the panel), the answers (Send, Always send, Don't
// send) and the Settings switch.
type Reports struct {
	Store  report.Store
	Sender report.Sender
}

// StartReports keeps the crash reports of this pcb-ui process
// (daemon.StartReports, as pcb does). It returns nil when reporting is off:
// a build without an address (dsn), a refused owner check (owner, nil: none)
// or a failure. stop is for a clean exit only.
func StartReports(state, dsn string, m report.Meta, env func(string) string, owner func() error) (r *Reports, stop func()) {
	store, stop, ok := daemon.StartReports(daemon.ReportStart{StateDir: state, DSN: dsn, Meta: m, Env: env, Owner: owner})
	if !ok {
		return nil, stop
	}
	return &Reports{Store: store, Sender: report.Sender{DSN: dsn, Store: store}}, stop
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

// errNotSentNow is Send's answer when nothing could be posted (a server's
// Retry-After or the daily limit) and reports still wait.
var errNotSentNow = errors.New("not sent now: try again later")

// Send posts the waiting reports now, whatever the mode (the person asked).
// It fails when a report was refused or the request failed, and when none
// was posted while some wait (a server's Retry-After, the daily limit).
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
	if n, _ := r.Pending(); len(results) == 0 && n > 0 {
		return errNotSentNow
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

// SetBy is report.EnvSwitch when it sets the mode in force (always or
// never), which the switch then cannot change, else "".
func (r *Reports) SetBy() string {
	if (report.Store{Env: r.Store.Env}).Mode() != report.Ask {
		return report.EnvSwitch
	}
	return ""
}

// SetEnabled flips the switch: on is always send, off is ask. It fails
// while PCB_CRASH_REPORTS sets the mode: the change would not be in force.
func (r *Reports) SetEnabled(ctx context.Context, on bool) error {
	if by := r.SetBy(); by != "" {
		return errors.New("crash reports: the mode is set by " + by)
	}
	if on {
		return r.Store.SetMode(ctx, report.Always)
	}
	return r.Store.SetMode(ctx, report.Ask)
}

// Watch calls show, on a goroutine of its own, each time a report is saved
// while the mode is ask and reports wait, until ctx ends. It has subscribed
// when it returns, so a report saved after is never missed. It does nothing
// without an installed recorder.
func (r *Reports) Watch(ctx context.Context, show func()) {
	p := report.Current()
	if p == nil {
		return
	}
	poke, unsubscribe := p.Subscribe()
	go func() {
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
	}()
}
