// Package install installs pcb as a systemd service, removes it, and
// inspects it (M5-Rpre5, M5-Rpre6). Every path it touches goes under
// Host.Root and every command through Host.Run, so tests run on a temp tree
// with a fake runner and never touch the real system.
package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
)

// Runner runs the system's commands: systemctl, journalctl, useradd,
// userdel and loginctl. A command that ran gives its exit code and a nil
// error; err is set only when it could not run (or was killed).
type Runner interface {
	// Output runs name and returns what it printed on stdout.
	Output(ctx context.Context, name string, args ...string) (stdout []byte, code int, err error)
	// Stream runs name with its output going to stdout and stderr.
	Stream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) (code int, err error)
}

// Host is the machine pcb installs on.
type Host struct {
	Root       string // prefix of every absolute path ("" in production; a temp dir in tests)
	Run        Runner
	Euid       func() int
	Executable func() (string, error)
	LookupUser func(name string) (uid, gid int, ok bool, err error)
	Chown      func(path string, uid, gid int) error
	Env        func(string) string
}

// OSHost is the machine pcb runs on.
func OSHost() Host {
	return Host{Run: osRunner{}, Euid: os.Geteuid, Executable: os.Executable, LookupUser: lookupUser,
		Chown: os.Chown, Env: os.Getenv}
}

// Path is where the absolute path p lies on h: p under Root.
func (h Host) Path(p string) string {
	if h.Root == "" {
		return p
	}
	return filepath.Join(h.Root, p)
}

func lookupUser(name string) (uid, gid int, ok bool, err error) {
	u, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, false, err
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, false, err
	}
	return uid, gid, true, nil
}

type osRunner struct{}

func (osRunner) Output(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	code, err := exitCode(cmd.Run())
	return out.Bytes(), code, err
}

func (osRunner) Stream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return exitCode(cmd.Run())
}

// exitCode splits a command's result: its exit code when it ran to the end,
// else -1 and the error.
func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode(), nil
	}
	return -1, err
}
