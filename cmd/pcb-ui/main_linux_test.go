//go:build linux && cgo

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/driver"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

func TestParseArgs(t *testing.T) {
	t.Run("background", func(t *testing.T) {
		o, done, _ := parseArgs([]string{"--background"}, new(bytes.Buffer), new(bytes.Buffer))
		if done || !o.background {
			t.Fatalf("o=%+v done=%v, want background and not done", o, done)
		}
	})
	t.Run("default is not background", func(t *testing.T) {
		o, done, _ := parseArgs(nil, new(bytes.Buffer), new(bytes.Buffer))
		if done || o.background {
			t.Fatalf("o=%+v done=%v", o, done)
		}
	})
	t.Run("version", func(t *testing.T) {
		var out bytes.Buffer
		_, done, code := parseArgs([]string{"--version"}, &out, new(bytes.Buffer))
		if !done || code != 0 || !strings.Contains(out.String(), buildinfo.Version) {
			t.Fatalf("done=%v code=%d out=%q", done, code, out.String())
		}
	})
	t.Run("help exits 0 and names the flags", func(t *testing.T) {
		var errb bytes.Buffer
		_, done, code := parseArgs([]string{"--help"}, new(bytes.Buffer), &errb)
		if !done || code != 0 || !strings.Contains(errb.String(), "--background") {
			t.Fatalf("done=%v code=%d out=%q", done, code, errb.String())
		}
	})
	t.Run("unknown flag and stray argument are exit 2", func(t *testing.T) {
		for _, args := range [][]string{{"--nope"}, {"serve"}} {
			_, done, code := parseArgs(args, new(bytes.Buffer), new(bytes.Buffer))
			if !done || code != 2 {
				t.Errorf("%v: done=%v code=%d, want exit 2", args, done, code)
			}
		}
	})
}

// No graphical session, or one pcb-ui cannot open (live 2026-11-13, F8:
// over SSH with DISPLAY set and no X authority, Fyne printed its raw GLFW
// error and the process panicked, exit 2): a pcb message saying what is
// missing and what to do, exit 10 (the user must act), before any state is
// opened.
func TestCheckDisplay(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	opened := 0
	ok := func() error { opened++; return nil }
	err := checkDisplay(env(nil), ok)
	if err == nil || err.Error() != noDisplayLine || exitcode.From(err) != exitcode.MustAct || !strings.Contains(exitcode.HintOf(err), "pcb serve") || opened != 0 {
		t.Errorf("no display: err=%v (exit %d, hint %q), opened %d", err, exitcode.From(err), exitcode.HintOf(err), opened)
	}
	for _, k := range []string{"DISPLAY", "WAYLAND_DISPLAY"} {
		if err := checkDisplay(env(map[string]string{k: ":0"}), ok); err != nil {
			t.Errorf("%s set: %v", k, err)
		}
	}
	if opened != 2 {
		t.Errorf("the display was opened %d times, want once per check that has one", opened)
	}
	err = checkDisplay(env(map[string]string{"DISPLAY": ":0"}), func() error { return errors.New("X11: Failed to open display :0") })
	if err == nil || exitcode.From(err) != exitcode.MustAct ||
		err.Error() != "pcb-ui cannot open the display (DISPLAY=:0): X11: Failed to open display :0" ||
		exitcode.HintOf(err) != displayHint {
		t.Errorf("unusable display: err=%v (exit %d, hint %q)", err, exitcode.From(err), exitcode.HintOf(err))
	}
}

