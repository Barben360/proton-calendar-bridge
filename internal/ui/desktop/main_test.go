package desktop

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// A new state (bridgeLab's) never reaches the developer's desktop
	// keyring; the D-Bus tests pass their own bus address.
	_ = os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	os.Exit(m.Run())
}
