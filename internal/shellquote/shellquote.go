// Package shellquote quotes words for display as a POSIX shell command line.
package shellquote

import (
	"regexp"
	"strings"
)

var safe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./{}-]+$`)

// Quote returns s as one shell word: unchanged when it is plain, else in
// single quotes.
func Quote(s string) string {
	if safe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
