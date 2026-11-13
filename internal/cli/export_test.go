package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// SealerGuard is pcb mcp's sealer guard for the state env resolves to,
// with config.toml loaded, probing kr.
func SealerGuard(ctx context.Context, env func(string) string, kr service.Keyring) error {
	dirs, cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	return sealerGuard(ctx, dirs, cfg, kr)
}

// MCPBackoff is the waits of pcb mcp's serve.lock race, in order.
func MCPBackoff() []time.Duration { return lockBackoff() }

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
