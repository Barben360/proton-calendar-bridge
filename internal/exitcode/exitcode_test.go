package exitcode_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

func TestExitCodeFrom(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want exitcode.Code
	}{
		{"nil", nil, exitcode.OK},
		{"typed", &exitcode.Error{Code: exitcode.MustAct}, exitcode.MustAct},
		{"wrapped", fmt.Errorf("x: %w", &exitcode.Error{Code: exitcode.Partial}), exitcode.Partial},
		{"plain", errors.New("boom"), exitcode.Blocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitcode.From(tt.err); got != tt.want {
				t.Fatalf("From(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorUnwrapsAndMessages(t *testing.T) {
	inner := errors.New("inner")
	err := &exitcode.Error{Code: exitcode.Usage, Err: inner}
	if !errors.Is(err, inner) {
		t.Fatal("Error does not unwrap to its cause")
	}
	if err.Error() != "inner" {
		t.Fatalf("Error() = %q, want %q", err.Error(), "inner")
	}
}

func TestHintOf(t *testing.T) {
	wrapped := fmt.Errorf("ctx: %w", &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("x"), Hint: "run it"})
	if got := exitcode.HintOf(wrapped); got != "run it" {
		t.Errorf("HintOf(wrapped) = %q", got)
	}
	if got := exitcode.HintOf(errors.New("plain")); got != "" {
		t.Errorf("HintOf(plain) = %q", got)
	}
	if got := exitcode.HintOf(nil); got != "" {
		t.Errorf("HintOf(nil) = %q", got)
	}
}
