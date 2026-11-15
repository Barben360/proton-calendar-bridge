//go:build windows

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Open opens path for reading with FILE_SHARE_READ|WRITE|DELETE, so Write
// can rename a replacement over the file while it is open (os.Open leaves
// out FILE_SHARE_DELETE on Windows).
func Open(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// Rename moves from to to, replacing to (see rename: POSIX semantics for a
// file, os.Rename for a directory), retried every 50 ms for at most
// RenameRetry while a foreign reader or a scanner holds a file (sharing
// violation, access denied). A directory at to fails at once as on Unix
// (dirTarget), not with the access denied Windows gives for it.
func Rename(from, to string) error {
	if err := dirTarget(from, to); err != nil {
		return err
	}
	deadline := time.Now().Add(RenameRetry)
	for {
		err := rename(from, to)
		if err == nil || !(errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dirTarget is the refusal Go's os.Rename gives on Unix when to is a
// directory: EEXIST, unless from is the same directory under another case.
// Windows refuses to replace a directory too, but with access denied, which
// reads as a permission fault and is retried as a reader's for RenameRetry;
// no wait makes it succeed.
func dirTarget(from, to string) error {
	fi, err := os.Lstat(to)
	if err != nil || !fi.IsDir() {
		return nil
	}
	ofi, err := os.Lstat(from)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	if from == to || !os.SameFile(fi, ofi) {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EEXIST}
	}
	return nil
}

// rename moves a file with POSIX semantics (FileRenameInfoEx, Windows 10
// 1709+): it replaces a target another handle holds open with
// FILE_SHARE_DELETE, which MoveFileEx (os.Rename) refuses. A directory, and
// a system without the call, go through os.Rename.
func rename(from, to string) error {
	if fi, err := os.Lstat(from); err != nil || fi.IsDir() {
		return os.Rename(from, to)
	}
	err := posixRename(from, to)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return os.Rename(from, to)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

// posixRename renames from (a file, or the link itself if it is one) to to,
// replacing to even while another handle holds it with FILE_SHARE_DELETE.
func posixRename(from, to string) error {
	abs, err := windows.FullPath(to)
	if err != nil {
		return err
	}
	// The name keeps its NUL: SetFileInformationByHandle converts it to an NT
	// path as a NUL-terminated string and ignores FileNameLength, so without
	// one it reads past the name into the next heap object, and the rename
	// fails (invalid name, path not found) or lands on that longer name.
	name, err := windows.UTF16FromString(abs)
	if err != nil {
		return err
	}
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(src, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about it
	size := int(unsafe.Offsetof(renameInfo{}.FileName)) + len(name)*2
	buf := make([]uint64, (size+7)/8) // zeroed, aligned for the handle field
	info := (*renameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32((len(name) - 1) * 2) // bytes, without the NUL
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	err = windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, (*byte)(unsafe.Pointer(&buf[0])), uint32(size))
	runtime.KeepAlive(buf)
	return err
}

// renameInfo is FILE_RENAME_INFO (the buffer is longer than the struct: the
// name and its NUL follow FileName's first element).
type renameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}
