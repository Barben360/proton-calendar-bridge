// Package logfile is a size-capped log file: an io.WriteCloser that keeps
// one rotation. It imports only the standard library, and never logs or
// panics on its own: a failure is an error of Open, Write or Close.
package logfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File appends to a log file and rotates it to path+".1" before a write
// that would take it past max bytes; one rotation is kept, the older .1 is
// replaced. Files are created 0600 and their missing parent directories
// 0700. It is safe for concurrent use.
//
// A single write larger than max is never split: it rotates what the file
// holds (when not empty), then lands whole in the fresh file, which the
// next write rotates again.
type File struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File // nil once closed
	size int64
	// holdUntil is the size below which a failed rename is not retried.
	holdUntil int64
	closed    bool
}

// Open opens (or creates) the log at path, appending to what it holds, with
// the cap max (> 0). An existing file is tightened to 0600.
func Open(path string, max int64) (*File, error) {
	if max <= 0 {
		return nil, fmt.Errorf("log file %s: the size cap must be positive", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f := &File{path: path, max: max}
	if err := f.open(); err != nil {
		return nil, err
	}
	return f, nil
}

// open opens the file for appending and reads its size. mu is held, or f
// is not shared yet.
func (f *File) open() error {
	h, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := h.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", f.path)
	}
	if err == nil {
		err = h.Chmod(0o600)
	}
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = h.Close()
		return err
	}
	f.f, f.size = h, st.Size()
	return nil
}

// Write appends p, rotating first when the file would pass max.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if next := f.size + int64(len(p)); f.size > 0 && next > f.max && next > f.holdUntil {
		if err := f.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := f.f.Write(p)
	f.size += int64(n)
	return n, err
}

// rotate moves the file to path+".1" and opens a fresh one. mu is held. A
// failed rename is not an error of the write: the log keeps going in the
// same file, uncapped, and the rename is retried only once the file has
// grown by another max. Only a failure to reopen the file is returned.
func (f *File) rotate() error {
	if err := f.f.Close(); err != nil {
		return err
	}
	f.f = nil
	renameErr := os.Rename(f.path, f.path+".1")
	if err := f.open(); err != nil {
		f.closed = true
		return err
	}
	if renameErr != nil {
		f.holdUntil = f.size + f.max
	} else {
		f.holdUntil = 0
	}
	return nil
}

// Close closes the file; a later Write fails, a later Close is a no-op.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.f == nil {
		return nil
	}
	return f.f.Close()
}
