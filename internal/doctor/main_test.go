package doctor_test

import (
	"os"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
)

func TestMain(m *testing.M) {
	// The checks that run the whole list never bind a real port (the
	// developer's own pcb serve may hold 1080); the tests of the bind
	// probe give doctor.BindProbe to the check themselves.
	doctor.DefaultProbe = func(string) error { return nil }
	os.Exit(m.Run())
}