// The real binary's path with a display GLFW cannot open: the pcb message
// on stderr, exit 10, no Fyne error or panic, and no state created (the
// sealer is not chosen by a start that cannot show anything).
func TestUnusableDisplayIsAPcbMessage(t *testing.T) {
	if os.Getenv("PCB_UI_TEST_RUN") == "1" {
		t.Skip("the child")
	}
	tmp, err := os.MkdirTemp("", "pcbx") // short: a Wayland socket path is bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	runtimeDir := filepath.Join(tmp, "rt")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = []string{
		"PCB_UI_TEST_RUN=1", "HOME=" + tmp, "PATH=" + os.Getenv("PATH"),
		"DISPLAY=:247", "XDG_RUNTIME_DIR=" + runtimeDir, "XDG_SESSION_TYPE=x11",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(tmp, "nobus"),
		"PCB_CONFIG_DIR=" + filepath.Join(tmp, "cfg"), "PCB_STATE_DIR=" + filepath.Join(tmp, "state"),
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 10 {
		t.Fatalf("exit: %v, want 10\nstderr: %s", err, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{"Error: pcb-ui cannot open the display (DISPLAY=:247)", "Hint: " + displayHint} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
	for _, raw := range []string{"Fyne error", "panic", "goroutine"} {
		if strings.Contains(out, raw) {
			t.Errorf("stderr holds %q:\n%s", raw, out)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state directory was created: %v", err)
	}
}

// TestMain runs pcb-ui's run in the child of TestUnusableDisplayIsAPcbMessage,
// and classChild in that of TestWindowClassIsTheAppID (on the main thread,
// which GLFW needs).
func TestMain(m *testing.M) {
	if os.Getenv("PCB_UI_TEST_RUN") == "1" {
		os.Exit(run(nil))
	}
	if os.Getenv("PCB_UI_TEST_CLASS") == "1" {
		os.Exit(classChild())
	}
	os.Exit(m.Run())
}

func newSettings(t *testing.T) (*settings, *[]*url.URL) {
	t.Helper()
	var opened []*url.URL
	return &settings{
		level:      new(slog.LevelVar),
		logsDir:    "/state/logs",
		configPath: "/cfg/config.toml",
		open:       func(u *url.URL) error { opened = append(opened, u); return nil },
		exe:        func() (string, error) { return "/opt/pcb/bin/pcb-ui", nil },
	}, &opened
}

func TestSettingsOpenFiles(t *testing.T) {
	s, opened := newSettings(t)
	if err := s.OpenLogs(); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenConfig(); err != nil {
		t.Fatal(err)
	}
	got := []string{(*opened)[0].String(), (*opened)[1].String()}
	if got[0] != "file:///state/logs" || got[1] != "file:///cfg/config.toml" {
		t.Errorf("opened %v", got)
	}
}

func TestSettingsLevelIsTheLevelVar(t *testing.T) {
	s, _ := newSettings(t)
	s.SetLevel(slog.LevelDebug)
	if s.level.Level() != slog.LevelDebug || s.Level() != slog.LevelDebug {
		t.Errorf("level not applied: %v", s.level.Level())
	}
}

func TestSettingsAutostartWritesTheResolvedExecutable(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	s, _ := newSettings(t)
	if s.Autostart() {
		t.Fatal("autostart on in a fresh config dir")
	}
	if err := s.SetAutostart(true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg, "autostart", "pcb-ui.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Exec=/opt/pcb/bin/pcb-ui --background\n") || !s.Autostart() {
		t.Errorf("entry %q, Autostart=%v", b, s.Autostart())
	}
	if err := s.SetAutostart(false); err != nil {
		t.Fatal(err)
	}
	if s.Autostart() {
		t.Error("still on after SetAutostart(false)")
	}
}

func TestSettingsAutostartFailsWithoutExecutable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, _ := newSettings(t)
	s.exe = func() (string, error) { return "", errors.New("no exe") }
	if err := s.SetAutostart(true); err == nil || s.Autostart() {
		t.Errorf("err=%v autostart=%v, want an error and no entry", err, s.Autostart())
	}
}

func TestExecutableIsAbsoluteAndResolved(t *testing.T) {
	p, err := executable()
	if err != nil || !filepath.IsAbs(p) {
		t.Fatalf("%q, %v", p, err)
	}
	if r, _ := filepath.EvalSymlinks(p); r != p {
		t.Errorf("%q still has a symlink (%q)", p, r)
	}
}

func TestFileURLEscapesSpacesAndSpecials(t *testing.T) {
	u := fileURL("/home/my user/.local/state/pcb #1/logs")
	if got, want := u.String(), "file:///home/my%20user/.local/state/pcb%20%231/logs"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	back, err := url.Parse(u.String())
	if err != nil || back.Path != "/home/my user/.local/state/pcb #1/logs" {
		t.Errorf("round trip: %q, %v", back.Path, err)
	}
}

// The desktop entry names the app ID as its StartupWMClass: on Wayland the
// window's app_id is the app ID, and without the match the shell cannot tie
// the window to the entry, so its dock and switcher show no logo.
func TestDesktopEntryMatchesAppID(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "pcb-ui.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	want := "StartupWMClass=" + appID
	for _, l := range strings.Split(string(b), "\n") {
		if l == want {
			return
		}
	}
	t.Fatalf("packaging/pcb-ui.desktop lacks the line %q:\n%s", want, b)
}

// The windows pcb-ui opens on X11 (Fyne's choice on GNOME, through
// XWayland) carry the app ID as WM_CLASS, the desktop entry's
// StartupWMClass: GLFW's default, the title, left GNOME showing no logo.
// It runs on its own Xvfb; CI installs Xvfb and xprop, elsewhere a machine
// without them skips it.
func TestWindowClassIsTheAppID(t *testing.T) {
	xvfb, err := exec.LookPath("Xvfb")
	if err == nil {
		_, err = exec.LookPath("xprop")
	}
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI must install Xvfb and xprop (xvfb, x11-utils): %v", err)
		}
		t.Skipf("needs Xvfb and xprop: %v", err)
	}
	display := startXvfb(t, xvfb)
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = []string{
		"PCB_UI_TEST_CLASS=1", "HOME=" + tmp, "PATH=" + os.Getenv("PATH"),
		"DISPLAY=" + display, "FYNE_PLATFORM=x11", "XDG_RUNTIME_DIR=" + tmp,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(tmp, "nobus"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	want := `WM_CLASS(STRING) = "` + appID + `", "` + appID + `"`
	if !strings.Contains(string(out), want) {
		t.Fatalf("the window's class is not the app ID, want %q:\n%s", want, out)
	}
}

// startXvfb starts an Xvfb on a free display and returns its name.
func startXvfb(t *testing.T, xvfb string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	srv := exec.Command(xvfb, "-displayfd", "3", "-nolisten", "tcp", "-screen", "0", "1024x768x24")
	srv.ExtraFiles = []*os.File{w}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
		_ = r.Close()
	})
	got := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(r).ReadString('\n')
		got <- strings.TrimSpace(l)
	}()
	select {
	case n := <-got:
		if n == "" {
			t.Fatal("Xvfb wrote no display number")
		}
		return ":" + n
	case <-time.After(20 * time.Second):
		t.Fatal("Xvfb did not start within 20 s")
	}
	return ""
}

// classChild opens a window as pcb-ui does and prints xprop's WM_CLASS of
// it: exit 0 when xprop ran.
func classChild() int {
	a := app.NewWithID(appID)
	w := a.NewWindow("pcb")
	setWindowClass()
	w.Show()
	code := 1
	go fyne.Do(func() {
		w.(driver.NativeWindow).RunNative(func(ctx any) {
			defer a.Quit()
			x, ok := ctx.(driver.X11WindowContext)
			if !ok {
				fmt.Fprintf(os.Stderr, "not an X11 window: %T\n", ctx)
				return
			}
			out, err := exec.Command("xprop", "-id", fmt.Sprint(x.WindowHandle), "WM_CLASS").CombinedOutput()
			_, _ = os.Stdout.Write(out)
			if err == nil {
				code = 0
			}
		})
	})
	a.Run()
	return code
}
