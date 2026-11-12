package service

import (
	"context"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
)

// SessionClient exposes the stored session's client so tests can tell a
// reused client from a new one.
func (l *Local) SessionClient(ctx context.Context) (*api.Client, error) {
	c, _, _, err := l.session(ctx)
	return c, err
}

// HasCachedClient reports whether a session client is cached.
func (l *Local) HasCachedClient() bool {
	l.cmu.Lock()
	defer l.cmu.Unlock()
	return l.cached != nil
}

// Decrypts counts the rows decrypted so far.
func (l *Local) Decrypts() int64 { return l.r.decrypts.Load() }

// HasKeys reports whether unlocked keys are held.
func (l *Local) HasKeys() bool {
	l.r.kmu.RLock()
	defer l.r.kmu.RUnlock()
	return l.r.ks != nil
}

// BeforeKeyLoad makes fn run as each key load starts (set it before any
// read).
func (l *Local) BeforeKeyLoad(fn func()) { l.r.beforeLoad = fn }

// SetTouchHook observes background touches: f gets "start" and "done" with
// the client ID. Set it before the first CheckClient.
func (l *Local) SetTouchHook(f func(phase, id string)) { l.touchHook = f }

// SetTouchTimeout sets how long a background touch may wait for the clients lock.
func (l *Local) SetTouchTimeout(d time.Duration) { l.touchTimeout = d }

// AddressKeyTags exposes the address-key tags of u.
func AddressKeyTags(u *keys.Unlocked) []string { return addressKeyTags(u) }

// OnWriteWait makes fn run when a write finds its calendar's lock taken (by
// another write or the syncer's pass of the calendar), before it waits. Set
// it before Serve.
func (l *Local) OnWriteWait(fn func(calID string)) { l.cals.waiting = fn }

// LockCalendar takes calendar calID's write lock, as a write or the
// syncer's pass of the calendar does.
func (l *Local) LockCalendar(ctx context.Context, calID string) (func(), error) {
	return l.cals.lock(ctx, calID, nil)
}

// SetPreSendBudget sets how long a write may take before its Sync call
// (preSendBudget in production). Set it before Serve.
func (l *Local) SetPreSendBudget(d time.Duration) { l.preSend = d }

// SetWriteFault makes fn fail a write's cache steps: stage "pin" (the rows
// Proton answered as created, before the read-back) and "store" (the rows
// read back); a nil return lets the step run. Set it before Serve.
func (l *Local) SetWriteFault(fn func(stage string) error) { l.writeFault = fn }

// ErrorClass exposes errorClass.
func ErrorClass(err error) string { return errorClass(err) }

// MapError exposes mapError with no key file named: the mapped error carries
// its class, as a pass's failure does.
func MapError(err error) error { return mapError(err, "") }

// WakesSent counts the wakes Serve's loop was sent (a request that found it idle).
func (l *Local) WakesSent() int64 {
	if v := l.liveState(); v != nil {
		return v.wakes.Load()
	}
	return 0
}
