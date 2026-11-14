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
	// The state directory private to the user (an ACL on Windows) before the
	// log creates it.
	if err := acl.MkdirAll(dirs.State); err != nil {
		return fatal(a, &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: "check the permissions of the state directory"})
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
	showLogin := func(username string) { fyne.Do(func() { win.ShowLogin(username) }) }
	// ended is closed at logoff or shutdown: the bridge's drain is then 2 s.
	ended := make(chan struct{})
	bridge := ui.NewBridge(ui.BridgeOptions{Env: os.Getenv, Logger: log, Level: level, ShowUI: show, ShowLogin: showLogin, SessionEnd: ended})
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
	// No tray to come back from: the window opens even with --background,
	// says so once, and closing it quits (R14).
	var ta desktop.TrayApp
	if da, ok := a.(fdesktop.App); ok && desktop.TrayHost("") {
		ta = da
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

	a.Run()
	cancel()
	_ = bridge.Stop() // the loop ended some other way (a signal Fyne took)
	return 0
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
	for {
		v := b.View(ctx, time.Now())
		fyne.Do(func() {
			win.Update(v)
			if tray != nil {
				tray.Update(v)
			}
		})
		ui.Send(log, n.Next(v), func(note ui.Notification) error {
			return desktop.Notify("", note, func(account string) { fyne.Do(func() { win.Show(account) }) })
		})
		select {
		case <-ctx.Done():
			return
		case <-b.Changes():
		}
	}
}

// printError writes err and its hint as the CLI does.
func printError(w io.Writer, err error) {
	fmt.Fprintf(w, "Error: %s\nHint: %s\n", err, exitcode.HintOf(err))
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
