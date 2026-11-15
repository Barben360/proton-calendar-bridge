package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// CacheFor is how long an answer is kept before the next request.
const CacheFor = 24 * time.Hour

// EnvSwitch turns the check off for one process when set to "off".
const EnvSwitch = "PCB_UPDATE_CHECK"

// State is update.json: the switch (Off) and the last answer.
type State struct {
	Off       bool      `json:"off,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Latest    string    `json:"latest,omitempty"`
	URL       string    `json:"url,omitempty"`
}

// StorePath is update.json in the state directory.
func StorePath(stateDir string) string { return filepath.Join(stateDir, "update.json") }

// Load reads path; a missing or unreadable file is the zero State (the
// switch on, nothing cached).
func Load(path string) State {
	var st State
	b, err := atomicfile.ReadFile(path)
	if err != nil || json.Unmarshal(b, &st) != nil {
		return State{}
	}
	return st
}

// Edit applies f to path's State and writes it back, under path+".lock" so
// that two processes (pcb-ui, pcb doctor, the switch command) never erase
// each other's change. Where locks are unsupported it writes unlocked. The
// directory must exist: a check never creates the state directory (pcb
// creates it, with its owner and mode, when it first needs it).
func Edit(ctx context.Context, path string, f func(*State)) error {
	unlock, err := filelock.Lock(ctx, path+".lock")
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
	case err != nil:
		return err
	default:
		defer unlock()
	}
	st := Load(path)
	f(&st)
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(b, '\n'))
}

// Enabled reports whether the check runs: not when st.Off, nor when the
// environment's PCB_UPDATE_CHECK is "off".
func Enabled(env func(string) string, st State) bool {
	return !st.Off && !strings.EqualFold(strings.TrimSpace(env(EnvSwitch)), "off")
}

// Result is one answer: the newest stable release and whether it is above
// the installed version, or why there is none (Err).
type Result struct {
	Latest    Release
	Available bool
	CheckedAt time.Time
	Err       error
}

// Cached answers from path when its last answer is under CacheFor old (and
// not in the future: a clock set back re-checks), else asks GitHub and
// keeps the answer, best effort: a state pcb cannot write only loses the
// cache. A failure is not kept, so the next call retries.
func (c Checker) Cached(ctx context.Context, path string, now time.Time) Result {
	if !Checkable(c.Installed) {
		return Result{Err: ErrDevBuild}
	}
	st := Load(path)
	if age := now.Sub(st.CheckedAt); !st.CheckedAt.IsZero() && age >= 0 && age < CacheFor {
		if rel, ok := c.cachedRelease(st); ok {
			return c.result(rel, st.CheckedAt)
		}
	}
	return c.Fresh(ctx, path, now)
}

// Fresh asks GitHub now and keeps the answer in path, best effort.
func (c Checker) Fresh(ctx context.Context, path string, now time.Time) Result {
	rel, err := c.Latest(ctx)
	if err != nil {
		return Result{Err: err, CheckedAt: now}
	}
	_ = Edit(ctx, path, func(s *State) { s.CheckedAt, s.Latest, s.URL = now, rel.Version, rel.URL })
	return c.result(rel, now)
}

// Last is path's last answer, whatever its age, when it passes the
// redirect's own checks: what pcb-ui keeps showing while a check fails.
func (c Checker) Last(path string) (Release, bool) { return c.cachedRelease(Load(path)) }

// cachedRelease is st's answer when it passes the redirect's own checks
// (update.json is data from disk).
func (c Checker) cachedRelease(st State) (Release, bool) {
	u, err := url.Parse(st.URL)
	if err != nil {
		return Release{}, false
	}
	rel, err := c.release(u)
	return rel, err == nil && rel.Version == st.Latest
}

func (c Checker) result(rel Release, at time.Time) Result {
	return Result{Latest: rel, Available: Newer(c.Installed, rel.Version), CheckedAt: at}
}
