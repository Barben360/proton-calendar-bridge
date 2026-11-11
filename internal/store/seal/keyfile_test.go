package seal_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

func keyPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "cfg", "secrets.key")
}

func TestKeyFileRoundTrip(t *testing.T) {
	k := &seal.KeyFile{Path: keyPath(t)}
	sealed, err := k.Seal([]byte("hunter2"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("plaintext visible in the sealed blob")
	}
	got, err := k.Open(sealed, []byte("ctx"))
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if k.Name() != "keyfile" {
		t.Fatalf("name = %q", k.Name())
	}
	again, _ := k.Seal([]byte("hunter2"), []byte("ctx"))
	if bytes.Equal(sealed, again) {
		t.Fatal("nonce reused: two seals are identical")
	}
}

func TestKeyFileCreatesKeyOnce(t *testing.T) {
	path := keyPath(t)
	a, b := &seal.KeyFile{Path: path}, &seal.KeyFile{Path: path}
	sealed, err := a.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	got, err := b.Open(sealed, nil)
	if err != nil || string(got) != "x" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := b.Seal([]byte("y"), nil); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(first, again) || len(first) != 32 {
		t.Fatalf("key changed or wrong size (%d)", len(first))
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("key mode %o", info.Mode().Perm())
		}
		dir, _ := os.Stat(filepath.Dir(path))
		if dir.Mode().Perm() != 0o700 {
			t.Fatalf("key dir mode %o", dir.Mode().Perm())
		}
	}
}

func TestOpenWithoutKeyIsErrNoKey(t *testing.T) {
	path := keyPath(t)
	_, err := (&seal.KeyFile{Path: path}).Open(make([]byte, 64), nil)
	if !errors.Is(err, seal.ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("Open created a key")
	}
}

func TestKeyFileRefusesLooseMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	path := keyPath(t)
	k := &seal.KeyFile{Path: path}
	sealed, err := k.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"open": func() error { _, err := k.Open(sealed, nil); return err },
		"seal": func() error { _, err := k.Seal([]byte("x"), nil); return err },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "chmod 600 "+path) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestKeyFileRefusesWrongSize(t *testing.T) {
	path := keyPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&seal.KeyFile{Path: path}).Seal([]byte("x"), nil); err == nil {
		t.Fatal("a 5-byte key was accepted")
	}
}

func TestTamperedIsErrCorrupt(t *testing.T) {
	k := &seal.KeyFile{Path: keyPath(t)}
	sealed, _ := k.Seal([]byte("secret"), nil)
	for _, i := range []int{0, len(sealed) - 1} { // nonce, tag
		bad := bytes.Clone(sealed)
		bad[i] ^= 1
		if _, err := k.Open(bad, nil); !errors.Is(err, seal.ErrCorrupt) {
			t.Fatalf("flip %d: err = %v", i, err)
		}
	}
	if _, err := k.Open(sealed[:5], nil); !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("short blob: err = %v", err)
	}
}

func TestAADBindsContext(t *testing.T) {
	k := &seal.KeyFile{Path: keyPath(t)}
	sealed, _ := k.Seal([]byte("secret"), []byte("A"))
	if _, err := k.Open(sealed, []byte("B")); !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
}
