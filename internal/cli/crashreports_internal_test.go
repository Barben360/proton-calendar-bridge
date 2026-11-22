package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// stateFiles lists every file and directory under dir, relative to it.
func stateFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// startApp is the app pcb's start-up sees: a build with a crash report
// address, the state directory state and the effective UID euid.
func startApp(root string, euid int) *app {
	env := map[string]string{"PCB_STATE_DIR": filepath.Join(root, "state"), "PCB_CONFIG_DIR": filepath.Join(root, "cfg")}
	return &app{IO: IO{
		Env:       func(k string) string { return env[k] },
		ReportDSN: "https://key@reports.example/7",
		Host:      install.Host{Euid: func() int { return euid }},
		Exe:       func() (string, error) { return "", errors.New("no binary in tests") },
	}}
}

// sudo pcb … with HOME still the user's: root must not leave a file of its
// own in the user's state directory (crash file, reports directory, lock).
// Not parallel: the crash output and the recorder are the process's.
func TestExecuteAsRootOnUserStateMakesNoReportFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the root check is Unix's: Windows checks the directory's owner and access list")
	}
	if os.Geteuid() == 0 {
		t.Skip("needs a state directory that root does not own")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}

	a := startApp(root, 0)
	stop := a.startReports(newRoot(a), []string{"status"})
	got := stateFiles(t, state)
	installed := report.Current() != nil
	stop()
	if len(got) != 0 || installed {
		t.Errorf("as root on the user's state: files %v, recorder installed %v; want none", got, installed)
	}

	// The same start as the directory's owner keeps a crash file: what
	// stopped root was the owner check.
	a = startApp(root, os.Geteuid())
	stop = a.startReports(newRoot(a), []string{"status"})
	got = stateFiles(t, state)
	p := report.Current()
	stop()
	crash := false
	for _, f := range got {
		crash = crash || strings.HasPrefix(filepath.Base(f), "crash-pcb-") && strings.HasSuffix(f, ".txt")
	}
	if !crash || p == nil || p.Meta.Command != "pcb status" || p.Meta.Binary != "pcb" {
		t.Errorf("as the owner: files %v, recorder %+v; want a crash file and a recorder of pcb status", got, p)
	}
	if report.Current() != nil {
		t.Error("stop left the recorder installed")
	}
	for _, f := range stateFiles(t, state) {
		if strings.HasPrefix(filepath.Base(f), "crash-") {
			t.Errorf("stop left %s", f)
		}
	}
}

func TestStartReportsWithoutAddressDoesNothing(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	a := startApp(root, os.Geteuid())
	a.ReportDSN = ""
	stop := a.startReports(newRoot(a), []string{"status"})
	got := stateFiles(t, state)
	installed := report.Current() != nil
	stop()
	if len(got) != 0 || installed {
		t.Errorf("no address: files %v, recorder installed %v; want none", got, installed)
	}
}

func TestServeReportsLogsWaitingReportsInAskMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    report.Mode
		pending bool
		dsn     string
		want    bool
	}{
		{"ask with a report", report.Ask, true, "http://k@127.0.0.1:1/1", true},
		{"ask with none", report.Ask, false, "http://k@127.0.0.1:1/1", false},
		{"always", report.Always, true, "http://k@127.0.0.1:1/1", false},
		{"never", report.Never, true, "http://k@127.0.0.1:1/1", false},
		{"no address", report.Ask, true, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := report.Store{StateDir: t.TempDir(), Env: func(string) string { return "" }}
			if err := store.SetMode(t.Context(), c.mode); err != nil {
				t.Fatal(err)
			}
			if c.pending {
				ev := report.PanicEvent("x", []report.Frame{{Function: "github.com/Barben360/proton-calendar-bridge/internal/daemon.f", InApp: true}}, report.Meta{}, time.Now())
				if err := store.Save(t.Context(), ev); err != nil {
					t.Fatal(err)
				}
			}
			report.Install(&report.Process{Store: store, DSN: c.dsn})
			defer report.Uninstall()
			var logs bytes.Buffer
			sender, poke, unsubscribe := serveReports(slog.New(slog.NewTextHandler(&logs, nil)))
			unsubscribe()
			got := strings.Contains(logs.String(), "crash reports waiting: pcb crash-reports show")
			if got != c.want {
				t.Errorf("logged %v, want %v: %q", got, c.want, logs.String())
			}
			if (sender != nil) != (c.dsn != "") || (poke != nil) != (c.dsn != "") {
				t.Errorf("sender %v, poke %v with dsn %q", sender, poke, c.dsn)
			}
		})
	}
}
