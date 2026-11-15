package doctor

import (
	"context"
	"fmt"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// CacheCheck verifies the event cache file: its mode (Windows: its access
// list), its schema and the
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
	var aclMsg, aclFix string
	checked := false // the access list is checked (Windows), not the mode
	if err == nil && info.Exists {
		aclMsg, aclFix, checked = aclProblem(path, false)
	}
	switch {
	case err != nil:
		r.Status, r.Message, r.Fix = StatusError, err.Error(), rebuild
	case !info.Exists:
		r.Status, r.Message = StatusOK, "no cache yet"
	case aclMsg != "":
		r.Status, r.Message, r.Fix = StatusError, aclMsg, aclFix
	case !checked && info.Mode.Perm()&^0o600 != 0:
		r.Status = StatusError
		r.Message = path + " has mode " + modeString(info.Mode.Perm()) + ", want 600"
		r.Fix = "chmod 600 " + shellquote.Quote(path)
	case info.QuickCheck != "ok":
		r.Status, r.Message, r.Fix = StatusError, "cache is damaged: "+info.QuickCheck, rebuild
	case info.Schema > cache.SchemaVersion:
		r.Status, r.Fix = StatusError, "upgrade pcb (or remove the cache and run `pcb sync --full` to rebuild it with this version)"
		r.Message = fmt.Sprintf("cache has schema version %d, newer than this pcb supports (%d)", info.Schema, cache.SchemaVersion)
	case info.Schema < 1: // version 1 is an M3 file, upgraded by the next opening for writing
		r.Status, r.Fix = StatusError, rebuild
		r.Message = fmt.Sprintf("cache has an unknown schema version %d", info.Schema)
	default:
		access := "600"
		if checked {
			access = "private"
		}
		r.Status, r.Message = StatusOK, fmt.Sprintf("%s %s, schema %d, integrity ok", path, access, info.Schema)
	}
	return r
}
