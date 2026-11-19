package cli_test

import (
	"os"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
)

func TestMain(m *testing.M) {
	// A new state never reaches the developer's desktop keyring.
	_ = os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	// pcb doctor never binds the default CalDAV port: the developer's own
	// pcb serve may hold it. Any other port is really probed, so a test
	// can hold one of its own.
	doctor.DefaultProbe = func(addr string) error {
		if addr == config.DefaultListen {
			return nil
		}
		return doctor.BindProbe(addr)
	}
	os.Exit(m.Run())
}
