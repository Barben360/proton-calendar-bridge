package atomicfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

// A directory where the file should be fails at once with the error Go
// gives on Unix (EEXIST), never a permission error: Windows' rename returns
// access denied there, which would be retried as a reader's and read as a
// permission fault (exit 10 where Unix gives 20).
func TestWriteOverADirectoryFailsAtOnceAsOnUnix(t *testing.T) {
	for _, full := range []bool{false, true} {
		dir := t.TempDir()
		target := filepath.Join(dir, "d")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if full {
			if err := os.WriteFile(filepath.Join(target, "x"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		err := atomicfile.Write(target, []byte("x"))
		if !errors.Is(err, syscall.EEXIST) || errors.Is(err, fs.ErrPermission) {
			t.Errorf("non-empty %v: err = %v, want EEXIST and no permission error", full, err)
		}
		if d := time.Since(start); d >= atomicfile.RenameRetry/2 {
			t.Errorf("non-empty %v: failed after %v, want no retry", full, d)
		}
		if fi, err := os.Lstat(target); err != nil || !fi.IsDir() {
			t.Errorf("non-empty %v: the directory was replaced (%v)", full, err)
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
	atomicfile.RemoveStale(dir, now, atomicfile.TempPrefix, atomicfile.KeyPrefix)
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
	atomicfile.RemoveStale(filepath.Join(dir, "missing"), now, atomicfile.TempPrefix) // no panic, nothing to do
}

// Only the prefixes asked for go: the key file's directory, which may be
// shared, is swept of .key-* alone.
func TestRemoveStaleSweepsOnlyThePrefixesAsked(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".tmp-1", ".key-2"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	atomicfile.RemoveStale(dir, time.Now().Add(2*atomicfile.StaleAfter), atomicfile.KeyPrefix)
	if _, err := os.Stat(filepath.Join(dir, ".tmp-1")); err != nil {
		t.Errorf(".tmp-1 swept with only .key- asked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".key-2")); !os.IsNotExist(err) {
		t.Errorf(".key-2 still there: %v", err)
	}
}

// A symbolic link named like a temporary file is neither followed nor
// removed, whatever it points at.
func TestRemoveStaleNeverFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	dir, elsewhere := t.TempDir(), t.TempDir()
	outside := filepath.Join(elsewhere, "someone-elses")
	inside := filepath.Join(dir, "kept")
	for _, p := range []string{outside, inside} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{".tmp-out": outside, ".key-in": inside}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Everything, links included, is past the bound at this now.
	atomicfile.RemoveStale(dir, time.Now().Add(2*atomicfile.StaleAfter), atomicfile.TempPrefix, atomicfile.KeyPrefix)
	for name := range links {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the link %s was removed: %v", name, err)
		}
	}
	for _, p := range []string{outside, inside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the link target %s was removed: %v", p, err)
		}
	}
}

func TestReadFileReadsWhatWriteWrote(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := atomicfile.Write(p, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	b, err := atomicfile.ReadFile(p)
	if err != nil || string(b) != "v1" {
		t.Fatalf("ReadFile = %q, %v; want v1", b, err)
	}
}

func TestOpenMissingIsNotExist(t *testing.T) {
	_, err := atomicfile.Open(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open missing = %v, want fs.ErrNotExist", err)
	}
}

// Rename moves a symbolic link itself: the link lands at the new name, still
// pointing at its target, which stays where it was.
func TestRenameMovesALinkNotItsTarget(t *testing.T) {
	dir := t.TempDir()
	target, link, moved := filepath.Join(dir, "target"), filepath.Join(dir, "link"), filepath.Join(dir, "moved")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("this user may not create symbolic links (no developer mode or privilege): %v", err)
		}
		t.Fatal(err)
	}
	if err := atomicfile.Rename(link, moved); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(moved); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("Lstat(moved) = %v, %v; want the link", fi, err)
	}
	if got, err := os.Readlink(moved); err != nil || got != target {
		t.Errorf("moved points at %q, %v; want %q", got, err, target)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "x" {
		t.Errorf("target = %q, %v; want it untouched", b, err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the old link name is still there: %v", err)
	}
}
