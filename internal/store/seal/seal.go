// Package seal encrypts small secrets at rest. A Sealer holds the key
// outside the sealed data; the aad binds a blob to the record it belongs to.
// Protection is at rest only: keys and plaintext are not zeroed in memory
// (Go copies them).
package seal

import (
	"errors"
	"fmt"
	"io/fs"
)

// Sealer seals and opens secrets. Implementations never put the key or the
// plaintext in an error.
type Sealer interface {
	// Name identifies the backend; it is stored next to the sealed data.
	Name() string
	// Seal encrypts plaintext, authenticating aad (not stored in the output).
	Seal(plaintext, aad []byte) ([]byte, error)
	// Open decrypts what Seal produced with the same aad.
	Open(sealed, aad []byte) ([]byte, error)
}

var (
	// ErrNoKey means the key to open the data is not there. Callers must not
	// treat it as an empty store: a new key could not open the old data.
	ErrNoKey = errors.New("sealing key not found")
	// ErrCorrupt means the data is not authentic: tampered, truncated,
	// sealed with another key or for another context.
	ErrCorrupt = errors.New("sealed data is corrupt or not for this key")
	// ErrKeyMode means the key file's mode lets other users in: it is
	// refused, readable or not (fails closed). The error is a *KeyModeError.
	ErrKeyMode = errors.New("the key file is accessible by others")
)

// KeyModeError is a key file refused for its mode.
type KeyModeError struct {
	Path string
	Mode fs.FileMode
}

func (e *KeyModeError) Error() string {
	return fmt.Sprintf("key file %s is accessible by others (mode %04o). Fix: chmod 600 %s", e.Path, e.Mode, e.Path)
}

// Is makes errors.Is(err, ErrKeyMode) hold.
func (e *KeyModeError) Is(target error) bool { return target == ErrKeyMode }
