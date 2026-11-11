package filelock_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

func skipOffUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file locks are Unix only")
	}
}

func TestTryLockIsExclusive(t *testing.T) {
	skipOffUnix(t)
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
	skipOffUnix(t)
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
	skipOffUnix(t)
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

func TestUnsupportedOffUnix(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Unix supports locks")
	}
	if _, _, err := filelock.TryLock("x"); !errors.Is(err, filelock.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
