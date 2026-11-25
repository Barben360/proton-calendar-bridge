//go:build windows

package atomicfile_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

func TestWriteSucceedsWhileOurReaderHoldsTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := atomicfile.Write(p, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	f, err := atomicfile.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	start := time.Now()
	if err := atomicfile.Write(p, []byte("v2")); err != nil {
		t.Fatalf("Write while shared reader open: %v", err)
	}
	if d := time.Since(start); d >= 500*time.Millisecond {
		t.Fatalf("Write took %v, want < 500ms", d)
	}
	if old, _ := io.ReadAll(f); string(old) != "v1" {
		t.Fatalf("open reader sees %q, want v1", old)
	}
	if b, err := atomicfile.ReadFile(p); err != nil || string(b) != "v2" {
		t.Fatalf("ReadFile = %q, %v; want v2", b, err)
	}
}

func TestWriteWaitsForAForeignReader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := atomicfile.Write(p, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, func() { _ = f.Close() })
	start := time.Now()
	if err := atomicfile.Write(p, []byte("v2")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("Write returned after %v, want it to wait for the reader", d)
	}
	if b, _ := atomicfile.ReadFile(p); string(b) != "v2" {
		t.Fatalf("content = %q, want v2", b)
	}
}

func TestWriteGivesUpAfterTwoSeconds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := atomicfile.Write(p, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	start := time.Now()
	if err := atomicfile.Write(p, []byte("v2")); err == nil {
		t.Fatal("Write succeeded against a foreign reader held open")
	}
	if d := time.Since(start); d < 1900*time.Millisecond || d >= 3*time.Second {
		t.Fatalf("gave up after %v, want 1.9s..3s", d)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "f" {
		t.Fatalf("directory holds %v, want only f", entries)
	}
	_ = f.Close()
	if b, _ := atomicfile.ReadFile(p); string(b) != "v1" {
		t.Fatalf("content = %q, want v1", b)
	}
}

func TestRenameRetriesADirectory(t *testing.T) {
	root := t.TempDir()
	d1, d2 := filepath.Join(root, "d1"), filepath.Join(root, "d2")
	if err := os.Mkdir(d1, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d1, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(d1, "x"))
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, func() { _ = f.Close() })
	start := time.Now()
	if err := atomicfile.Rename(d1, d2); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("Rename returned after %v, want it to wait", d)
	}
	if _, err := os.Stat(filepath.Join(d2, "x")); err != nil {
		t.Fatal(err)
	}
}

// junk keeps the reader's garbage reachable until the next one replaces it,
// so the heap around a rename's buffer holds non-zero bytes.
var junk []byte

// Replacing a file again and again, while a pcb reader loops on it, always
// leaves exactly the file Write was given, with its new content: the rename
// never fails, never lands on another name, never loses the file. Target
// paths of 64 consecutive lengths cover every allocation size the rename's
// buffer can take, and the reader fills the heap with non-zero bytes (a
// buffer read past its end then meets garbage, not a lucky zero).
func TestWriteStressWithASharedReader(t *testing.T) {
	root := t.TempDir()
	const lengths, cycles = 64, 8 // 512 replacements
	for n := range lengths {
		dir := filepath.Join(root, fmt.Sprint(n))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, strings.Repeat("f", n+1))
		if err := atomicfile.Write(p, []byte("v0")); err != nil {
			t.Fatalf("first Write of a %d-character path: %v", len(p), err)
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if b, err := atomicfile.ReadFile(p); err == nil {
					junk = bytes.Repeat(b, 64+len(p))
				}
			}
		}()
		for i := 1; i <= cycles; i++ {
			want := fmt.Sprintf("v%d", i)
			if err := atomicfile.Write(p, []byte(want)); err != nil {
				close(stop)
				<-done
				t.Fatalf("Write %d of a %d-character path: %v", i, len(p), err)
			}
			b, err := atomicfile.ReadFile(p)
			if err != nil || string(b) != want {
				close(stop)
				<-done
				t.Fatalf("after Write %d of a %d-character path: ReadFile = %q, %v; want %q (directory: %v)", i, len(p), b, err, want, names(dir))
			}
		}
		close(stop)
		<-done
		if got := names(dir); len(got) != 1 || got[0] != filepath.Base(p) {
			t.Fatalf("directory of a %d-character path holds %q, want only %q", len(p), got, filepath.Base(p))
		}
	}
}

func names(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
