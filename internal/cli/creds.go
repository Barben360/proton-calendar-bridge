package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// maxPasswordFD bounds what --password-fd reads.
const maxPasswordFD = 4096

func mustAct(msg, hint string) error {
	return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New(msg), Hint: hint}
}

// credentials are what a login needs. TOTP stays empty unless PCB_TOTP is set:
// it is asked for only when the service wants it.
type credentials struct{ username, password, totp string }

// credentialSource says where the secrets may come from: a flag value, a
// descriptor or the environment, then (interactive only) the terminal.
type credentialSource struct {
	username      string // --username
	passwordFD    int    // --password-fd, when passwordFDSet
	passwordFDSet bool
	interactive   bool
}

// credentials gathers the credentials: username --username > PCB_USERNAME >
// prompt; password --password-fd > PCB_PASSWORD > prompt without echo. A
// missing one when not interactive is an exit 10 naming where to put it,
// before any service call.
func (a *app) credentials(ctx context.Context, src credentialSource) (credentials, error) {
	c := credentials{username: src.username, totp: a.Env("PCB_TOTP")}
	if c.username == "" {
		c.username = a.Env("PCB_USERNAME")
	}
	if c.username == "" {
		if !src.interactive {
			return credentials{}, mustAct("no Proton username: set PCB_USERNAME or pass --username",
				"set PCB_USERNAME and PCB_PASSWORD (or pass --password-fd), or run pcb login in a terminal")
		}
		line, err := a.Term.ReadLine(ctx, "Proton username: ")
		if err != nil {
			return credentials{}, noAnswer(ctx, "Proton username", err)
		}
		c.username = line
	}
	if c.username == "" {
		return credentials{}, UsageError(errors.New("the Proton username is empty"))
	}
	switch {
	case src.passwordFDSet:
		pw, err := readPasswordFD(src.passwordFD)
		if err != nil {
			return credentials{}, err
		}
		c.password = pw
	case a.Env("PCB_PASSWORD") != "":
		c.password = a.Env("PCB_PASSWORD")
	case src.interactive:
		pw, err := a.Term.ReadSecret(ctx, "Proton password: ")
		if err != nil {
			return credentials{}, noAnswer(ctx, "Proton password", err)
		}
		if pw == "" {
			return credentials{}, UsageError(errors.New("the Proton password is empty"))
		}
		c.password = pw
	default:
		return credentials{}, mustAct("no Proton password: set PCB_PASSWORD or pass --password-fd N",
			"set PCB_PASSWORD, or pass the password on a file descriptor (--password-fd N), or run pcb login in a terminal")
	}
	return c, nil
}

// noAnswer is what a prompt that failed becomes: a cancelled command when
// ctx ended (Ctrl-C), else an exit 10 naming what was expected (end of input,
// such as Ctrl-D).
func noAnswer(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return cancelled()
	}
	_ = err // the terminal's own error carries nothing the person can act on
	return mustAct("no "+what+" was entered (end of input)",
		"answer the prompt, or set PCB_USERNAME, PCB_PASSWORD and PCB_TOTP and use --non-interactive")
}

func cancelled() error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("the command was cancelled"), Hint: "run the command again"}
}

// isTerminalFD is term.IsTerminal; tests replace it.
var isTerminalFD = term.IsTerminal

// readPasswordFD reads a password from descriptor fd up to end of input (the
// writer must close its end), at most maxPasswordFD bytes, and closes it. A
// terminal is refused: type the password at the prompt instead. Exactly one trailing "\n" or
// "\r\n" is stripped (what echo and password managers append); the rest, spaces
// included, is the password. An empty password is a usage error.
func readPasswordFD(fd int) (string, error) {
	if fd < 0 {
		return "", UsageError(errors.New("--password-fd needs a descriptor number"))
	}
	if isTerminalFD(fd) {
		return "", &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("--password-fd %d is a terminal", fd),
			Hint: "pipe the password in (pcb login --password-fd 3 3<file), or omit --password-fd to be prompted"}
	}
	f := os.NewFile(uintptr(fd), "password-fd")
	if f == nil {
		return "", UsageError(fmt.Errorf("--password-fd %d is not a usable descriptor", fd))
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxPasswordFD+1))
	if err != nil {
		return "", UsageError(fmt.Errorf("cannot read the password from descriptor %d", fd))
	}
	defer clear(b)
	if len(b) > maxPasswordFD {
		return "", UsageError(fmt.Errorf("the password on descriptor %d is longer than %d bytes", fd, maxPasswordFD))
	}
	if rest, ok := bytes.CutSuffix(b, []byte("\n")); ok {
		b = bytes.TrimSuffix(rest, []byte("\r"))
	}
	if len(b) == 0 {
		return "", UsageError(fmt.Errorf("the password on descriptor %d is empty", fd))
	}
	return string(b), nil
}
