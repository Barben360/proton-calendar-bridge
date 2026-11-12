package install_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

func TestDetect(t *testing.T) {
	const userUnit = "/home/alice/.config/systemd/user/pcb.service"
	cases := []struct {
		name      string
		units     []string
		env       map[string]string
		scope     install.Scope
		unit      string
		wantError error
	}{
		{name: "neither", wantError: install.ErrNotInstalled},
		{name: "user", units: []string{userUnit}, scope: install.ScopeUser, unit: userUnit},
		{name: "system", units: []string{install.SystemUnitPath}, scope: install.ScopeSystem, unit: install.SystemUnitPath},
		{name: "both", units: []string{userUnit, install.SystemUnitPath}, wantError: install.ErrAmbiguous},
		{name: "XDG_CONFIG_HOME", units: []string{"/xdg/systemd/user/pcb.service"},
			env: map[string]string{"XDG_CONFIG_HOME": "/xdg"}, scope: install.ScopeUser, unit: "/xdg/systemd/user/pcb.service"},
		{name: "XDG_CONFIG_HOME hides ~/.config", units: []string{userUnit},
			env: map[string]string{"XDG_CONFIG_HOME": "/xdg"}, wantError: install.ErrNotInstalled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSys(t)
			for k, v := range c.env {
				s.env[k] = v
			}
			for _, u := range c.units {
				s.write(u, "[Unit]\n", 0o644)
			}
			scope, unit, err := install.Detect(s.host())
			if !errors.Is(err, c.wantError) || scope != c.scope || unit != c.unit {
				t.Fatalf("Detect = %q, %q, %v; want %q, %q, %v", scope, unit, err, c.scope, c.unit, c.wantError)
			}
		})
	}
}

// A user uninstall acts on the directories the unit runs with, not on the
// caller's current environment (M5-P18).
func TestScopeDirsFollowTheUserUnit(t *testing.T) {
	s := newSys(t)
	unit := install.UserUnit(install.UnitParams{Exec: "/bin/pcb", ConfigDir: "/srv/pcb-config", StateDir: "/srv/pcb-state"})
	s.write("/home/alice/.config/systemd/user/pcb.service", string(unit), 0o644)
	dirs, err := install.ScopeDirs(s.host(), install.ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if want := (paths.Dirs{Config: "/srv/pcb-config", State: "/srv/pcb-state"}); dirs != want {
		t.Fatalf("dirs = %+v, want %+v", dirs, want)
	}
	if err := os.Remove(filepath.Join(s.root, "home/alice/.config/systemd/user/pcb.service")); err != nil {
		t.Fatal(err)
	}
	dirs, err = install.ScopeDirs(s.host(), install.ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if want := (paths.Dirs{Config: "/home/alice/.config/pcb", State: "/home/alice/.local/state/pcb"}); dirs != want {
		t.Fatalf("without a unit dirs = %+v, want %+v", dirs, want)
	}
}

func TestShowParsesTheProperties(t *testing.T) {
	s := newSys(t)
	s.out["systemctl show pcb.service --property=ActiveState,SubState,UnitFileState,MainPID"] = "ActiveState=failed\nSubState=failed\nUnitFileState=enabled\nMainPID=0\n"
	st, err := install.Show(context.Background(), s.host(), install.ScopeSystem)
	if err != nil {
		t.Fatal(err)
	}
	want := install.UnitStatus{Scope: install.ScopeSystem, Unit: install.SystemUnitPath, ActiveState: "failed", SubState: "failed", UnitFileState: "enabled"}
	if st != want {
		t.Fatalf("Show = %+v, want %+v", st, want)
	}
}
