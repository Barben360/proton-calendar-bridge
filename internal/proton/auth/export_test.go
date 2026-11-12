package auth

import (
	"testing"
	"time"
)

// SetCallTimeout sets the per-call deadline of the login calls for t's
// duration. It changes package state: t must not run in parallel.
func SetCallTimeout(t testing.TB, d time.Duration) {
	t.Helper()
	old := callTimeout
	callTimeout = d
	t.Cleanup(func() { callTimeout = old })
}
