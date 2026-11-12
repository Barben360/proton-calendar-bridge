package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// Phase is where the login stands.
type Phase string

// The login phases.
const (
	PhaseLoggedOut    Phase = "logged-out"
	PhaseReady        Phase = "ready"
	PhaseNeedsCaptcha Phase = "needs-captcha"
	PhaseNeedsLogin   Phase = "needs-login"
)

// Captcha is a pending human verification.
type Captcha struct {
	URL   string    `json:"url"`
	Token string    `json:"token"`
	At    time.Time `json:"at,omitzero"`
}

// State is the login state in state.json. It is not sealed, so it never
// holds a username, token or password. SecretsGeneration mirrors the
// generation sealed in secrets.sealed (Secrets): a sealed file below it was
// restored or replaced (ErrRolledBack).
type State struct {
	AccountID   string    `json:"account_id,omitempty"`
	Phase       Phase     `json:"phase"`
	Reason      string    `json:"reason,omitempty"`
	Captcha     *Captcha  `json:"captcha,omitempty"`
	Unattended  bool      `json:"unattended,omitempty"`
	LastLogin   time.Time `json:"last_login,omitzero"`
	LastRelogin time.Time `json:"last_relogin,omitzero"`

	SecretsGeneration uint64 `json:"secrets_generation,omitempty"`
}

func statePath(stateDir string) string { return filepath.Join(stateDir, "state.json") }

// LoadState reads state.json. A missing file is logged out; a malformed one
// is an error (fail closed: an unreadable state must not read as ready).
func LoadState(stateDir string) (State, error) {
	raw, err := os.ReadFile(statePath(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return State{Phase: PhaseLoggedOut}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return State{}, fmt.Errorf("%s is malformed: %w", statePath(stateDir), err)
	}
	if dec.More() {
		return State{}, fmt.Errorf("%s is malformed: data after the JSON value", statePath(stateDir))
	}
	switch s.Phase {
	case PhaseLoggedOut, PhaseReady, PhaseNeedsCaptcha, PhaseNeedsLogin:
		return s, nil
	}
	return State{}, fmt.Errorf("%s: unknown phase %q", statePath(stateDir), s.Phase)
}

// ErrStateUnchanged, returned by UpdateState's fn, writes nothing; UpdateState
// then returns nil.
var ErrStateUnchanged = errors.New("state unchanged")

// stateLockTimeout bounds the wait for state.lock. Its holders only load and
// write state.json, so a longer wait means a stuck process.
const stateLockTimeout = 10 * time.Second

// stateMus serialises the state.json writers of one process, per state
// directory. Off Unix, where there is no file lock, it is all there is
// (M5-P15, as clients.json, M3-R18): two processes writing at the same
// moment can lose an update there.
var stateMus sync.Map // cleaned state dir -> *sync.Mutex

// stateLockBusy, when set, runs when a writer finds state.lock taken, before
// it waits (tests).
var stateLockBusy func()

// lockState takes the state-file lock of stateDir: the process mutex, then
// <stateDir>/state.lock. It is a leaf lock: its holders take no other lock,
// so it can be taken under the secrets lock.
func lockState(stateDir string) (unlock func(), err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	m, _ := stateMus.LoadOrStore(filepath.Clean(stateDir), &sync.Mutex{})
	mu := m.(*sync.Mutex) //nolint:forcetypeassert // only *sync.Mutex is stored
	mu.Lock()
	path := filepath.Join(stateDir, "state.lock")
	release, ok, err := filelock.TryLock(path)
	if err == nil && !ok {
		if stateLockBusy != nil {
			stateLockBusy()
		}
		ctx, cancel := context.WithTimeout(context.Background(), stateLockTimeout)
		release, err = filelock.Lock(ctx, path)
		cancel()
	}
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		return mu.Unlock, nil
	case err != nil:
		mu.Unlock()
		return nil, fmt.Errorf("cannot take %s: %w", path, err)
	}
	return func() { release(); mu.Unlock() }, nil
}

// UpdateState loads state.json, applies fn and writes the result atomically,
// all under the state-file lock, so no other writer lands between the read
// and the write. A state.json that cannot be read is returned as an error and
// never handed to fn. When fn fails nothing is written; ErrStateUnchanged
// makes that a success.
func UpdateState(stateDir string, fn func(*State) error) error {
	unlock, err := lockState(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := LoadState(stateDir)
	if err != nil {
		return err
	}
	switch err := fn(&s); {
	case errors.Is(err, ErrStateUnchanged):
		return nil
	case err != nil:
		return err
	}
	return writeState(stateDir, s)
}

// SaveState replaces state.json (0600) atomically with s, creating stateDir
// (0700), under the state-file lock. It keeps the higher of s's and the
// file's SecretsGeneration, so a writer that built s from scratch never
// lowers the mirror; a state.json that cannot be read is replaced as is.
func SaveState(stateDir string, s State) error {
	unlock, err := lockState(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if cur, err := LoadState(stateDir); err == nil {
		s.SecretsGeneration = max(s.SecretsGeneration, cur.SecretsGeneration)
	}
	return writeState(stateDir, s)
}

func writeState(stateDir string, s State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(statePath(stateDir), append(raw, '\n'))
}
