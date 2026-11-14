package shellquote_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/.config/pcb": "/home/u/.config/pcb",
		"/home/my user/pcb":   `'/home/my user/pcb'`,
		"it's":                `'it'\''s'`,
		"":                    "''",
		"a$b":                 "'a$b'",
	} {
		if got := shellquote.Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}
