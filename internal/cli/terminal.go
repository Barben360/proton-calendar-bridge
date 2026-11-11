package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Terminal asks the person questions. Prompts go to stderr, so stdout stays
// the command's result.
type Terminal interface {
	// Interactive reports whether a person can answer (stdin is a terminal).
	Interactive() bool
	// ReadLine shows prompt and returns the answer without its line end.
	ReadLine(prompt string) (string, error)
	// ReadSecret is ReadLine without echoing the answer.
	ReadSecret(prompt string) (string, error)
}

type osTerminal struct {
	in  *bufio.Reader
	out io.Writer
}

func newOSTerminal(stderr io.Writer) Terminal {
	return &osTerminal{in: bufio.NewReader(os.Stdin), out: stderr}
}

func (t *osTerminal) Interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func (t *osTerminal) ReadLine(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	line, err := t.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (t *osTerminal) ReadSecret(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(t.out)
	return string(b), err
}
