// Package buildinfo holds the version stamped at link time (make build).
package buildinfo

// Set with -ldflags "-X github.com/Barben360/proton-calendar-bridge/internal/buildinfo.Version=…".
var (
	Version = "dev"
	Commit  = "dev"
	Date    = "dev"
)
