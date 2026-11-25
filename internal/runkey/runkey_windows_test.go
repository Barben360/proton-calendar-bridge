package runkey_test

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"golang.org/x/sys/windows/registry"

	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
)

// A test-only subkey: the Run value of the machine's user is never written.
func TestRunKeyWriteDelete(t *testing.T) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sub := `Software\pcb-test-` + hex.EncodeToString(b)
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, sub) })
	s := runkey.At(sub)

	if _, ok, err := s.Get(); ok || err != nil {
		t.Fatalf("Get before Set = ok %v, err %v", ok, err)
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete of a missing value: %v", err)
	}
	data := runkey.Data(`C:\Users\Zoë O'Brien\pcb-ui.exe`)
	if err := s.Set(data); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Get(); got != data || !ok || err != nil {
		t.Fatalf("Get = %q, %v, %v; want %q", got, ok, err, data)
	}
	// The value really is in the registry, under the name pcb-ui.
	k, err := registry.OpenKey(registry.CURRENT_USER, sub, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := k.GetStringValue(runkey.Value)
	_ = k.Close()
	if err != nil || got != data {
		t.Fatalf("registry value = %q, %v", got, err)
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(); ok || err != nil {
		t.Fatalf("Get after Delete = ok %v, err %v", ok, err)
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}
