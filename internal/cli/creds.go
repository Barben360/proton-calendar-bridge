package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// maxPasswordFD bounds what --password-fd reads.
const maxPasswordFD = 4096

func mustAct(msg, hint string) error {
	return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New(msg), Hint: hint}
}

// credentials are what a login needs. TOTP stays empty unless PCB_TOTP is
// set, and the mailbox password unless --mailbox-password-fd or
// PCB_MAILBOX_PASSWORD gives it: each is asked for only when the service
// wants it.
type credentials struct{ username, password, totp, mailbox string }

// credentialSource says where the secrets may come from: a flag value, a
// descriptor or the environment, then (interactive only) the terminal.
type credentialSource struct {
	username      string // --username
	passwordFD    int    // --password-fd, when passwordFDSet
	passwordFDSet bool
	mailboxFD     int // --mailbox-password-fd, when mailboxFDSet
	mailboxFDSet  bool
	interactive   bool
}

// credentials gathers the credentials: username --username > PCB_USERNAME >
// prompt; password --password-fd > PCB_PASSWORD > prompt without echo;
// mailbox password --mailbox-password-fd > PCB_MAILBOX_PASSWORD (else none).
// A missing one when not interactive is an exit 10 naming where to put it,
// before any service call; one descriptor for both passwords is a usage
// error, before anything is read.
func (a *app) credentials(ctx context.Context, src credentialSource) (credentials, error) {
	if src.passwordFDSet && src.mailboxFDSet && src.passwordFD == src.mailboxFD {
		return credentials{}, UsageError(fmt.Errorf("--password-fd and --mailbox-password-fd both name descriptor %d: give each password its own",
			src.passwordFD))
	}
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
	mb, err := a.mailboxPassword(src.mailboxFD, src.mailboxFDSet)
	if err != nil {
		return credentials{}, err
	}
	c.mailbox = mb
	return c, nil
}

// mailboxPassword is the mailbox password given ahead: read from descriptor
// fd when set (--mailbox-password-fd), else PCB_MAILBOX_PASSWORD; "" if
// neither gives one (the prompt asks for it when the service wants it).
func (a *app) mailboxPassword(fd int, set bool) (string, error) {
	if set {
		return readSecretFD(fd, mailboxFDSecret)
	}
	return a.Env("PCB_MAILBOX_PASSWORD"), nil
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

// fdSecret is a secret pcb reads from a descriptor: the flag that names
// the descriptor and what the secret is, as the messages say it.
type fdSecret struct{ flag, what string }

var (
	passwordFDSecret = fdSecret{flag: "--password-fd", what: "password"}
	mailboxFDSecret  = fdSecret{flag: "--mailbox-password-fd", what: "mailbox password"}
)

// readPasswordFD is readSecretFD for the login password (--password-fd).
func readPasswordFD(fd int) (string, error) { return readSecretFD(fd, passwordFDSecret) }

// readSecretFD reads the secret s from descriptor fd up to end of input (the
// writer must close its end), at most maxPasswordFD bytes, and closes it. A
// terminal is refused: type the secret at the prompt instead. Exactly one
// trailing "\n" or "\r\n" is stripped (what echo and password managers
// append); the rest, spaces included, is the secret. An empty one is a usage
// error.
func readSecretFD(fd int, s fdSecret) (string, error) {
	if fd < 0 {
		return "", UsageError(fmt.Errorf("%s needs a descriptor number", s.flag))
	}
	h, err := descriptor(fd, s)
	if err != nil {
		return "", err
	}
	if isTerminalFD(int(h)) {
		return "", &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("%s %d is a terminal", s.flag, fd), Hint: fdHint(s)}
	}
	f := os.NewFile(h, strings.TrimPrefix(s.flag, "--"))
	if f == nil {
		return "", UsageError(fmt.Errorf("%s %d is not a usable descriptor", s.flag, fd))
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxPasswordFD+1))
	if err != nil {
		return "", UsageError(fmt.Errorf("cannot read the %s from descriptor %d", s.what, fd))
	}
	defer clear(b)
	if len(b) > maxPasswordFD {
		return "", UsageError(fmt.Errorf("the %s on descriptor %d is longer than %d bytes", s.what, fd, maxPasswordFD))
	}
	if rest, ok := bytes.CutSuffix(b, []byte("\n")); ok {
		b = bytes.TrimSuffix(rest, []byte("\r"))
	}
	if len(b) == 0 {
		return "", UsageError(fmt.Errorf("the %s on descriptor %d is empty", s.what, fd))
	}
	return string(b), nil
}
