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

// State is the login state in state.json. It is not sealed: the address is
// not a secret; never a token or password. SecretsGeneration mirrors the
// generation sealed in secrets.sealed (Secrets): a sealed file below it was
// restored or replaced (ErrRolledBack).
//
// Address is the account's Proton address, kept after a sign-out so a
// signed-out account still shows its name. CalDAV is the account's CalDAV
// switch: nil (an M5 state.json) is on. Created is when the account was
// added.
type State struct {
	AccountID   string    `json:"account_id,omitempty"`
	Phase       Phase     `json:"phase"`
	Reason      string    `json:"reason,omitempty"`
	Captcha     *Captcha  `json:"captcha,omitempty"`
	Unattended  bool      `json:"unattended,omitempty"`
	LastLogin   time.Time `json:"last_login,omitzero"`
	LastRelogin time.Time `json:"last_relogin,omitzero"`
	Address     string    `json:"address,omitempty"`
	CalDAV      *bool     `json:"caldav,omitempty"`
	Created     time.Time `json:"created,omitzero"`

	SecretsGeneration uint64 `json:"secrets_generation,omitempty"`
}

func statePath(stateDir string) string { return filepath.Join(stateDir, "state.json") }

// MaxSecretsGeneration bounds State.SecretsGeneration. One write per second
// for a century stays far below it, and the next generation (+1) can never
// wrap: a mirror above it was edited or damaged.
const MaxSecretsGeneration = 1 << 53

// ErrGenerationOutOfRange is the cause of LoadState's error for a
// secrets_generation above MaxSecretsGeneration. It fails closed like any
// malformed state.json; logout resets it.
var ErrGenerationOutOfRange = errors.New("the secrets generation is out of range")

// ErrNewerState is the cause of LoadState's error for a state.json that
// holds a field this build does not know: a newer pcb wrote it. It stays
// refused (fail closed), but it is not damaged: upgrading pcb reads it.
var ErrNewerState = errors.New("state.json was written by a newer pcb")

// LoadState reads state.json. A missing file is logged out; a malformed one
// is an error (fail closed: an unreadable state must not read as ready), and
// one that is only newer wraps ErrNewerState.
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
		if knownButNewer(raw) {
			return State{}, fmt.Errorf("%s: %w", statePath(stateDir), ErrNewerState)
		}
		return State{}, fmt.Errorf("%s is malformed: %w", statePath(stateDir), err)
	}
	if dec.More() {
		return State{}, fmt.Errorf("%s is malformed: data after the JSON value", statePath(stateDir))
	}
	if s.SecretsGeneration > MaxSecretsGeneration {
		return State{}, fmt.Errorf("%s: secrets_generation %d: %w", statePath(stateDir), s.SecretsGeneration, ErrGenerationOutOfRange)
	}
	switch s.Phase {
	case PhaseLoggedOut, PhaseReady, PhaseNeedsCaptcha, PhaseNeedsLogin:
		return s, nil
	}
	return State{}, fmt.Errorf("%s: unknown phase %q", statePath(stateDir), s.Phase)
}

// knownButNewer reports whether raw, refused by the strict decoder, is a
// valid state.json once its unknown fields are ignored: one JSON value whose
// known fields all decode, so only fields this build does not know refused it.
func knownButNewer(raw []byte) bool {
	var s State
	dec := json.NewDecoder(bytes.NewReader(raw))
	return dec.Decode(&s) == nil && !dec.More()
}

// ErrStateUnchanged, returned by UpdateState's fn, writes nothing; UpdateState
// then returns nil.
var ErrStateUnchanged = errors.New("state unchanged")

// ErrStateLock is the cause of an UpdateState or SaveState that could not
// take the state-file lock: another process held it past the timeout, the
// caller's context ended, or the lock file cannot be created. It wraps the
// reason (context.DeadlineExceeded for the timeout).
var ErrStateLock = errors.New("cannot take the state-file lock")

// stateLockTimeout bounds the wait for the state-file lock, in-process
// and between processes alike. Its holders only load and write state.json,
// so a longer wait means a stuck process. Tests shorten it.
var stateLockTimeout = 10 * time.Second

// stateSems serialise the state.json writers of one process, per state
// directory: a channel of capacity one, so the wait honours the deadline.
// Off Unix, where there is no file lock, it is all there is (M5-P15, as
// clients.json, M3-R18): two processes writing at the same moment can lose
// an update there.
var stateSems sync.Map // cleaned state dir -> chan struct{}

// stateLockBusy, when set, runs when a writer finds state.lock taken, before
// it waits (tests).
var stateLockBusy func()

// lockState takes the state-file lock of stateDir within stateLockTimeout
// and ctx: the process semaphore, then <stateDir>/state.lock. It is a leaf
// lock: its holders take no other lock, so it can be taken under the
// secrets lock. Its errors wrap ErrStateLock.
func lockState(ctx context.Context, stateDir string) (unlock func(), err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStateLock, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStateLock, err)
	}
	ctx, cancel := context.WithTimeout(ctx, stateLockTimeout)
	defer cancel()
	v, _ := stateSems.LoadOrStore(filepath.Clean(stateDir), make(chan struct{}, 1))
	sem := v.(chan struct{}) //nolint:forcetypeassert // only chan struct{} is stored
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrStateLock, ctx.Err())
	}
	path := filepath.Join(stateDir, "state.lock")
	release, ok, err := filelock.TryLock(path)
	if err == nil && !ok {
		if stateLockBusy != nil {
			stateLockBusy()
		}
		release, err = filelock.Lock(ctx, path)
	}
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		return func() { <-sem }, nil
	case err != nil:
		<-sem
		return nil, fmt.Errorf("%w %s: %w", ErrStateLock, path, err)
	}
	return func() { release(); <-sem }, nil
}

// UpdateState is UpdateStateContext without a caller's context: the wait
// for the lock is bounded by its timeout only.
func UpdateState(stateDir string, fn func(*State) error) error {
	return UpdateStateContext(context.Background(), stateDir, fn)
}

// UpdateStateContext loads state.json, applies fn and writes the result
// atomically, all under the state-file lock, so no other writer lands
// between the read and the write. The wait for the lock ends with ctx or
// its timeout (ErrStateLock). A state.json that cannot be read is returned
// as an error and never handed to fn. When fn fails nothing is written;
// ErrStateUnchanged makes that a success.
func UpdateStateContext(ctx context.Context, stateDir string, fn func(*State) error) error {
	unlock, err := lockState(ctx, stateDir)
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

// SaveState is SaveStateContext without a caller's context.
func SaveState(stateDir string, s State) error {
	return SaveStateContext(context.Background(), stateDir, s)
}

// SaveStateContext replaces state.json (0600) atomically with s, creating
// stateDir (0700), under the state-file lock (waited for as in
// UpdateStateContext). It keeps the higher of s's and the file's
// SecretsGeneration, so a writer that built s from scratch never lowers the
// mirror; a state.json that cannot be read is replaced as is (the caller
// seeds s.SecretsGeneration when it knows better, as logout does).
func SaveStateContext(ctx context.Context, stateDir string, s State) error {
	unlock, err := lockState(ctx, stateDir)
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
