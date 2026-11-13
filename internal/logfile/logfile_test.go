package logfile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/logfile"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, f *logfile.File, s string) {
	t.Helper()
	n, err := f.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%d bytes) = %d, %v", len(s), n, err)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestLogfileRotatesAtMax(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "logs", "ui.log")
	f, err := logfile.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	write(t, f, "0123456789") // exactly max: stays
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("rotated at max itself: %v", err)
	}
	write(t, f, "X") // max+1 in total: rotates first
	if got := read(t, path+".1"); got != "0123456789" {
		t.Errorf(".1 = %q, want the old content", got)
	}
	if got := read(t, path); got != "X" {
		t.Errorf("log = %q, want the rest", got)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, path + ".1"} {
			if m := mode(t, p); m != 0o600 {
				t.Errorf("%s mode = %o, want 600", p, m)
			}
		}
		if m := mode(t, filepath.Dir(path)); m != 0o700 {
			t.Errorf("directory mode = %o, want 700", m)
		}
	}
}

func TestLogfileKeepsOneRotation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ui.log")
	f, err := logfile.Open(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		write(t, f, s)
	}
	if got := read(t, path+".1"); got != "bbbb" {
		t.Errorf(".1 = %q, want only the last rotation (bbbb)", got)
	}
	if got := read(t, path); got != "cccc" {
		t.Errorf("log = %q, want cccc", got)
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Errorf("a second rotation exists: %v", err)
	}
}

// A write larger than max is not split: it rotates what is there, then
// lands whole in the fresh file, which the next write rotates again.
func TestLogfileOversizeWriteIsKeptWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ui.log")
	f, err := logfile.Open(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	write(t, f, "ab")
	write(t, f, "0123456789")
	if got := read(t, path); got != "0123456789" {
		t.Errorf("log = %q, want the oversize write whole", got)
	}
	if got := read(t, path+".1"); got != "ab" {
		t.Errorf(".1 = %q, want ab", got)
	}
	write(t, f, "z")
	if got := read(t, path+".1"); got != "0123456789" || read(t, path) != "z" {
		t.Errorf(".1 = %q, log = %q; the next write must rotate the oversize one", got, read(t, path))
	}
	// An oversize write into an empty file rotates nothing.
	path2 := filepath.Join(t.TempDir(), "ui.log")
	g, err := logfile.Open(path2, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	write(t, g, "0123456789")
	if _, err := os.Stat(path2 + ".1"); !os.IsNotExist(err) {
		t.Errorf("an empty log was rotated: %v", err)
	}
}

// A reopened log continues its size: appends, and rotates on the old bytes.
func TestLogfileReopenAppendsAndCountsExistingBytes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ui.log")
	f, err := logfile.Open(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	write(t, f, "12345")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = logfile.Open(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	write(t, f, "678")
	if got := read(t, path); got != "12345678" {
		t.Fatalf("log = %q, want the old bytes kept and appended to", got)
	}
	write(t, f, "9")
	if read(t, path+".1") != "12345678" || read(t, path) != "9" {
		t.Errorf("log = %q, .1 = %q; the old bytes must count toward max", read(t, path), read(t, path+".1"))
	}
}

func TestLogfileTightensAnExistingFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no modes")
	}
	path := filepath.Join(t.TempDir(), "ui.log")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := logfile.Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if m := mode(t, path); m != 0o600 {
		t.Errorf("mode = %o, want 600", m)
	}
}

func TestLogfileConcurrentWritesAreWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ui.log")
	f, err := logfile.Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 99)
	line = append(line, '\n')
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_, _ = f.Write(line)
			}
		}()
	}
	wg.Wait()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(read(t, path)); got != 8*50*100 {
		t.Errorf("log holds %d bytes, want %d", got, 8*50*100)
	}
}

func TestLogfileWriteAfterCloseFails(t *testing.T) {
	t.Parallel()
	f, err := logfile.Open(filepath.Join(t.TempDir(), "ui.log"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err == nil {
		t.Error("Write after Close succeeded")
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestLogfileOpenRefusesBadArguments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := logfile.Open(filepath.Join(dir, "ui.log"), 0); err == nil {
		t.Error("Open with max 0 succeeded")
	}
	if _, err := logfile.Open(dir, 10); err == nil {
		t.Error("Open on a directory succeeded")
	}
}

// A rotation that cannot rename (here .1 is a directory) loses no line: the
// log goes on in the same file, and the rename is retried only once it has
// grown by another max, not on every write.
func TestLogfileFailedRenameLosesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ui.log")
	if err := os.MkdirAll(filepath.Join(path+".1", "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := logfile.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	write(t, f, "aaaaaa")
	write(t, f, "bbbbbb") // would pass max: the rename fails, the line is kept
	write(t, f, "cc")
	if got := read(t, path); got != "aaaaaabbbbbbcc" {
		t.Errorf("log = %q, want every write kept", got)
	}
	// The obstacle leaves; the file is at 14 bytes, held until it passes 6+10.
	if err := os.RemoveAll(path + ".1"); err != nil {
		t.Fatal(err)
	}
	write(t, f, "e")
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("the rename was retried on the next write: %v", err)
	}
	write(t, f, "ffffff") // passes the hold: rotates again
	if got := read(t, path+".1"); got != "aaaaaabbbbbbcce" {
		t.Errorf(".1 = %q, want everything before this write", got)
	}
	if got := read(t, path); got != "ffffff" {
		t.Errorf("log = %q, want a fresh file", got)
	}
}

// One Close that fails during a rotation does not stop the log: the
// rotation goes on (the file is renamed and a fresh one opened), and every
// later write lands (review M5).
func TestLogfileFailedCloseKeepsLogging(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ui.log")
	f, err := logfile.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	write(t, f, "aaaaaa")
	if err := logfile.CloseHandle(f); err != nil {
		t.Fatal(err)
	}
	write(t, f, "bbbbbb") // rotates: its Close fails
	write(t, f, "cc")
	write(t, f, "dddddd") // rotates again, normally
	if got := read(t, path+".1"); got != "bbbbbbcc" {
		t.Errorf(".1 = %q, want the writes after the failed Close", got)
	}
	if got := read(t, path); got != "dddddd" {
		t.Errorf("log = %q, want the last write", got)
	}
}
