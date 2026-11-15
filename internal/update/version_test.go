package update_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

func TestCheckable(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.1.0": true, "v0.2.0-rc.1": true, "v10.20.30": true,
		"dev": false, "v0.1.0-3-gabc123": false, "v0.1.0-dirty": false, "v0.1.0-rc.1-dirty": false,
		"0.1.0": false, "v0.1": false, "v0.1.0+meta": false, "": false, "v01.0.0": false,
	} {
		if got := update.Checkable(v); got != want {
			t.Errorf("Checkable(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		installed, latest string
		want              bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.1.0", "v0.1.1", true},
		{"v0.1.0", "v1.0.0", true},
		{"v0.1.0-rc.1", "v0.1.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.2.0", "v0.1.0", false},
		{"v0.2.0-rc.1", "v0.1.0", false},
		{"v0.9.0", "v0.10.0", true},
		{"v0.10.0", "v0.9.0", false},
		{"dev", "v0.1.0", false},
		{"v0.1.0", "garbage", false},
	} {
		if got := update.Newer(c.installed, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.installed, c.latest, got, c.want)
		}
	}
}
