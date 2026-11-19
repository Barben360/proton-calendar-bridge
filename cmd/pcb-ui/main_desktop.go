//go:build (linux || windows) && cgo

// Command pcb-ui is the desktop app of pcb: the bridge runs inside it, with a
// window and a tray icon.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	fdesktop "fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/logfile"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
	"github.com/Barben360/proton-calendar-bridge/internal/ui/desktop"
	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

const appID = "io.github.barben360.pcb-ui"

// logMax is the size at which pcb-ui.log rotates to pcb-ui.log.1.
const logMax = 10 * 1024 * 1024

// openLog opens <state>/logs/pcb-ui.log. The state and logs directories
// are made first, private to the user as every directory pcb makes (an
// access list owned by the user on Windows, even elevated): logfile, which
// imports only the standard library, would make them with os.MkdirAll.
func openLog(state string) (*logfile.File, error) {
	logs := filepath.Join(state, "logs")
	if err := acl.MkdirAll(logs); err != nil {
		return nil, err
	}
	return logfile.Open(filepath.Join(logs, "pcb-ui.log"), logMax)
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	// --version, --help and usage errors are written once parsed, after
	// attachConsole: a Windows pcb-ui started from a terminal attaches to it
	// only then, so a normal start is not tied to that terminal (its Ctrl-C
	// and its closing would end pcb-ui).
	var stdout, stderr bytes.Buffer
	opts, done, code := parseArgs(args, &stdout, &stderr)
	if done {
		attachConsole()
		_, _ = os.Stdout.Write(stdout.Bytes())
		_, _ = os.Stderr.Write(stderr.Bytes())
		return code
	}
	if err := preflight(); err != nil {
		showPreflightError(err)
		return int(exitcode.From(err))
	}
	// The crash file comes first, before any other goroutine: a crash from
	// here on is offered at the next start. Directories that do not resolve
	// are reported below, by LoadConfig.
	var reports *ui.Reports
	if dirs, err := daemon.ResolveDirs(os.Getenv); err == nil {
		var stopReports func()
		reports, stopReports = ui.StartReports(dirs.State, buildinfo.ReportDSN,
			report.Meta{Binary: "pcb-ui", Command: "pcb-ui", Install: installKind(), Version: buildinfo.Version}, os.Getenv,
			func() error { return service.CheckStateOwner(dirs.State, os.Geteuid(), nil) })
		defer stopReports()
	}
	a := app.NewWithID(appID)
	a.SetIcon(desktop.AppIcon())
	if code, failed := probe(a); failed {
		return code
	}
	dirs, cfg, err := daemon.LoadConfig(os.Getenv)
	if err != nil {
		return fatal(a, err)
	}
	lvl, err := config.ParseLevel(cfg.Log.Level)
	if err != nil {
		return fatal(a, &exitcode.Error{Code: exitcode.Usage, Err: err, Hint: "use debug, info, warn or error in [log] level"})
	}
	lf, err := openLog(dirs.State)
	if err != nil {
		return fatal(a, &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: "check the permissions of the state directory"})
	}
	defer func() { _ = lf.Close() }()
	level := new(slog.LevelVar)
	level.Set(lvl)
	log := slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: level}))

	var win *desktop.Window
	show := func(account string) { fyne.Do(func() { win.Show(account) }) }
	showLogin := func(username string) { fyne.Do(func() { win.ShowLogin(username) }) }
	// ended is closed at logoff or shutdown: the bridge's drain is then 2 s.
	ended := make(chan struct{})
	bo := ui.BridgeOptions{Env: os.Getenv, Logger: log, Level: level, ShowUI: show, ShowLogin: showLogin, SessionEnd: ended}
	if reports != nil {
		bo.Reports = &reports.Sender
	}
	bridge := ui.NewBridge(bo)
	updates := newUpdates(a, dirs.State)
	set := &settings{level: level, logsDir: filepath.Join(dirs.State, "logs"), configPath: config.Path(dirs), open: openPath(a.OpenURL), exe: executable,
		updates: updates, poke: make(chan struct{}, 1), reports: reports}
	win = desktop.NewWindow(a, ui.NewActions(bridge), set, func() desktop.WizardModel { return ui.NewWizard(bridge) }, a.OpenURL)
	if reports != nil {
		win.SetReports(reports)
	}
	setWindowClass()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx); err != nil {
		return fatal(a, err)
	}

	var once sync.Once
	quit := func() {
		once.Do(func() {
			go func() {
				_ = bridge.Stop()
				fyne.Do(a.Quit)
			}()
		})
	}
	// No tray to come back from: the window opens even with --background,
	// says so once, and closing it quits (R14).
	// An autostarted login (--background) can beat the tray host to the
	// session bus: wait for it a few seconds instead of probing once.
	var ta desktop.TrayApp
	if da, ok := a.(fdesktop.App); ok {
		wait := time.Duration(0)
		if opts.background {
			wait = trayHostWait
		}
		if desktop.WaitForTrayHost(func() bool { return desktop.TrayHost("") }, wait, trayHostEvery) {
			ta = da
		}
	}
	tray := desktop.Attach(ta, win, quit, opts.background)
	// At logoff or shutdown Windows ends pcb-ui as soon as end returns: the
	// bridge drains (2 s) within it. Linux sends SIGTERM instead.
	stopWatch, err := watchSessionEnd(func() {
		close(ended)
		_ = bridge.Stop()
		fyne.Do(a.Quit)
	})
	if err != nil {
		log.Warn("pcb-ui will not drain at logoff or shutdown", "err", err)
	} else {
		defer stopWatch()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		quit()
	}()
	go follow(ctx, bridge, win, tray, log)
	go followUpdates(ctx, updates, set.poke, win, trayOrNil(tray), desktopNotifier{}, log)

	if reports != nil {
		askReports(ctx, reports, win, !opts.background)
	}

	a.Run()
	cancel()
	_ = bridge.Stop() // the loop ended some other way (a signal Fyne took)
	return 0
}

