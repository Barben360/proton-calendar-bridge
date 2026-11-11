package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
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
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize(), gcm.NonceSize()+len(plaintext)+gcm.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("random nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// Open implements Sealer. It never creates the key file.
func (k *KeyFile) Open(sealed, aad []byte) ([]byte, error) {
	key, err := k.key(false)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrCorrupt
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrCorrupt
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
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

// read loads the key, failing closed on loose permissions or a wrong size.
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
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key file %s is accessible by others (mode %04o). Fix: chmod 600 %s", k.Path, info.Mode().Perm(), k.Path)
	}
	key := make([]byte, keySize+1)
	n, err := f.Read(key)
	if n != keySize {
		return nil, fmt.Errorf("%w: key file %s is not %d bytes", ErrCorrupt, k.Path, keySize)
	}
	if err != nil {
		return nil, err
	}
	return key[:keySize], nil
}

// create writes a new random key unless one appeared meanwhile. The key is
// written to a private temporary file and hard linked into place, so a
// concurrent reader never sees a partial key and the first writer wins.
func (k *KeyFile) create() error {
	dir := filepath.Dir(k.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // the key lives on through the link
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("random key: %w", err)
	}
	if _, err := tmp.Write(key); err != nil {
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
	err = link(tmp.Name(), k.Path)
	switch {
	case err == nil:
		syncDir(dir)
	case errors.Is(err, fs.ErrExist):
		err = nil // another writer won: its key stands
	case noHardLinks(err):
		err = k.createExclusive(dir, key)
	default:
		return fmt.Errorf("creating key file %s: %w", k.Path, err)
	}
	if err != nil {
		return fmt.Errorf("creating key file %s: %w", k.Path, err)
	}
	return nil
}

// createExclusive is the fallback for filesystems without hard links: the
// final path is created exclusively, then written. A reader that wins the
// race sees a short file, which read refuses by its exact size.
func (k *KeyFile) createExclusive(dir string, key []byte) error {
	f, err := os.OpenFile(k.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(key); err != nil {
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
	syncDir(dir)
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

// syncDir fsyncs a directory, best effort.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
