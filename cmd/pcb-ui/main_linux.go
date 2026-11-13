//go:build linux && cgo

// Command pcb-ui is the desktop app of pcb: the bridge runs inside it, with a
// window and a tray icon.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"
	fdesktop "fyne.io/fyne/v2/driver/desktop"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/logfile"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
	"github.com/Barben360/proton-calendar-bridge/internal/ui/desktop"
)

const appID = "io.github.barben360.pcb-ui"

// logMax is the size at which pcb-ui.log rotates to pcb-ui.log.1.
const logMax = 10 * 1024 * 1024

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	opts, done, code := parseArgs(args, os.Stdout, os.Stderr)
	if done {
		return code
	}
	if err := checkDisplay(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return int(exitcode.Usage)
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
	lf, err := logfile.Open(filepath.Join(dirs.State, "logs", "pcb-ui.log"), logMax)
	if err != nil {
		return fatal(a, &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: "check the permissions of the state directory"})
	}
	defer func() { _ = lf.Close() }()
	level := new(slog.LevelVar)
	level.Set(lvl)
	log := slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: level}))

	var win *desktop.Window
	show := func(account string) { fyne.Do(func() { win.Show(account) }) }
	bridge := ui.NewBridge(ui.BridgeOptions{Env: os.Getenv, Logger: log, Level: level, ShowUI: show})
	set := &settings{level: level, logsDir: filepath.Join(dirs.State, "logs"), configPath: config.Path(dirs), open: a.OpenURL, exe: executable}
	win = desktop.NewWindow(a, ui.NewActions(bridge), set, func() desktop.WizardModel { return ui.NewWizard(bridge) }, a.OpenURL)

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
	var tray *desktop.Tray
	if da, ok := a.(fdesktop.App); ok && desktop.TrayHost("") {
		tray = desktop.NewTray(da, win, quit)
	} else {
		// No tray to come back from: the window opens even with --background,
		// and says so once.
		win.HintNoTray()
		win.Show("")
	}
	if tray != nil && !opts.background {
		win.Show("")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		quit()
	}()
	go follow(ctx, bridge, win, tray, log)

	a.Run()
	cancel()
	_ = bridge.Stop() // the loop ended some other way (a signal Fyne took)
	return 0
}

// probe runs the single-instance check: a running pcb-ui is shown (exit 0),
// a running pcb serve is a dialog and exit 20.
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
	for {
		v := b.View(ctx, time.Now())
		fyne.Do(func() {
			win.Update(v)
			if tray != nil {
				tray.Update(v)
			}
		})
		for _, note := range n.Next(v) {
			err := desktop.Notify("", note, func(account string) { fyne.Do(func() { win.Show(account) }) })
			if err != nil {
				log.Warn("notification failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-b.Changes():
		}
	}
}

// fatal shows err (message and hint) in a dialog, waits for it to close and
// returns its exit code.
func fatal(a fyne.App, err error) int {
	text := err.Error()
	if h := exitcode.HintOf(err); h != "" {
		text += "\n\n" + h
	}
	w := a.NewWindow("pcb")
	// Master: closing the window by its title bar quits the app too (the
	// dialog's OnClosed does not fire then, and the hidden main window would
	// keep the loop alive).
	w.SetMaster()
	d := dialog.NewInformation("pcb-ui cannot start", text, w)
	d.SetOnClosed(func() { a.Quit() })
	w.Resize(fyne.NewSize(480, 200))
	w.Show()
	d.Show()
	a.Run()
	return int(exitcode.From(err))
}
