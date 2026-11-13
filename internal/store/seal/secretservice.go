package seal

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// Keyring is the part of a desktop keyring (freedesktop Secret Service) the
// SecretService sealer needs: one secret per attribute set.
type Keyring interface {
	// Get returns the secret of the item whose attributes include attrs:
	// ErrNoKey when none matches, ErrLocked when the keyring is locked. It
	// never prompts.
	Get(ctx context.Context, attrs map[string]string) ([]byte, error)
	// Set stores secret under label and attrs, replacing an item with the
	// same attributes. ErrLocked when the keyring is locked.
	Set(ctx context.Context, label string, attrs map[string]string, secret []byte) error
	// Locked reports whether the default collection is locked.
	Locked(ctx context.Context) (bool, error)
}

const (
	createLockWait = 30 * time.Second
	itemLabel      = "pcb sealing key"
	itemApp        = "pcb"
)

// SecretService seals with AES-256-GCM under a random key kept in the
// desktop keyring, as one item per state directory. The key is then not on
// the disk next to the sealed data: it is released to the user's logged-in
// session only. It does not protect against processes of the same session.
type SecretService struct {
	Keyring  Keyring
	StateDir string // the state directory the key belongs to
}

// Name implements Sealer.
func (SecretService) Name() string { return "secret-service" }

func (s *SecretService) attrs() map[string]string {
	dir := s.StateDir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return map[string]string{"application": itemApp, "state": dir}
}

// Seal implements Sealer. It creates the keyring item on first use. The
// output is nonce || ciphertext, as KeyFile's.
func (s *SecretService) Seal(plaintext, aad []byte) ([]byte, error) {
	key, err := s.key(true)
	if err != nil {
		return nil, err
	}
	return sealAEAD(key, plaintext, aad)
}

// Open implements Sealer. It never creates the keyring item.
func (s *SecretService) Open(sealed, aad []byte) ([]byte, error) {
	key, err := s.key(false)
	if err != nil {
		return nil, err
	}
	return openAEAD(key, sealed, aad)
}

func (s *SecretService) key(create bool) ([]byte, error) {
	ctx := context.Background()
	attrs := s.attrs()
	key, err := s.Keyring.Get(ctx, attrs)
	if errors.Is(err, ErrNoKey) && create {
		key, err = s.create(ctx, attrs)
	} else if errors.Is(err, ErrNoKey) {
		return nil, fmt.Errorf("%w: no item for %s in the desktop keyring", ErrNoKey, attrs["state"])
	}
	if err != nil {
		return nil, err
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("%w: the keyring item for %s is not %d bytes", ErrCorrupt, attrs["state"], keySize)
	}
	return key, nil
}

// create makes the key under an exclusive lock on <StateDir>/sealer.lock:
// the keyring replaces an item with the same attributes, so two first
// sealers racing would each store a key and the first one's data would be
// sealed under a key that no longer exists. Under the lock the loser finds
// the winner's key. Off Unix the lock is a no-op, as elsewhere.
func (s *SecretService) create(ctx context.Context, attrs map[string]string) ([]byte, error) {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, createLockWait)
	defer cancel()
	unlock, err := filelock.Lock(lctx, filepath.Join(s.StateDir, "sealer.lock"))
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
	case err != nil:
		return nil, fmt.Errorf("locking the key creation: %w", err)
	default:
		defer unlock()
	}
	key, err := s.Keyring.Get(ctx, attrs)
	if !errors.Is(err, ErrNoKey) {
		return key, err // another process created it meanwhile (or a real error)
	}
	fresh := make([]byte, keySize)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("random key: %w", err)
	}
	if err := s.Keyring.Set(ctx, itemLabel, attrs, fresh); err != nil {
		return nil, err
	}
	return s.Keyring.Get(ctx, attrs) // what the keyring now holds
}

// Probe reports whether the keyring is usable: nil when a Secret Service
// answers and its default collection is unlocked, else ErrNoService,
// ErrLocked or the failure.
func Probe(ctx context.Context, k Keyring) error {
	locked, err := k.Locked(ctx)
	if err != nil {
		return err
	}
	if locked {
		return ErrLocked
	}
	return nil
}
