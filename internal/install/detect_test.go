package install_test

import (
	"context"
	"errors"
	"slices"
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

// A user uninstall acts on the directories systemd says the unit runs with
// (M5-R6), quoting included, and never on the caller's environment: when
// systemd cannot tell, ScopeDirs fails.
func TestScopeDirsAskSystemd(t *testing.T) {
	const show = "systemctl --user show pcb.service -p Environment"
	s := newSys(t)
	s.env["PCB_CONFIG_DIR"] = "/caller/pcb"
	s.env["PCB_STATE_DIR"] = "/caller/state/pcb"
	unit := install.UserUnit(install.UnitParams{Exec: "/bin/pcb", ConfigDir: "/srv/pcb-config", StateDir: "/srv/pcb-state"})
	s.write("/home/alice/.config/systemd/user/pcb.service", string(unit), 0o644)
	dirs, err := install.ScopeDirs(context.Background(), s.host(), install.ScopeUser)
	if want := (paths.Dirs{Config: "/srv/pcb-config", State: "/srv/pcb-state"}); err != nil || dirs != want {
		t.Fatalf("dirs = %+v, %v; want %+v", dirs, err, want)
	}
	if !slices.Contains(s.calls, show) {
		t.Fatalf("calls = %q, want %q", s.calls, show)
	}

	// A drop-in's quoted value, as systemctl prints it.
	s.out[show] = `Environment="PCB_CONFIG_DIR=/srv/my config/pcb" PCB_STATE_DIR=/srv/state/pcb LANG=C` + "\n"
	dirs, err = install.ScopeDirs(context.Background(), s.host(), install.ScopeUser)
	if want := (paths.Dirs{Config: "/srv/my config/pcb", State: "/srv/state/pcb"}); err != nil || dirs != want {
		t.Fatalf("quoted: dirs = %+v, %v; want %+v", dirs, err, want)
	}

	for what, set := range map[string]func(){
		"no environment":   func() { s.out[show] = "Environment=\n" },
		"one variable":     func() { s.out[show] = "Environment=PCB_STATE_DIR=/srv/state/pcb\n" },
		"systemctl failed": func() { s.out[show] = ""; s.codes[show] = 1 },
	} {
		set()
		if dirs, err := install.ScopeDirs(context.Background(), s.host(), install.ScopeUser); err == nil {
			t.Errorf("%s: dirs = %+v, want a failure, not the caller's environment", what, dirs)
		}
		delete(s.codes, show)
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
