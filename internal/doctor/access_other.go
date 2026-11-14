//go:build !windows

package doctor

import (
	"os"
	"strconv"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// aclProblem is Windows'; elsewhere checked is false and callers judge the
// Unix mode.
func aclProblem(string, bool) (message, fix string, checked bool) { return "", "", false }

// unreadableKeyFix is the fix for a key file this user cannot read.
func unreadableKeyFix(path string) string {
	return "chown " + ownerText() + " " + shellquote.Quote(path) + " (a container secret: on the host, chown 65532:65532 and chmod 600 the key file)"
}

// ownerText is the owner a chown fix names: this process's UID.
func ownerText() string {
	if uid := os.Geteuid(); uid >= 0 {
		return strconv.Itoa(uid)
	}
	return "$USER"
}
