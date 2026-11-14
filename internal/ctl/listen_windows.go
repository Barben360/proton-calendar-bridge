//go:build windows

package ctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// Listen opens the control pipe of path's directory (pipeName), path being
// <state>\pcb.sock as on Unix. Its security descriptor gives the pipe to
// this user's SID alone: owner, and a protected DACL of one full-access
// ACE, so no other user opens it nor creates an instance of it; go-winio
// refuses remote clients (PIPE_REJECT_REMOTE_CLIENTS). A name already
// served, by a second pcb of this state or another user's process
// squatting it, is ErrPipeTaken at once: never shared. Closing the listener
// removes the pipe.
func Listen(path string) (net.Listener, error) {
	name, err := pipeName(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("the control pipe's directory: %w", err)
	}
	sid, err := currentSID()
	if err != nil {
		return nil, fmt.Errorf("reading this user's SID: %w", err)
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: "O:" + sid + "D:P(A;;GA;;;" + sid + ")"})
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.ERROR_PIPE_BUSY):
		return nil, fmt.Errorf("%w: %s", ErrPipeTaken, name)
	case err != nil:
		return nil, fmt.Errorf("opening the control pipe %s: %w", name, err)
	}
	return ln, nil
}

// dialSocket connects to the control pipe of path's directory: ErrNoDaemon
// when the directory or the pipe is missing. A pipe this user may not open,
// or one whose server process runs as another user (a squatter), is
// refused, blocked (20), before anything is sent on it. peer, the Unix
// server-UID check, is unused: serverSIDOf checks here.
func dialSocket(ctx context.Context, path string, _ func(net.Conn) (int, bool)) (net.Conn, error) {
	name, err := pipeName(filepath.Dir(path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNoDaemon
	case err != nil:
		return nil, err
	}
	hint := "run pcb as the user that runs pcb-ui or pcb serve, and find which process created " + name
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := winio.DialPipeContext(ctx, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNoDaemon
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return nil, &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused("cannot open the control pipe " + name + ": access denied (another user's process may hold it)"),
			Hint: hint}
	case err != nil:
		return nil, err
	}
	sid, err := serverSIDOf(c)
	if err != nil {
		_ = c.Close()
		return nil, &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused(fmt.Sprintf("the user of the process answering on the control pipe %s cannot be read (%v): pcb refuses to talk to it", name, err)),
			Hint: hint}
	}
	if own, err := currentSID(); err != nil || sid != own {
		_ = c.Close()
		return nil, &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused(fmt.Sprintf("the process answering on the control pipe %s runs as another user (%s): pcb refuses to talk to it", name, sid)),
			Hint: hint}
	}
	return c, nil
}

// pipeName is the control pipe of the state directory dir: pipeNameFor its
// final path, read from a handle on the directory, so its case, 8.3 short
// form or a junction to it name the same pipe. A missing directory is
// fs.ErrNotExist.
func pipeName(dir string) (string, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: dir, Err: err}
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", &os.PathError{Op: "GetFinalPathNameByHandle", Path: dir, Err: err}
		}
		if int(n) < len(buf) {
			return pipeNameFor(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n) // n counts the NUL when the buffer is short
	}
}

// currentSID is the SID of this process's user, read once. It repeats the
// few lines of acl.UserSID: internal/ctl stays a thin adapter that imports
// no other package of the module but the service's plain types (its arch
// test).
var currentSID = sync.OnceValues(func() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
})
