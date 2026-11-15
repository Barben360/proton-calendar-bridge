package ctl

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// pipePrefix starts the name of every control pipe on Windows.
const pipePrefix = `\\.\pipe\pcb-`

// pipeNameFor is the control pipe of the state directory whose final path
// (GetFinalPathNameByHandle) is final: the first 16 hex digits of the
// SHA-256 of that path lower-cased, so one directory gets one pipe whatever
// its case, short form or junction, two states get two, and the name leaks
// no path. Plain logic, tested on every OS; only Windows serves the pipe.
func pipeNameFor(final string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(final)))
	return pipePrefix + hex.EncodeToString(sum[:])[:16]
}
