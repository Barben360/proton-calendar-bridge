//go:build windows

package seal_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

func dpapiPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "state", seal.DPAPIKeyName)
}

func TestDPAPIRoundTrip(t *testing.T) {
	path := dpapiPath(t)
	d := &seal.DPAPI{Path: path}
	sealed, err := d.Seal([]byte("hunter2"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("plaintext visible in the sealed blob")
	}
	for name, o := range map[string]*seal.DPAPI{"same": d, "fresh": {Path: path}} {
		got, err := o.Open(sealed, []byte("ctx"))
		if err != nil || string(got) != "hunter2" {
			t.Fatalf("%s: Open = %q, %v", name, got, err)
		}
	}
	if _, err := d.Open(sealed, []byte("other")); !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("Open with another aad = %v, want ErrCorrupt", err)
	}
	if err := d.Check(); err != nil {
		t.Fatalf("Check = %v", err)
	}
}

// The file holds the key as DPAPI protects it in the user's scope, never
// the raw key; it and its directory are private.
func TestDPAPIKeyFileIsProtectedNotRaw(t *testing.T) {
	path := dpapiPath(t)
	if _, err := (&seal.DPAPI{Path: path}).Seal([]byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) <= 32 {
		t.Fatalf("key file is %d bytes: a raw key, not a DPAPI blob", len(blob))
	}
	key, err := seal.Unprotect(blob)
	if err != nil || len(key) != 32 {
		t.Fatalf("Unprotect = %d bytes, %v; want the 32-byte key", len(key), err)
	}
	if bytes.Contains(blob, key) {
		t.Fatal("the raw key is in the file")
	}
	// The blob's header (DPAPI_BLOB: version, provider GUID, master-key
	// version and GUID, then dwFlags at offset 40) records the scope: no
	// CRYPTPROTECT_LOCAL_MACHINE, so only this user's master key opens it.
	if len(blob) < 44 || !seal.HasDPAPIHeader(blob) {
		t.Fatalf("blob of %d bytes has no DPAPI header (version 1, provider GUID): % x", len(blob), blob[:min(len(blob), 20)])
	}
	if flags := binary.LittleEndian.Uint32(blob[40:44]); flags&windows.CRYPTPROTECT_LOCAL_MACHINE != 0 {
		t.Fatalf("blob flags %#x: protected in the machine's scope", flags)
	}
	if err := acl.Check(path); err != nil {
		t.Fatalf("acl.Check(key) = %v", err)
	}
	acltest.WantPrivate(t, filepath.Dir(path), 0o700)
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".key-*")); len(left) != 0 {
		t.Fatalf("temporary key left: %v", left)
	}
}

func TestDPAPITamperedBlobIsCorrupt(t *testing.T) {
	path := dpapiPath(t)
	d := &seal.DPAPI{Path: path}
	sealed, err := d.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)/2] ^= 1
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"open":  func() error { _, err := d.Open(sealed, nil); return err },
		"seal":  func() error { _, err := d.Seal([]byte("y"), nil); return err },
		"check": d.Check,
	} {
		if err := run(); !errors.Is(err, seal.ErrCorrupt) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s = %v, want ErrCorrupt naming %s", name, err, path)
		}
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, blob) {
		t.Error("a corrupt key was replaced")
	}
}

// A key protected otherwise than pcb protects it is refused as not for this
// sealer: with other entropy (what another program of this user, or a
// copy re-protected without pcb's entropy, gives) or a raw key of the right
// size. A test cannot run as another Windows user; another user's blob is
// refused by CryptUnprotectData the same way (ErrCorrupt).
func TestDPAPIForeignBlobIsRefused(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	other, err := seal.ProtectWith(key, []byte("another program"), windows.CRYPTPROTECT_UI_FORBIDDEN)
	if err != nil {
		t.Fatal(err)
	}
	none, err := seal.ProtectWith(key, nil, windows.CRYPTPROTECT_UI_FORBIDDEN)
	if err != nil {
		t.Fatal(err)
	}
	// A blob for the whole machine opens for every local account: another
	// account could plant a key it knows, under pcb's public entropy.
	machine, err := seal.ProtectWith(key, seal.DPAPIEntropy(), windows.CRYPTPROTECT_UI_FORBIDDEN|windows.CRYPTPROTECT_LOCAL_MACHINE)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"other entropy": other, "no entropy": none, "raw key": key, "machine scope": machine} {
		path := dpapiPath(t)
		if err := acl.MkdirAll(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		err := (&seal.DPAPI{Path: path}).Check() // the key alone: no sealed data to fail on
		if !errors.Is(err, seal.ErrCorrupt) {
			t.Errorf("%s: Check = %v, want ErrCorrupt", name, err)
		}
		if name == "machine scope" && (err == nil || !strings.Contains(err.Error(), "protected for the whole machine, not this user")) {
			t.Errorf("machine scope: Check = %v, want the scope named", err)
		}
		// Windows' own codes for these, which make them corrupt rather
		// than a key this session cannot reach.
		var errno windows.Errno
		_ = errors.As(err, &errno)
		if want := map[string]windows.Errno{"other entropy": windows.ERROR_INVALID_DATA, "no entropy": windows.ERROR_INVALID_DATA,
			"raw key": windows.ERROR_INVALID_PARAMETER}[name]; want != 0 && errno != want {
			t.Errorf("%s: CryptUnprotectData errno %#x, want %#x", name, uint32(errno), uint32(want))
		}
	}
	// The same key under pcb's entropy opens: the refusals above are the
	// entropy's, not the key's.
	good, err := seal.ProtectWith(key, seal.DPAPIEntropy(), windows.CRYPTPROTECT_UI_FORBIDDEN)
	if err != nil {
		t.Fatal(err)
	}
	path := dpapiPath(t)
	if err := acl.MkdirAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&seal.DPAPI{Path: path}).Check(); err != nil {
		t.Fatalf("Check of a key under pcb's entropy = %v", err)
	}
}

