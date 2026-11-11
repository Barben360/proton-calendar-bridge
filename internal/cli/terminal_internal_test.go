package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
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

func TestPasswordFDRefusesATerminal(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = wr.Close() // end of input, so a read can never hang
	t.Cleanup(func() { _ = rd.Close() })
	old := isTerminalFD
	isTerminalFD = func(int) bool { return true }
	t.Cleanup(func() { isTerminalFD = old })
	_, err = readPasswordFD(int(rd.Fd()))
	if code := exitcodeOf(err); code != 2 {
		t.Fatalf("err = %v (exit %d), want a usage error", err, code)
	}
	if hintOf(err) == "" {
		t.Error("no hint")
	}
}

func exitcodeOf(err error) int { return int(exitcode.From(err)) }
func hintOf(err error) string  { return exitcode.HintOf(err) }