// trayHostWait is how long an autostarted pcb-ui (--background) waits for
// the tray host to register, trying every trayHostEvery.
const (
	trayHostWait  = 15 * time.Second
	trayHostEvery = 500 * time.Millisecond
)

// installKind is the install tag of a report (update.InstallKind).
func installKind() string {
	in := update.Install{GOOS: runtime.GOOS, Env: os.Getenv}
	if exe, err := executable(); err == nil {
		in.Exe = exe
		in.Home, _ = os.UserHomeDir()
	}
	return update.InstallKind(in)
}

// askReports asks in a dialog, once the loop runs, about the reports kept
// by an earlier run (mode ask only), and shows the panel for each report
// saved from now on. A --background start (raise false) shows the dialog
// only when the window is first shown.
func askReports(ctx context.Context, r *ui.Reports, win *desktop.Window, raise bool) {
	if n, _ := r.Pending(); n > 0 && r.Asking() {
		go fyne.Do(func() { win.AskReports(raise) })
	}
	r.Watch(ctx, func() { fyne.Do(win.ShowReportPanel) })
}

// probe runs the single-instance check: a running pcb-ui is shown (exit 0),
// a running pcb serve or agent session (pcb mcp) is a dialog and exit 20.
func probe(a fyne.App) (code int, failed bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := ui.Probe(ctx, os.Getenv, "")
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, ui.ErrShown):
		return 0, true
	default:
		return fatal(a, err), true
	}
}

// follow redraws the window and the tray at every change and sends the
// notifications.
func follow(ctx context.Context, b *ui.Bridge, win *desktop.Window, tray *desktop.Tray, log *slog.Logger) {
	var n ui.Notifier
	sender := &alertSender{log: log, send: func(note ui.Notification) error {
		return desktop.Notify("", note, func(account string) { fyne.Do(func() { win.Show(account) }) })
	}}
	for {
		v := b.View(ctx, time.Now())
		fyne.Do(func() {
			win.Update(v)
			if tray != nil {
				tray.Update(v)
			}
		})
		sender.sendAll(&n, n.Next(v))
		select {
		case <-ctx.Done():
			return
		case <-b.Changes():
		}
	}
}

// alertSender sends alert notifications. A failed one is offered again at
// the next view (Changes fires at least every 30 s), once the notification
// daemon is up; its failure is logged once, not at every retry.
type alertSender struct {
	log    *slog.Logger
	send   func(ui.Notification) error
	failed map[string]bool // alerts whose failure was logged, until one is sent
}

func (s *alertSender) sendAll(n *ui.Notifier, notes []ui.Notification) {
	if s.failed == nil {
		s.failed = map[string]bool{}
	}
	var unsent []ui.Notification
	for _, note := range notes {
		key := note.Account + "\x00" + note.Code
		err := s.send(note)
		if err == nil {
			s.log.Info("notification sent", "alert", note.Code, "account", note.Account)
			delete(s.failed, key)
			continue
		}
		if !s.failed[key] {
			s.log.Warn("notification failed", "alert", note.Code, "account", note.Account, "err", err)
		}
		s.failed[key] = true
		unsent = append(unsent, note)
	}
	n.Unsent(unsent)
}

