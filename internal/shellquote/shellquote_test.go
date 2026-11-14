package shellquote_test

import (
	"runtime"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

func TestPOSIX(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/.config/pcb": "/home/u/.config/pcb",
		"/home/my user/pcb":   `'/home/my user/pcb'`,
		"it's":                `'it'\''s'`,
		"":                    "''",
		"a$b":                 "'a$b'",
	} {
		if got := shellquote.POSIX(in); got != want {
			t.Errorf("POSIX(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPowerShell(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\u\AppData\Local\pcb`: `C:\Users\u\AppData\Local\pcb`,
		`C:\Users\my user\pcb`:         `'C:\Users\my user\pcb'`,
		`it's`:                         `'it''s'`,
		"it\u2019s":                    "'it\u2019\u2019s'", // PowerShell ends a '...' string on these too
		"it\u2018s\u201a\u201b":        "'it\u2018\u2018s\u201a\u201a\u201b\u201b'",
		"":                             `''`,
		`a$b`:                          `'a$b'`, `a@b`: `'a@b'`, `{x}`: `'{x}'`, `a,b`: `'a,b'`, `a;b`: `'a;b'`, `(x)`: `'(x)'`,
		"a`b": "'a`b'", `*S-1-1-0`: `'*S-1-1-0'`, `Zoë`: `'Zoë'`,
	} {
		if got := shellquote.PowerShell(in); got != want {
			t.Errorf("PowerShell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteIsTheOSQuoting(t *testing.T) {
	want := shellquote.POSIX("a b")
	if runtime.GOOS == "windows" {
		want = shellquote.PowerShell("a b")
	}
	if got := shellquote.Quote("a b"); got != want {
		t.Errorf("Quote(a b) = %q, want %q", got, want)
	}
}
