package syncer

import (
	"context"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// Watch loop timings.
const (
	DefaultInterval = 30 * time.Second
	MinInterval     = 10 * time.Second // a shorter Interval is raised to it
	MaxBackoff      = 10 * time.Minute // or Interval when longer
	MaxRetryAfter   = time.Hour        // the longest wait a failure may ask for
	jitter          = 0.10             // ± fraction of a backoff wait
)

// WatchOptions tune Watch. The zero value runs passes every 30 s and never
// stops on an error.
type WatchOptions struct {
	Interval time.Duration                                    // between passes; default DefaultInterval, at least MinInterval
	Sleep    func(ctx context.Context, d time.Duration) error // default: a timer, cut short by ctx
	OnPass   func(Report, error, time.Duration)               // every pass's outcome and the wait before the next (0: Watch returns); nil: nothing
	Stop     func(error) bool                                 // true: Watch returns this pass error; nil: never
	Rand     func() float64                                   // jitter source in [0, 1); default math/rand/v2
	// RetryAfter is the least wait a pass error asks for (a 429's
	// Retry-After), 0 for none; nil: none. Watch waits the longer of it,
	// capped at MaxRetryAfter, and the back-off.
	RetryAfter func(error) time.Duration
	// NextInterval is the wait after a successful pass (Serve slows it when
	// no client is about); nil: Interval. It is raised to at least
	// MinInterval. A failure's back-off still starts from Interval.
	NextInterval func() time.Duration
	// Wake ends a sleep that follows a successful pass, so the next pass
	// starts at once. During a back-off or a Retry-After wait a wake is
	// received and dropped: it never shortens one.
	Wake <-chan struct{}
}

// Watch runs pass until ctx ends or Stop accepts a pass error, which it
// returns (ctx.Err() otherwise). After a successful pass it waits Interval;
// after a failed one Interval doubled per consecutive failure, capped at
// the longer of MaxBackoff and Interval (a failure never polls faster than a
// success), ±10 %, or longer when the error asks (RetryAfter). A pass that
// fails because ctx ended is not reported.
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
		wait := o.Interval
		stop := err != nil && o.Stop != nil && o.Stop(err)
		switch {
		case stop:
			wait = 0
		case err != nil:
			failures++
			wait = backoff(o.Interval, failures, o.Rand())
			if o.RetryAfter != nil {
				wait = max(wait, min(o.RetryAfter(err), MaxRetryAfter))
			}
		default:
			failures = 0
			if o.NextInterval != nil {
				wait = max(o.NextInterval(), MinInterval)
			}
		}
		if o.OnPass != nil {
			o.OnPass(rep, err, wait)
		}
		if stop {
			return err
		}
		if err := sleepOrWake(ctx, o, wait, err == nil); err != nil {
			return err
		}
	}
}

// sleepOrWake sleeps wait; after a success (cuttable) a receive on o.Wake
// ends the sleep early and returns nil. It returns ctx's error when ctx
// ended, or the sleep's own.
func sleepOrWake(ctx context.Context, o WatchOptions, wait time.Duration, cuttable bool) error {
	if o.Wake == nil {
		return finishSleep(ctx, o.Sleep(ctx, wait))
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var woken atomic.Bool
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-o.Wake:
				if cuttable {
					woken.Store(true)
					cancel()
					return
				}
			case <-sctx.Done():
				return
			}
		}
	}()
	err := o.Sleep(sctx, wait)
	cancel()
	<-stopped
	if err != nil && woken.Load() && ctx.Err() == nil {
		return nil
	}
	return finishSleep(ctx, err)
}

func finishSleep(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
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
