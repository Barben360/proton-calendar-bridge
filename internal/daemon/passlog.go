package daemon

import (
	"log/slog"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// ResolveDirs resolves the config and state directories: exit 20 with a
// hint when no absolute base is found.
func ResolveDirs(env func(string) string) (paths.Dirs, error) {
	dirs, err := paths.Resolve(env)
	if err != nil {
		return paths.Dirs{}, &exitcode.Error{Code: exitcode.Blocked, Err: err,
			Hint: "set HOME, or PCB_CONFIG_DIR and PCB_STATE_DIR, or XDG_CONFIG_HOME and XDG_STATE_HOME, to absolute paths"}
	}
	return dirs, nil
}

// LoadConfig resolves the directories and loads config.toml.
func LoadConfig(env func(string) string) (paths.Dirs, config.Config, error) {
	dirs, err := ResolveDirs(env)
	if err != nil {
		return paths.Dirs{}, config.Config{}, err
	}
	cfg, err := config.Load(config.Path(dirs), dirs)
	if err != nil {
		return paths.Dirs{}, config.Config{}, err
	}
	return dirs, cfg, nil
}

// RenderLog logs the render counters of each pass (ruling M3-R34) while one
// is non-zero, and once when they change (so a return to zero is logged);
// counts and calendar IDs only. Serve calls OnPass from one goroutine.
type RenderLog struct {
	last service.RenderCounts
}

func (rl *RenderLog) Log(log *slog.Logger, c *service.RenderCounts) {
	if c == nil || c.Zero() && rl.last.Zero() {
		return
	}
	rl.last = *c
	attrs := []any{"undecryptable", c.Undecryptable, "unverified", c.Unverified, "duplicate_masters", c.DuplicateMasters, "dropped_alarms", c.DroppedAlarms}
	if len(c.KeysErrors) > 0 {
		attrs = append(attrs, "keys_error_ids", c.KeysErrors)
	}
	if c.Zero() {
		log.Info("render counters", attrs...)
		return
	}
	log.Warn("render counters", attrs...)
}

// LogPass logs one sync pass: counts and calendar IDs, never names or
// content; a failed pass at warning level with its (secret-free) error.
func LogPass(log *slog.Logger, p service.WatchPass) {
	var added, updated, deleted int
	var failed []string
	for _, c := range p.Result.Calendars {
		added, updated, deleted = added+c.Added, updated+c.Updated, deleted+c.Deleted
		if c.Error != "" {
			failed = append(failed, c.ID)
		}
	}
	attrs := []any{"calendars", len(p.Result.Calendars), "added", added, "updated", updated, "deleted", deleted}
	if len(failed) > 0 {
		attrs = append(attrs, "failed_ids", failed)
	}
	if p.Next > 0 {
		attrs = append(attrs, "next", p.Next.Round(time.Second).String())
	}
	if p.Err != nil {
		attrs = append(attrs, "error", p.Err.Error())
		if h := exitcode.HintOf(p.Err); h != "" {
			attrs = append(attrs, "hint", h)
		}
		log.Warn("sync pass failed", attrs...)
		return
	}
	log.Info("sync pass", attrs...)
}
