//go:build !windows

package cli

import "os"

// sessionEndSignal is nil: off Windows no signal marks a session end, and
// SIGTERM (systemd's stop included) drains for the full 5 s.
var sessionEndSignal os.Signal
