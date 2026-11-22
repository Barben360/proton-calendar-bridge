package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

const (
	// noReportAddress is what pcb says when the build has no crash report
	// address (a dev build, a build from source).
	noReportAddress = "this build has no crash report address"
	// reportNotice is the once-a-day line on stderr while reports wait.
	reportNotice = "pcb kept a crash report: pcb crash-reports show"
	// reportLockWait bounds how long start-up and the notice wait for the
	// reports' locks: neither may hold a command up.
	reportLockWait = time.Second
)

// reportStore is the crash reports of the state directory; ok is false
// when the directory cannot be resolved.
func (a *app) reportStore() (report.Store, bool) {
	dirs, err := resolveDirs(a.Env)
	if err != nil {
		return report.Store{Env: a.Env, Now: a.Now}, false
	}
	return report.Store{StateDir: dirs.State, Env: a.Env, Now: a.Now}, true
}

// installKind is the install tag of a report (update.InstallKind).
func (a *app) installKind() string {
	in, ok := a.thisInstall()
	if !ok {
		in = update.Install{GOOS: runtime.GOOS, Env: a.Env}
	}
	return update.InstallKind(in)
}

// startReports keeps the crash reports of this pcb process
// (daemon.StartReports), for a state directory that resolves. The returned
// function ends it.
func (a *app) startReports(root *cobra.Command, args []string) (stop func()) {
	store, ok := a.reportStore()
	if !ok {
		return func() {}
	}
	cmd, _, err := root.Find(args)
	if err != nil || cmd == nil {
		cmd = root
	}
	_, stop, _ = daemon.StartReports(daemon.ReportStart{
		StateDir: store.StateDir, DSN: a.ReportDSN, Env: a.Env, Now: a.Now, Owner: a.stateOwner,
		Meta: report.Meta{Binary: "pcb", Command: cmd.CommandPath(), Install: a.installKind(), Version: buildinfo.Version},
	})
	return stop
}

// reportNoticeAfter prints, once a day, that crash reports wait for an
// answer: after a command a person runs (stderr a terminal, no --json),
// never after pcb crash-reports itself, only in mode ask.
func (a *app) reportNoticeAfter(root *cobra.Command, args []string) {
	if a.ReportDSN == "" || jsonRequested(args) || !a.StderrTerminal() {
		return
	}
	if cmd, _, err := root.Find(args); err == nil && cmd.Name() == crashReportsName && cmd.Parent() == root {
		return
	}
	store, ok := a.reportStore()
	if !ok || a.stateOwner() != nil || store.Mode() != report.Ask {
		return
	}
	if pend, err := store.Pending(); err != nil || len(pend) == 0 {
		return
	}
	today := a.Now().UTC().Format(time.DateOnly)
	ctx, cancel := context.WithTimeout(context.Background(), reportLockWait)
	defer cancel()
	_ = store.Edit(ctx, func(st *report.State) {
		if st.NoticedDay == today {
			return
		}
		st.NoticedDay = today
		fmt.Fprintln(a.Stderr, reportNotice)
	})
}

// crashReportsResult is pcb doctor's crash reports line: a warning while
// reports wait for an answer, never an error.
func (a *app) crashReportsResult() doctor.Result {
	r := doctor.Result{Name: "crash reports", Status: doctor.StatusSkip}
	if a.ReportDSN == "" {
		r.Message = noReportAddress
		return r
	}
	store, _ := a.reportStore()
	pend, err := store.Pending()
	if err != nil {
		r.Message = "the waiting reports cannot be read: " + err.Error()
		return r
	}
	r.Status = doctor.StatusOK
	switch mode := store.Mode(); {
	case mode == report.Ask && len(pend) > 0:
		r.Status = doctor.StatusWarn
		r.Message = fmt.Sprintf("%d waiting: pcb crash-reports show, then send or discard", len(pend))
		r.Fix = "pcb crash-reports show"
	case mode == report.Always, mode == report.Never:
		r.Message = string(mode)
	default:
		r.Message = "none waiting"
	}
	return r
}

