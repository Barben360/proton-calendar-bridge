package syncer_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

var (
	errFlaky = errors.New("proton unavailable")
	errStop  = errors.New("log in again")
)

// script returns a pass answering outcomes in order (nil: success), and the
// number of passes run.
func script(t *testing.T, outcomes ...error) (func(context.Context) (syncer.Report, error), *int) {
	t.Helper()
	n := new(int)
	return func(context.Context) (syncer.Report, error) {
		if *n >= len(outcomes) {
			t.Fatalf("pass %d run, only %d scripted", *n+1, len(outcomes))
		}
		err := outcomes[*n]
		*n++
		return syncer.Report{Calendars: []syncer.CalendarReport{{ID: "c" + string(rune('0'+*n))}}}, err
	}, n
}

func repeat(err error, n int) []error { return slices.Repeat([]error{err}, n) }

func TestWatchBacksOffAndStops(t *testing.T) {
	t.Parallel()
	// 7 failures (30 s doubling to the 10 min cap), a success, a failure
	// (back to 30 s: the success reset the count), then a stop.
	outcomes := append(append(append(repeat(errFlaky, 7), nil), errFlaky), errStop)
	p, runs := script(t, outcomes...)
	var waits, nexts []time.Duration
	var seen []error
	err := syncer.Watch(t.Context(), p, syncer.WatchOptions{
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		OnPass: func(r syncer.Report, err error, next time.Duration) {
			seen = append(seen, err)
			nexts = append(nexts, next)
		},
		Stop: func(err error) bool { return errors.Is(err, errStop) },
		Rand: func() float64 { return 0.5 }, // no jitter
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("Watch = %v, want the stop error", err)
	}
	s := time.Second
	want := []time.Duration{30 * s, 60 * s, 120 * s, 240 * s, 480 * s, 600 * s, 600 * s, 30 * s, 30 * s}
	if !slices.Equal(waits, want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
	if *runs != len(outcomes) || !slices.Equal(seen, outcomes) {
		t.Errorf("%d passes, OnPass saw %v; want %d passes, each reported", *runs, seen, len(outcomes))
	}
	// OnPass is told each wait before it starts, and 0 for the pass Watch
	// stops on.
	if wantNext := append(slices.Clone(want), 0); !slices.Equal(nexts, wantNext) {
		t.Errorf("OnPass next %v, want %v", nexts, wantNext)
	}
}

func TestWatchJitter(t *testing.T) {
	t.Parallel()
	s := time.Second
	for _, c := range []struct {
		rand   float64
		lo, hi time.Duration // inclusive
	}{
		{0, 540 * s, 540 * s},        // -10 %
		{0.999999, 659 * s, 660 * s}, // just under +10 %
		{0.25, 570 * s, 570 * s},     // -5 %
		{0.75, 630 * s, 630 * s},     // +5 %
	} {
		p, _ := script(t, append(repeat(errFlaky, 6), errStop)...)
		var waits []time.Duration
		_ = syncer.Watch(t.Context(), p, syncer.WatchOptions{
			Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
			Stop:  func(err error) bool { return errors.Is(err, errStop) },
			Rand:  func() float64 { return c.rand },
		})
		if len(waits) != 6 || waits[5] < c.lo || waits[5] > c.hi {
			t.Errorf("rand %v: waits %v, want the capped one in [%v, %v]", c.rand, waits, c.lo, c.hi)
		}
	}
	// The default source stays within ±10 % and is not constant.
	p, _ := script(t, append(repeat(errFlaky, 40), errStop)...)
	var waits []time.Duration
	_ = syncer.Watch(t.Context(), p, syncer.WatchOptions{
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		Stop:  func(err error) bool { return errors.Is(err, errStop) },
	})
	capped := waits[6:]
	for _, w := range capped {
		if w < 540*s || w > 660*s {
			t.Errorf("default jitter: capped wait %v outside [9 min, 11 min]", w)
		}
	}
	if slices.Min(capped) == slices.Max(capped) {
		t.Errorf("default jitter: %d capped waits all %v, want them to vary", len(capped), capped[0])
	}
}

func TestWatchInterval(t *testing.T) {
	t.Parallel()
	s := time.Second
	for _, c := range []struct {
		interval time.Duration
		want     []time.Duration // after success, failure, failure
	}{
		{0, []time.Duration{30 * s, 30 * s, 60 * s}},           // default
		{45 * s, []time.Duration{45 * s, 45 * s, 90 * s}},      // as given
		{time.Second, []time.Duration{10 * s, 10 * s, 20 * s}}, // raised to the 10 s floor
		// Above the 10 min cap: a failure never polls faster than a success.
		{15 * time.Minute, []time.Duration{15 * time.Minute, 15 * time.Minute, 15 * time.Minute}},
	} {
		p, _ := script(t, nil, errFlaky, errFlaky, errStop)
		var waits []time.Duration
		_ = syncer.Watch(t.Context(), p, syncer.WatchOptions{
			Interval: c.interval,
			Sleep:    func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
			Stop:     func(err error) bool { return errors.Is(err, errStop) },
			Rand:     func() float64 { return 0.5 },
		})
		if !slices.Equal(waits, c.want) {
			t.Errorf("Interval %v: waits %v, want %v", c.interval, waits, c.want)
		}
	}
}

func TestWatchHonoursContext(t *testing.T) {
	t.Parallel()
	// Cancelled while sleeping: Watch returns the context's error.
	ctx, cancel := context.WithCancel(t.Context())
	p, runs := script(t, nil, nil, nil)
	sleeps := 0
	err := syncer.Watch(ctx, p, syncer.WatchOptions{Sleep: func(ctx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}})
	if !errors.Is(err, context.Canceled) || *runs != 2 {
		t.Errorf("cancelled in the second sleep: Watch = %v after %d passes, want context.Canceled after 2", err, *runs)
	}

	// Cancelled during a pass: its error is neither reported, nor offered to
	// Stop, nor backed off from.
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	reported, stopAsked, slept := 0, 0, 0
	err = syncer.Watch(ctx, func(ctx context.Context) (syncer.Report, error) {
		cancel()
		return syncer.Report{}, errFlaky
	}, syncer.WatchOptions{
		Sleep:  func(context.Context, time.Duration) error { slept++; return nil },
		OnPass: func(syncer.Report, error, time.Duration) { reported++ },
		Stop:   func(error) bool { stopAsked++; return false },
	})
	if !errors.Is(err, context.Canceled) || reported != 0 || stopAsked != 0 || slept != 0 {
		t.Errorf("cancelled in a pass: Watch = %v (reported %d, Stop asked %d, slept %d), want context.Canceled and none",
			err, reported, stopAsked, slept)
	}

	// Already cancelled: no pass runs.
	done, stop := context.WithCancel(t.Context())
	stop()
	p, runs = script(t)
	if err := syncer.Watch(done, p, syncer.WatchOptions{}); !errors.Is(err, context.Canceled) || *runs != 0 {
		t.Errorf("cancelled before: Watch = %v after %d passes, want context.Canceled, none", err, *runs)
	}
}

// errWait asks for a wait of its own, like a 429 with Retry-After.
type errWait time.Duration

func (e errWait) Error() string { return "rate limited" }

// A failure that says how long to wait is not retried sooner, even past the
// back-off; the wait is capped at MaxRetryAfter.
func TestWatchHonoursRetryAfter(t *testing.T) {
	t.Parallel()
	m := time.Minute
	p, _ := script(t, errWait(20*m), errWait(5*time.Hour), errWait(10*time.Second), nil, errStop)
	var waits []time.Duration
	err := syncer.Watch(t.Context(), p, syncer.WatchOptions{
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		Stop:  func(err error) bool { return errors.Is(err, errStop) },
		Rand:  func() float64 { return 0.5 },
		RetryAfter: func(err error) time.Duration {
			var w errWait
			if errors.As(err, &w) {
				return time.Duration(w)
			}
			return 0
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("Watch = %v", err)
	}
	// 20 min as asked; 5 h capped at 1 h; 10 s below the 120 s back-off of a
	// third failure; then the interval after a success.
	want := []time.Duration{20 * m, time.Hour, 120 * time.Second, 30 * time.Second}
	if !slices.Equal(waits, want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
}

// NextInterval replaces Interval after a success: it is asked for each
// wait, raised to MinInterval, and a failure's back-off ignores it.
func TestNextIntervalIsUsedAfterASuccess(t *testing.T) {
	t.Parallel()
	p, _ := script(t, nil, nil, errFlaky, nil, errStop)
	s := time.Second
	next := []time.Duration{5 * time.Minute, 2 * s, 5 * time.Minute}
	var waits []time.Duration
	err := syncer.Watch(t.Context(), p, syncer.WatchOptions{
		NextInterval: func() time.Duration { d := next[0]; next = next[1:]; return d },
		Sleep:        func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		Stop:         func(err error) bool { return errors.Is(err, errStop) },
		Rand:         func() float64 { return 0.5 },
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("Watch = %v", err)
	}
	// 5 min; 2 s raised to the 10 s minimum; a failure backs off from Interval (30 s), not from NextInterval; 5 min.
	if want := []time.Duration{5 * time.Minute, syncer.MinInterval, 30 * s, 5 * time.Minute}; !slices.Equal(waits, want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
}

// A wake ends the sleep after a success and no other: a back-off keeps its
// whole wait, and the wake is spent, not kept for the next sleep.
func TestWakeCutsOnlyASuccessSleep(t *testing.T) {
	t.Parallel()
	wake := make(chan struct{}) // unbuffered: a send returns once Watch's listener took it
	sleeping := make(chan struct{})
	release := make(chan struct{})
	var cut []bool // per sleep: ended by the wake (its context ended)
	var waits []time.Duration
	p, _ := script(t, errFlaky, nil, nil, errStop)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		done <- syncer.Watch(ctx, p, syncer.WatchOptions{
			Wake: wake,
			Sleep: func(sctx context.Context, d time.Duration) error {
				waits = append(waits, d)
				sleeping <- struct{}{}
				select {
				case <-release:
					cut = append(cut, false)
					return nil
				case <-sctx.Done():
					cut = append(cut, true)
					return sctx.Err()
				}
			},
			Stop: func(err error) bool { return errors.Is(err, errStop) },
			Rand: func() float64 { return 0.5 },
		})
	}()
	// step runs one send of the scenario, failing the test (and ending Watch)
	// rather than hanging when Watch does not take it.
	step := func(what string, ch chan struct{}) {
		t.Helper()
		select {
		case ch <- struct{}{}:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("Watch did not take %s", what)
		}
	}
	await := func(what string) {
		t.Helper()
		select {
		case <-sleeping:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("Watch did not reach %s", what)
		}
	}
	// Sleep 1 follows a failure: two wakes change nothing, then it runs its course.
	await("the back-off sleep")
	step("a wake in the back-off", wake)
	step("a second wake (sent once the first was handled)", wake)
	step("the release of the back-off sleep", release)
	// Sleep 2 follows a success: a wake ends it.
	await("the sleep after a success")
	step("a wake after a success", wake)
	// Sleep 3 (a success again) shows the wake was spent: it runs its course.
	await("the next sleep")
	step("the release of the last sleep", release)
	if err := <-done; !errors.Is(err, errStop) {
		t.Fatalf("Watch = %v", err)
	}
	if want := []bool{false, true, false}; !slices.Equal(cut, want) {
		t.Errorf("sleeps cut by the wake %v, want %v", cut, want)
	}
	if want := []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second}; !slices.Equal(waits, want) {
		t.Errorf("recorded waits %v, want %v", waits, want)
	}
}
