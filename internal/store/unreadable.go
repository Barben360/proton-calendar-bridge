package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
)

// StateUnreadable reports whether err from LoadState is an OS failure to read
// an existing state.json (permission denied, I/O error) rather than a parse
// or version refusal, and the path that failed. Such a file may be intact: it
// is never a reason to reset the login.
func StateUnreadable(err error) (path string, ok bool) {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Path, true
	}
	return "", false
}

// Unreadable describes path for a "cannot be read by this user" message:
// "owner NAME, mode 0000" when stat works, "" otherwise.
func Unreadable(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	owner := "unknown"
	if uid, ok := fileOwner(path); ok {
		owner = "#" + strconv.Itoa(uid)
		if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
			owner = u.Username
		}
	}
	return fmt.Sprintf("owner %s, mode %04o", owner, info.Mode().Perm())
}

// CurrentUser names the user pcb runs as.
func CurrentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "the user pcb runs as"
}

// UnreadableStateMessage is the text for a state.json that exists but this
// user cannot read: it may be intact, so no reset is suggested.
func UnreadableStateMessage(path string) string {
	msg := "the login state file " + path + " cannot be read by this user"
	if d := Unreadable(path); d != "" {
		msg += " (" + d + ")"
	}
	return msg
}

// UnreadableStateFix is the hint that goes with UnreadableStateMessage.
func UnreadableStateFix() string {
	return "make it readable by " + CurrentUser() + " (or run pcb as its owner)"
}
