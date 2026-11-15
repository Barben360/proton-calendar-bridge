package desktop

import (
	"strconv"
	"strings"
)

// toastText is a notification's title or body as Fyne's Windows toast can
// carry it. Fyne writes each, quoted by Go's %q, into a PowerShell
// double-quoted string: Go's escapes are literal text there (a line break
// showed as \n, live 2026-11-15), and a double quote (PowerShell also reads
// “ ” „ as one), a $ or a backtick would end or expand the string. So a
// line break becomes " — " (as the window shows an alert), any other
// character %q would escape a space, a double quote or a backtick a single
// quote, and $ and \ their fullwidth forms. Untagged, so Linux tests it.
func toastText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == '\n':
			b.WriteString(" — ")
		case r == '"', r == '“', r == '”', r == '„', r == '`':
			b.WriteRune('\'')
		case r == '$':
			b.WriteRune('＄')
		case r == '\\':
			b.WriteRune('＼')
		case !strconv.IsPrint(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
