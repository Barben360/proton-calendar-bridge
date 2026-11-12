package atomicfile_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

func TestWriteMode0600AndReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("old content, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.Write(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "new" {
		t.Fatalf("content = %q, want new", b)
	}
	st, _ := os.Stat(p)
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestWriteLeavesNoTempOnFailure(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("a read-only directory does not stop root or Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700) //nolint:errcheck // test cleanup
	if err := atomicfile.Write(filepath.Join(dir, "f"), []byte("x")); err == nil {
		t.Fatal("write into a read-only directory succeeded")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		t.Errorf("leftover %s", e.Name())
	}
}

func TestWriteRenameFailureRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "d")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.Write(target, []byte("x")); err == nil {
		t.Fatal("renaming over a non-empty directory succeeded")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

// RemoveStale removes the temporary files a crash left (state Minor): those
// of Write (.tmp-*) and of the key file's creation (.key-*), once older than
// the bound; a younger one may belong to a write in progress, and nothing
// else is touched.
func TestRemoveStaleRemovesOnlyOldTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old, young := now.Add(-atomicfile.StaleAfter-time.Minute), now.Add(-atomicfile.StaleAfter+time.Minute)
	files := map[string]time.Time{
		".tmp-123": old, ".key-456": old, // gone
		".tmp-789": young, ".key-000": young, // a write in progress
		"state.json": old, "tmp-1": old, ".tmpx": old, // not ours
	}
	for name, at := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(dir, ".tmp-dir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, old, old); err != nil {
		t.Fatal(err)
	}
	atomicfile.RemoveStale(dir, now)
	for name := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		gone := os.IsNotExist(err)
		if want := name == ".tmp-123" || name == ".key-456"; gone != want {
			t.Errorf("%s removed = %v, want %v", name, gone, want)
		}
	}
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("a directory was removed: %v", err)
	}
	atomicfile.RemoveStale(filepath.Join(dir, "missing"), now) // no panic, nothing to do
}