const crashReportsName = "crash-reports"

// crashReportsData is pcb crash-reports' result: the mode in force, the
// mode PCB_CRASH_REPORTS forces ("" when it does not), the reports
// waiting, and whether this build can send them.
type crashReportsData struct {
	Mode            report.Mode `json:"mode"`
	EnvOverride     report.Mode `json:"env_override"`
	Pending         int         `json:"pending"`
	BuildHasAddress bool        `json:"build_has_address"`
}

// crashSendData is pcb crash-reports send's result: the status after the
// send and one result per report posted.
type crashSendData struct {
	crashReportsData
	Results []crashSendResult `json:"results"`
}

type crashSendResult struct {
	ID    string `json:"id"`
	Sent  bool   `json:"sent"`
	Error string `json:"error,omitempty"`
}

func (a *app) newCrashReports() *cobra.Command {
	return OneOperand(&cobra.Command{
		Use:   crashReportsName + " status|show|send|discard|always|ask|never",
		Short: "See, send or discard the crash reports pcb kept, or choose when they are sent",
		Long: "After a crash or an unexpected error, pcb keeps a report (no event, name, address, secret or path: " +
			"show prints exactly what would be sent) and sends it only once you agree. " +
			"status shows the mode and how many wait; show prints them; send sends them now; discard deletes them. " +
			"always: pcb serve and pcb-ui send them as they come; ask (the default): they wait for you; never: none is kept and those waiting are deleted. " +
			report.EnvSwitch + "=always or never overrides the mode for one process.",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, ok := a.reportStore()
			text := ""
			switch verb := args[0]; verb {
			case "status":
			case "show":
				return a.showReports(cmd, store)
			case "send":
				return a.sendReports(cmd, store)
			case "discard":
				n, err := a.discardReports(cmd, store)
				if err != nil {
					return Finish(cmd, nil, "", err)
				}
				text = reportCount(n) + " discarded\n"
			case string(report.Always), string(report.Ask), string(report.Never):
				if err := a.stateOwner(); err != nil {
					return Finish(cmd, nil, "", err)
				}
				var err error
				if !ok {
					_, err = resolveDirs(a.Env)
				} else if err = acl.MkdirAll(store.StateDir); err == nil {
					err = store.SetMode(cmd.Context(), report.Mode(verb))
				}
				if err != nil {
					return Finish(cmd, nil, "", fmt.Errorf("save the crash report mode: %w", err))
				}
			default:
				return UsageError(fmt.Errorf("%q takes status, show, send, discard, always, ask or never, not %q", cmd.CommandPath(), verb))
			}
			d, err := a.reportStatus(store)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, d, text+reportsText(d), nil)
		},
	}, "status, show, send, discard, always, ask or never")
}

// reportStatus is the status of store's reports.
func (a *app) reportStatus(store report.Store) (crashReportsData, error) {
	pend, err := store.Pending()
	if err != nil {
		return crashReportsData{}, fmt.Errorf("read the crash reports: %w", err)
	}
	d := crashReportsData{Mode: store.Mode(), Pending: len(pend), BuildHasAddress: a.ReportDSN != ""}
	// Without a state directory only the environment can set a mode.
	if env := (report.Store{Env: a.Env}).Mode(); env != report.Ask {
		d.EnvOverride = env
	}
	return d, nil
}

// reportCount is "1 crash report", "2 crash reports".
func reportCount(n int) string {
	if n == 1 {
		return "1 crash report"
	}
	return fmt.Sprintf("%d crash reports", n)
}

func reportsText(d crashReportsData) string {
	mode := string(d.Mode)
	if d.EnvOverride != "" {
		mode += " (set by " + report.EnvSwitch + ")"
	}
	text := fmt.Sprintf("crash reports: %s; %d waiting", mode, d.Pending)
	if !d.BuildHasAddress {
		text += "\n" + noReportAddress
	}
	return text
}

