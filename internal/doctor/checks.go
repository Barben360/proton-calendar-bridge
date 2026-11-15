package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// PlatformCheck reports whether pcb is supported on GOOS/GOARCH
// (runtime.GOOS and runtime.GOARCH in production): Linux amd64 and arm64
// are, and so is windows/amd64 since its live check (windowsTested);
// anything else runs, unsupported.
type PlatformCheck struct{ GOOS, GOARCH string }

// windowsTested is whether windows/amd64 passed the live checks
// (spec/HUMAN-CHECKS.md step 12, spec/experiments/W1-windows-live.md):
// without them pcb doctor would warn that it is untested.
const windowsTested = true

// Name implements Check.
func (PlatformCheck) Name() string { return "platform" }

// Run implements Check.
func (c PlatformCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Status: StatusOK, Message: c.GOOS + "/" + c.GOARCH}
	switch {
	case c.GOOS == "linux" && (c.GOARCH == "amd64" || c.GOARCH == "arm64"):
	case c.GOOS == "windows" && c.GOARCH == "amd64":
		if !windowsTested {
			r.Status = StatusWarn
			r.Message += ": runs, untested"
		}
	default:
		r.Status = StatusWarn
		r.Message += ": runs, unsupported"
	}
	return r
}

// DirModeCheck verifies that a directory grants no more than Want (on
// Windows: that its access list and owner keep other users out).
type DirModeCheck struct {
	Label string
	Path  string
	Want  fs.FileMode
}

// Name implements Check.
func (c DirModeCheck) Name() string { return c.Label }

// Run implements Check. A missing directory is skipped: it is created later
// with the right mode.
func (c DirModeCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	info, err := os.Stat(c.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Status, r.Message = StatusSkip, c.Path+" does not exist yet"
	case err != nil:
		r.Status, r.Message = StatusError, err.Error()
	case !info.IsDir():
		r.Status, r.Message = StatusError, c.Path+" is not a directory"
	default:
		r = judgeAccess(r, c.Path, true, info.Mode().Perm(), c.Want)
	}
	return r
}

// judgeAccess rates an existing path as pcb uses it: by its access list and,
// for a directory, its owner on Windows (aclProblem), else by its mode
// against want.
func judgeAccess(r Result, path string, dir bool, mode, want fs.FileMode) Result {
	if msg, fix, checked := aclProblem(path, dir); checked {
		if msg != "" {
			r.Status, r.Message, r.Fix = StatusError, msg, fix
		} else {
			r.Status, r.Message = StatusOK, path+" private"
		}
		return r
	}
	if extra := mode &^ want; extra != 0 {
		r.Status = StatusError
		r.Message = path + " has mode " + modeString(mode) + ", want " + modeString(want)
		r.Fix = "chmod " + modeString(want) + " " + shellquote.Quote(path)
	} else {
		r.Status, r.Message = StatusOK, path+" "+modeString(mode)
	}
	return r
}

func modeString(m fs.FileMode) string {
	const digits = "01234567"
	return string([]byte{digits[m>>6&7], digits[m>>3&7], digits[m&7]})
}

// FailedCheck reports a check that could not even be set up, as an error.
type FailedCheck struct {
	Label   string
	Message string
	Fix     string
}

// Name implements Check.
func (c FailedCheck) Name() string { return c.Label }

// Run implements Check.
func (c FailedCheck) Run(context.Context) Result {
	return Result{Name: c.Label, Status: StatusError, Message: c.Message, Fix: c.Fix}
}

// ConfigCheck verifies that config.toml, if present, loads.
type ConfigCheck struct {
	Path string
	Dirs paths.Dirs
}

// Name implements Check.
func (ConfigCheck) Name() string { return "config file" }

// Run implements Check.
func (c ConfigCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	if _, err := config.Load(c.Path, c.Dirs); err != nil {
		r.Status, r.Message, r.Fix = StatusError, err.Error(), "edit "+shellquote.Quote(c.Path)
		return r
	}
	r.Status, r.Message = StatusOK, c.Path
	if _, err := os.Stat(c.Path); errors.Is(err, fs.ErrNotExist) {
		r.Message = "defaults, no file"
	}
	return r
}

// FileModeCheck verifies that a regular file grants no more than Want (on
// Windows: that its access list keeps other users out).
type FileModeCheck struct {
	Label string
	Path  string
	Want  fs.FileMode
}

// Name implements Check.
func (c FileModeCheck) Name() string { return c.Label }

// Run implements Check. A missing file is skipped: it is created later with
// the right mode.
func (c FileModeCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	info, err := os.Stat(c.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Status, r.Message = StatusSkip, c.Path+" does not exist yet"
	case err != nil:
		r.Status, r.Message = StatusError, err.Error()
	case !info.Mode().IsRegular():
		r.Status, r.Message = StatusError, c.Path+" is not a regular file"
	default:
		r = judgeAccess(r, c.Path, false, info.Mode().Perm(), c.Want)
	}
	return r
}
