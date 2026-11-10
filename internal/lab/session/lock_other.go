//go:build !unix

package session

import "github.com/Barben360/proton-calendar-bridge/internal/exitcode"
import "errors"

// lockLogin is unsupported off Unix: the lab runs on Linux and macOS.
func lockLogin(string) (func(), error) {
	return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("pcb-lab login needs a Unix system for its lock")}
}
