package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// MinLoginInterval is the shortest time between two lab logins (SPEC §2:
// good citizen towards Proton).
const MinLoginInterval = 60 * time.Second

const lastLoginFile = "last-login"

// checkGuard refuses when the previous login attempt is under
// MinLoginInterval old. An unreadable or malformed stamp is refused too:
// failing closed costs a manual delete, failing open costs a login storm.
func checkGuard(dir string, now time.Time) error {
	b, err := os.ReadFile(filepath.Join(dir, lastLoginFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var last time.Time
	if err == nil {
		last, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	}
	if err != nil {
		return &exitcode.Error{
			Code: exitcode.Blocked,
			Err:  fmt.Errorf("cannot read the last login time in %s", lastLoginFile),
			Hint: "wait a minute, then delete " + filepath.Join(dir, lastLoginFile),
		}
	}
	if wait := MinLoginInterval - now.Sub(last); wait > 0 {
		return &exitcode.Error{
			Code: exitcode.Blocked,
			Err:  fmt.Errorf("previous lab login was %s ago; at most one login per %s", now.Sub(last).Round(time.Second), MinLoginInterval),
			Hint: fmt.Sprintf("retry in %s", wait.Round(time.Second)),
		}
	}
	return nil
}

// stampLogin records an attempt before it is made, so that a crash, a
// CAPTCHA or a failure still counts towards the window.
func stampLogin(dir string, now time.Time) error {
	return writeFile(filepath.Join(dir, lastLoginFile), []byte(now.UTC().Format(time.RFC3339Nano)))
}
