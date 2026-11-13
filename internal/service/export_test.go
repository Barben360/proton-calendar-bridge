package service

import (
	"context"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
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

// WakePending reports whether a wake waits in Serve's channel, not yet taken
// by the loop.
func (l *Local) WakePending() bool {
	v := l.liveState()
	return v != nil && len(v.wake) > 0
}

// WakesSent counts the wakes Serve's loop was sent (a request that found it idle).
func (l *Local) WakesSent() int64 {
	if v := l.liveState(); v != nil {
		return v.wakes.Load()
	}
	return 0
}

// OnHumanNeeded makes f run when a needs-login mark is decided (state.json
// still describes the failed login), before it is written. Set it before
// the first call.
func (l *Local) OnHumanNeeded(f func()) { l.humanHook = f }

// SetStateTimeout bounds humanNeeded's wait for state.lock.
func (l *Local) SetStateTimeout(d time.Duration) { l.stateTimeout = d }

// OnLoginCheck makes f run at each login check of a Serve waiting for a
// login (the first one, each poll and each wake). Set it before Serve.
func (l *Local) OnLoginCheck(f func()) { l.checkHook = f }

// OnWipe makes f run in Logout just before the local wipe.
func (l *Local) OnWipe(f func()) { l.wipeHook = f }

// BeforeServe makes f run each time a Serve waiting for a login found one
// ready, before it opens the cache. Set it before Serve.
func (l *Local) BeforeServe(f func()) { l.serveHook = f }

// SetLoginPollTimer replaces the timer between two login checks: f gets the
// interval and returns the channel that ends the wait. Set it before Serve.
func (l *Local) SetLoginPollTimer(f func(d time.Duration) <-chan time.Time) { l.pollHook = f }

// ServingHandle reports whether Serve holds its cache handle for the reads.
func (l *Local) ServingHandle() bool {
	l.smu.RLock()
	defer l.smu.RUnlock()
	return l.served != nil
}

// SkipSRP makes Login take its session from session (tokens and user ID of
// the username's account) instead of SRP; all that follows SRP runs as usual.
// The password and any CAPTCHA are not checked. nil restores SRP.
func (l *Local) SkipSRP(session func(username string) (api.Tokens, string)) {
	if session == nil {
		l.skipSRP = nil
		return
	}
	l.skipSRP = func(username string) *auth.Pending {
		tok, id := session(username)
		return auth.Resume(l.mgr, tok, id)
	}
}

// SkipReloginSRP makes the unattended relogins take what SRP would answer
// from relogin, called under the login guard where SRP would run: a
// session (its tokens and user ID; all that follows SRP runs as usual), or
// an error as auth.Begin's (CaptchaError: Proton asks for a CAPTCHA). The
// stored password is not checked. nil restores SRP. Set it before the
// relogin.
func (l *Local) SkipReloginSRP(relogin func(username string) (api.Tokens, string, error)) {
	if relogin == nil {
		l.skipRelogin = nil
		return
	}
	l.skipRelogin = func(username string) (*auth.Pending, error) {
		tok, id, err := relogin(username)
		if err != nil {
			return nil, err
		}
		return auth.Resume(l.mgr, tok, id), nil
	}
}

// CaptchaError is the error a login's SRP step returns when Proton asks for
// a CAPTCHA, its verification URL and token as given.
func CaptchaError(url, token string) error { return &auth.CaptchaError{URL: url, Token: token} }

// SetKeyPassHook makes fn see the key passphrase a login derives, before it
// is sealed (to check that it is zeroed after). Set it before Login.
func (l *Local) SetKeyPassHook(fn func(keyPass []byte)) { l.keyPassHook = fn }

// SkipSRP makes the logins of every Local the Hub holds or opens take their
// session from session instead of SRP, as Local.SkipSRP does. nil restores
// SRP.
func (h *Hub) SkipSRP(session func(username string) (api.Tokens, string)) {
	h.mu.Lock()
	h.onOpen = func(l *Local) { l.SkipSRP(session) }
	locals := make([]*Local, 0, len(h.accounts))
	for _, a := range h.accounts {
		locals = append(locals, a.local)
	}
	h.mu.Unlock()
	for _, l := range locals {
		l.SkipSRP(session)
	}
}

// LocalOf is the Local of the Hub's account id, nil if none.
func (h *Hub) LocalOf(id string) *Local {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a, ok := h.accounts[id]; ok {
		return a.local
	}
	return nil
}

// OnPark makes f run each time a serve loop parked its account (after
// a.loop is cleared, before the loop's end is signalled). Set it before
// Serve.
func (h *Hub) OnPark(f func(id string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onPark = f
}

// SyncLockRetry is how long a loop waits before it takes a held sync.lock
// again.
const SyncLockRetry = syncLockRetry

// KeyringRetry is how long a loop waits before it asks a locked desktop
// keyring again.
const KeyringRetry = keyringRetry
