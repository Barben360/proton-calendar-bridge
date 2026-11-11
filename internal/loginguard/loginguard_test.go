package loginguard_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
)

var t0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T, age time.Duration) (dir, stamp string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the guard needs file locks")
	}
	dir = t.TempDir()
	stamp = filepath.Join(dir, "last-login")
	if age > 0 {
		if err := os.WriteFile(stamp, []byte(t0.Add(-age).Format(time.RFC3339Nano)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, stamp
}

func code(t *testing.T, err error) *exitcode.Error {
	t.Helper()
	var e *exitcode.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not an *exitcode.Error", err)
	}
	return e
}

func TestAcquireRefusesWithin(t *testing.T) {
	dir, stamp := setup(t, 59*time.Second)
	before, _ := os.ReadFile(stamp)
	_, err := loginguard.Acquire(dir, loginguard.MinInterval, t0)
	if e := code(t, err); e.Code != exitcode.Blocked || !strings.Contains(e.Hint, "retry in 1s") {
		t.Fatalf("err = %v (hint %q)", err, e.Hint)
	}
	if after, _ := os.ReadFile(stamp); string(after) != string(before) {
		t.Fatalf("stamp rewritten by a refusal: %q", after)
	}
}

func TestAcquireAllowsAfter(t *testing.T) {
	dir, stamp := setup(t, 61*time.Second)
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	b, _ := os.ReadFile(stamp)
	if got, _ := time.Parse(time.RFC3339Nano, string(b)); !got.Equal(t0) {
		t.Fatalf("stamp = %q, want %s", b, t0)
	}
}

func TestAcquireAllowsWhenNoStamp(t *testing.T) {
	dir, stamp := setup(t, 0)
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("no stamp written: %v", err)
	}
}

func TestMalformedStampFailsClosed(t *testing.T) {
	dir, stamp := setup(t, 0)
	if err := os.WriteFile(stamp, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loginguard.Acquire(dir, loginguard.MinInterval, t0)
	e := code(t, err)
	if e.Code != exitcode.Blocked || !strings.Contains(e.Hint, "last-login") {
		t.Fatalf("err = %v (hint %q)", err, e.Hint)
	}
	if _, ok := loginguard.WaitFor(err); ok {
		t.Fatal("a malformed stamp reported a wait")
	}
}

func TestHeldReleaseThenReacquire(t *testing.T) {
	dir, _ := setup(t, 0)
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(time.Hour))
	if e := code(t, err); e.Code != exitcode.Blocked || !strings.Contains(e.Error(), "another login is running") {
		t.Fatalf("err = %v", err)
	}
	release()
	r2, err := loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	r2()
}

func TestConcurrentAcquireOneWins(t *testing.T) {
	dir, _ := setup(t, 0)
	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok      int
		blocked int
		start   = make(chan struct{})
		release []func()
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := loginguard.Acquire(dir, loginguard.MinInterval, t0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
				release = append(release, r)
			case exitcode.From(err) == exitcode.Blocked:
				blocked++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, r := range release {
		r()
	}
	if ok != 1 || blocked != n-1 {
		t.Fatalf("ok = %d, blocked = %d; want 1, %d", ok, blocked, n-1)
	}
}

func TestWaitForExtractsWait(t *testing.T) {
	dir, _ := setup(t, 20*time.Second)
	_, err := loginguard.Acquire(dir, 60*time.Second, t0)
	if wait, ok := loginguard.WaitFor(err); !ok || wait != 40*time.Second {
		t.Fatalf("WaitFor = %v, %v; want 40s, true", wait, ok)
	}
	if _, ok := loginguard.WaitFor(nil); ok {
		t.Fatal("WaitFor(nil) reported a wait")
	}
}
