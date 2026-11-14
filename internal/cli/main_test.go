package cli_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// A new state never reaches the developer's desktop keyring.
	_ = os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	os.Exit(m.Run())
}
