package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Terminal asks the person questions. Prompts go to stderr, so stdout stays
// the command's result. A read ends with ctx.Err() when ctx is cancelled
// (Ctrl-C), so a prompt never outlives its command.
type Terminal interface {
	// Interactive reports whether a person can answer (stdin is a terminal).
	Interactive() bool
	// ReadLine shows prompt and returns the answer without its line end.
	ReadLine(ctx context.Context, prompt string) (string, error)
	// ReadSecret is ReadLine without echoing the answer.
	ReadSecret(ctx context.Context, prompt string) (string, error)
}

type osTerminal struct {
	stdin *os.File
	in    *bufio.Reader
	out   io.Writer
}

func newOSTerminal(stderr io.Writer) Terminal {
	return &osTerminal{stdin: os.Stdin, in: bufio.NewReader(os.Stdin), out: stderr}
}

func (t *osTerminal) Interactive() bool { return term.IsTerminal(int(t.stdin.Fd())) }

// read runs f on its own goroutine and returns its result, or ctx.Err() when
// ctx ends first. The goroutine stays blocked in the read until the process
// exits, which is right after a cancelled command.
func read(ctx context.Context, f func() (string, error)) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := f()
		ch <- result{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (t *osTerminal) ReadLine(ctx context.Context, prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	return read(ctx, func() (string, error) {
		line, err := t.in.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	})
}

func (t *osTerminal) ReadSecret(ctx context.Context, prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	fd := int(t.stdin.Fd())
	state, stateErr := term.GetState(fd)
	s, err := read(ctx, func() (string, error) {
		b, err := term.ReadPassword(fd)
		return string(b), err
	})
	if ctx.Err() != nil && stateErr == nil {
		_ = term.Restore(fd, state) // the interrupted read left echo off
	}
	fmt.Fprintln(t.out)
	return s, err
}
