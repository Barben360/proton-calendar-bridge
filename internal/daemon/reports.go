package daemon

import (
	"context"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// reportLockWait bounds how long start-up waits for the reports' locks:
// it must not hold a command or a window up.
const reportLockWait = time.Second

// ReportStart is what a pcb binary's crash report start-up needs.
type ReportStart struct {
	StateDir string
	// DSN is the build's crash report address; empty keeps nothing.
	DSN  string
	Meta report.Meta
	Env  func(string) string
	Now  func() time.Time
	// Owner refuses a user who does not own the state directory (root on a
	// user's state would leave files there): nil checks nothing.
	Owner func() error
}

// StartReports keeps the crash reports of this process, for a build with
// an address, a state directory and a user who owns it: a crash file for a
// fatal crash, the crash files of dead processes turned into reports, and
// the recorder of unexpected errors. Anything failing leaves reporting off,
// silently (ok false). stop ends it; it is for a clean exit only, so that
// a crash leaves its file behind.
func StartReports(o ReportStart) (store report.Store, stop func(), ok bool) {
	stop = func() {}
	store = report.Store{StateDir: o.StateDir, Env: o.Env, Now: o.Now}
	if o.DSN == "" || o.StateDir == "" || (o.Owner != nil && o.Owner() != nil) {
		return store, stop, false
	}
	stopCrash, err := report.StartCrashFile(o.StateDir, o.Meta)
	if err != nil {
		return store, stop, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportLockWait)
	_, _ = store.Collect(ctx)
	cancel()
	report.Install(&report.Process{Store: store, Meta: o.Meta, DSN: o.DSN})
	return store, func() {
		report.Uninstall()
		stopCrash()
	}, true
}
