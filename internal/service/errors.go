package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
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
	class      string        // the error class Health reports (errorClass), "" when the code says enough
	login      bool          // a MustAct a login fixes (loginFixes)
	locked     bool          // the desktop keyring is locked (keyringLocked)
	switched   bool          // another account is logged in than Serve serves (accountSwitched)
}

func (d *detail) Error() string { return d.msg }

// classed is a Blocked error of a known error class (errorClass).
func classed(class, msg, hint string) *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: &detail{msg: msg, class: class}, Hint: hint}
}

// withClass is ee marked with class, its code, message, hint and
// finality kept.
func withClass(ee *exitcode.Error, class string) *exitcode.Error {
	d := detail{msg: ee.Err.Error()}
	var old *detail
	if errors.As(ee.Err, &old) {
		d = *old
	}
	d.class = class
	return &exitcode.Error{Code: ee.Code, Err: &d, Hint: ee.Hint}
}

// loginNeeded is a MustAct that a login fixes (ruling M5-R8): not logged
// in, the session ended, the login marked needs-login or needs-captcha, the
// sealed secrets rolled back.
func loginNeeded(msg, hint string) *exitcode.Error {
	return &exitcode.Error{Code: exitcode.MustAct, Err: &detail{msg: msg, login: true}, Hint: hint}
}

// asLoginNeeded is ee, a MustAct, marked as one a login fixes, its message,
// hint and class kept.
func asLoginNeeded(ee *exitcode.Error) *exitcode.Error {
	d := detail{msg: ee.Err.Error()}
	var old *detail
	if errors.As(ee.Err, &old) {
		d = *old
	}
	d.login = true
	return &exitcode.Error{Code: ee.Code, Err: &d, Hint: ee.Hint}
}

// keyringFix is what a person does about a locked desktop keyring; the
// alert's fix says the same.
const keyringFix = "unlock it (log in to the desktop session), pcb retries every 30 s"

// keyringLockedError is the Blocked error of a locked desktop keyring: no
// retry of the same call fixes it, but the desktop session unlocking does.
func keyringLockedError() *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: &detail{msg: "the desktop keyring is locked", locked: true}, Hint: keyringFix}
}

// keyringLocked reports whether err is keyringLockedError's, by type.
func keyringLocked(err error) bool {
	var d *detail
	return errors.As(err, &d) && d.locked
}

// loginFixes reports whether err is a MustAct a login fixes: Serve with
// WaitForLogin waits for one after it; any other stop ends Serve (ruling
// M5-R8). It reads the type only, never the text.
func loginFixes(err error) bool {
	var d *detail
	return exitcode.From(err) == exitcode.MustAct && errors.As(err, &d) && d.login
}

// switchedAccount is the stop of a pass on Serve's cache once another
// process logged in to another account: final without WaitForLogin (exit
// 20, restart pcb serve); with it, Serve waits and serves the new account
// (ruling M5-R10).
func switchedAccount() *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Blocked,
		Err:  &detail{msg: "pcb is now logged in to another Proton account than pcb serve started with", final: true, switched: true},
		Hint: "restart pcb serve"}
}

// accountSwitched reports whether err is switchedAccount's, by type.
func accountSwitched(err error) bool {
	var d *detail
	return errors.As(err, &d) && d.switched
}

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

// stopsServe is stopsWatch plus a locked desktop keyring: Serve's watch
// ends on it and Serve waits for the keyring (keyringRetry). pcb sync
// --watch keeps its back-off instead.
func stopsServe(err error) bool { return stopsWatch(err) || keyringLocked(err) }

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

// ErrCalendarPending is the cause of the error a read returns for a
// calendar the cache knows but has not synced yet (ruling M3-R25): it will
// answer once a pass reaches it (CalDAV: 503, never a 4xx that would disable
// the calendar in Thunderbird).
var ErrCalendarPending = errors.New("the calendar is not synced yet")

// ErrWaitingForLogin is the cause of the error a read or a write returns
// while Serve waits for a login (ServeOptions.WaitForLogin): it answers once
// a login is ready (CalDAV: 503, never a 4xx).
var ErrWaitingForLogin = errors.New("pcb serve is waiting for a login")

