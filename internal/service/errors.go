package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

// This file is the only place the service builds *exitcode.Error. Failures
// are classified by type (errors.Is and errors.As), never by message text
// (R23a), and the messages are written here: the text of an unknown error is
// dropped, since nothing vouches that it is secret-free.

func fail(code exitcode.Code, msg, hint string) *exitcode.Error {
	return &exitcode.Error{Code: code, Err: errors.New(msg), Hint: hint}
}

// detail is the cause of an *exitcode.Error built here when a watch needs
// more than its code: how long Proton asked to wait, or that no retry can
// help. Its text is the message.
type detail struct {
	msg        string
	limited    bool          // Proton is rate limiting (a 429): the hint is a one-shot "retry in X"
	retryAfter time.Duration // Proton's Retry-After, 0 if none
	final      bool          // a retry cannot help: a person must change something
}

func (d *detail) Error() string { return d.msg }

// final is a Blocked error that no retry can fix.
func final(msg, hint string) *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: &detail{msg: msg, final: true}, Hint: hint}
}

// stopsWatch reports whether a watch must stop on err: a person must act
// (MustAct), or the failure is final (a newer cache, an unsupported system).
func stopsWatch(err error) bool {
	var (
		ee *exitcode.Error
		d  *detail
	)
	return (errors.As(err, &ee) && ee.Code == exitcode.MustAct) || (errors.As(err, &d) && d.final)
}

// retryAfter is how long err asks a watch to wait before the next pass.
func retryAfter(err error) time.Duration {
	var d *detail
	if errors.As(err, &d) {
		return d.retryAfter
	}
	return 0
}

// ErrInvalidSyncToken is the cause of the error Changes returns for a sync
// token it cannot answer from (another cache life, a position the calendar
// never reached, or not a token of ours): the client must list the calendar
// in full (CalDAV: 403 valid-sync-token).
var ErrInvalidSyncToken = errors.New("the sync token is not valid for this calendar")

// ErrNoSuchCalendar is the cause of the error a read returns for a calendar
// the cache does not hold or never synced (CalDAV: 404).
var ErrNoSuchCalendar = errors.New("no such calendar in the event cache")

func invalidSyncToken() *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Usage, Err: ErrInvalidSyncToken, Hint: "list the calendar in full (an empty sync token)"}
}

func noSuchCalendar() *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Usage, Err: ErrNoSuchCalendar, Hint: "pcb calendar list shows your calendars; one added since the last sync needs pcb sync first"}
}

func usage(msg, hint string) *exitcode.Error   { return fail(exitcode.Usage, msg, hint) }
func mustAct(msg, hint string) *exitcode.Error { return fail(exitcode.MustAct, msg, hint) }
func blocked(msg, hint string) *exitcode.Error { return fail(exitcode.Blocked, msg, hint) }

// mapError turns any failure into an *exitcode.Error with a hint. keyFile is
// named when the sealed store cannot be opened.
func mapError(err error, keyFile string) error {
	var (
		ee  *exitcode.Error
		ce  *auth.CaptchaError
		uve *auth.UnsupportedVerificationError
		ae  *api.Error
		ts  *loginguard.TooSoon
		ne  net.Error
		ue  *url.Error
	)
	cf := cacheFailure(err)
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee):
		if ee.Hint == "" {
			return &exitcode.Error{Code: ee.Code, Err: ee.Err, Hint: "run pcb doctor to see what is wrong"}
		}
		return ee
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return blocked("the operation was cancelled or timed out", "run the command again")
	case cf != nil:
		return cf
	case errors.Is(err, auth.ErrBadCredentials):
		return mustAct("wrong Proton username or password", "check them, then run pcb login again (Proton allows one attempt per minute)")
	case errors.Is(err, auth.ErrTOTPRejected):
		return mustAct("the 2FA code was rejected by Proton", "run pcb login again with a fresh code from your authenticator")
	case errors.Is(err, auth.ErrCaptchaRejected):
		return mustAct("the CAPTCHA was not accepted by Proton", "run pcb login again to get a new CAPTCHA, solve it and pass its token")
	case errors.As(err, &ce):
		return mustAct("Proton asks for a CAPTCHA", "solve it at "+ce.URL+", then run pcb login again with its token")
	case errors.Is(err, auth.ErrTwoPassword):
		return mustAct("the Proton account uses two-password mode, which is not supported",
			"switch the account to single-password mode in the Proton account settings, then run pcb login")
	case errors.Is(err, auth.ErrFIDO2):
		return mustAct("the account's only second factor is a security key (FIDO2), which is not supported",
			"add an authenticator app (TOTP) as a second factor in the Proton account settings, then run pcb login")
	case errors.Is(err, keys.ErrKeysLocked):
		return mustAct("the password does not unlock the account keys", "check the password; accounts in two-password mode are not supported")
	case errors.Is(err, api.ErrSessionEnded):
		return mustAct("the Proton session has ended", "run pcb login")
	case errors.As(err, &uve):
		return blocked(uve.Error(), "complete the verification once in a browser on this network, then retry later")
	case errors.Is(err, auth.ErrServerProof):
		return blocked("Proton's answer could not be authenticated", "retry later; if it persists, check for a proxy that rewrites traffic")
	case errors.Is(err, seal.ErrNoKey):
		return blocked("the sealing key file "+keyFile+" is missing", "restore it from a backup, or run pcb logout to discard the stored session and log in again")
	case errors.Is(err, seal.ErrCorrupt):
		return blocked("the stored secrets cannot be opened with the key file "+keyFile, "restore both from a backup, or run pcb logout to discard the stored session and log in again")
	case errors.As(err, &ts):
		return blocked(ts.Error(), "retry in "+ts.Wait.Round(time.Second).String())
	case errors.As(err, &ae):
		return protonDown(ae)
	case errors.As(err, &ne), errors.As(err, &ue):
		return blocked("cannot reach Proton", "check the network connection, then retry")
	}
	return blocked("unexpected error", "run pcb doctor; if it persists, report it without any secret")
}

