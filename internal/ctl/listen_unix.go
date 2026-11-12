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

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
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
// listen there (too long a path) or nothing does (missing, or refused). A
// path that is not a socket owned by this process's effective UID is
// refused before any dial (ruling M5-R2), and so is a connection whose
// server peer reports another UID (re-review N1: the socket may have been
// swapped between the lstat and the dial), before anything is sent on it.
func dialSocket(ctx context.Context, path string, peer func(net.Conn) (int, bool)) (net.Conn, error) {
	if len(path) > maxPath {
		return nil, ErrNoDaemon
	}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNoDaemon
	case err != nil:
		return nil, err
	}
	euid := os.Geteuid()
	if err := checkSocket(path, fi, euid); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "unix", path)
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return nil, ErrNoDaemon
	case err != nil:
		return nil, err
	}
	if uid, checked := peer(c); checked && uid != euid {
		_ = c.Close()
		return nil, &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused(fmt.Sprintf("the process answering on the control socket %s runs as another user (uid %d): pcb refuses to talk to it", path, uid)),
			Hint: "run pcb as the user that runs pcb serve, and check who replaced " + path}
	}
	return c, nil
}

// checkSocket refuses a path (lstat'ed as fi) that is not a socket owned by
// euid: a symlink, a file, or another user's socket is never dialled.
func checkSocket(path string, fi fs.FileInfo, euid int) error {
	if fi.Mode().Type() != fs.ModeSocket {
		return &exitcode.Error{Code: exitcode.Blocked, Err: refused(path + " is not a socket: pcb refuses to use it as the control socket"),
			Hint: "move " + path + " away: pcb serve keeps its control socket there"}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != euid {
		owner := -1
		if ok {
			owner = int(st.Uid)
		}
		return &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused(fmt.Sprintf("the control socket %s belongs to another user (uid %d): pcb refuses to use it", path, owner)),
			Hint: "run pcb as the user that runs pcb serve, or point PCB_STATE_DIR at your own state directory"}
	}
	return nil
}
