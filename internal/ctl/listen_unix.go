//go:build unix

package ctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// Listen opens the control socket at path: refused when the path is too
// long, when its directory grants its group or others anything, or when a
// file that is not a socket holds it. A socket already there is stale and
// replaced without a probe: pcb serve holds sync.lock before it binds, so
// no other daemon owns it (ruling M5-P2). The socket is 0600 once bound;
// closing the listener removes it.
func Listen(path string) (net.Listener, error) {
	if len(path) > maxPath {
		return nil, ErrPathTooLong
	}
	dir := filepath.Dir(path)
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("the control socket's directory: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, ErrUnsafeDir
	}
	switch li, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case li.Mode().Type() != fs.ModeSocket:
		return nil, ErrNotASocket
	default:
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("removing the stale control socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restricting the control socket: %w", err)
	}
	return ln, nil
}

// dialSocket connects to the socket at path: ErrNoDaemon when nothing can
// listen there (too long a path) or nothing does (missing, or refused).
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	if len(path) > maxPath {
		return nil, ErrNoDaemon
	}
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "unix", path)
	switch {
	case err == nil:
		return c, nil
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return nil, ErrNoDaemon
	}
	return nil, err
}
