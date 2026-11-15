package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// A read on a stdin nobody writes to ends when its context does.
func TestOSTerminalReadLineEndsWithContext(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wr.Close(); _ = rd.Close() })
	term := &osTerminal{stdin: rd, in: bufio.NewReader(rd), out: io.Discard}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := term.ReadLine(ctx, "? "); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the context's", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadLine did not return after its context ended")
	}
}

// A read whose context already ended never starts: a no-echo password read
// begun after Ctrl-C would outlive the restore and leave echo off.
func TestReadWithEndedContextNeverStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 20 {
		started := make(chan struct{})
		_, err := read(ctx, func() (string, error) {
			close(started)
			return "answer", nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the context's", err)
		}
		select {
		case <-started:
			t.Fatal("the read started although its context had ended")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestOSTerminalReadLineAnswers(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	if _, err := wr.WriteString("alice\r\nrest"); err != nil {
		t.Fatal(err)
	}
	_ = wr.Close()
	term := &osTerminal{stdin: rd, in: bufio.NewReader(rd), out: io.Discard}
	if got, err := term.ReadLine(t.Context(), "? "); err != nil || got != "alice" {
		t.Errorf("ReadLine = %q, %v", got, err)
	}
	if got, err := term.ReadLine(t.Context(), "? "); err != nil || got != "rest" {
		t.Errorf("last line without a newline = %q, %v", got, err)
	}
	if _, err := term.ReadLine(t.Context(), "? "); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want EOF", err)
	}
}

// ux Minor: Ctrl-D at a prompt ends the prompt's line, so "Error: ..."
// does not follow the prompt on the same line; an answer adds nothing.
func TestOSTerminalEndOfInputEndsThePromptLine(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	if _, err := wr.WriteString("alice\n"); err != nil {
		t.Fatal(err)
	}
	_ = wr.Close()
	var out strings.Builder
	term := &osTerminal{stdin: rd, in: bufio.NewReader(rd), out: &out}
	if got, err := term.ReadLine(t.Context(), "Proton username: "); err != nil || got != "alice" {
		t.Fatalf("ReadLine = %q, %v", got, err)
	}
	if _, err := term.ReadLine(t.Context(), "Proton username: "); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
	if got, want := out.String(), "Proton username: Proton username: \n"; got != want {
		t.Errorf("prompts = %q, want %q", got, want)
	}
}

// The refusal comes before any read, so descriptor 0 is safe here. Its hint
// shows a redirection the platform has: 3<file is a POSIX shell's, and
// Windows takes only 0, 1 or 2.
func TestPasswordFDRefusesATerminal(t *testing.T) {
	old := isTerminalFD
	isTerminalFD = func(int) bool { return true }
	t.Cleanup(func() { isTerminalFD = old })
	_, err := readPasswordFD(0)
	if code := exitcodeOf(err); code != 2 {
		t.Fatalf("err = %v (exit %d), want a usage error", err, code)
	}
	want, not := "--password-fd 3 3<", "--password-fd 0 <"
	if runtime.GOOS == "windows" {
		want, not = not, want
	}
	if h := hintOf(err); !strings.Contains(h, want) || strings.Contains(h, not) {
		t.Errorf("hint %q, want %q and not %q", h, want, not)
	}
}

func exitcodeOf(err error) int { return int(exitcode.From(err)) }
func hintOf(err error) string  { return exitcode.HintOf(err) }
