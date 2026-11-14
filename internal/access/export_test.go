package access

// MaxTracked and Tracked let the external tests see the limiter's bound.
const MaxTracked = maxTracked

// Tracked is the number of keys the limiter holds.
func (l *Limiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
