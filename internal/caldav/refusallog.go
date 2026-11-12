package caldav

import (
	"sync"
	"time"
)

// A client that retries a refused write (Thunderbird keeps an offline-queued
// one) would fill the log: the same code and href is logged once per window.
const (
	refusalWindow  = 10 * time.Minute
	refusalLogSize = 1000 // distinct (code, href) kept; older ones are pruned
)

type refusalKey struct{ code, href string }

type refusalEntry struct {
	at         time.Time // last line logged
	suppressed int       // refusals since, not logged
}

// refusalLog remembers when each refusal was last logged, in bounded memory.
type refusalLog struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[refusalKey]*refusalEntry
}

func newRefusalLog(now func() time.Time) *refusalLog {
	return &refusalLog{now: now, m: map[refusalKey]*refusalEntry{}}
}

// allow reports whether this refusal is logged now; repeated is how many
// identical ones were swallowed since the last line (0 for a first one).
func (l *refusalLog) allow(code, href string) (repeated int, ok bool) {
	k, t := refusalKey{code, href}, l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, found := l.m[k]; found {
		if t.Sub(e.at) < refusalWindow {
			e.suppressed++
			return 0, false
		}
		repeated, e.at, e.suppressed = e.suppressed, t, 0
		return repeated, true
	}
	if len(l.m) >= refusalLogSize {
		l.prune(t)
	}
	l.m[k] = &refusalEntry{at: t}
	return 0, true
}

// prune drops the entries whose window ended; when none did, the oldest.
func (l *refusalLog) prune(now time.Time) {
	var oldest refusalKey
	var oldestAt time.Time
	for k, e := range l.m {
		if now.Sub(e.at) >= refusalWindow {
			delete(l.m, k)
			continue
		}
		if oldestAt.IsZero() || e.at.Before(oldestAt) {
			oldest, oldestAt = k, e.at
		}
	}
	if len(l.m) >= refusalLogSize {
		delete(l.m, oldest)
	}
}
