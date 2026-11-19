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

// CaptchaRetryInterval is the shortest time before a login attempt that
// carries a solved CAPTCHA token. A person solved the challenge in between,
// so the retry is not a login storm; the floor still bounds a script that
// replays a token in a loop.
const CaptchaRetryInterval = 10 * time.Second

// maxSkew is how far ahead of now a stamp may be and still count: beyond,
// the clock was stepped back (VM restore, NTP step, an RTC at boot) and the
// stamp is stale.
const maxSkew = 5 * time.Minute

// MaxDefer is the longest Retry-After Defer records, as ParseRetryAfter
// clamps a header to an hour.
const MaxDefer = time.Hour

const (
	lockFile      = "login.lock"
	stampFile     = "last-login"
	notBeforeFile = "not-before"
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
// left untouched; so is it while a Retry-After recorded by Defer runs). A stamp more than maxSkew ahead of now never blocks: it
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

// Defer records that Proton asked, at now, to wait before the next login
// (a 429's Retry-After): no attempt is allowed before now+wait, whatever the
// spacing. It outlives the process, is clamped to MaxDefer and never
// shortens a wait already recorded. Call it with the lock Acquire returned
// still held.
func Defer(dir string, now time.Time, wait time.Duration) error {
	until := now.Add(min(wait, MaxDefer))
	path := filepath.Join(dir, notBeforeFile)
	if b, err := atomicfile.ReadFile(path); err == nil {
		if cur, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil && cur.After(until) && !cur.After(now.Add(MaxDefer)) {
			return nil
		}
	}
	if err := atomicfile.Write(path, []byte(until.UTC().Format(time.RFC3339Nano))); err != nil {
		return fmt.Errorf("recording the login wait: %w", err)
	}
	return nil
}

// DeferLocked is Defer for a caller that does not hold the lock: it takes it
// for the write, and records nothing (an error) when another login holds it.
func DeferLocked(dir string, now time.Time, wait time.Duration) error {
	unlock, ok, err := filelock.TryLock(filepath.Join(dir, lockFile))
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("another login is running")
	}
	defer unlock()
	return Defer(dir, now, wait)
}

// notBefore is how long Proton's last Retry-After still asks to wait at
// now. A recorded time more than MaxDefer plus maxSkew ahead is from a
// clock that was stepped back: ignored and logged.
func notBefore(dir string, now time.Time, log *slog.Logger) (time.Duration, error) {
	path := filepath.Join(dir, notBeforeFile)
	b, err := atomicfile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	var until time.Time
	if err == nil {
		until, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	}
	if err != nil {
		return 0, &exitcode.Error{
			Code: exitcode.Blocked,
			Err:  fmt.Errorf("cannot read the login wait in %s", notBeforeFile),
			Hint: "wait a few minutes, then delete " + path,
		}
	}
	if until.After(now.Add(MaxDefer + maxSkew)) {
		if log != nil {
			log.Warn("the recorded login wait is in the far future: the clock was set back; ignoring it",
				"file", path, "not_before", until.UTC(), "now", now.UTC())
		}
		return 0, nil
	}
	return max(until.Sub(now), 0), nil
}

func check(dir string, min time.Duration, now time.Time, log *slog.Logger) error {
	deferred, err := notBefore(dir, now, log)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, stampFile)
	b, err := atomicfile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return tooSoon(deferred)
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
		return tooSoon(deferred)
	}
	return tooSoon(max(min-now.Sub(last), deferred))
}

// tooSoon is the refusal for a wait still to run, nil for none.
func tooSoon(wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	return &exitcode.Error{
		Code: exitcode.Blocked,
		Err:  &TooSoon{Wait: wait},
		Hint: fmt.Sprintf("retry in %s", wait.Round(time.Second)),
	}
}
