package access

import (
	"net"
	"strings"
	"sync"
	"time"
)

const (
	// MaxFailures is how many failed attempts an IP gets per window.
	MaxFailures = 5
	// Window is the length of the limiter's window; Touch is throttled to it too.
	Window = time.Minute
	// maxTracked bounds the limiter's map.
	maxTracked = 4096

	unparsableKey = "?"  // every remote that is not an IP shares this counter
	overflowKey   = "??" // every new IP while the table is full shares this one
)

// Limiter counts failed password checks per remote IP: after MaxFailures in
// a window the IP is blocked until the window ends. The window is fixed: it
// starts at an IP's first failure. Safe for concurrent use.
//
// Memory is bounded: expired entries are pruned, and a new IP arriving while
// maxTracked live entries exist is counted under one shared overflow key
// (an attacker with thousands of addresses can make the overflow key block
// new addresses for a minute, but cannot make the limiter grow or forget).
type Limiter struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*window
}

type window struct {
	start time.Time
	n     int
}

// NewLimiter returns a Limiter on now (nil: time.Now).
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, entries: map[string]*window{}}
}

// ipKey is the IP of remote ("host:port", "[v6]:port" or a bare IP) in its
// canonical form, or unparsableKey.
func ipKey(remote string) string {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	if i := strings.IndexByte(host, '%'); i >= 0 { // IPv6 zone
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return unparsableKey
	}
	return ip.String()
}

// live returns key's entry if its window has not ended.
func (l *Limiter) live(key string, now time.Time) *window {
	w := l.entries[key]
	if w != nil && !now.Before(w.start.Add(Window)) {
		delete(l.entries, key)
		return nil
	}
	return w
}

// Allow reports whether remote may try a password. When it may not,
// retryAfter is how long until its window ends.
func (l *Limiter) Allow(remote string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ipKey(remote)
	w := l.live(key, now)
	if w == nil && len(l.entries) >= maxTracked {
		l.prune(now)
		if len(l.entries) >= maxTracked {
			w = l.live(overflowKey, now)
		}
	}
	if w != nil && w.n >= MaxFailures {
		return false, w.start.Add(Window).Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt from remote.
func (l *Limiter) Fail(remote string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ipKey(remote)
	w := l.live(key, now)
	if w == nil {
		if len(l.entries) >= maxTracked {
			l.prune(now)
		}
		if len(l.entries) >= maxTracked {
			key = overflowKey
			w = l.live(key, now)
		}
	}
	if w == nil {
		w = &window{start: now}
		l.entries[key] = w
	}
	w.n++
}

func (l *Limiter) prune(now time.Time) {
	for k, w := range l.entries {
		if !now.Before(w.start.Add(Window)) {
			delete(l.entries, k)
		}
	}
}
