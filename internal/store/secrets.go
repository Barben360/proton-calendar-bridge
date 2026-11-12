package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// secretsVersion is the envelope version written. Version 2 seals the
// generation with the accounts; version 1 (M1-M4) sealed the accounts only,
// and reads as generation 0.
const secretsVersion = 2

// Secrets is the sealed store of accounts (SPEC C2). The file only holds an
// envelope and one sealed blob; the key stays with the Sealer.
//
// Rollback (R3m, M5-P14): every write seals a generation one above the last
// and then mirrors it in state.json (State.SecretsGeneration); removing the
// file moves the mirror one above too. A sealed generation below the mirror is
// a restored or replaced file (ErrRolledBack). The version and the sealer are
// in the AAD, so editing the envelope's version cannot make a v2 file read as
// v1 (generation 0): it no longer authenticates. A file ahead of the mirror is
// accepted (a crash, or a failed mirror write, between the two writes).
// Restoring the whole state directory, state.json included, is not detected.
type Secrets struct {
	Path     string // secrets.sealed; state.json is in the same directory
	LockPath string // secrets.lock
	Sealer   seal.Sealer
	// Log, when set, returns the logger a failed mirror write is reported
	// to (warning level; generations and the file error only). Nil
	// discards it.
	Log func() *slog.Logger
}

// NewSecrets returns the store in stateDir, sealed by s.
func NewSecrets(stateDir string, s seal.Sealer) *Secrets {
	return &Secrets{
		Path:     filepath.Join(stateDir, "secrets.sealed"),
		LockPath: filepath.Join(stateDir, "secrets.lock"),
		Sealer:   s,
	}
}

// ErrRolledBack means secrets.sealed holds a generation below the one
// state.json mirrors: an older file was restored, or the file was replaced.
// Load returns it as a *RolledBackError.
var ErrRolledBack = errors.New("secrets.sealed is older than the last one pcb wrote")

// RolledBackError is ErrRolledBack with the two generations.
type RolledBackError struct {
	Generation uint64 // sealed in the file
	Mirror     uint64 // last written, from state.json
}

func (e *RolledBackError) Error() string {
	return fmt.Sprintf("%v (generation %d < %d)", ErrRolledBack, e.Generation, e.Mirror)
}

// Unwrap makes errors.Is(err, ErrRolledBack) hold.
func (e *RolledBackError) Unwrap() error { return ErrRolledBack }

// RolledBackHint is what a person does about a rolled-back store: only they
// know whether the restore was meant, and a new login is the way out either
// way.
const RolledBackHint = "if you restored it on purpose, run pcb logout, then pcb login"

// Explain is the message shown for e (pcb status, the service's MustAct,
// pcb doctor): Error and its cause.
func (e *RolledBackError) Explain() string { return e.Error() + ": it was restored or replaced" }

// ErrUnsupported means secrets.sealed is a store this build cannot open:
// written by a newer pcb (an envelope version it does not know) or sealed by
// a sealer it does not have. Load returns it as an *UnsupportedError. Unlike
// a lost key, a newer pcb can still read it, so nothing discards it unasked.
var ErrUnsupported = errors.New("secrets.sealed cannot be opened by this pcb")

// UnsupportedError is ErrUnsupported with its cause: Newer when the version
// is above the one this build writes, else Sealer names the other sealer.
type UnsupportedError struct {
	Newer  bool
	Sealer string // as the file names it, unchecked
}

func (e *UnsupportedError) Error() string { return e.Explain() }

// Unwrap makes errors.Is(err, ErrUnsupported) hold.
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

// maxSealerShown bounds the sealer name an explanation quotes: the file is
// not trusted to hold a short or printable one.
const maxSealerShown = 32

// Explain is the cause, safe to show (pcb status, the service's errors).
func (e *UnsupportedError) Explain() string {
	if e.Newer {
		return "secrets.sealed was written by a newer pcb"
	}
	name := e.Sealer
	if r := []rune(name); len(r) > maxSealerShown {
		name = string(r[:maxSealerShown]) + "…"
	}
	return fmt.Sprintf("secrets.sealed is sealed with %q, which this pcb cannot use", name)
}

