package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// Keyring is the desktop keyring, as seal.Keyring.
type Keyring interface {
	Get(ctx context.Context, attrs map[string]string) ([]byte, error)
	Set(ctx context.Context, label string, attrs map[string]string, secret []byte) error
	Locked(ctx context.Context) (bool, error)
}

// SystemKeyring is the keyring of the session bus.
func SystemKeyring() Keyring { return seal.DBusKeyring("") }

// Sealer names layout.json records.
const (
	SealerKeyFile       = "keyfile"
	SealerSecretService = "secret-service"
)

// NewSealer is the sealer layout.json names: the desktop keyring's for
// secret-service (one key for stateRoot, shared by every account), else
// the key file's.
func NewSealer(name, keyFile, stateRoot string, kr Keyring) seal.Sealer {
	if name == SealerSecretService {
		return &seal.SecretService{Keyring: kr, StateDir: stateRoot}
	}
	return &seal.KeyFile{Path: keyFile}
}

// SealerCheck reports the sealer layout.json records.
type SealerCheck struct{ Sealer string }

// Name implements Check.
func (SealerCheck) Name() string { return "sealer" }

// Run implements Check.
func (c SealerCheck) Run(context.Context) Result {
	return Result{Name: c.Name(), Status: StatusOK, Message: c.Sealer}
}

// probeTimeout bounds the keyring probe: a Secret Service that does not
// answer is an error, not a hang.
const probeTimeout = 5 * time.Second

// KeyringCheck probes the desktop keyring a secret-service state seals
// with. A locked one is a warning: pcb retries it by itself.
type KeyringCheck struct{ Keyring Keyring }

// Name implements Check.
func (KeyringCheck) Name() string { return "keyring" }

// Run implements Check.
func (c KeyringCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name()}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
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
