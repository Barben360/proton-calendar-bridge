package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// UnattendedKeyringWarning is the line pcb doctor prints for unattended
// relogin on a state sealed with the desktop keyring; sealed names the file
// that holds the secrets (a full path in pcb doctor, a label in the desktop
// app's add-account wizard, which shows the same line).
func UnattendedKeyringWarning(sealed string) string {
	return "unattended relogin is on: whoever can use this desktop session's keyring and read " + sealed + " owns the Proton account"
}

// SecretsCheck reports how the secrets are protected. A 0600 key file stops
// other local users and casual disclosure, not root or the user's own
// processes, and a backup of one volume holding both key and data exposes
// both (E15); the desktop keyring keeps the key off the disk. A secrets.sealed older
// than the last one pcb wrote (restored or replaced, R3m) is an error; other
// failures to open it are left to pcb status and pcb logout.
type SecretsCheck struct {
	StateDir string // holds state.json and secrets.sealed
	KeyFile  string
	// Sealer opens secrets.sealed; nil is the key file's.
	Sealer seal.Sealer
}

// resolve is the sealer that opens the secrets, and whether it is the key
// file's.
func (c SecretsCheck) resolve() (s seal.Sealer, keyFile bool) {
	if c.Sealer == nil {
		return &seal.KeyFile{Path: c.KeyFile}, true
	}
	_, ok := c.Sealer.(*seal.KeyFile)
	return c.Sealer, ok
}

// Name implements Check.
func (SecretsCheck) Name() string { return "secrets" }

// deviceOf is device; tests replace it.
var deviceOf = device

// Run implements Check.
func (c SecretsCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Status: StatusSkip, Message: "no secrets stored yet"}
	state, err := store.LoadState(c.StateDir)
	switch {
	case errors.Is(err, store.ErrNewerState):
		r.Status, r.Message, r.Fix = StatusError, store.ErrNewerState.Error(), "upgrade pcb"
		return r
	case err != nil:
		if path, ok := store.StateUnreadable(err); ok {
			r.Status, r.Message = StatusError, store.UnreadableStateMessage(path)
			r.Fix = store.UnreadableStateFix()
			return r
		}
		r.Status, r.Message = StatusError, err.Error()
		r.Fix = "remove the damaged state.json, then log in again"
		return r
	}
	sealed := filepath.Join(c.StateDir, "secrets.sealed")
	sealer, keyFile := c.resolve()
	var msgs []string
	if exists(sealed) {
		if keyFile && !exists(c.KeyFile) {
			r.Status = StatusError
			r.Message = "sealed secrets exist but the key file is missing: they cannot be read; `pcb logout` wipes them, then log in again"
			r.Fix = "pcb logout"
			return r
		}
		var (
			rb *store.RolledBackError
			ue *store.UnsupportedError
		)
		_, err := store.NewSecrets(c.StateDir, sealer).Load()
		switch {
		case errors.Is(err, seal.ErrNoKey) && !keyFile:
			r.Status = StatusError
			r.Message = "sealed secrets exist but the desktop keyring holds no key for them: they cannot be read; `pcb logout` wipes them, then log in again"
			r.Fix = "pcb logout"
			return r
		case errors.As(err, &rb):
			r.Status = StatusError
			r.Message, r.Fix = rb.Explain(), store.RolledBackHint
			return r
		case errors.As(err, &ue):
			r.Status = StatusError
			r.Message, r.Fix = ue.Explain(), "upgrade pcb"
			return r
		}
		if keyFile {
			msgs = append(msgs, "secrets sealed with a key file (headless fallback)")
			if c.sameVolume() {
				msgs = append(msgs, "the key and the sealed data are on the same volume, so a backup or copy of that volume exposes both")
			}
		} else {
			r.Status, r.Message = StatusOK, "secrets sealed with the desktop keyring"
		}
	}
	if state.Unattended {
		if keyFile {
			msgs = append(msgs, "unattended relogin is on: whoever can read "+c.KeyFile+" and "+sealed+" owns the Proton account")
		} else {
			msgs = append(msgs, UnattendedKeyringWarning(sealed))
		}
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

// KeyFileCheck checks the key file as pcb reads it (M5-R15 C3): its mode
// (FileModeCheck), then that this user can open it and that it holds a
// 32-byte key. A missing key file is skipped: the first login creates it.
type KeyFileCheck struct {
	Path string
}

// Name implements Check.
func (KeyFileCheck) Name() string { return "key file" }

// Run implements Check.
func (c KeyFileCheck) Run(ctx context.Context) Result {
	r := FileModeCheck{Label: c.Name(), Path: c.Path, Want: 0o600}.Run(ctx)
	if r.Status != StatusOK {
		return r
	}
	err := (&seal.KeyFile{Path: c.Path}).Check()
	switch {
	case err == nil:
		r.Message += ", 32 bytes"
	case errors.Is(err, fs.ErrPermission):
		r.Status = StatusError
		r.Message = c.Path + " is not readable by this user" + uidText() + ": " + causeOf(err)
		r.Fix = "chown " + ownerText() + " " + shellquote.Quote(c.Path) + " (a container secret: on the host, chown 65532:65532 and chmod 600 the key file)"
	case errors.Is(err, seal.ErrCorrupt):
		r.Status = StatusError
		r.Message = c.Path + " is not a 32-byte key"
		r.Fix = "restore it from a backup; with no secrets.sealed, remove it and the next login creates a new one"
	default:
		r.Status, r.Message = StatusError, c.Path+" cannot be read: "+causeOf(err)
	}
	return r
}

// causeOf is the OS error of a path error (permission denied), else err.
func causeOf(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// uidText names this process's user for a permission message; "" where
// there are no UIDs.
func uidText() string {
	if uid := os.Geteuid(); uid >= 0 {
		return " (uid " + strconv.Itoa(uid) + ")"
	}
	return ""
}

// ownerText is the owner a chown fix names: this process's UID.
func ownerText() string {
	if uid := os.Geteuid(); uid >= 0 {
		return strconv.Itoa(uid)
	}
	return "$USER"
}
