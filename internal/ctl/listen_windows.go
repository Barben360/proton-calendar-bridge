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
	"time"

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
// removes the pipe once the server's end of every connection has closed
// too (a client's end never holds the name).
//
// A name nothing answers on may be the previous server of this state still
// closing its end of a connection (a restart: go-winio's Close returns at
// once to a second caller while the first still closes the handle).
// Listen waits for it, up to relistenFor, probing between tries: a server
// that answers, or a pipe this user may not open, ends the wait at once.
// A name still held then is ErrPipeHeld.
func Listen(path string) (net.Listener, error) {
	name, err := pipeName(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("the control pipe's directory: %w", err)
	}
	sid, err := currentSID()
	if err != nil {
		return nil, fmt.Errorf("reading this user's SID: %w", err)
	}
	conf := &winio.PipeConfig{SecurityDescriptor: "O:" + sid + "D:P(A;;GA;;;" + sid + ")"}
	deadline := time.Now().Add(relistenFor)
	for goneSeen := false; ; {
		ln, err := listenPipe(name, conf)
		switch {
		case err == nil:
			return ln, nil
		case errors.Is(err, windows.ERROR_PIPE_BUSY):
			return nil, fmt.Errorf("%w: %s", ErrPipeTaken, name)
		case !errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return nil, fmt.Errorf("opening the control pipe %s: %w", name, err)
		}
		// The name exists (or another user's pipe denies this one).
		switch probe(name) {
		case pipeServed:
			return nil, fmt.Errorf("%w: %s", ErrPipeTaken, name)
		case pipeGone:
			// Gone: try again at once the first time. A name that keeps
			// vanishing while ListenPipe keeps being denied (a token that
			// may not create it) gets a step, not a busy loop.
			if goneSeen {
				time.Sleep(goneStep)
			}
			goneSeen = true
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s", ErrPipeHeld, name)
		}
	}
}

// relistenFor bounds Listen's wait for a name nothing answers on;
// probeFor bounds each probe, the step between two tries; goneStep is the
// step after a name found gone more than once.
const (
	relistenFor = 500 * time.Millisecond
	probeFor    = 25 * time.Millisecond
	goneStep    = 10 * time.Millisecond
)

// listenPipe and probe are go-winio's ListenPipe and probePipe; tests
// replace them.
var (
	listenPipe = winio.ListenPipe
	probe      = probePipe
)

// A probe's finding (probePipe).
type pipeProbe int

const (
	pipeBusy   pipeProbe = iota // every instance busy or closing: try again
	pipeGone                    // the name no longer exists: try again
	pipeServed                  // taken for good: never shared
)

// probePipe tells whether the pipe name is taken for good (pipeServed): a
// server answers on it (the probe connects, and closes at once, sending
// nothing), or it is a pipe this user may not open (another user's). A
// name that is gone, or whose every instance is busy or closing (the probe
// times out), is worth another try. A pcb server always has an instance
// listening while it serves (go-winio makes the next one at each Accept),
// so it answers within the probe.
func probePipe(name string) pipeProbe {
	ctx, cancel := context.WithTimeout(context.Background(), probeFor)
	defer cancel()
	c, err := winio.DialPipeContext(ctx, name)
	switch {
	case err == nil:
		_ = c.Close()
		return pipeServed
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return pipeServed
	case errors.Is(err, fs.ErrNotExist):
		return pipeGone
	}
	<-ctx.Done() // a step, not a busy loop, on any other failure
	return pipeBusy
}

// dialSocket connects to the control pipe of path's directory: ErrNoDaemon
// when the directory or the pipe is missing. A pipe this user may not open,
// or one another user owns (a squatter's), is refused, blocked (20), before
// anything is sent on it. peer, the Unix server-UID check, is unused:
// serverSIDOf checks here.
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
	if err := checkPipeOwner(c, name, hint); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// checkPipeOwner refuses the pipe connection c unless this user owns the
// pipe. The owner, not the server's process ID: a PID outlives nothing, so
// a squatter serving from a handle its exited helper created would report
// a dead PID that may now be one of ours; no user can make another user's
// SID the owner of a pipe without SeRestorePrivilege.
func checkPipeOwner(c net.Conn, name, hint string) error {
	own, err := currentSID()
	if err != nil {
		return &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused(fmt.Sprintf("cannot read this user's SID (%v): pcb refuses to talk to the control pipe %s", err, name)),
			Hint: "check that pcb runs in a normal login session of this user"}
	}
	sid, err := serverSIDOf(c)
	if err != nil {
		return &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused(fmt.Sprintf("the owner of the control pipe %s cannot be read (%v): pcb refuses to talk to it", name, err)),
			Hint: hint}
	}
	if sid != own {
		return &exitcode.Error{Code: exitcode.Blocked,
			Err:  refused(fmt.Sprintf("the control pipe %s is owned by another user (%s): pcb refuses to talk to it", name, sid)),
			Hint: hint}
	}
	return nil
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
