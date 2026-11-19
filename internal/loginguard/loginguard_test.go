package loginguard_test

import (
	"bytes"
	"errors"
	"log/slog"
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
	_, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
	if e := code(t, err); e.Code != exitcode.Blocked || !strings.Contains(e.Hint, "retry in 1s") {
		t.Fatalf("err = %v (hint %q)", err, e.Hint)
	}
	if after, _ := os.ReadFile(stamp); string(after) != string(before) {
		t.Fatalf("stamp rewritten by a refusal: %q", after)
	}
}

func TestAcquireAllowsAfter(t *testing.T) {
	dir, stamp := setup(t, 61*time.Second)
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
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
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
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
	_, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
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
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(time.Hour), nil)
	if e := code(t, err); e.Code != exitcode.Blocked || !strings.Contains(e.Error(), "another login is running") {
		t.Fatalf("err = %v", err)
	}
	release()
	r2, err := loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(time.Hour), nil)
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
			r, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
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
	_, err := loginguard.Acquire(dir, 60*time.Second, t0, nil)
	if wait, ok := loginguard.WaitFor(err); !ok || wait != 40*time.Second {
		t.Fatalf("WaitFor = %v, %v; want 40s, true", wait, ok)
	}
	if _, ok := loginguard.WaitFor(nil); ok {
		t.Fatal("WaitFor(nil) reported a wait")
	}
}

// A stamp in the future beyond the clock skew (the clock was stepped back:
// VM restore, NTP step, an RTC at boot) is stale: the login goes ahead, the
// stamp becomes now and the odd stamp is logged once (ruling M4 = state-I3).
func TestFutureStampIsStale(t *testing.T) {
	dir, stamp := setup(t, 0)
	if err := os.WriteFile(stamp, []byte(t0.Add(48*time.Hour).Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, log)
	if err != nil {
		t.Fatalf("a stamp 48 h ahead blocked the login: %v", err)
	}
	release()
	b, _ := os.ReadFile(stamp)
	if got, _ := time.Parse(time.RFC3339Nano, string(b)); !got.Equal(t0) {
		t.Fatalf("stamp = %q, want now (%s)", b, t0)
	}
	release, err = loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(61*time.Second), log)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "the last login time is in the future") {
		t.Fatalf("logged %d warnings, want 1 naming the future stamp:\n%s", n, logs.String())
	}
}

// The skew boundary: a stamp up to 5 min ahead (included) still counts as
// a recent attempt, waited out in full; just past it, the stamp is stale.
func TestStampSkewBoundary(t *testing.T) {
	cases := []struct {
		ahead time.Duration
		wait  time.Duration // 0: the login goes ahead
	}{
		{4 * time.Minute, 5 * time.Minute},
		{5 * time.Minute, 6 * time.Minute},
		{5*time.Minute + time.Second, 0},
		{6 * time.Minute, 0},
	}
	for _, c := range cases {
		t.Run(c.ahead.String(), func(t *testing.T) {
			dir, stamp := setup(t, 0)
			if err := os.WriteFile(stamp, []byte(t0.Add(c.ahead).Format(time.RFC3339Nano)), 0o600); err != nil {
				t.Fatal(err)
			}
			release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
			if c.wait == 0 {
				if err != nil {
					t.Fatalf("a stamp %v ahead blocked the login: %v", c.ahead, err)
				}
				release()
				return
			}
			if wait, ok := loginguard.WaitFor(err); !ok || wait != c.wait {
				t.Fatalf("a stamp %v ahead: WaitFor = %v, %v; want %v, true", c.ahead, wait, ok, c.wait)
			}
		})
	}
}

// Defer records a Retry-After longer than MinInterval: the next login is
// refused until it is over, naming the wait, and a new process (a new
// Acquire on the same directory) sees it (issue 45).
func TestDeferExtendsTheWait(t *testing.T) {
	dir, _ := setup(t, 0)
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := loginguard.Defer(dir, t0, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	release()
	_, err = loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(61*time.Second), nil)
	if wait, ok := loginguard.WaitFor(err); !ok || wait != 239*time.Second {
		t.Fatalf("61 s after a 300 s Retry-After: err = %v, want TooSoon 239s", err)
	}
	if e := code(t, err); e.Code != exitcode.Blocked || !strings.Contains(e.Hint, "retry in 3m59s") {
		t.Fatalf("hint %q, want retry in 3m59s", e.Hint)
	}
	release, err = loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(301*time.Second), nil)
	if err != nil {
		t.Fatalf("after the Retry-After the login was still refused: %v", err)
	}
	release()
}

// A wait shorter than the spacing changes nothing; a huge one is clamped
// to an hour.
func TestDeferClampsAndNeverShortens(t *testing.T) {
	dir, _ := setup(t, 0)
	release, _ := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
	if err := loginguard.Defer(dir, t0, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(30*time.Second), nil); err == nil {
		t.Fatal("a 5 s Retry-After shortened the 60 s spacing")
	}

	dir, _ = setup(t, 0)
	release, _ = loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
	if err := loginguard.Defer(dir, t0, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	release()
	_, err := loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(30*time.Minute), nil)
	if wait, ok := loginguard.WaitFor(err); !ok || wait != 30*time.Minute {
		t.Fatalf("48 h Retry-After 30 min later: err = %v, want TooSoon 30m (clamped to 1 h)", err)
	}
	r, err := loginguard.Acquire(dir, loginguard.MinInterval, t0.Add(61*time.Minute), nil)
	if err != nil {
		t.Fatalf("a 48 h Retry-After still blocked after the 1 h clamp: %v", err)
	}
	r()
}

// A not-before more than an hour (plus the skew) ahead is from a clock
// that was stepped back: it is ignored and logged, as a stale stamp is.
func TestFutureNotBeforeIsStale(t *testing.T) {
	dir, _ := setup(t, 0)
	nb := t0.Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(dir, "not-before"), []byte(nb), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	release, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("a not-before 48 h ahead blocked the login: %v", err)
	}
	release()
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("no warning logged:\n%s", logs.String())
	}
}

func TestMalformedNotBeforeFailsClosed(t *testing.T) {
	dir, _ := setup(t, 0)
	if err := os.WriteFile(filepath.Join(dir, "not-before"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loginguard.Acquire(dir, loginguard.MinInterval, t0, nil)
	if e := code(t, err); e.Code != exitcode.Blocked {
		t.Fatalf("err = %v, want Blocked", err)
	}
}
