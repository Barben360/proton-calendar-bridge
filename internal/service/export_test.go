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
