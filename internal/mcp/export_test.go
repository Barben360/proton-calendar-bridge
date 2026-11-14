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

// LocalZoneFrom is localZone on goos over the given environment, link
// reader and file reader.
func LocalZoneFrom(goos string, env func(string) (string, bool), readlink func(string) (string, error), readFile func(string) ([]byte, error)) *time.Location {
	return localZoneFrom(goos, env, readlink, readFile)
}

// ZoneLabel is the name results give loc at t.
func ZoneLabel(loc *time.Location, t time.Time) string { return zoneLabel(loc, t) }

// SetLocalZone replaces how NewServer finds the process's zone, and
// returns what restores it. Not for parallel tests.
func SetLocalZone(f func() *time.Location) (restore func()) {
	old := localZone
	localZone = f
	return func() { localZone = old }
}
