package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

const secretsVersion = 1

// Secrets is the sealed store of accounts (SPEC C2). The file only holds an
// envelope and one sealed blob; the key stays with the Sealer.
type Secrets struct {
	Path     string // secrets.sealed
	LockPath string // secrets.lock
	Sealer   seal.Sealer
}

// NewSecrets returns the store in stateDir, sealed by s.
func NewSecrets(stateDir string, s seal.Sealer) *Secrets {
	return &Secrets{
		Path:     filepath.Join(stateDir, "secrets.sealed"),
		LockPath: filepath.Join(stateDir, "secrets.lock"),
		Sealer:   s,
	}
}

type envelope struct {
	Version int    `json:"version"`
	Sealer  string `json:"sealer"`
	Data    []byte `json:"data"` // base64 in JSON
}

func (s *Secrets) aad(sealer string) []byte { return []byte("pcb-secrets/v1/" + sealer) }

// Load returns the accounts by ID; a missing file is an empty store. It
// fails with seal.ErrNoKey when the key is gone and seal.ErrCorrupt when the
// data does not authenticate: never with an empty map in their place.
func (s *Secrets) Load() (map[string]Account, error) {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Account{}, nil
	}
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: not a sealed store: %w", s.Path, seal.ErrCorrupt)
	}
	if env.Version != secretsVersion {
		return nil, fmt.Errorf("%s: unsupported version %d", s.Path, env.Version)
	}
	if env.Sealer != s.Sealer.Name() {
		return nil, fmt.Errorf("%s is sealed with %q, not %q", s.Path, env.Sealer, s.Sealer.Name())
	}
	plain, err := s.Sealer.Open(env.Data, s.aad(env.Sealer))
	if err != nil {
		return nil, err
	}
	accounts := map[string]Account{}
	if err := json.Unmarshal(plain, &accounts); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, seal.ErrCorrupt) // never echo the decoder's text
	}
	return accounts, nil
}

// Update runs fn on the accounts under the store lock and saves the result;
// an empty result removes the file. If the existing file cannot be opened
// (no key, corrupt) it returns that error and writes nothing, so a lost key
// can never turn into an overwrite. If fn fails nothing is written either.
func (s *Secrets) Update(ctx context.Context, fn func(accounts map[string]Account) error) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	unlock, err := filelock.Lock(ctx, s.LockPath)
	if err != nil {
		return err
	}
	defer unlock()
	accounts, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(accounts); err != nil {
		return err
	}
	if len(accounts) == 0 {
		return s.Wipe()
	}
	plain, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	name := s.Sealer.Name()
	sealed, err := s.Sealer.Seal(plain, s.aad(name))
	if err != nil {
		return err
	}
	out, err := json.Marshal(envelope{Version: secretsVersion, Sealer: name, Data: sealed})
	if err != nil {
		return err
	}
	return atomicfile.Write(s.Path, out)
}

// Wipe removes the sealed file, for a store that can no longer be opened.
// A missing file is not an error.
func (s *Secrets) Wipe() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
