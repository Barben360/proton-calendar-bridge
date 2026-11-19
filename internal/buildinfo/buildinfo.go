// Package buildinfo holds the version stamped at link time (make build).
package buildinfo

// Set with -ldflags "-X github.com/Barben360/proton-calendar-bridge/internal/buildinfo.Version=…".
var (
	Version = "dev"
	Commit  = "dev"
	Date    = "dev"
	// ReportDSN is where approved crash reports go (a Sentry DSN); empty
	// (dev and source builds): no report is ever collected, saved or sent.
	ReportDSN = ""
)
