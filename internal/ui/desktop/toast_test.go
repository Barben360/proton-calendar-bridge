package desktop

import (
	"strconv"
	"strings"
	"testing"
)

// Fyne's Windows toast puts the title and the body, quoted by Go's %q,
// into a PowerShell double-quoted string: a line break showed as a literal
// \n (live 2026-11-15), and a double quote, a $ or a backtick would end or
// expand that string.
func TestToastText(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"not logged in to Proton\nrun pcb login", "not logged in to Proton — run pcb login"},
		{"line one\r\nline two", "line one — line two"},
		{"a\tb", "a b"},
		{`say "hi"`, "say 'hi'"},
		{"“smart” „low", "'smart' 'low"},
		{"$(whoami) and $env:USERNAME", "＄(whoami) and ＄env:USERNAME"},
		{"tick`n", "tick'n"},
		{`C:\Users\Zoë O'Brien\pcb`, `C:＼Users＼Zoë O'Brien＼pcb`},
		{"zero\u200bwidth", "zero width"},
		{"  edge\n", "edge"},
		{"user@example.invalid", "user@example.invalid"},
	} {
		if got := toastText(tt.in); got != tt.want {
			t.Errorf("toastText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Whatever the text, %q adds no escape to it and PowerShell finds nothing
// in it that ends or expands a double-quoted string.
func TestToastTextIsInertInFynesTemplate(t *testing.T) {
	for _, in := range []string{
		"a\"; Remove-Item x; \"", "$(Get-Process)", "`$x", "\x00\x07\x1b[31m", "\\\"", "“x”", "a\u2028b\u00a0c",
		"not logged in to Proton\nrun pcb login",
	} {
		out := toastText(in)
		if q := strconv.Quote(out); q[1:len(q)-1] != out {
			t.Errorf("toastText(%q) = %q: %%q escapes it as %s", in, out, q)
		}
		if strings.ContainsAny(out, "\"`$\\“”„") {
			t.Errorf("toastText(%q) = %q keeps a character PowerShell reads in a double-quoted string", in, out)
		}
	}
}
