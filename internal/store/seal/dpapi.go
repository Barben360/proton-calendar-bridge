package seal

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"syscall"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

// DPAPIKeyName is the DPAPI key's file name, in the state directory.
const DPAPIKeyName = "secrets.key.dpapi"

// ErrNoDPAPI is a DPAPI sealer used off Windows: there is no DPAPI there.
var ErrNoDPAPI = errors.New("DPAPI is Windows only")

// ErrProtect means Windows could not protect a new key for this user
// (CryptProtectData failed), or could not unprotect the existing one for a
// reason other than a damaged or foreign blob: typically a logon without
// the user's password, such as SSH with a key, where DPAPI cannot reach the
// user's master key. Nothing was written or lost: signing in with the
// password lets it work. The error is a *ProtectError.
var ErrProtect = errors.New("DPAPI could not protect the sealing key for this Windows user")

// ProtectError is ErrProtect with Windows' cause; Unprotect is set when it
// was the existing key that could not be unprotected.
type ProtectError struct {
	Unprotect bool
	Err       error
}

func (e *ProtectError) Error() string {
	if e.Unprotect {
		return "DPAPI could not unprotect the sealing key for this Windows user: " + e.Err.Error()
	}
	return ErrProtect.Error() + ": " + e.Err.Error()
}

// Unwrap is the cause.
func (e *ProtectError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrProtect) hold.
func (e *ProtectError) Is(target error) bool { return target == ErrProtect }

// integrityErrnos are the CryptUnprotectData failures that say the blob
// itself is damaged or not pcb's (other entropy, not a DPAPI blob; the
// codes seen on Windows 11 are in TestDPAPIForeignBlobIsRefused): those are
// ErrCorrupt, as is ERROR_INVALID_PARAMETER for a file without a DPAPI
// header (corruptBlob). Any other failure (ERROR_ACCESS_DENIED in a logon
// without the password, NTE_BAD_KEY_STATE for another user's blob or a
// profile without its master key, an unknown code) is a *ProtectError: the
// key may open in another session, so nothing must call it lost.
var integrityErrnos = []syscall.Errno{
	13,         // ERROR_INVALID_DATA: other entropy, altered blob
	0x80090002, // NTE_BAD_HASH
	0x80090005, // NTE_BAD_DATA
	0x80090006, // NTE_BAD_SIGNATURE
	0x80091007, // CRYPT_E_HASH_VALUE
}

// errInvalidParameter is ERROR_INVALID_PARAMETER, what CryptUnprotectData
// gives for a file that is no DPAPI blob at all (a raw key, a short file).
const errInvalidParameter = syscall.Errno(87)

// dpapiProvider is the provider GUID every DPAPI blob names at offset 4,
// df9d8cd0-1501-11d1-8c7a-00c04fc297eb, in its byte order there.
var dpapiProvider = []byte{0xd0, 0x8c, 0x9d, 0xdf, 0x01, 0x15, 0xd1, 0x11, 0x8c, 0x7a, 0x00, 0xc0, 0x4f, 0xc2, 0x97, 0xeb}

// HasDPAPIHeader reports whether blob starts as a DPAPI blob does:
// dwVersion 1, then the provider GUID (pcb doctor tells a file that is no
// DPAPI blob from one Windows refuses).
func HasDPAPIHeader(blob []byte) bool {
	return len(blob) >= 20 && binary.LittleEndian.Uint32(blob) == 1 && bytes.Equal(blob[4:20], dpapiProvider)
}

// corruptBlob reports whether err, the failure to unprotect blob, is an
// integrity failure: one of integrityErrnos, or ERROR_INVALID_PARAMETER
// for a file without a DPAPI header (with one, the parameter Windows
// refused may be of this session: the safe side is not to call it lost).
func corruptBlob(blob []byte, err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if errno == errInvalidParameter {
		return !HasDPAPIHeader(blob)
	}
	return slices.Contains(integrityErrnos, errno)
}

// errMachineScope is a key protected for the whole machine
// (CRYPTPROTECT_LOCAL_MACHINE), which any local account unprotects: pcb
// only writes user-scoped keys, so another account planted it.
var errMachineScope = errors.New("protected for the whole machine, not this user")

// protectKey and unprotectKey are protect and unprotect; tests replace
// them.
var (
	protectKey   = protect
	unprotectKey = unprotect
)

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
	defer clear(key)
	return sealAEAD(key, plaintext, aad)
}

// Open implements Sealer. It never creates the key.
func (d *DPAPI) Open(sealed, aad []byte) ([]byte, error) {
	key, err := d.key(false)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return openAEAD(key, sealed, aad)
}

// Check reads the key as Seal and Open do, without creating it: nil when it
// is usable, else why not (fs.ErrNotExist, a *fs.PathError, a
// *KeyModeError, ErrCorrupt for a damaged or foreign blob or a key of the
// wrong size, a *ProtectError when Windows cannot unprotect it in this
// session, ErrNoDPAPI off Windows). The key is not kept.
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
	key, err := unprotectKey(blob)
	switch {
	case errors.Is(err, ErrNoDPAPI):
		return nil, err
	case errors.Is(err, errMachineScope):
		return nil, fmt.Errorf("%w: key file %s is %w", ErrCorrupt, d.Path, err)
	case corruptBlob(blob, err):
		return nil, fmt.Errorf("%w: key file %s is damaged or not pcb's: %w", ErrCorrupt, d.Path, err)
	case err != nil:
		return nil, &ProtectError{Unprotect: true, Err: err}
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
	blob, err := protectKey(key)
	clear(key)
	switch {
	case errors.Is(err, ErrNoDPAPI):
		return err
	case err != nil:
		return &ProtectError{Err: err}
	}
	return createOnce(d.Path, blob)
}
