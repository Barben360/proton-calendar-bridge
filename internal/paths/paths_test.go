package paths_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

func TestResolveXDG(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		env := map[string]string{"XDG_CONFIG_HOME": "/x/cfg", "XDG_STATE_HOME": "/x/state", "HOME": "/home/u"}
		got := paths.Resolve(func(k string) string { return env[k] })
		if want := (paths.Dirs{Config: "/x/cfg/pcb", State: "/x/state/pcb"}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("unset falls back to HOME", func(t *testing.T) {
		env := map[string]string{"HOME": "/home/u"}
		got := paths.Resolve(func(k string) string { return env[k] })
		if want := (paths.Dirs{Config: "/home/u/.config/pcb", State: "/home/u/.local/state/pcb"}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}
