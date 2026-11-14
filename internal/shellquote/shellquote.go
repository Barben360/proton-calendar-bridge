// Package shellquote quotes words for display as a command line: POSIX shell
// on Unix, PowerShell on Windows.
package shellquote

import (
	"regexp"
	"strings"
)

var (
	posixSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./{}-]+$`)
	psSafe    = regexp.MustCompile(`^[A-Za-z0-9_./\\:=+%-]+$`)
	// PowerShell ends a '...' string on any of these quote characters, so
	// each is doubled inside one.
	psQuotes = strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")
)

// POSIX returns s as one POSIX shell word: unchanged when it is plain, else
// in single quotes.
func POSIX(s string) string {
	if posixSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PowerShell returns s as one PowerShell word: unchanged when it is plain,
// else in single quotes.
func PowerShell(s string) string {
	if psSafe.MatchString(s) {
		return s
	}
	return "'" + psQuotes.Replace(s) + "'"
}
