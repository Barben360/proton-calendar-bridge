package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// PlatformCheck reports whether pcb is supported on GOOS/GOARCH
// (runtime.GOOS and runtime.GOARCH in production).
type PlatformCheck struct{ GOOS, GOARCH string }

// Name implements Check.
func (PlatformCheck) Name() string { return "platform" }

// Run implements Check.
func (c PlatformCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Status: StatusOK, Message: c.GOOS + "/" + c.GOARCH}
	if c.GOOS != "linux" || (c.GOARCH != "amd64" && c.GOARCH != "arm64") {
		r.Status = StatusWarn
		r.Message += ": runs, unsupported"
	}
	return r
}

// DirModeCheck verifies that a directory grants no more than Want.
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
		if extra := info.Mode().Perm() &^ c.Want; extra != 0 {
			r.Status = StatusError
			r.Message = c.Path + " has mode " + modeString(info.Mode().Perm()) + ", want " + modeString(c.Want)
			r.Fix = "chmod " + modeString(c.Want) + " " + c.Path
		} else {
			r.Status, r.Message = StatusOK, c.Path+" "+modeString(info.Mode().Perm())
		}
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