type envelope struct {
	Version int    `json:"version"`
	Sealer  string `json:"sealer"`
	Data    []byte `json:"data"` // base64 in JSON
}

// payload is what version 2 seals.
type payload struct {
	Generation uint64             `json:"generation"`
	Accounts   map[string]Account `json:"accounts"`
}

func aad(version int, sealer string) []byte {
	return []byte("pcb-secrets/v" + strconv.Itoa(version) + "/" + sealer)
}

// nextGeneration is the generation after the file's gen and the mirror,
// saturating at MaxSecretsGeneration so pcb never writes a mirror its own
// LoadState refuses: a mirror edited to the bound recovers with one logout.
// (At saturation a file one write old reads as current: the bound is only
// reachable by editing state.json, the M5-P14 residual.)
func nextGeneration(gen, mirror uint64) uint64 {
	if last := max(gen, mirror); last < MaxSecretsGeneration {
		return last + 1
	}
	return MaxSecretsGeneration
}

func (s *Secrets) stateDir() string { return filepath.Dir(s.Path) }

// mirror is the generation state.json mirrors, 0 when it has none. A
// state.json that cannot be read gives 0: the check is then skipped, which
// is the residual of a replaced state directory (M5-P14), and every service
// path refuses an unreadable state.json before it uses the secrets.
func (s *Secrets) mirror() uint64 {
	st, err := LoadState(s.stateDir())
	if err != nil {
		return 0
	}
	return st.SecretsGeneration
}

// Load returns the accounts by ID; a missing file is an empty store. It
// fails with seal.ErrNoKey when the key is gone, seal.ErrCorrupt when the
// data does not authenticate and ErrRolledBack when the file is older than
// the last one written: never with an empty map in their place.
func (s *Secrets) Load() (map[string]Account, error) {
	accounts, _, _, err := s.load()
	return accounts, err
}

// load is Load with the file's generation and the mirror. The mirror is read
// first: writers seal the file before they move the mirror, so a load that
// races a write never sees a mirror above the file it then reads.
func (s *Secrets) load() (accounts map[string]Account, gen, mirror uint64, err error) {
	mirror = s.mirror()
	accounts, gen, found, err := s.open()
	if err != nil {
		return nil, 0, 0, err
	}
	if found && gen < mirror {
		return nil, 0, 0, &RolledBackError{Generation: gen, Mirror: mirror}
	}
	return accounts, gen, mirror, nil
}

// open reads and opens the file with no rollback check; a missing file
// (found false) is an empty store of generation 0.
func (s *Secrets) open() (accounts map[string]Account, gen uint64, found bool, err error) {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Account{}, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	accounts, gen, err = s.decode(raw)
	return accounts, gen, true, err
}

// decode opens the envelope raw.
func (s *Secrets) decode(raw []byte) (map[string]Account, uint64, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0, fmt.Errorf("%s: not a sealed store: %w", s.Path, seal.ErrCorrupt)
	}
	switch {
	case env.Version > secretsVersion:
		return nil, 0, &UnsupportedError{Newer: true}
	case env.Version < 1:
		return nil, 0, fmt.Errorf("%s: envelope version %d: %w", s.Path, env.Version, seal.ErrCorrupt)
	case env.Sealer != s.Sealer.Name():
		return nil, 0, &UnsupportedError{Sealer: env.Sealer}
	}
	plain, err := s.Sealer.Open(env.Data, aad(env.Version, env.Sealer))
	if err != nil {
		return nil, 0, err
	}
	// Never echo the decoder's text: it may quote the plaintext.
	if env.Version == 1 {
		accounts := map[string]Account{}
		if err := json.Unmarshal(plain, &accounts); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", s.Path, seal.ErrCorrupt)
		}
		return accounts, 0, nil
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", s.Path, seal.ErrCorrupt)
	}
	if p.Accounts == nil {
		p.Accounts = map[string]Account{}
	}
	return p.Accounts, p.Generation, nil
}

