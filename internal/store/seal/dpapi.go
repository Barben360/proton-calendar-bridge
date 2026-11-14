package seal

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

// DPAPIKeyName is the DPAPI key's file name, in the state directory.
const DPAPIKeyName = "secrets.key.dpapi"

// ErrNoDPAPI is a DPAPI sealer used off Windows: there is no DPAPI there.
var ErrNoDPAPI = errors.New("DPAPI is Windows only")

// maxDPAPIBlob bounds what is read of a DPAPI key file: a protected 32-byte
// key takes a few hundred bytes.
const maxDPAPIBlob = 64 << 10

// DPAPI seals with AES-256-GCM, as KeyFile does, under a random key kept in
// Path as Windows' DPAPI protects it (CryptProtectData, the user's scope):
// only this Windows user, signed in, can unprotect it; a copy of the file
// opens nowhere else. Like KeyFile it does not protect against processes of
// the same user. Off Windows it fails with ErrNoDPAPI and creates nothing.
type DPAPI struct{ Path string }

// Name implements Sealer.
func (DPAPI) Name() string { return "dpapi" }

// Seal implements Sealer. It creates the key if it does not exist.
func (d *DPAPI) Seal(plaintext, aad []byte) ([]byte, error) {
	key, err := d.key(true)
	if err != nil {
		return nil, err
	}
	return sealAEAD(key, plaintext, aad)
}

// Open implements Sealer. It never creates the key.
func (d *DPAPI) Open(sealed, aad []byte) ([]byte, error) {
	key, err := d.key(false)
	if err != nil {
		return nil, err
	}
	return openAEAD(key, sealed, aad)
}

// Check reads the key as Seal and Open do, without creating it: nil when it
// is usable, else why not (fs.ErrNotExist, a *fs.PathError, a
// *KeyModeError, ErrCorrupt for a blob this user cannot unprotect or a key
// of the wrong size, ErrNoDPAPI off Windows). The key is not kept.
func (d *DPAPI) Check() error {
	key, err := d.read()
	clear(key)
	return err
}

// key reads the key, creating it first when create is set and it is absent.
func (d *DPAPI) key(create bool) ([]byte, error) {
	key, err := d.read()
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, fmt.Errorf("%w: %s", ErrNoKey, d.Path)
		}
		if err := d.create(); err != nil {
			return nil, err
		}
		key, err = d.read()
	}
	return key, err
}

// read loads and unprotects the key, failing closed on an access list
// letting others in, a blob this user cannot unprotect or a wrong size.
func (d *DPAPI) read() ([]byte, error) {
	f, err := atomicfile.Open(d.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read only
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkKeyAccess(d.Path, info); err != nil {
		return nil, err
	}
	blob, err := io.ReadAll(io.LimitReader(f, maxDPAPIBlob))
	if err != nil {
		return nil, err
	}
	key, err := unprotect(blob)
	switch {
	case errors.Is(err, ErrNoDPAPI):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: key file %s cannot be unprotected by this Windows user: %w", ErrCorrupt, d.Path, err)
	case len(key) != keySize:
		clear(key)
		return nil, &KeySizeError{Path: d.Path, Size: int64(len(key))}
	}
	return key, nil
}

// create protects a new random key and writes it unless one appeared
// meanwhile (createOnce: the first creator wins). Off Windows protect fails
// before anything is written.
func (d *DPAPI) create() error {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("random key: %w", err)
	}
	blob, err := protect(key)
	clear(key)
	if err != nil {
		return err
	}
	return createOnce(d.Path, blob)
}
