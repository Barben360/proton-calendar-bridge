package seal_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

func TestDPAPIName(t *testing.T) {
	if got := (&seal.DPAPI{}).Name(); got != "dpapi" {
		t.Fatalf("Name = %q, want dpapi", got)
	}
	if seal.DPAPIKeyName != "secrets.key.dpapi" {
		t.Fatalf("DPAPIKeyName = %q", seal.DPAPIKeyName)
	}
}

// Off Windows there is no DPAPI: every use fails with ErrNoDPAPI and
// nothing is written, not even the key's directory.
func TestDPAPIOffWindowsFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("DPAPI works here: TestDPAPIRoundTrip")
	}
	dir := filepath.Join(t.TempDir(), "state")
	d := &seal.DPAPI{Path: filepath.Join(dir, seal.DPAPIKeyName)}
	if _, err := d.Seal([]byte("x"), nil); !errors.Is(err, seal.ErrNoDPAPI) {
		t.Fatalf("Seal = %v, want ErrNoDPAPI", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Seal created %s (%v)", dir, err)
	}
	// A key copied from Windows is not opened either.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.Path, make([]byte, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open(make([]byte, 64), nil); !errors.Is(err, seal.ErrNoDPAPI) || errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrNoDPAPI (not ErrCorrupt)", err)
	}
	if err := d.Check(); !errors.Is(err, seal.ErrNoDPAPI) {
		t.Fatalf("Check = %v, want ErrNoDPAPI", err)
	}
}

// When Windows cannot protect a new key (a logon without the user's
// password, such as SSH with a key), Seal says so (ErrProtect, the cause
// kept) and writes nothing.
func TestDPAPIProtectFailureIsErrProtect(t *testing.T) {
	cause := errors.New("access is denied")
	defer seal.SetProtect(func([]byte) ([]byte, error) { return nil, cause })()
	path := filepath.Join(t.TempDir(), "state", seal.DPAPIKeyName)
	_, err := (&seal.DPAPI{Path: path}).Seal([]byte("x"), nil)
	if !errors.Is(err, seal.ErrProtect) || !errors.Is(err, cause) || errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("Seal = %v, want ErrProtect wrapping the cause", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a key was written (%v)", err)
	}
}

// An existing key Windows cannot unprotect is corrupt only for an
// integrity failure; any other failure (a logon without the password,
// another user's blob, an unknown code) is a *ProtectError: the key may
// open in another session, so nothing calls it lost, and Seal never
// replaces it.
func TestDPAPIUnprotectFailuresAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cause   error
		corrupt bool
	}{
		{"access denied", syscall.Errno(5), false},          // ERROR_ACCESS_DENIED: a key logon
		{"bad key state", syscall.Errno(0x8009000B), false}, // NTE_BAD_KEY_STATE: another user, no master key
		{"unknown", errors.New("odd"), false},               // anything else
		{"bad data", syscall.Errno(0x80090005), true},       // NTE_BAD_DATA
		{"invalid data", syscall.Errno(13), true},           // ERROR_INVALID_DATA
		{"hash value", syscall.Errno(0x80091007), true},     // CRYPT_E_HASH_VALUE
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", seal.DPAPIKeyName)
			if err := acl.MkdirAll(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
			blob := bytes.Repeat([]byte{1}, 200)
			if err := os.WriteFile(path, blob, 0o600); err != nil {
				t.Fatal(err)
			}
			cause := fmt.Errorf("CryptUnprotectData: %w", tc.cause)
			defer seal.SetUnprotect(func([]byte) ([]byte, error) { return nil, cause })()
			d := &seal.DPAPI{Path: path}
			for name, run := range map[string]func() error{
				"check": d.Check,
				"open":  func() error { _, err := d.Open(make([]byte, 64), nil); return err },
				"seal":  func() error { _, err := d.Seal([]byte("x"), nil); return err },
			} {
				err := run()
				var pe *seal.ProtectError
				switch {
				case !errors.Is(err, tc.cause):
					t.Errorf("%s = %v: the cause is lost", name, err)
				case tc.corrupt && (!errors.Is(err, seal.ErrCorrupt) || errors.Is(err, seal.ErrProtect)):
					t.Errorf("%s = %v, want ErrCorrupt", name, err)
				case !tc.corrupt && (!errors.As(err, &pe) || !pe.Unprotect || errors.Is(err, seal.ErrCorrupt) ||
					!strings.HasPrefix(err.Error(), "DPAPI could not unprotect the sealing key for this Windows user: ")):
					t.Errorf("%s = %v, want an unprotect ProtectError, not ErrCorrupt", name, err)
				}
			}
			if again, _ := os.ReadFile(path); !bytes.Equal(again, blob) {
				t.Error("Seal replaced a key it could not unprotect")
			}
		})
	}
}
