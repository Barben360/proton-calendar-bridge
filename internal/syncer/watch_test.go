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
	var waits []time.Duration
	var seen []error
	err := syncer.Watch(t.Context(), p, syncer.WatchOptions{
		Sleep:  func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		OnPass: func(r syncer.Report, err error) { seen = append(seen, err) },
		Stop:   func(err error) bool { return errors.Is(err, errStop) },
		Rand:   func() float64 { return 0.5 }, // no jitter
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
		OnPass: func(syncer.Report, error) { reported++ },
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
