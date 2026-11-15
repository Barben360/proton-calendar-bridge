package seal

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

const keySize = 32

// KeyFile seals with AES-256-GCM under a random key kept in a 0600 file.
// That protects against other local users and casual disclosure of the
// sealed data, not against root or processes of the same user; keep the key
// on a different path (ideally volume) from the sealed data.
type KeyFile struct{ Path string }

// Name implements Sealer.
func (KeyFile) Name() string { return "keyfile" }

// Seal implements Sealer. It creates the key file if it does not exist.
// The output is nonce || ciphertext.
func (k *KeyFile) Seal(plaintext, aad []byte) ([]byte, error) {
	key, err := k.key(true)
	if err != nil {
		return nil, err
	}
	return sealAEAD(key, plaintext, aad)
}

// Open implements Sealer. It never creates the key file.
func (k *KeyFile) Open(sealed, aad []byte) ([]byte, error) {
	key, err := k.key(false)
	if err != nil {
		return nil, err
	}
	return openAEAD(key, sealed, aad)
}

// Check reads the key as Seal and Open do, without creating it: nil when
// the key file is usable, else why not (fs.ErrNotExist, a *fs.PathError,
// a *KeyModeError, ErrCorrupt for a wrong size). The key is not kept.
func (k *KeyFile) Check() error {
	key, err := k.read()
	clear(key)
	return err
}

// key reads the key, creating it first when create is set and it is absent.
func (k *KeyFile) key(create bool) ([]byte, error) {
	key, err := k.read()
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, fmt.Errorf("%w: %s", ErrNoKey, k.Path)
		}
		if err := k.create(); err != nil {
			return nil, err
		}
		key, err = k.read()
	}
	return key, err
}

// read loads the key, failing closed on loose permissions (a mode or an
// access list letting others in) or a wrong size.
func (k *KeyFile) read() ([]byte, error) {
	f, err := os.Open(k.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read only
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkKeyAccess(k.Path, info); err != nil {
		return nil, err
	}
	key := make([]byte, keySize+1)
	n, err := f.Read(key)
	if n != keySize {
		return nil, &KeySizeError{Path: k.Path, Size: info.Size()}
	}
	if err != nil {
		return nil, err
	}
	return key[:keySize], nil
}

// create writes a new random key unless one appeared meanwhile (createOnce).
func (k *KeyFile) create() error {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("random key: %w", err)
	}
	defer clear(key)
	return createOnce(k.Path, key)
}

// createOnce writes data, a new key (KeyFile's raw, DPAPI's protected), to
// path unless a file is there already: the first creator wins and a later
// one keeps the first's. The directory is made private (acl.MkdirAll) and
// must stay so (checkKeyDir: on Windows the key takes its access list).
// data goes to a private temporary file (atomicfile.KeyPrefix) hard linked
// into place, so a concurrent reader never sees a partial key; without hard
// links the final path is created exclusively, then written.
func createOnce(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := acl.MkdirAll(dir); err != nil {
		if derr := checkKeyDir(dir); derr != nil { // made meanwhile, not as pcb makes it
			return derr
		}
		return err
	}
	if err := checkKeyDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, atomicfile.KeyPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // the key lives on through the link
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	err = link(tmp.Name(), path)
	switch {
	case err == nil:
		atomicfile.SyncDir(dir)
	case errors.Is(err, fs.ErrExist):
		err = nil // another writer won: its key stands
	case noHardLinks(err):
		err = createInPlace(dir, path, data)
	default:
		return fmt.Errorf("creating key file %s: %w", path, err)
	}
	if err != nil {
		return fmt.Errorf("creating key file %s: %w", path, err)
	}
	return nil
}

// createInPlace is createOnce's fallback for filesystems without hard
// links: path is created exclusively, then written. A reader that wins the
// race sees a short file, which the readers refuse (KeyFile by its exact
// size, DPAPI as a blob it cannot unprotect).
func createInPlace(dir, path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	atomicfile.SyncDir(dir)
	return nil
}

// link is os.Link; tests replace it.
var link = os.Link

// noHardLinks reports whether err says the filesystem cannot hard link.
func noHardLinks(err error) bool {
	for _, e := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EXDEV, syscall.EMLINK} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
