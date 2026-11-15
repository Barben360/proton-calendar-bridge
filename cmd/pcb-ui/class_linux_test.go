//go:build linux && cgo

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/driver"
)

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

func init() { childHook = classChild }

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
