package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// SystemKeyring is the keyring of the session bus.
func SystemKeyring() service.Keyring { return seal.DBusKeyring("") }

// SealerCheck reports the sealer layout.json records. On a fresh state
// (no layout.json) none is recorded: the first login picks it by [secrets]
// backend, which is all that can be said (the keyring is not probed here).
type SealerCheck struct {
	Sealer  string
	Fresh   bool
	Backend string // [secrets] backend, for a fresh state
}

// Name implements Check.
func (SealerCheck) Name() string { return "sealer" }

// Run implements Check.
func (c SealerCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Status: StatusOK, Message: c.Sealer}
	if c.Fresh && c.Backend != service.SealerKeyFile {
		r.Message = "not chosen yet: the first login picks it ([secrets] backend = " + c.Backend + ")"
	}
	return r
}

// KeyringCheck probes the desktop keyring a secret-service state seals
// with. A locked one is a warning: pcb retries it by itself.
type KeyringCheck struct{ Keyring service.Keyring }

// Name implements Check.
func (KeyringCheck) Name() string { return "keyring" }

// Run implements Check.
func (c KeyringCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name()}
	ctx, cancel := context.WithTimeout(ctx, service.ProbeTimeout)
	defer cancel()
	err := seal.Probe(ctx, c.Keyring)
	switch {
	case err == nil:
		r.Status, r.Message = StatusOK, "the desktop keyring is unlocked"
	case errors.Is(err, seal.ErrLocked):
		r.Status, r.Message, r.Fix = StatusWarn, "the desktop keyring is locked", service.KeyringFix
	case errors.Is(err, seal.ErrNoService):
		r.Status, r.Message = StatusError, "this state is sealed by the desktop keyring but no Secret Service answers on the session bus"
		r.Fix = "start a Secret Service (GNOME Keyring, KWallet) in the desktop session"
	default:
		r.Status, r.Message = StatusError, "the desktop keyring cannot be used: "+err.Error()
		r.Fix = "check the Secret Service in the desktop session"
	}
	return r
}

// DesktopCheck warns when pcb-ui and the pcb service are both installed:
// both would serve the one state directory. Quiet otherwise.
type DesktopCheck struct {
	// AppDirs are the applications directories pcb-ui.desktop may lie in.
	AppDirs []string
	// ServiceInstalled reports whether a pcb systemd unit is installed.
	ServiceInstalled func() bool
}

// Name implements Check.
func (DesktopCheck) Name() string { return "pcb-ui" }

// Run implements Check.
func (c DesktopCheck) Run(ctx context.Context) Result {
	if rs := c.RunAll(ctx); len(rs) > 0 {
		return rs[0]
	}
	return Result{Name: c.Name(), Status: StatusOK, Message: "not installed beside the pcb service"}
}

// RunAll implements Multi: the warning, or nothing.
func (c DesktopCheck) RunAll(context.Context) []Result {
	if c.ServiceInstalled == nil || !c.ServiceInstalled() {
		return nil
	}
	for _, d := range c.AppDirs {
		if _, err := os.Stat(filepath.Join(d, "pcb-ui.desktop")); err == nil {
			return []Result{{Name: c.Name(), Status: StatusWarn,
				Message: "pcb-ui and the pcb service are both installed: only one can serve this state directory",
				Fix:     "uninstall one (pcb uninstall, or remove pcb-ui.desktop)"}}
		}
	}
	return nil
}
