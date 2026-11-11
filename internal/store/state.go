package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
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
// holds a username, token or password.
type State struct {
	AccountID   string    `json:"account_id,omitempty"`
	Phase       Phase     `json:"phase"`
	Reason      string    `json:"reason,omitempty"`
	Captcha     *Captcha  `json:"captcha,omitempty"`
	Unattended  bool      `json:"unattended,omitempty"`
	LastLogin   time.Time `json:"last_login,omitzero"`
	LastRelogin time.Time `json:"last_relogin,omitzero"`
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

// SaveState writes state.json (0600) atomically, creating stateDir (0700).
func SaveState(stateDir string, s State) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(statePath(stateDir), append(raw, '\n'))
}
