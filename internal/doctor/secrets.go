package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// SecretsCheck reports how the secrets are protected. Only the key-file
// sealer exists so far: a 0600 key file stops other local users and casual
// disclosure, not root or the user's own processes, and a backup of one
// volume holding both key and data exposes both (E15). A secrets.sealed older
// than the last one pcb wrote (restored or replaced, R3m) is an error; other
// failures to open it are left to pcb status and pcb logout.
type SecretsCheck struct {
	StateDir string // holds state.json and secrets.sealed
	KeyFile  string
}

// Name implements Check.
func (SecretsCheck) Name() string { return "secrets" }

// deviceOf is device; tests replace it.
var deviceOf = device

// Run implements Check.
func (c SecretsCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Status: StatusSkip, Message: "no secrets stored yet"}
	state, err := store.LoadState(c.StateDir)
	if err != nil {
		r.Status, r.Message = StatusError, err.Error()
		r.Fix = "remove the damaged state.json, then log in again"
		return r
	}
	sealed := filepath.Join(c.StateDir, "secrets.sealed")
	var msgs []string
	if exists(sealed) {
		if !exists(c.KeyFile) {
			r.Status = StatusError
			r.Message = "sealed secrets exist but the key file is missing: they cannot be read; `pcb logout` wipes them, then log in again"
			r.Fix = "pcb logout"
			return r
		}
		var rb *store.RolledBackError
		if _, err := store.NewSecrets(c.StateDir, &seal.KeyFile{Path: c.KeyFile}).Load(); errors.As(err, &rb) {
			r.Status = StatusError
			r.Message, r.Fix = rb.Explain(), store.RolledBackHint
			return r
		}
		msgs = append(msgs, "secrets sealed with a key file (headless fallback)")
		if c.sameVolume() {
			msgs = append(msgs, "the key and the sealed data are on the same volume, so a backup or copy of that volume exposes both")
		}
	}
	if state.Unattended {
		msgs = append(msgs, "unattended relogin is on: whoever can read "+c.KeyFile+" and "+sealed+" owns the Proton account")
	}
	if len(msgs) > 0 {
		r.Status, r.Message = StatusWarn, strings.Join(msgs, "; ")
	}
	return r
}

// sameVolume reports whether the key file's directory and the state
// directory are on one device, symlinks resolved. Unknown counts as no.
func (c SecretsCheck) sameVolume() bool {
	keyDir, err1 := filepath.EvalSymlinks(filepath.Dir(c.KeyFile))
	stateDir, err2 := filepath.EvalSymlinks(c.StateDir)
	if err1 != nil || err2 != nil {
		return false
	}
	a, ok1 := deviceOf(keyDir)
	b, ok2 := deviceOf(stateDir)
	return ok1 && ok2 && a == b
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