func invalidSyncToken() *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Usage, Err: ErrInvalidSyncToken, Hint: "list the calendar in full (an empty sync token)"}
}

func calendarPending() *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: ErrCalendarPending, Hint: "wait for the sync pass to reach it, or run pcb sync"}
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
		rb  *store.RolledBackError
		use *store.UnsupportedError
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
		return withClass(cf, classCache)
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
		return loginNeeded("the Proton session has ended", "run pcb login")
	case errors.As(err, &uve):
		return blocked(uve.Error(), "complete the verification once in a browser on this network, then retry later")
	case errors.Is(err, auth.ErrServerProof):
		return blocked("Proton's answer could not be authenticated", "retry later; if it persists, check for a proxy that rewrites traffic")
	case errors.As(err, &rb):
		return rolledBack(rb)
	case errors.As(err, &use):
		return unsupportedStore(use)
	case errors.Is(err, seal.ErrLocked):
		return keyringLockedError()
	case errors.Is(err, seal.ErrNoKey) && keyFile == keyringRef:
		return mustAct("the sealing key is not in the desktop keyring", "pcb logout then pcb login")
	case errors.Is(err, seal.ErrCorrupt) && keyFile == keyringRef:
		return mustAct("the stored secrets cannot be opened with the key in the desktop keyring", "pcb logout then pcb login")
	case errors.Is(err, seal.ErrNoKey):
		return mustAct("the sealing key file "+keyFile+" is missing", "restore it from a backup, or run pcb logout to discard the stored session and log in again")
	case errors.Is(err, seal.ErrCorrupt):
		return mustAct("the stored secrets cannot be opened with the key file "+keyFile, "restore both from a backup, or run pcb logout to discard the stored session and log in again")
	case errors.As(err, &ts):
		return blocked(ts.Error(), "retry in "+ts.Wait.Round(time.Second).String())
	case errors.As(err, &ae):
		return protonDown(ae)
	case errors.As(err, &ne), errors.As(err, &ue):
		return classed(classNetwork, "cannot reach Proton", "check the network connection, then retry")
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
		return &exitcode.Error{Code: exitcode.Blocked, Err: &detail{msg: "Proton is rate limiting requests", limited: true, retryAfter: e.RetryAfter, class: classRateLimited},
			Hint: "retry in " + wait}
	case e.Status >= 500:
		return classed(classProtonUnavailable, "Proton is unavailable", "try again later")
	case e.Code == api.CodeHumanVerification:
		// It never clears by waiting: a person completes it in a browser.
		return classed(classRefused, "Proton asks for a human verification (code 9001)",
			"open the Proton web app from this machine's network and complete the verification")
	}
	return classed(classRefused, fmt.Sprintf("Proton refused the request (code %d)", e.Code), "try again later; if it persists, run pcb doctor")
}

// rolledBack is what a person must do about a restored or replaced
// secrets.sealed (store.ErrRolledBack): only they know whether the restore
// was meant, and a new login is the way out either way.
func rolledBack(rb *store.RolledBackError) *exitcode.Error {
	return loginNeeded(rb.Explain(), store.RolledBackHint)
}

// unsupportedStore is a secrets.sealed this pcb cannot open (state-I1): a
// newer pcb wrote it, or another sealer sealed it. A person upgrades pcb;
// nothing here discards it (pcb logout does, once confirmed).
func unsupportedStore(e *store.UnsupportedError) *exitcode.Error {
	return mustAct(e.Explain(), "upgrade pcb")
}

// CheckStateOwner refuses euid 0 on a state directory another user owns (a
// system install's pcb user): every file root writes there would become
// root's, and that user could no longer read it (state Minor). userName
// names a UID (nil or failing: "#UID", which sudo takes too). A directory
// that does not exist yet, or a system without owners, passes.
func CheckStateOwner(stateDir string, euid int, userName func(uid int) (string, error)) error {
	if euid != 0 {
		return nil
	}
	uid, ok := store.DirOwner(stateDir)
	if !ok || uid == 0 {
		return nil
	}
	name, sudoName := "#"+strconv.Itoa(uid), "'#"+strconv.Itoa(uid)+"'"
	if userName != nil {
		if n, err := userName(uid); err == nil && n != "" {
			name, sudoName = n, n
		}
	}
	return mustAct("the state directory belongs to "+name+": run pcb as that user", "sudo -u "+sudoName+" pcb …")
}

