//go:build windows

package paths_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveWindowsDefaults(t *testing.T) {
	env := map[string]string{`APPDATA`: `C:\Users\u\AppData\Roaming`, `LOCALAPPDATA`: `C:\Users\u\AppData\Local`,
		`HOME`: `C:\h`, `XDG_CONFIG_HOME`: `C:\x`, `XDG_STATE_HOME`: `C:\y`}
	got, err := paths.Resolve(envOf(env))
	want := paths.Dirs{Config: `C:\Users\u\AppData\Roaming\pcb`, State: `C:\Users\u\AppData\Local\pcb`}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v (HOME and XDG are ignored)", got, err, want)
	}
}

func TestResolveWindowsOverrides(t *testing.T) {
	env := map[string]string{`PCB_CONFIG_DIR`: `D:\c`, `PCB_STATE_DIR`: `D:\s`,
		`APPDATA`: `C:\a`, `LOCALAPPDATA`: `C:\l`}
	got, err := paths.Resolve(envOf(env))
	if want := (paths.Dirs{Config: `D:\c`, State: `D:\s`}); err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	_, err = paths.Resolve(envOf(map[string]string{`PCB_CONFIG_DIR`: `rel\c`, `APPDATA`: `C:\a`, `LOCALAPPDATA`: `C:\l`}))
	if err == nil || err.Error() != "PCB_CONFIG_DIR must be an absolute path" {
		t.Fatalf("relative override: %v", err)
	}
}

func TestResolveWindowsNeedsAbsoluteBases(t *testing.T) {
	const cfg = "cannot locate the user directories: set PCB_CONFIG_DIR or APPDATA to an absolute path"
	const state = "cannot locate the user directories: set PCB_STATE_DIR or LOCALAPPDATA to an absolute path"
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, cfg},
		{map[string]string{`APPDATA`: `C:\a`}, state},
		{map[string]string{`APPDATA`: `rel`, `LOCALAPPDATA`: `rel`}, cfg},
		{map[string]string{`HOME`: `C:\h`}, cfg},
	} {
		got, err := paths.Resolve(envOf(tc.env))
		if err == nil || err.Error() != tc.want {
			t.Errorf("env %v: got %+v, %v; want error %q", tc.env, got, err, tc.want)
		}
	}
}
