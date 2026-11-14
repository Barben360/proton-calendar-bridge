//go:build windows

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
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

// Rename is os.Rename, retried every 50 ms for at most RenameRetry while a
// reader or a scanner holds the file (sharing violation, access denied).
func Rename(from, to string) error {
	deadline := time.Now().Add(RenameRetry)
	for {
		err := rename(from, to)
		if err == nil || !(errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
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

func posixRename(from, to string) error {
	abs, err := windows.FullPath(to)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(abs)
	if err != nil {
		return err
	}
	name = name[:len(name)-1] // without the NUL: the length is explicit
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(src, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about it
	buf := make([]byte, int(unsafe.Offsetof(renameInfo{}.FileName))+len(name)*2)
	info := (*renameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(len(buf)))
}

// renameInfo is FILE_RENAME_INFO (the buffer is longer than the struct: the
// name follows FileName's first element).
type renameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}