// A protected key that is not 32 bytes is refused by its size.
func TestDPAPIWrongKeySizeIsRefused(t *testing.T) {
	blob, err := seal.ProtectWith([]byte("short"), seal.DPAPIEntropy(), windows.CRYPTPROTECT_UI_FORBIDDEN)
	if err != nil {
		t.Fatal(err)
	}
	path := dpapiPath(t)
	if err := acl.MkdirAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	var se *seal.KeySizeError
	if _, err := (&seal.DPAPI{Path: path}).Seal([]byte("x"), nil); !errors.As(err, &se) || se.Size != 5 || !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("Seal = %v, want a KeySizeError of 5 bytes", err)
	}
}

func TestDPAPIOpenWithoutKeyIsErrNoKey(t *testing.T) {
	path := dpapiPath(t)
	d := &seal.DPAPI{Path: path}
	if _, err := d.Open(make([]byte, 64), nil); !errors.Is(err, seal.ErrNoKey) {
		t.Fatalf("Open = %v, want ErrNoKey", err)
	}
	if err := d.Check(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Check = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open or Check created the key (%v)", err)
	}
}

// Concurrent first seals agree on one key: the first creator's.
func TestDPAPIFirstCreatorWins(t *testing.T) {
	path := dpapiPath(t)
	const n = 8
	blobs := make([][]byte, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, err := (&seal.DPAPI{Path: path}).Seal([]byte(fmt.Sprintf("secret-%d", i)), nil)
			if err != nil {
				t.Error(err)
			}
			blobs[i] = b
		}()
	}
	close(start)
	wg.Wait()
	key, _ := os.ReadFile(path)
	fresh := &seal.DPAPI{Path: path}
	for i, b := range blobs {
		got, err := fresh.Open(b, nil)
		if err != nil || string(got) != fmt.Sprintf("secret-%d", i) {
			t.Fatalf("blob %d: %q, %v", i, got, err)
		}
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(key, again) {
		t.Fatal("key changed after creation")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".key-*")); len(left) != 0 {
		t.Fatalf("temporary key left: %v", left)
	}
}

// The DPAPI key gets KeyFile's checks: a key whose access list lets
// Everyone in, or a directory open to others, is refused (ErrKeyMode)
// before it is used or made.
func TestDPAPIRefusesAKeyOrDirectoryNotPrivate(t *testing.T) {
	t.Run("key", func(t *testing.T) {
		path := dpapiPath(t)
		d := &seal.DPAPI{Path: path}
		sealed, err := d.Seal([]byte("x"), nil)
		if err != nil {
			t.Fatal(err)
		}
		acltest.Allow(t, path, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE)
		for name, run := range map[string]func() error{
			"check": d.Check,
			"open":  func() error { _, err := d.Open(sealed, nil); return err },
		} {
			var me *seal.KeyModeError
			if err := run(); !errors.As(err, &me) || me.Open == nil || !slices.Equal(me.Open.SIDs(), []string{acltest.Everyone}) || me.Path != path {
				t.Errorf("%s = %v, want a KeyModeError for Everyone", name, err)
			}
		}
	})
	t.Run("directory", func(t *testing.T) {
		path := dpapiPath(t)
		if err := acl.MkdirAll(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		acltest.OpenToOthers(t, filepath.Dir(path))
		_, err := (&seal.DPAPI{Path: path}).Seal([]byte("x"), nil)
		var me *seal.KeyModeError
		if !errors.As(err, &me) || !me.Dir || me.Open == nil || !slices.Equal(me.Open.SIDs(), []string{acltest.Everyone}) {
			t.Fatalf("Seal = %v, want a KeyModeError for the directory", err)
		}
		wantNoKey(t, path)
	})
}
