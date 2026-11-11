package doctor

import (
	"context"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// CacheCheck verifies the event cache file: its mode, its schema and the
// SQLite integrity check. The cache holds only what Proton returned, so the
// repair is always to remove it and sync again.
type CacheCheck struct {
	StateDir string
}

// Name implements Check.
func (CacheCheck) Name() string { return "cache" }

// Run implements Check.
func (c CacheCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	path := cache.Path(c.StateDir)
	info, err := cache.Inspect(path)
	rebuild := "remove " + shellquote.Quote(path) + " (and its -wal and -shm files), then run `pcb sync --full`"
	switch {
	case err != nil:
		r.Status, r.Message, r.Fix = StatusError, err.Error(), rebuild
	case !info.Exists:
		r.Status, r.Message = StatusOK, "no cache yet"
	case info.Mode.Perm() != 0o600:
		r.Status = StatusError
		r.Message = path + " has mode " + modeString(info.Mode.Perm()) + ", want 600"
		r.Fix = "chmod 600 " + shellquote.Quote(path)
	case info.QuickCheck != "ok":
		r.Status, r.Message, r.Fix = StatusError, "cache is damaged: "+info.QuickCheck, rebuild
	case info.Schema != 1:
		r.Status, r.Fix = StatusError, rebuild
		r.Message = "cache has an unknown schema version"
	default:
		r.Status, r.Message = StatusOK, path+" 600, schema 1, integrity ok"
	}
	return r
}
