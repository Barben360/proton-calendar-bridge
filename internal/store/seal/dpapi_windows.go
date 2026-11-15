//go:build windows

package seal

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiEntropy is the secondary entropy every DPAPI key is protected with:
// another program of the same user calling CryptUnprotectData on the file
// without it gets nothing.
var dpapiEntropy = []byte("proton-calendar-bridge secrets.key.dpapi v1")

// dpapiDescription is the description stored in the blob.
const dpapiDescription = "pcb sealing key"

// protect is CryptProtectData of data in the user's scope (no
// CRYPTPROTECT_LOCAL_MACHINE), never prompting.
func protect(data []byte) ([]byte, error) {
	return protectWith(data, dpapiEntropy, windows.CRYPTPROTECT_UI_FORBIDDEN)
}

// unprotect is CryptUnprotectData of blob, never prompting, refusing a
// blob protected for the whole machine (errMachineScope).
func unprotect(blob []byte) ([]byte, error) {
	data, err := unprotectWith(blob, dpapiEntropy)
	if err != nil {
		return nil, err
	}
	if !userScope(blob) {
		clear(data)
		return nil, errMachineScope
	}
	return data, nil
}

// userScope reports whether blob, which CryptUnprotectData accepted, is
// protected for the user: its header (DPAPI_BLOB: version, provider GUID,
// master-key version and GUID, then dwFlags at offset 40, all covered by
// the blob's integrity check) lacks CRYPTPROTECT_LOCAL_MACHINE.
func userScope(blob []byte) bool {
	return len(blob) >= 44 && binary.LittleEndian.Uint32(blob[40:44])&windows.CRYPTPROTECT_LOCAL_MACHINE == 0
}

func protectWith(data, entropy []byte, flags uint32) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(dpapiDescription)
	if err != nil {
		return nil, err
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(blobOf(data), name, blobOf(entropy), 0, nil, flags, &out); err != nil {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	return takeBlob(&out), nil
}

func unprotectWith(blob, entropy []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blobOf(blob), nil, blobOf(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	return takeBlob(&out), nil
}

// blobOf is a DataBlob over b (an empty one for an empty b).
func blobOf(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

// takeBlob copies what DPAPI allocated into Go memory, wipes it and frees
// it (LocalFree).
func takeBlob(b *windows.DataBlob) []byte {
	if b.Data == nil {
		return nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(b.Data))) //nolint:errcheck // nothing to do on failure
	src := unsafe.Slice(b.Data, b.Size)
	out := make([]byte, len(src))
	copy(out, src)
	clear(src)
	return out
}
