package paths_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

func TestResolveXDG(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		env := map[string]string{"XDG_CONFIG_HOME": "/x/cfg", "XDG_STATE_HOME": "/x/state", "HOME": "/home/u"}
		got, err := paths.Resolve(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if want := (paths.Dirs{Config: "/x/cfg/pcb", State: "/x/state/pcb"}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("unset falls back to HOME", func(t *testing.T) {
		env := map[string]string{"HOME": "/home/u"}
		got, err := paths.Resolve(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if want := (paths.Dirs{Config: "/home/u/.config/pcb", State: "/home/u/.local/state/pcb"}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("relative XDG is ignored", func(t *testing.T) {
		env := map[string]string{"XDG_CONFIG_HOME": "rel/cfg", "XDG_STATE_HOME": "rel/state", "HOME": "/home/u"}
		got, err := paths.Resolve(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if want := (paths.Dirs{Config: "/home/u/.config/pcb", State: "/home/u/.local/state/pcb"}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("absolute XDG wins without HOME", func(t *testing.T) {
		env := map[string]string{"XDG_CONFIG_HOME": "/x/cfg", "XDG_STATE_HOME": "/x/state"}
		got, err := paths.Resolve(func(k string) string { return env[k] })
		if err != nil || got.Config != "/x/cfg/pcb" || got.State != "/x/state/pcb" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("no absolute base is an error", func(t *testing.T) {
		for _, env := range []map[string]string{
			{},
			{"HOME": "relative/home"},
			{"XDG_CONFIG_HOME": "rel", "XDG_STATE_HOME": "rel"},
			{"XDG_CONFIG_HOME": "/x/cfg"}, // state has no base
		} {
			if got, err := paths.Resolve(func(k string) string { return env[k] }); err == nil {
				t.Errorf("env %v: got %+v, want error", env, got)
			}
		}
	})
}
