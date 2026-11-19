package desktop

import (
	"crypto/rand"
	"fmt"
	"testing"

	"golang.org/x/sys/windows/registry"

	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
)

// "Start at login" on Windows writes the Run value pcb-ui with the quoted
// exe and --background, and switching it off deletes it. A test subkey
// stands in for the Run key: the real autostart of the user running the
// tests is never touched.
func TestSetAutostartWritesTheRunValue(t *testing.T) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sub := fmt.Sprintf(`Software\pcb-test-%x`, b)
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, sub) })
	old := runKey
	runKey = func() runkey.Store { return runkey.At(sub) }
	t.Cleanup(func() { runKey = old })

	exe := `C:\x y\Zoë O'Brien\pcb-ui.exe`
	if Autostart(exe) {
		t.Fatal("autostart on before any SetAutostart")
	}
	if err := SetAutostart(true, exe); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := runkey.At(sub).Get(); got != `"C:\x y\Zoë O'Brien\pcb-ui.exe" --background` || !ok || err != nil {
		t.Errorf("Run value = %q, %v, %v", got, ok, err)
	}
	if !Autostart(exe) {
		t.Error("Autostart(exe) = false after SetAutostart(true)")
	}
	if Autostart(`C:\elsewhere\pcb-ui.exe`) {
		t.Error("Autostart is on for an executable the value does not start")
	}
	if err := SetAutostart(false, exe); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := runkey.At(sub).Get(); ok || err != nil {
		t.Errorf("the value is still there after SetAutostart(false): ok %v, err %v", ok, err)
	}
	if Autostart(exe) {
		t.Error("Autostart() = true after SetAutostart(false)")
	}
}

// The real Run key, read only: the package's default store is the user's
// own Run key.
func TestRunKeyIsTheUsersRunKey(t *testing.T) {
	if runKey() != runkey.System() {
		t.Error("runKey is not runkey.System()")
	}
}