// showReports prints the waiting reports, exactly as they would be sent:
// indented in text, the events themselves with --json.
func (a *app) showReports(cmd *cobra.Command, store report.Store) error {
	pend, err := store.Pending()
	if err != nil {
		return Finish(cmd, nil, "", fmt.Errorf("read the crash reports: %w", err))
	}
	events := make([]json.RawMessage, 0, len(pend))
	var text strings.Builder
	for i, p := range pend {
		events = append(events, json.RawMessage(p.JSON))
		var b bytes.Buffer
		if err := json.Indent(&b, p.JSON, "", "  "); err != nil {
			return Finish(cmd, nil, "", fmt.Errorf("read the crash report %s: %w", p.ID, err))
		}
		if i > 0 {
			text.WriteString("\n\n")
		}
		text.Write(b.Bytes())
	}
	if len(pend) == 0 {
		text.WriteString("no crash report waiting")
	}
	return Finish(cmd, events, text.String(), nil)
}

// discardReports deletes the waiting reports and returns how many there
// were. With none it touches nothing.
func (a *app) discardReports(cmd *cobra.Command, store report.Store) (int, error) {
	pend, err := store.Pending()
	if err != nil {
		return 0, fmt.Errorf("read the crash reports: %w", err)
	}
	if len(pend) == 0 {
		return 0, nil
	}
	if err := a.stateOwner(); err != nil {
		return 0, err
	}
	if err := store.Discard(cmd.Context()); err != nil {
		return 0, fmt.Errorf("discard the crash reports: %w", err)
	}
	return len(pend), nil
}

// sendReports sends the waiting reports now, whatever the mode (the user
// asks), and says what became of each: exit 40 when some are left.
func (a *app) sendReports(cmd *cobra.Command, store report.Store) error {
	pend, err := store.Pending()
	if err != nil {
		return Finish(cmd, nil, "", fmt.Errorf("read the crash reports: %w", err))
	}
	if len(pend) == 0 {
		d, err := a.reportStatus(store)
		return Finish(cmd, crashSendData{crashReportsData: d, Results: []crashSendResult{}}, "no crash report waiting", err)
	}
	if a.ReportDSN == "" {
		return Finish(cmd, nil, "", &exitcode.Error{Code: exitcode.Blocked, Err: errors.New(noReportAddress),
			Hint: "pcb crash-reports show prints them, pcb crash-reports discard deletes them"})
	}
	if err := a.stateOwner(); err != nil {
		return Finish(cmd, nil, "", err)
	}
	results, sendErr := report.Sender{DSN: a.ReportDSN, Store: store}.SendPending(cmd.Context())
	d := crashSendData{Results: make([]crashSendResult, 0, len(results))}
	var text strings.Builder
	sent := 0
	for _, r := range results {
		res := crashSendResult{ID: r.ID, Sent: r.Sent}
		line := r.ID + ": sent"
		if r.Sent {
			sent++
		} else {
			res.Error = r.Err.Error()
			line = r.ID + ": not sent (" + res.Error + ")"
		}
		d.Results = append(d.Results, res)
		text.WriteString(line + "\n")
	}
	status, err := a.reportStatus(store)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	d.crashReportsData = status
	text.WriteString(reportsText(status))
	switch {
	case sendErr != nil:
		err = &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("send the crash reports: %w", sendErr), Hint: "run pcb crash-reports send again later"}
	case sent < len(pend):
		err = &exitcode.Error{Code: exitcode.Partial, Err: fmt.Errorf("%d of %s not sent", len(pend)-sent, reportCount(len(pend))),
			Hint: fmt.Sprintf("they wait (the server may have asked pcb to wait, Retry-After; at most %d are sent a day; a report is dropped after %d failed sends): run pcb crash-reports send again later",
				report.DailyCap, report.MaxAttempts)}
	}
	return Finish(cmd, d, text.String(), err)
}
