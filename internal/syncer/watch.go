package syncer

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

// Watch loop timings.
const (
	DefaultInterval = 30 * time.Second
	MinInterval     = 10 * time.Second // a shorter Interval is raised to it
	MaxBackoff      = 10 * time.Minute // or Interval when longer
	jitter          = 0.10             // ± fraction of a backoff wait
)

// WatchOptions tune Watch. The zero value runs passes every 30 s and never
// stops on an error.
type WatchOptions struct {
	Interval time.Duration                                    // between passes; default DefaultInterval, at least MinInterval
	Sleep    func(ctx context.Context, d time.Duration) error // default: a timer, cut short by ctx
	OnPass   func(Report, error)                              // told every pass's outcome; nil: nothing
	Stop     func(error) bool                                 // true: Watch returns this pass error; nil: never
	Rand     func() float64                                   // jitter source in [0, 1); default math/rand/v2
}

// Watch runs pass until ctx ends or Stop accepts a pass error, which it
// returns (ctx.Err() otherwise). After a successful pass it waits Interval;
// after a failed one Interval doubled per consecutive failure, capped at
// the longer of MaxBackoff and Interval (a failure never polls faster than a
// success), ±10 %. A pass that fails because ctx ended is not reported.
func Watch(ctx context.Context, pass func(context.Context) (Report, error), o WatchOptions) error {
	if o.Interval == 0 {
		o.Interval = DefaultInterval
	}
	o.Interval = max(o.Interval, MinInterval)
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rep, err := pass(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if o.OnPass != nil {
			o.OnPass(rep, err)
		}
		wait := o.Interval
		if err != nil {
			if o.Stop != nil && o.Stop(err) {
				return err
			}
			failures++
			wait = backoff(o.Interval, failures, o.Rand())
		} else {
			failures = 0
		}
		if err := o.Sleep(ctx, wait); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// backoff is interval × 2^(failures-1), capped at max(MaxBackoff,
// interval), scaled by 1 ± jitter according to r in [0, 1).
func backoff(interval time.Duration, failures int, r float64) time.Duration {
	limit := max(MaxBackoff, interval)
	d := interval
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	return time.Duration(math.Round(float64(d) * (1 - jitter + 2*jitter*r)))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