// protonDown maps an API answer that is not a credential problem: Proton
// unavailable or rate limiting, or a refusal pcb cannot act on.
func protonDown(e *api.Error) error {
	switch {
	case e.Status == 429:
		wait := "a few minutes"
		if e.RetryAfter > 0 {
			wait = e.RetryAfter.Round(time.Second).String()
		}
		return &exitcode.Error{Code: exitcode.Blocked, Err: &detail{msg: "Proton is rate limiting requests", limited: true, retryAfter: e.RetryAfter},
			Hint: "retry in " + wait}
	case e.Status >= 500:
		return blocked("Proton is unavailable", "try again later")
	}
	return blocked(fmt.Sprintf("Proton refused the request (code %d)", e.Code), "try again later; if it persists, run pcb doctor")
}

// storeError maps a failure of the sealed store: a lost or foreign key names
// the key file; anything else is a store that cannot be used.
func storeError(err error, keyFile string) error {
	if errors.Is(err, seal.ErrNoKey) || errors.Is(err, seal.ErrCorrupt) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return mapError(err, keyFile)
	}
	var ee *exitcode.Error
	if errors.As(err, &ee) {
		return mapError(err, keyFile)
	}
	return blocked("cannot use the stored secrets", "check the permissions of the state directory, or run pcb logout to discard the stored session")
}

// stateError maps an unreadable state.json (it fails closed).
func stateError(err error) error {
	if errors.Is(err, context.Canceled) {
		return mapError(err, "")
	}
	return blocked("the login state file cannot be read", "run pcb logout to reset it")
}

// acquireError maps what loginguard.Acquire returns: its own refusals are
// already *exitcode.Error; the raw rest (no file locks off Unix, an
// unwritable directory) becomes Blocked.
func acquireError(err error, stateDir string) error {
	var ee *exitcode.Error
	switch {
	case errors.As(err, &ee):
		return err
	case errors.Is(err, filelock.ErrUnsupported):
		return final("login locking needs a Unix system", "run pcb on Linux or macOS")
	}
	return blocked("cannot take the login lock", "make sure "+stateDir+" is writable by this user")
}

// keysError is the secret-free reason a calendar's keys did not unlock:
// Proton refusing the calendar (a 4xx; transient failures stop the whole
// listing, see api.SessionWide) says so; anything else is the keys themselves.
func keysError(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return protonDown(ae).Error()
	}
	return "the calendar keys do not unlock with the account keys"
}

// cacheFailure classifies a failure of the event cache by type, or returns
// nil when err is not one: a newer schema needs a newer pcb; a file that is
// not a database needs a person to remove it (MustAct, so a watch stops);
// a file-system error of the cache files (cache.IOError: permissions,
// disk; other files' errors are not matched) is Blocked with the operating system's
// reason and no advice to remove anything; any other failure of the cache
// during a pass (*syncer.StoreError) is Blocked.
func cacheFailure(err error) *exitcode.Error {
	var (
		ioe *cache.IOError
		se  *syncer.StoreError
	)
	switch {
	case errors.Is(err, cache.ErrChanged):
		return fail(exitcode.Blocked, "the event cache changed while reading", "run the command again")
	case errors.Is(err, cache.ErrNewerSchema):
		return final("the event cache was written by a newer pcb", "upgrade pcb (or remove pcb.db from the state directory and run pcb sync --full)")
	case errors.Is(err, cache.ErrCorrupt):
		return fail(exitcode.MustAct, "the event cache is damaged", "remove pcb.db (and its -wal and -shm files) from the state directory, then run pcb sync --full")
	case errors.As(err, &ioe):
		return fail(exitcode.Blocked, "the event cache cannot be used: "+ioe.Reason(), "check the permissions and the free space of the state directory")
	case errors.As(err, &se):
		return fail(exitcode.Blocked, "the event cache could not be updated", "run pcb doctor to check it, then pcb sync again")
	}
	return nil
}
