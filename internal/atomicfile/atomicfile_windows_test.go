//go:build windows

package atomicfile_test

import (
	"io"
	"os"
	"path/filepath"
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
	defer f.Close()
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
	defer f.Close()
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
