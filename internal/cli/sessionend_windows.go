package cli

import (
	"os"
	"syscall"
)

// sessionEndSignal is the signal that marks the end of the session: Go
// delivers a console's close, logoff and shutdown events as SIGTERM, and
// Windows ends the process about 5 s later (spec §5.11), so pcb serve
// drains for 2 s only.
var sessionEndSignal os.Signal = syscall.SIGTERM