// Update runs fn on the accounts under the store lock and saves the result
// as version 2, one generation above the last; an empty result removes the
// file. The mirror in state.json follows the file. If the existing file
// cannot be opened (no key, corrupt, rolled back) it returns that error and
// writes nothing, so a lost key can never turn into an overwrite. If fn fails
// nothing is written either.
func (s *Secrets) Update(ctx context.Context, fn func(accounts map[string]Account) error) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	unlock, err := filelock.Lock(ctx, s.LockPath)
	if err != nil {
		return err
	}
	defer unlock()
	accounts, gen, mirror, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(accounts); err != nil {
		return err
	}
	next := nextGeneration(gen, mirror)
	if len(accounts) == 0 {
		removed, err := s.wipe()
		if removed {
			s.setMirror(ctx, next)
		}
		return err
	}
	plain, err := json.Marshal(payload{Generation: next, Accounts: accounts})
	if err != nil {
		return err
	}
	name := s.Sealer.Name()
	sealed, err := s.Sealer.Seal(plain, aad(secretsVersion, name))
	clear(plain)
	if err != nil {
		return err
	}
	out, err := json.Marshal(envelope{Version: secretsVersion, Sealer: name, Data: sealed})
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.Path, out); err != nil {
		return err
	}
	s.setMirror(ctx, next)
	return nil
}

// setMirror raises the mirror in state.json to gen, best effort: if the
// write fails the sealed file is ahead of the mirror, which Load accepts, and
// the next write moves the mirror again; the failure is logged. It ignores
// ctx's cancellation: the file change it follows is already done, and the
// state-file lock's own timeout bounds it.
func (s *Secrets) setMirror(ctx context.Context, gen uint64) {
	err := UpdateStateContext(context.WithoutCancel(ctx), s.stateDir(), func(st *State) error {
		if st.SecretsGeneration >= gen {
			return ErrStateUnchanged
		}
		st.SecretsGeneration = gen
		return nil
	})
	if err != nil && s.Log != nil {
		s.Log().Warn("the secrets generation could not be recorded in state.json; a restored secrets.sealed goes unnoticed until the next write",
			"generation", gen, "cause", stateWriteCause(err))
	}
}

// stateWriteCause is why state.json could not be updated, in pcb's words
// and secret-free: the lock held too long, a newer or damaged file, or the
// operating system's reason (permission denied, no space left on device),
// never the Go error chain (ruling A11).
func stateWriteCause(err error) string {
	var pe *fs.PathError
	switch {
	case errors.Is(err, ErrStateLock) && errors.Is(err, context.DeadlineExceeded):
		return "another pcb process held the state-file lock (state.lock) too long"
	case errors.Is(err, ErrNewerState):
		return ErrNewerState.Error()
	case errors.Is(err, ErrGenerationOutOfRange):
		return "state.json holds a secrets generation out of range"
	case errors.As(err, &pe):
		return "state.json or its lock cannot be used: " + pe.Err.Error()
	}
	return "state.json cannot be read or written"
}

// Wipe removes the sealed file under the store lock (a logout, or a store
// that can no longer be opened or was rolled back); the mirror moves one
// above the last generation, so the removed file is refused if restored.
// next is that generation, 0 when there was no file (a missing file is not
// an error): a caller that rewrites state.json from scratch seeds it with
// next, since the mirror write fails when state.json is unreadable.
func (s *Secrets) Wipe(ctx context.Context) (next uint64, err error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return 0, err
	}
	unlock, err := filelock.Lock(ctx, s.LockPath)
	if err != nil {
		return 0, err
	}
	defer unlock()
	// The file's generation when it still opens (a rolled-back store), so
	// the mirror ends above it too.
	_, gen, _, _ := s.open()
	mirror := s.mirror()
	removed, err := s.wipe()
	if !removed {
		return 0, err
	}
	next = nextGeneration(gen, mirror)
	s.setMirror(ctx, next)
	return next, err
}

// wipe removes the file, then the mirror moves (the callers): a load racing
// the removal never sees the new mirror with the old file. The directory is
// fsynced, so a power loss does not bring the file back. removed reports
// whether there was a file.
func (s *Secrets) wipe() (removed bool, err error) {
	err = os.Remove(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err == nil {
		syncDir(s.stateDir())
	}
	return err == nil, err
}

// syncDir fsyncs a directory, best effort; tests watch it.
var syncDir = atomicfile.SyncDir
