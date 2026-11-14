package access

import (
	"container/list"
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
	// maxTracked bounds the limiter's table.
	maxTracked = 4096

	unparsableKey = "?" // every remote that is not an IP shares this counter
)

// Limiter counts failed password checks per remote IP: after MaxFailures in
// a window the IP is blocked until the window ends. The window is fixed: it
// starts at an IP's first failure. IPv4 is keyed by address, IPv6 by /64
// (one subscriber owns a whole /64), anything unparsable by one shared key.
// Safe for concurrent use.
//
// Memory is bounded without a shared bucket (ruling M3-R20): only failures
// create entries, entries expire in the order they were created, so the
// table is a FIFO queue; expired ones are dropped from its front, and when
// maxTracked live ones exist the oldest is evicted. A flood from many
// addresses therefore never locks out an address it did not come from; its
// own addresses get at most MaxFailures guesses each per window (an evicted
// one starts over, which costs the attacker nothing a per-IP limit could
// prevent).
type Limiter struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*list.Element // key -> element holding *window
	order   *list.List               // oldest window first
}

type window struct {
	key   string
	start time.Time
	n     int
}

// NewLimiter returns a Limiter on now (nil: time.Now).
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, entries: map[string]*list.Element{}, order: list.New()}
}

// ipKey is the key of remote ("host:port", "[v6]:port" or a bare IP): the
// IPv4 address, the IPv6 /64, or unparsableKey.
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
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// prune drops the windows that ended, oldest first.
func (l *Limiter) prune(now time.Time) {
	for e := l.order.Front(); e != nil; e = l.order.Front() {
		w := e.Value.(*window)
		if now.Before(w.start.Add(Window)) {
			return
		}
		l.order.Remove(e)
		delete(l.entries, w.key)
	}
}

// Allow reports whether remote may try a password. When it may not,
// retryAfter is how long until its window ends.
func (l *Limiter) Allow(remote string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	if e := l.entries[ipKey(remote)]; e != nil {
		if w := e.Value.(*window); w.n >= MaxFailures {
			return false, w.start.Add(Window).Sub(now)
		}
	}
	return true, 0
}

// Fail records a failed attempt from remote.
func (l *Limiter) Fail(remote string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	key := ipKey(remote)
	if e := l.entries[key]; e != nil {
		e.Value.(*window).n++
		return
	}
	if len(l.entries) >= maxTracked {
		oldest := l.order.Front()
		delete(l.entries, oldest.Value.(*window).key)
		l.order.Remove(oldest)
	}
	l.entries[key] = l.order.PushBack(&window{key: key, start: now, n: 1})
}
