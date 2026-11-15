package runkey_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
)

func TestDataQuotesTheExe(t *testing.T) {
	t.Parallel()
	got := runkey.Data(`C:\Program Files\pcb\pcb-ui.exe`)
	if want := `"C:\Program Files\pcb\pcb-ui.exe" --background`; got != want {
		t.Errorf("Data = %q, want %q", got, want)
	}
}

// Review Focus 1: the value must start the right exe whatever the profile
// path holds (space, apostrophe, non-ASCII).
func TestExeParsesData(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		`C:\Users\Zoë O'Brien\AppData\Local\pcb\pcb-ui.exe`,
		`C:\Users\Zoë O’Brien\pcb-ui.exe`,
		`C:\a b\pcb-ui.exe`,
		`C:\x\pcb-ui.exe`,
	} {
		if got, ok := runkey.Exe(runkey.Data(p)); !ok || got != p {
			t.Errorf("Exe(Data(%q)) = %q, %v", p, got, ok)
		}
	}
	for _, c := range []struct {
		data, want string
		ok         bool
	}{
		{`C:\x\pcb-ui.exe --background`, `C:\x\pcb-ui.exe`, true},
		{`C:\x\pcb-ui.exe`, `C:\x\pcb-ui.exe`, true},
		{`"C:\x`, "", false},
		{`""`, "", false},
		{"", "", false},
		{"   ", "", false},
	} {
		if got, ok := runkey.Exe(c.data); got != c.want || ok != c.ok {
			t.Errorf("Exe(%q) = %q, %v; want %q, %v", c.data, got, ok, c.want, c.ok)
		}
	}
}

func TestValueAndKeyPath(t *testing.T) {
	t.Parallel()
	if runkey.Value != "pcb-ui" || runkey.KeyPath != `Software\Microsoft\Windows\CurrentVersion\Run` {
		t.Errorf("Value=%q KeyPath=%q", runkey.Value, runkey.KeyPath)
	}
}
