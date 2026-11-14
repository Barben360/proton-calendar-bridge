package filelock_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

func TestTryLockIsExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	unlock, ok, err := filelock.TryLock(p)
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v", ok, err)
	}
	if _, ok2, err := filelock.TryLock(p); err != nil || ok2 {
		t.Fatalf("second TryLock while held = %v, %v; want false, nil", ok2, err)
	}
	unlock()
	unlock2, ok, err := filelock.TryLock(p)
	if err != nil || !ok {
		t.Fatalf("TryLock after unlock = %v, %v", ok, err)
	}
	unlock2()
}

func TestLockWaitsForRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	unlock, _, err := filelock.TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(150*time.Millisecond, unlock)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	u, err := filelock.Lock(ctx, p)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer u()
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("Lock returned before the holder released")
	}
}

func TestLockHonoursContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	unlock, _, err := filelock.TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := filelock.Lock(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestLockedFileContentStaysUsable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "serve.lock")
	unlock, ok, err := filelock.TryLock(p)
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	if err := os.WriteFile(p, []byte("4242 serve\n"), 0o600); err != nil {
		t.Fatalf("write while locked: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "4242 serve\n" {
		t.Fatalf("read while locked = %q, %v", b, err)
	}
	if err := os.Truncate(p, 0); err != nil {
		t.Fatalf("truncate while locked: %v", err)
	}
}

func TestTryLockExcludesAnotherProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelper$")
	cmd.Env = append(os.Environ(), "PCB_FILELOCK_HELPER="+p)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("helper said %q, %v", line, err)
	}
	if _, ok, err := filelock.TryLock(p); err != nil || ok {
		t.Fatalf("TryLock while another process holds it = %v, %v; want false, nil", ok, err)
	}
	_ = stdin.Close()
	released = true
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	u, err := filelock.Lock(ctx, p)
	if err != nil {
		t.Fatalf("Lock after the holder exited: %v", err)
	}
	u()
}

func TestLockHelper(t *testing.T) {
	p := os.Getenv("PCB_FILELOCK_HELPER")
	if p == "" {
		t.Skip("helper process of TestTryLockExcludesAnotherProcess")
	}
	unlock, ok, err := filelock.TryLock(p)
	if err != nil || !ok {
		t.Fatalf("helper TryLock = %v, %v", ok, err)
	}
	defer unlock()
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
