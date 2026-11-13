package mcp

import "time"

// SetSessionIdle sets how long a remote session may stay unused for the
// handlers built afterwards (a test cannot wait an hour), and returns
// what restores it. Not for parallel tests.
func SetSessionIdle(d time.Duration) (restore func()) {
	old := sessionIdle
	sessionIdle = d
	return func() { sessionIdle = old }
}

// SessionIdle is the idle time of a remote session.
func SessionIdle() time.Duration { return sessionIdle }
