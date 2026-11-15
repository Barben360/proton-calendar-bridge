package cli

import (
	"runtime"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
)

// With no state directory to find the control channel in, the daemon line
// is a skip naming it: the socket on Unix, the pipe on Windows.
func TestDoctorDaemonUnplacedNamesTheChannel(t *testing.T) {
	want := "the control socket cannot be located: offline checks only"
	if runtime.GOOS == "windows" {
		want = "the control pipe cannot be located: offline checks only"
	}
	got := doctorDaemon{unplaced: true}.results()
	if len(got) != 1 || got[0].Name != "daemon" || got[0].Status != doctor.StatusSkip || got[0].Message != want {
		t.Errorf("results = %+v, want one skip %q", got, want)
	}
}
