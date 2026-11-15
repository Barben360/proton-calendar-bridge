//go:build windows

package store

import (
	"io/fs"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

// describeOwner is Unreadable's "owner DOMAIN\name" for path: Windows has
// no modes, and its access list is what pcb doctor reports.
func describeOwner(path string, _ fs.FileInfo) string {
	owner, err := acl.Owner(path)
	if err != nil {
		return "owner unknown"
	}
	return "owner " + acl.AccountName(owner)
}
