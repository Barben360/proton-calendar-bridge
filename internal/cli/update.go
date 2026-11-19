package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

// executable is IO.Exe in production: this binary, symlinks resolved.
func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// updatePath is update.json in the state directory ("" when it cannot be
// resolved: nothing is kept, the switch reads on).
func (a *app) updatePath() string {
	dirs, err := resolveDirs(a.Env)
	if err != nil {
		return ""
	}
	return update.StorePath(dirs.State)
}

// thisInstall is this install as update.Hint reads it; ok is false when the
// running binary cannot be found.
func (a *app) thisInstall() (in update.Install, ok bool) {
	exe, err := a.Exe()
	if err != nil {
		return update.Install{}, false
	}
	home := a.Env("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return update.Install{GOOS: runtime.GOOS, Exe: exe, Home: home, Env: a.Env}, true
}

// updateHint is how this install is updated (update.Hint).
func (a *app) updateHint() string {
	in, ok := a.thisInstall()
	if !ok {
		return ""
	}
	return update.Hint(in)
}

// updateData is pcb version --check's "update".
type updateData struct {
	Latest    string `json:"latest,omitempty"`
	URL       string `json:"url,omitempty"`
	Available bool   `json:"available"`
	Hint      string `json:"hint,omitempty"`
	Error     string `json:"error,omitempty"`
}

// checkNow is pcb version --check: GitHub asked now, whatever the switch
// and the cache say (an explicit request), the answer kept best effort.
func (a *app) checkNow(ctx context.Context) (*updateData, string) {
	res := a.Update.Fresh(ctx, a.updatePath(), time.Now())
	d := &updateData{Latest: res.Latest.Version, URL: res.Latest.URL, Available: res.Available}
	switch {
	case errors.Is(res.Err, update.ErrDevBuild):
		d.Error = res.Err.Error()
		return d, "check skipped: dev build"
	case res.Err != nil:
		d.Error = res.Err.Error()
		return d, "check failed: " + res.Err.Error()
	case !res.Available:
		return d, "up to date"
	}
	d.Hint = a.updateHint()
	text := fmt.Sprintf("update available: %s %s", d.Latest, d.URL)
	if d.Hint != "" {
		text += "\n" + d.Hint
	}
	return d, text
}

// updateResult is pcb doctor's update line, from the 24 h cache. Never an
// error: an available update is a warning, anything that stops the check
// a skip.
func (a *app) updateResult(ctx context.Context) doctor.Result {
	r := doctor.Result{Name: "update", Status: doctor.StatusSkip}
	path := a.updatePath()
	if !update.Enabled(a.Env, update.Load(path)) {
		r.Message = "switched off"
		return r
	}
	res := a.Update.Cached(ctx, path, time.Now())
	switch {
	case errors.Is(res.Err, update.ErrDevBuild):
		r.Message = "dev build"
	case res.Err != nil:
		r.Message = "check failed: " + res.Err.Error()
	case res.Available:
		r.Status = doctor.StatusWarn
		r.Message = fmt.Sprintf("%s is available (you have %s)", res.Latest.Version, a.Update.Installed)
		r.Fix = res.Latest.URL
		if h := a.updateHint(); h != "" {
			r.Fix += "; " + h
		}
	default:
		r.Status, r.Message = doctor.StatusOK, "up to date ("+a.Update.Installed+")"
	}
	return r
}

// updateCheckData is pcb update-check's result.
type updateCheckData struct {
	Enabled bool `json:"enabled"`
	EnvOff  bool `json:"env_off"`
}

func (a *app) newUpdateCheck() *cobra.Command {
	return OneOperand(&cobra.Command{
		Use:   "update-check on|off|status",
		Short: "Turn the check for a newer pcb release on or off, or show it",
		Long: "pcb doctor and pcb-ui ask github.com once a day whether a newer stable release exists. " +
			"This switch, kept in the state directory, turns that off; PCB_UPDATE_CHECK=off does too, for one process. " +
			"pcb version --check always asks.",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := a.updatePath()
			switch args[0] {
			case "on", "off":
				if err := a.stateOwner(); err != nil {
					return Finish(cmd, nil, "", err)
				}
				off := args[0] == "off"
				err := acl.MkdirAll(filepath.Dir(path))
				if err == nil {
					err = update.Edit(cmd.Context(), path, func(s *update.State) { s.Off = off })
				}
				if err != nil {
					return Finish(cmd, nil, "", fmt.Errorf("save the update check switch: %w", err))
				}
			case "status":
			default:
				return UsageError(fmt.Errorf("%q takes on, off or status, not %q", cmd.CommandPath(), args[0]))
			}
			st := update.Load(path)
			d := updateCheckData{Enabled: update.Enabled(a.Env, st), EnvOff: !update.Enabled(a.Env, update.State{})}
			text := "update check: on"
			switch {
			case st.Off:
				text = "update check: off"
			case d.EnvOff:
				text = "update check: off (off by " + update.EnvSwitch + ")"
			}
			return Finish(cmd, d, text, nil)
		},
	}, "on, off or status")
}