// keyCause is why the key file or the sealed store could not be used, safe
// to show (it never holds a secret): the OS error with its operation and
// path (permission denied, read-only file system) or the key file's mode
// refusal; "" when there is none to give (M5-R15 C2).
func keyCause(err error) string {
	var (
		me *seal.KeyModeError
		se *seal.KeySizeError
		pe *fs.PathError
		le *os.LinkError
	)
	switch {
	case errors.As(err, &me):
		return fmt.Sprintf("mode %04o lets other users read it, and pcb refuses it", me.Mode)
	case errors.As(err, &se):
		return fmt.Sprintf("it holds %d bytes, not a 32-byte key", se.Size)
	case errors.As(err, &pe):
		return pe.Error()
	case errors.As(err, &le):
		return le.Error()
	}
	return ""
}

// keyFault reports whether err, a failure to use the key file or the
// sealed store, is one a person repairs (ux-I3): a key file that is
// missing, refused for its mode or size, not for this data, or that this
// user may not read or write. It is exit 10 in every command (MustAct, so
// systemd does not restart pcb serve); any other cause (a full disk) stays
// Blocked, as it may pass.
func keyFault(err error) bool {
	var me *seal.KeyModeError
	return errors.Is(err, seal.ErrNoKey) || errors.Is(err, seal.ErrCorrupt) || // a wrong size is ErrCorrupt
		errors.As(err, &me) || errors.Is(err, fs.ErrPermission)
}

// keyFailure is a failure to use the key file or the sealed store, with
// msg and hint: MustAct for a keyFault, else Blocked.
func keyFailure(err error, msg, hint string) *exitcode.Error {
	if keyFault(err) {
		return mustAct(msg, hint)
	}
	return blocked(msg, hint)
}

// keyFix is the hint for a key file that cannot be used.
const keyFix = "the key file must be readable by the user pcb runs as, 32 bytes, mode 0600 (pcb doctor)"

// keyringFailFix is keyFix's for a key in the desktop keyring that cannot be used.
const keyringFailFix = "check that the desktop keyring is running and unlocked (pcb doctor)"

// storeError maps a failure of the sealed store: a lost or foreign key names
// the key file, a rolled-back file is a MustAct; anything else is a store
// that cannot be used.
func storeError(err error, keyFile string) error {
	if errors.Is(err, seal.ErrNoKey) || errors.Is(err, seal.ErrCorrupt) || errors.Is(err, seal.ErrLocked) || errors.Is(err, store.ErrRolledBack) ||
		errors.Is(err, store.ErrUnsupported) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return mapError(err, keyFile)
	}
	var ee *exitcode.Error
	if errors.As(err, &ee) {
		return mapError(err, keyFile)
	}
	if cause := keyCause(err); cause != "" {
		if keyFile == keyringRef { // a PathError of the sealed file or the state directory, not of a key
			return keyFailure(err, "cannot use the stored secrets: "+cause, "make sure the state directory and its files are readable and writable by this user (pcb doctor)")
		}
		return keyFailure(err, "cannot use the stored secrets or the key file "+keyFile+": "+cause, keyFix+"; check the permissions of the state directory")
	}
	return blocked("cannot use the stored secrets", "check the permissions of the state directory and of the files in it (pcb doctor)")
}

