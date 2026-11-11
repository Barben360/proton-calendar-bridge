package service

import (
	"context"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
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
