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
	"sync"
	"syscall"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
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
	acltest.WantPrivate(t, path, 0o600)
	acltest.WantPrivate(t, filepath.Dir(path), 0o700)
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
		t.Skip("Unix modes: the access-list twin is TestKeyFileWithAnEveryoneACEIsRefused")
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
		var me *seal.KeyModeError
		if err == nil || !strings.Contains(err.Error(), "chmod 600 "+path) || !errors.Is(err, seal.ErrKeyMode) || !errors.As(err, &me) || me.Mode != 0o644 {
			t.Fatalf("%s: err = %v, want a KeyModeError with mode 0644", name, err)
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

func TestConcurrentFirstSealsAgreeOnOneKey(t *testing.T) {
	path := keyPath(t)
	const n = 8
	blobs := make([][]byte, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, err := (&seal.KeyFile{Path: path}).Seal([]byte(fmt.Sprintf("secret-%d", i)), nil)
			if err != nil {
				t.Error(err)
			}
			blobs[i] = b
		}()
	}
	close(start)
	wg.Wait()
	key, _ := os.ReadFile(path)
	fresh := &seal.KeyFile{Path: path}
	for i, b := range blobs {
		got, err := fresh.Open(b, nil)
		if err != nil || string(got) != fmt.Sprintf("secret-%d", i) {
			t.Fatalf("blob %d: %q, %v", i, got, err)
		}
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(key, again) {
		t.Fatal("key changed after creation")
	}
}

func TestKeyCreationFallsBackWithoutHardLinks(t *testing.T) {
	for _, cause := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EXDEV, syscall.EMLINK} {
		restore := seal.SetLink(func(o, n string) error { return &os.LinkError{Op: "link", Old: o, New: n, Err: cause} })
		path := keyPath(t)
		k := &seal.KeyFile{Path: path}
		sealed, err := k.Seal([]byte("x"), nil)
		restore()
		if err != nil {
			t.Fatalf("%v: %v", cause, err)
		}
		got, err := (&seal.KeyFile{Path: path}).Open(sealed, nil)
		if err != nil || string(got) != "x" {
			t.Fatalf("%v: %q, %v", cause, got, err)
		}
		acltest.WantPrivate(t, path, 0o600)
		if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".key-*")); len(left) != 0 {
			t.Fatalf("temporary key left: %v", left)
		}
	}
}

func TestKeyCreationReportsOtherLinkErrors(t *testing.T) {
	restore := seal.SetLink(func(o, n string) error { return &os.LinkError{Op: "link", Old: o, New: n, Err: syscall.EIO} })
	defer restore()
	path := keyPath(t)
	_, err := (&seal.KeyFile{Path: path}).Seal([]byte("x"), nil)
	if err == nil || !strings.Contains(err.Error(), path) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("a key was created despite the error")
	}
}

// Check reads the key as Seal and Open do, without creating it: a usable
// file is nil, a missing one is fs.ErrNotExist and stays missing, a loose
// mode and a wrong size are refused.
func TestKeyFileCheck(t *testing.T) {
	path := keyPath(t)
	k := &seal.KeyFile{Path: path}
	if err := k.Check(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Check with no key file = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Check created the key file: %v", err)
	}
	if _, err := k.Seal([]byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if err := k.Check(); err != nil {
		t.Fatalf("Check on a usable key = %v, want nil", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := k.Check(); !errors.Is(err, seal.ErrKeyMode) {
			t.Errorf("Check on a 0644 key = %v, want ErrKeyMode", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := k.Check(); err == nil {
		t.Error("Check accepted a 5-byte key")
	}
}
