// Package loginguard keeps pcb a good citizen towards Proton (SPEC §2): at
// most one login attempt per interval per state directory, checked and
// stamped under a lock before any network call.
package loginguard

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// MinInterval is the shortest time between two login attempts.
const MinInterval = 60 * time.Second

// maxSkew is how far ahead of now a stamp may be and still count: beyond,
// the clock was stepped back (VM restore, NTP step, an RTC at boot) and the
// stamp is stale.
const maxSkew = 5 * time.Minute

const (
	lockFile  = "login.lock"
	stampFile = "last-login"
)

// TooSoon is the cause of a refusal because the previous attempt is too
// recent. Wait is how long to wait.
type TooSoon struct{ Wait time.Duration }

func (e *TooSoon) Error() string {
	return fmt.Sprintf("the previous login attempt was too recent; wait %s", e.Wait.Round(time.Second))
}

// WaitFor returns the wait of a TooSoon in err's chain.
func WaitFor(err error) (time.Duration, bool) {
	var t *TooSoon
	if errors.As(err, &t) {
		return t.Wait, true
	}
	return 0, false
}

// Acquire takes the login lock in dir, checks the last attempt against min
// and stamps now. The caller makes its network call, then calls release;
// holding the lock for the whole attempt keeps concurrent logins out. Every
// refusal is a Blocked *exitcode.Error: another login holds the lock, the
// stamp is malformed (fail closed: a manual delete costs less than a login
// storm), or the previous attempt is younger than min (the stamp is then
// left untouched). A stamp more than maxSkew ahead of now never blocks: it
// is logged to log (nil discards it) and replaced by now, so it is logged
// once.
func Acquire(dir string, min time.Duration, now time.Time, log *slog.Logger) (release func(), err error) {
	unlock, ok, err := filelock.TryLock(filepath.Join(dir, lockFile))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("another login is running"), Hint: "wait for it to finish"}
	}
	if err := check(dir, min, now, log); err != nil {
		unlock()
		return nil, err
	}
	// Stamped before the attempt, so a crash, a CAPTCHA or a failure still
	// counts towards the window.
	if err := atomicfile.Write(filepath.Join(dir, stampFile), []byte(now.UTC().Format(time.RFC3339Nano))); err != nil {
		unlock()
		return nil, fmt.Errorf("recording the login time: %w", err)
	}
	return unlock, nil
}

func check(dir string, min time.Duration, now time.Time, log *slog.Logger) error {
	path := filepath.Join(dir, stampFile)
	b, err := os.ReadFile(path)
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
			Err:  fmt.Errorf("cannot read the last login time in %s", stampFile),
			Hint: "wait a minute, then delete " + path,
		}
	}
	if last.After(now.Add(maxSkew)) {
		if log != nil {
			log.Warn("the last login time is in the future: the clock was set back; ignoring it",
				"file", path, "last_login", last.UTC(), "now", now.UTC())
		}
		return nil
	}
	if wait := min - now.Sub(last); wait > 0 {
		return &exitcode.Error{
			Code: exitcode.Blocked,
			Err:  &TooSoon{Wait: wait},
			Hint: fmt.Sprintf("retry in %s", wait.Round(time.Second)),
		}
	}
	return nil
}