// printError writes err and its hint as the CLI does.
func printError(w io.Writer, err error) {
	fmt.Fprintf(w, "Error: %s\nHint: %s\n", err, exitcode.HintOf(err))
}

// updateEvery is how often pcb-ui looks again: the 24 h cache decides when
// a request goes out, this when Later's 48 h are seen to end.
const updateEvery = time.Hour

// newUpdates is pcb-ui's update check: GitHub against this build's version,
// update.json in the state directory, Later and the notified version in
// Fyne's preferences, the install line on Linux only (the release page is
// the hint on Windows).
func newUpdates(a fyne.App, state string) *ui.Updates {
	u := &ui.Updates{
		Checker: update.Checker{Installed: buildinfo.Version},
		Path:    update.StorePath(state), Env: os.Getenv, Prefs: a.Preferences(), Now: time.Now,
	}
	if runtime.GOOS == "linux" {
		if exe, err := executable(); err == nil {
			home, _ := os.UserHomeDir()
			u.Hint = update.Hint(update.Install{GOOS: runtime.GOOS, Exe: exe, Home: home, Env: os.Getenv})
		}
	}
	return u
}

// updateWindow, updateTray and updateNotifier are what followUpdates asks of
// the window, the tray and the desktop notification (a test passes fakes).
type updateWindow interface {
	SetUpdate(n ui.UpdateNotice, later func())
	OpenURL(raw string)
}

type updateTray interface{ SetUpdate(n ui.UpdateNotice) }

type updateNotifier interface {
	Notify(note ui.Notification, onClick func(account string)) error
}

// trayOrNil is t as an updateTray, or a true nil when there is no tray: an
// interface holding a nil *Tray is not nil, and followUpdates would call it.
func trayOrNil(t *desktop.Tray) updateTray {
	if t == nil {
		return nil
	}
	return t
}

// desktopNotifier sends through the desktop's notification service.
type desktopNotifier struct{}

func (desktopNotifier) Notify(note ui.Notification, onClick func(string)) error {
	return desktop.Notify("", note, onClick)
}

// followUpdates draws each update notice in the window and the tray (nil:
// no tray), and sends its notification once per version (a failed send is
// retried at the next check); a click on it opens the release page (Linux:
// Windows reports no click).
func followUpdates(ctx context.Context, u *ui.Updates, poke chan struct{}, win updateWindow, tray updateTray, notifier updateNotifier, log *slog.Logger) {
	later := func() {
		u.Later()
		pokeLoop(poke)
	}
	u.Run(ctx, updateEvery, poke, func(n ui.UpdateNotice) {
		fyne.Do(func() {
			win.SetUpdate(n, later)
			if tray != nil {
				tray.SetUpdate(n)
			}
		})
	}, func(note ui.Notification, page string) error {
		var err error
		ui.Send(log, []ui.Notification{note}, func(note ui.Notification) error {
			err = notifier.Notify(note, func(string) { fyne.Do(func() { win.OpenURL(page) }) })
			return err
		})
		return err
	})
}

// fatal shows err (message and hint) in a window, waits for it to close and
// returns its exit code.
func fatal(a fyne.App, err error) int {
	errorWindow(a, err, a.Quit).Show()
	a.Run()
	return int(exitcode.From(err))
}

// errorWidth is where the startup error's text wraps, in characters.
const errorWidth = 64

// errorWindow is the startup error: a window whose content is the title,
// the message, the hint and OK, sized to that content (live 2026-11-13, F9:
// a dialog inside a larger empty window left black bars beside its card).
// OK runs done; the window is the master, so closing it by its title bar
// quits the app too (the hidden main window would keep the loop alive).
func errorWindow(a fyne.App, err error, done func()) fyne.Window {
	text := err.Error()
	if h := exitcode.HintOf(err); h != "" {
		text += "\n\n" + h
	}
	w := a.NewWindow("pcb-ui cannot start")
	setWindowClass()
	w.SetMaster()
	title := widget.NewLabelWithStyle("pcb-ui cannot start", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	ok := widget.NewButton("OK", done)
	ok.Importance = widget.HighImportance
	content := container.NewPadded(container.NewVBox(title, widget.NewLabel(wrapText(text, errorWidth)), container.NewHBox(layout.NewSpacer(), ok)))
	w.SetContent(content)
	w.SetFixedSize(true)
	w.Resize(content.MinSize())
	return w
}

// wrapText breaks text into lines of at most width characters at spaces
// (a longer word stays whole), keeping its own line breaks.
func wrapText(text string, width int) string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