// stateError maps an unreadable state.json (it fails closed). A secrets
// generation out of range was edited or damaged: a person resets it. A file
// a newer pcb wrote is not damaged: a person upgrades pcb, and a logout,
// which would revoke the session, is never suggested (state-I2).
func stateError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return mapError(err, "")
	case errors.Is(err, store.ErrNewerState):
		return mustAct("state.json was written by a newer pcb", "upgrade pcb")
	case errors.Is(err, store.ErrGenerationOutOfRange):
		return mustAct("the login state file holds a secrets generation out of range: it was edited or damaged", "run pcb logout, then pcb login")
	}
	if path, ok := store.StateUnreadable(err); ok {
		return mustAct(store.UnreadableStateMessage(path), store.UnreadableStateFix())
	}
	return mustAct("the login state file cannot be read", "run pcb logout to reset it")
}

// stateLockError maps a state-file lock that could not be taken
// (store.ErrStateLock): held past its timeout by another process, or not
// creatable.
func stateLockError(err error, stateDir string) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return blocked("another pcb process holds the login state file's lock (state.lock) too long", "run the command again; if it persists, look for a stuck pcb process")
	case errors.Is(err, context.Canceled):
		return mapError(err, "")
	}
	return blocked("cannot take the lock of the login state file", "make sure "+stateDir+" is writable by this user")
}

// stateWriteError maps a failed SaveState or UpdateState of state.json.
func (l *Local) stateWriteError(err error) error {
	if errors.Is(err, store.ErrStateLock) {
		return stateLockError(err, l.o.Dirs.State)
	}
	return storeError(err, l.keyFile())
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

// SeveralAccountsError is the cause of a refusal to choose an account when
// several could be meant and none is named. Accounts lists them, sorted by
// address (--json: data.accounts).
type SeveralAccountsError struct {
	Accounts []AccountInfo
}

func (*SeveralAccountsError) Error() string { return "several accounts: name one with --account" }

// SeveralAccounts is the refusal (Usage) to choose among list, sorted by
// address, when none is named: its cause is a *SeveralAccountsError.
func SeveralAccounts(list []AccountInfo) error { return severalAccounts(list) }

func severalAccounts(list []AccountInfo) *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Usage, Err: &SeveralAccountsError{Accounts: list},
		Hint: "name one with --account or PCB_ACCOUNT (pcb account list)"}
}

// ErrNoAccount is the cause of a refusal to choose an account when none
// matches: the selector names none (or one since removed), or none is
// ready to be chosen without a selector.
var ErrNoAccount = errors.New("no account")

// noAccount is the refusal (Usage) to choose the account selector names,
// with hint.
func noAccount(selector, hint string) *exitcode.Error {
	return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("%w %s", ErrNoAccount, selector), Hint: hint}
}

// noReadyAccount is the refusal to choose without a selector when no
// account is ready, with hint: a person must add one or finish a pending
// login (MustAct, ruling R38).
func noReadyAccount(hint string) *exitcode.Error {
	return &exitcode.Error{Code: exitcode.MustAct, Err: ErrNoAccount, Hint: hint}
}

// layoutError maps a failure to read or migrate the state layout of root.
// legacy names the account files of an older pcb at the root. A layout pcb
// cannot use needs a person (MustAct, ruling R36, as stateError's): a unit
// restarted on 20 would loop on it. Its own refusals (exitcode.Error: a
// lock another pcb holds, Blocked) pass through.
func layoutError(err error, root string, legacy []string) error {
	var (
		ee *exitcode.Error
		pe *fs.PathError
	)
	switch {
	case errors.As(err, &ee):
		return err
	case errors.Is(err, store.ErrLegacyBesideLayout):
		msg := "account files of an older pcb are beside the per-account layout"
		if len(legacy) > 0 {
			msg += ": " + strings.Join(legacy, ", ")
		}
		return mustAct(msg+" in "+root, "remove them or move the state directory aside: this pcb version cannot be downgraded")
	case errors.Is(err, store.ErrNewerLayout), errors.Is(err, store.ErrUnknownSealer):
		return mustAct("the state layout (layout.json) was written by a newer pcb", "upgrade pcb")
	case errors.As(err, &pe):
		return mustAct("cannot use the state layout: "+pe.Op+" "+pe.Path+": "+pe.Err.Error(),
			"check the permissions of "+root+" and of the files in it")
	}
	return mustAct("the state layout in "+root+" is damaged (layout.json, accounts/)", "run pcb doctor")
}
