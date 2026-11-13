package cli

import (
	"context"
	"log/slog"
	"time"
)

// SetLocalZone replaces the zone of date bounds and printed times, returning
// the function that restores it.
func SetLocalZone(z *time.Location) (restore func()) {
	old := localZone
	localZone = z
	return func() { localZone = old }
}

// LoggerFrom is the logger a command's context carries (nil: none), the
// one IO.OpenHub's accounts warn on.
func LoggerFrom(ctx context.Context) *slog.Logger { return loggerFrom(ctx) }
