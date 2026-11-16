package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// pendingLogin is a login that passed SRP and waits for its 2FA code or,
// for an account in two-password mode, its mailbox password. It lives in
// memory only: a second SRP is never needed.
type pendingLogin struct {
	p          *auth.Pending
	username   string
	password   []byte
	unattended bool
	relogin    bool        // an unattended relogin, not a person's login
	totpURI    string      // the otpauth:// URI, when one was given
	tries      int         // 2FA codes Proton rejected so far
	sealed     bool        // its tokens are in secrets.sealed: the session must not be revoked
	last       store.State // state.json when the login began
	live       *api.Client // the client of the key calls; its tokens outlive p.Tokens after a refresh
	expires    time.Time   // when it stops waiting (pendingTTL after Login), whatever the step
	stop       func() bool // stops its expiry timer; nil until it waits

	// step is what the held login waits for: StateNeedsTOTP or
	// StateNeedsMailboxPassword. Once it is the latter, the 2FA step is done
	// and salts, user and addrs are fetched: a mailbox password is tried
	// against them without any Proton call.
	step         LoginState
	mailbox      []byte // the mailbox password, from its arrival to the derivation
	mailboxTries int    // mailbox passwords that did not unlock the keys so far
	salts        gpa.Salts
	user         gpa.User
	addrs        []gpa.Address
}

// pendingTTL is how long a login waits for its 2FA code and its mailbox
// password, in all (ruling M5-Rpre4): then expire revokes its session and
// zeroes its passwords.
const pendingTTL = 5 * time.Minute

// release ends pl for good: its expiry timer stopped, so a stale timer
// never touches a newer login, and its passwords zeroed.
func (pl *pendingLogin) release() {
	if pl.stop != nil {
		pl.stop()
	}
	clear(pl.password)
	clear(pl.mailbox)
	pl.salts, pl.user, pl.addrs = nil, gpa.User{}, nil
}

// keysFetched reports whether pl passed the 2FA step and fetched what the
// key passphrase is derived from.
func (pl *pendingLogin) keysFetched() bool { return pl.step == StateNeedsMailboxPassword }

// loginWho is whom a login waiting for its code is for: the username it
// was given and the Proton user ID SRP answered.
type loginWho struct{ username, userID string }

// waitingFor is what the held login waits for and until when (Status).
type waitingFor struct {
	step    LoginState
	expires time.Time
}

// stepNoun names what a login waiting at step waits for.
func stepNoun(step LoginState) string {
	if step == StateNeedsMailboxPassword {
		return "mailbox password"
	}
	return "2FA code"
}

// hold makes pl the login waiting at step (StateNeedsTOTP or
// StateNeedsMailboxPassword). It holds mu. The first time, it arms the
// expiry timer for what is left until pl.expires; a later hold (a rejected
// code or password, the 2FA step passed) keeps the first deadline.
func (l *Local) hold(pl *pendingLogin, step LoginState) {
	pl.step = step
	l.pending = pl
	l.waiting.Store(&waitingFor{step: step, expires: pl.expires})
	l.who.Store(&loginWho{username: pl.username, userID: pl.p.UserID})
	if pl.stop == nil {
		pl.stop = l.o.AfterFunc(pl.expires.Sub(l.o.Now()), func() { l.expire(pl) })
	}
}

// heldResult is the result of a login held at its step.
func heldResult(pl *pendingLogin) LoginResult {
	exp := pl.expires
	return LoginResult{State: pl.step, Username: pl.username, AccountID: pl.p.UserID, LoginExpires: &exp}
}

// unhold forgets the login waiting for its code. It holds mu.
func (l *Local) unhold() {
	l.pending = nil
	l.waiting.Store(nil)
	l.who.Store(nil)
}

// waitingLogin is whom the held login is for, nil if none.
func (l *Local) waitingLogin() *loginWho { return l.who.Load() }

// expire ends pl once pendingTTL has passed: its half session revoked
// (best effort, within revokeTimeout), its passwords zeroed, and the submits
// then say it expired. A timer whose login completed, failed or was
// replaced meanwhile finds another pending login, or none, and does nothing.
func (l *Local) expire(pl *pendingLogin) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != pl || l.closed.Load() {
		return
	}
	l.dropPending(context.Background())
	l.expired = pl.step
}

// waitsFor checks that a login is held at step, for its submit: an expired
// one or none, or one held at the other step, is a MustAct naming the step.
// It holds mu.
func (l *Local) waitsFor(step LoginState) error {
	switch {
	case l.pending == nil && l.expired != "":
		return mustAct("the login waiting for its "+stepNoun(l.expired)+" expired after 5 minutes", "run pcb login again")
	case l.pending == nil:
		return mustAct("no login is waiting for a "+stepNoun(step), "run pcb login first")
	case l.pending.step != step:
		return mustAct("no login is waiting for a "+stepNoun(step), "the login is waiting for its "+stepNoun(l.pending.step))
	}
	return nil
}

// abort revokes the login's session and closes it. After a refresh during
// the key calls the session lives on the client's current tokens, which
// go-proton-api's client does not know: those are the ones revoked.
func (l *Local) abort(ctx context.Context, pl *pendingLogin) {
	if pl.live == nil {
		pl.p.Abort(ctx)
		return
	}
	pl.p.Close()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	_ = auth.Logout(rctx, pl.live)
}

// maxTOTPTries is how many 2FA codes a login may submit in all (R7m).
const maxTOTPTries = 3

const unattendedHint = "unattended relogin needs the TOTP secret as an otpauth:// URI"

// Login implements API. A new Login supersedes (aborts) a login still waiting
// for its 2FA code. A login without UnattendedRelogin turns unattended relogin
// off for the account: the stored credentials are replaced by tokens only.
func (l *Local) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return LoginResult{}, errClosed()
	}
	if req.Username == "" || req.Password == "" {
		return LoginResult{}, usage("a username and a password are required", "pass the username and the password (PCB_USERNAME, PCB_PASSWORD or the prompt)")
	}
	var totp *otp.TOTP
	if req.TOTP != "" {
		var err error
		if totp, err = otp.Parse(req.TOTP); err != nil {
			return LoginResult{}, usage("the TOTP is neither a 6-8 digit code nor an otpauth:// URI", "pass a code from your authenticator, or the otpauth:// URI")
		}
		if req.UnattendedRelogin && !isURI(req.TOTP) {
			return LoginResult{}, usage(unattendedHint, "pass the otpauth:// URI instead of a one-time code")
		}
	}
	if err := l.sealerUsable(ctx); err != nil {
		return LoginResult{}, err
	}
	l.dropPending(ctx)
	l.expired = ""

	accounts, err := l.secrets.Load()
	if err != nil {
		return LoginResult{}, storeError(err, l.keyFile())
	}
	last, err := store.LoadState(l.o.Dirs.State)
	if err != nil {
		return LoginResult{}, stateError(err)
	}
	now := l.o.Now()
	release, now, err := l.acquire(ctx, req.CaptchaToken != "", now)
	if err != nil {
		return LoginResult{}, err
	}
	defer release()

	p, err := l.begin(ctx, req)
	if err != nil {
		var ce *auth.CaptchaError
		switch {
		case errors.As(err, &ce) && last.Phase == store.PhaseReady:
			// The stored session still works: the person gets the CAPTCHA
			// and its token, and state.json stays as it was (state-I4).
			return captchaResult(ce, req.Username), nil
		case errors.As(err, &ce):
			return l.needsCaptcha(ctx, ce, last, now, req.Username, false)
		}
		return LoginResult{}, l.fail(err)
	}
	if _, known := accounts[p.UserID]; len(accounts) > 0 && !known {
		p.Abort(ctx)
		return LoginResult{}, mustAct("another Proton account is already logged in; pcb handles one account", "run pcb logout first, then log in again")
	}
	pl := &pendingLogin{p: p, username: req.Username, password: []byte(req.Password), unattended: req.UnattendedRelogin, last: last,
		expires: l.o.Now().Add(pendingTTL)}
	if p.TwoPassword && req.MailboxPassword != "" {
		pl.mailbox = []byte(req.MailboxPassword) // kept across a 2FA hold, zeroed once derived
	}
	if isURI(req.TOTP) {
		pl.totpURI = strings.TrimSpace(req.TOTP)
	}
	if p.NeedsTOTP && totp == nil {
		l.hold(pl, StateNeedsTOTP)
		return heldResult(pl), nil
	}
	return l.finish(ctx, pl, totp, now)
}

// sealerUsable checks, on a state sealed with the desktop keyring, that the
// keyring answers and is unlocked before a login spends anything: the SRP
// call, Proton's login and the guard's 60 s would be lost on a seal that
// cannot succeed (ruling R16). It is the locked or the unavailable error
// then; nil on a key-file state, which has nothing to ask.
func (l *Local) sealerUsable(ctx context.Context) error {
	ss, ok := l.o.Sealer.(*seal.SecretService)
	if !ok {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if err := seal.Probe(pctx, ss.Keyring); err != nil {
		return l.fail(err)
	}
	return nil
}

// begin runs Login's SRP step, unless a test replaced it (skipSRP).
func (l *Local) begin(ctx context.Context, req LoginRequest) (*auth.Pending, error) {
	if l.skipSRP != nil {
		return l.skipSRP(req.Username), nil
	}
	return auth.Begin(ctx, l.mgr, req.Username, []byte(req.Password), req.CaptchaToken)
}

// SubmitTOTP implements API. The code is a 6-8 digit code or an otpauth://
// URI (required when the login asked for unattended relogin).
func (l *Local) SubmitTOTP(ctx context.Context, code string) (LoginResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return LoginResult{}, errClosed()
	}
	if err := l.waitsFor(StateNeedsTOTP); err != nil {
		return LoginResult{}, err
	}
	totp, err := otp.Parse(code)
	if err != nil {
		return LoginResult{}, usage("the 2FA code is neither a 6-8 digit code nor an otpauth:// URI", "pass the code from your authenticator")
	}
	if l.pending.unattended && !isURI(code) {
		return LoginResult{}, usage(unattendedHint, "pass the otpauth:// URI instead of a one-time code")
	}
	pl := l.pending
	if isURI(code) {
		pl.totpURI = strings.TrimSpace(code)
	}
	l.unhold()
	return l.finish(ctx, pl, totp, l.o.Now())
}

// SubmitMailboxPassword implements API. The password is tried against the
// salts and keys the login fetched after its 2FA step: no Proton call.
func (l *Local) SubmitMailboxPassword(ctx context.Context, password string) (LoginResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return LoginResult{}, errClosed()
	}
	if err := l.waitsFor(StateNeedsMailboxPassword); err != nil {
		return LoginResult{}, err
	}
	if password == "" {
		return LoginResult{}, usage("the mailbox password is empty", "enter the mailbox password of the account")
	}
	pl := l.pending
	pl.mailbox = []byte(password)
	l.unhold()
	return l.finish(ctx, pl, nil, l.o.Now())
}

// AbandonLogin drops the login waiting for its 2FA code or its mailbox
// password (its half session revoked) and changes nothing else: a stored
// session stays and serves. Nothing waiting, nothing happens.
func (l *Local) AbandonLogin(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return
	}
	l.dropPending(ctx)
	l.expired = ""
}

// dropPending aborts a login waiting for its code. It holds mu.
func (l *Local) dropPending(ctx context.Context) {
	if l.pending == nil {
		return
	}
	l.abort(ctx, l.pending)
	l.pending.release()
	l.unhold()
}

// acquire takes the login guard. With a CAPTCHA token the user already
// waited for Proton's challenge, so a TooSoon is waited out instead of
// refused; the returned time is the one the attempt is stamped with.
func (l *Local) acquire(ctx context.Context, wait bool, now time.Time) (func(), time.Time, error) {
	if err := acl.MkdirAll(l.o.GuardDir); err != nil {
		return nil, now, acquireError(err, l.o.GuardDir)
	}
	release, err := loginguard.Acquire(l.o.GuardDir, loginguard.MinInterval, now, l.log())
	if d, ok := loginguard.WaitFor(err); ok && wait {
		if err := l.o.Sleep(ctx, d); err != nil {
			return nil, now, l.fail(err)
		}
		now = now.Add(d)
		release, err = loginguard.Acquire(l.o.GuardDir, loginguard.MinInterval, now, l.log())
	}
	if err != nil {
		return nil, now, acquireError(err, l.o.GuardDir)
	}
	return release, now, nil
}

// needsCaptcha marks the login needs-captcha; relogin: an unattended
// relogin's, not a person's (the address rule of loginAddress).
func (l *Local) needsCaptcha(ctx context.Context, ce *auth.CaptchaError, last store.State, now time.Time, username string, relogin bool) (LoginResult, error) {
	err := l.writeLogin(ctx, last, func(cur store.State) store.State {
		return store.State{
			AccountID:   last.AccountID,
			Phase:       store.PhaseNeedsCaptcha,
			Reason:      "Proton asks for a CAPTCHA",
			Captcha:     &store.Captcha{URL: ce.URL, Token: ce.Token, At: now},
			Unattended:  last.Unattended,
			LastLogin:   last.LastLogin,
			LastRelogin: last.LastRelogin,
			Address:     loginAddress(cur, username, relogin),
			CalDAV:      cur.CalDAV,
			MCP:         cur.MCP,
			MCPReadOnly: cur.MCPReadOnly,
			Created:     createdOr(cur, now),
		}
	})
	if err != nil {
		return LoginResult{}, err
	}
	return captchaResult(ce, username), nil
}

// writeLogin writes the state.json a login ends with, built by next from
// the file as it is now, under state.lock (ruling R19): what the login does
// not own (the adapter switches, the creation time, the address a relogin
// keeps) is read there, not from last, the state the login began with, so
// a switch made meanwhile is kept. The secrets generation mirror is the
// file's. A state.json that can no longer be read is replaced as is, built
// from last.
func (l *Local) writeLogin(ctx context.Context, last store.State, next func(cur store.State) store.State) error {
	loaded := false
	err := store.UpdateStateContext(ctx, l.o.Dirs.State, func(cur *store.State) error {
		loaded = true
		st := next(*cur)
		st.SecretsGeneration = cur.SecretsGeneration
		*cur = st
		return nil
	})
	if err != nil && !loaded && !errors.Is(err, store.ErrStateLock) {
		err = store.SaveStateContext(ctx, l.o.Dirs.State, next(last))
	}
	if err != nil {
		return l.stateWriteError(err)
	}
	return nil
}

// loginAddress is the address a login writes into state.json, cur being
// the file as it is: a person's login writes the username given; an
// unattended relogin keeps the address the account shows, else the stored
// username.
func loginAddress(cur store.State, username string, relogin bool) string {
	if relogin && cur.Address != "" {
		return cur.Address
	}
	return username
}

// createdOr is when the account was added: last's, else now.
func createdOr(last store.State, now time.Time) time.Time {
	if last.Created.IsZero() {
		return now
	}
	return last.Created
}

func captchaResult(ce *auth.CaptchaError, username string) LoginResult {
	return LoginResult{State: StateNeedsCaptcha, Username: username, CaptchaURL: ce.URL, CaptchaToken: ce.Token}
}

// finish runs what follows SRP: the 2FA code, the keys, the mailbox
// password of a two-password account, the sealing. A failure aborts the
// session it opened and saves nothing, except a rejected 2FA code or a
// mailbox password that does not unlock the keys: the login then stays held
// for up to maxTOTPTries codes and maxMailboxTries passwords in all. A
// two-password account without its mailbox password is held for it.
func (l *Local) finish(ctx context.Context, pl *pendingLogin, totp *otp.TOTP, now time.Time) (LoginResult, error) {
	res, err := l.complete(ctx, pl, totp, now)
	if err == nil {
		pl.p.Close()
		pl.release()
		res.DefaultClientCreated = l.createDefaultClient(ctx)
		l.wakeLogin() // a Serve waiting for a login serves at once (ruling M5-P9)
		return res, nil
	}
	switch {
	case errors.Is(err, errNeedsMailboxPassword):
		l.hold(pl, StateNeedsMailboxPassword)
		return heldResult(pl), nil
	case errors.Is(err, auth.ErrTOTPRejected):
		if pl.tries++; pl.tries < maxTOTPTries {
			l.hold(pl, StateNeedsTOTP)
			return LoginResult{}, mustAct("the 2FA code was rejected by Proton",
				fmt.Sprintf("enter the current code again (%d tries left)", maxTOTPTries-pl.tries))
		}
		l.abort(ctx, pl)
		pl.release()
		return LoginResult{}, mustAct("the 2FA code was rejected "+strconv.Itoa(maxTOTPTries)+" times and the login was cancelled",
			"run pcb login again")
	case pl.p.TwoPassword && errors.Is(err, keys.ErrKeysLocked):
		if pl.mailboxTries++; pl.mailboxTries < maxMailboxTries {
			l.hold(pl, StateNeedsMailboxPassword)
			return LoginResult{}, mustAct("the mailbox password does not unlock the account keys",
				fmt.Sprintf("enter the mailbox password again (%d tries left)", maxMailboxTries-pl.mailboxTries))
		}
		l.abort(ctx, pl)
		pl.release()
		return LoginResult{}, mustAct("the mailbox password did not unlock the account keys "+strconv.Itoa(maxMailboxTries)+" times and the login was cancelled",
			"run pcb login again")
	}
	if !pl.sealed {
		l.abort(ctx, pl)
	}
	pl.release()
	return LoginResult{}, l.fail(err)
}

// maxMailboxTries is how many mailbox passwords a login may try in all,
// counted apart from its 2FA codes.
const maxMailboxTries = 3

// errNeedsMailboxPassword: a two-password account reached the derivation
// without its mailbox password; finish holds the login for it.
var errNeedsMailboxPassword = errors.New("the login needs the mailbox password")

// complete runs the steps after SRP that are not done yet: the 2FA code and
// the fetch of what the key passphrase is derived from (once: a login held
// for its mailbox password has both behind it), then the derivation and the
// sealing.
func (l *Local) complete(ctx context.Context, pl *pendingLogin, totp *otp.TOTP, now time.Time) (LoginResult, error) {
	if !pl.keysFetched() {
		var err error
		if now, err = l.passTOTP(ctx, pl, totp, now); err != nil {
			return LoginResult{}, err
		}
		if err := l.fetchKeys(ctx, pl); err != nil {
			return LoginResult{}, err
		}
	}
	keyPass, err := l.deriveKeyPass(pl)
	if err != nil {
		return LoginResult{}, err
	}
	defer clear(keyPass) // sealed below; the account copy shares it
	return l.seal(ctx, pl, keyPass, now)
}

// passTOTP submits the 2FA code when the account asks for one, first
// waiting for the next period when the code would repeat the last one. It
// returns the time after that wait.
func (l *Local) passTOTP(ctx context.Context, pl *pendingLogin, totp *otp.TOTP, now time.Time) (time.Time, error) {
	if !pl.p.NeedsTOTP {
		return now, nil
	}
	if wait := totp.Wait(now); wait > 0 {
		if err := l.o.Sleep(ctx, wait); err != nil {
			return now, err
		}
		now = now.Add(wait)
	}
	return now, pl.p.SubmitTOTP(ctx, totp.Code(now))
}

// fetchKeys opens the login's client and fetches the key salts (behind the
// scope unlock, with the login password), the user and the addresses into
// pl, for the derivation.
func (l *Local) fetchKeys(ctx context.Context, pl *pendingLogin) error {
	c := l.client(pl.p.Tokens)
	pl.live = c
	scope := auth.Scope{M: l.mgr, C: c, Username: pl.username, Password: pl.password}
	salts, err := keys.FetchSalts(ctx, c, scope)
	if err != nil {
		return err
	}
	user, addrs, err := keys.Fetch(ctx, c)
	if err != nil {
		return err
	}
	pl.salts, pl.user, pl.addrs = salts, user, addrs
	return nil
}

// reopenKeys is what follows a relogin's SRP for a two-password account:
// the 2FA code, then the user and addresses only (no salts, no scope
// unlock) and the keys opened with the sealed keyPass, which seals the new
// tokens. A keyPass that no longer opens the keys is keys.ErrKeysLocked.
func (l *Local) reopenKeys(ctx context.Context, pl *pendingLogin, keyPass []byte, totp *otp.TOTP, now time.Time) error {
	now, err := l.passTOTP(ctx, pl, totp, now)
	if err != nil {
		return err
	}
	c := l.client(pl.p.Tokens)
	pl.live = c
	user, addrs, err := keys.Fetch(ctx, c)
	if err != nil {
		return err
	}
	u, err := keys.Unlock(user, addrs, keyPass)
	if err != nil {
		return err
	}
	u.Close()
	_, err = l.seal(ctx, pl, keyPass, now)
	return err
}

// deriveKeyPass derives the key passphrase from what fetchKeys fetched: from
// the login password, or from the mailbox password of a two-password account
// (errNeedsMailboxPassword without one). The mailbox password is zeroed
// once tried, whatever the outcome. The result is a secret.
func (l *Local) deriveKeyPass(pl *pendingLogin) ([]byte, error) {
	password := pl.password
	if pl.p.TwoPassword {
		if len(pl.mailbox) == 0 {
			return nil, errNeedsMailboxPassword
		}
		password = pl.mailbox
		defer func() { clear(pl.mailbox); pl.mailbox = nil }()
	}
	keyPass, err := keys.DeriveKeyPass(pl.user, pl.addrs, pl.salts, password)
	if err != nil {
		return nil, err
	}
	if l.keyPassHook != nil {
		l.keyPassHook(keyPass)
	}
	return keyPass, nil
}

// seal stores the login's session and keyPass and marks the login ready.
// A state.json that cannot be written puts the secrets back as they were.
func (l *Local) seal(ctx context.Context, pl *pendingLogin, keyPass []byte, now time.Time) (LoginResult, error) {
	p, c := pl.p, pl.live
	// The client's tokens, not p.Tokens: a refresh during the key calls spent
	// the refresh token SRP returned.
	tok := c.Tokens()
	acc := store.Account{
		ID: p.UserID, Username: pl.username,
		UID: tok.UID, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		KeyPass: keyPass,
	}
	if pl.unattended {
		acc.Relogin = &store.Relogin{Password: string(pl.password)}
		acc.Relogin.TOTPURI = pl.totpURI
	}
	var old store.Account
	var replaced bool
	if err := l.secrets.Update(ctx, func(accounts map[string]store.Account) error {
		old, replaced = accounts[acc.ID]
		accounts[acc.ID] = acc
		return nil
	}); err != nil {
		return LoginResult{}, storeError(err, l.keyFile())
	}
	pl.sealed = true
	l.dropClient()
	// LastLogin is a person's last login; an unattended relogin only moves
	// LastRelogin.
	err := l.writeLogin(ctx, pl.last, func(cur store.State) store.State {
		st := store.State{
			AccountID: acc.ID, Phase: store.PhaseReady, Unattended: pl.unattended,
			LastLogin: now, LastRelogin: pl.last.LastRelogin,
			Address: loginAddress(cur, pl.username, pl.relogin), CalDAV: cur.CalDAV, MCP: cur.MCP, MCPReadOnly: cur.MCPReadOnly, Created: createdOr(cur, now),
		}
		if pl.relogin {
			st.LastLogin, st.LastRelogin = pl.last.LastLogin, now
		}
		return st
	})
	if err != nil {
		// Put the secrets back as they were, so the stored session stays the
		// one state.json describes. If that fails too, the new session is
		// what is sealed and must not be revoked.
		if rerr := l.secrets.Update(context.WithoutCancel(ctx), func(accounts map[string]store.Account) error {
			if replaced {
				accounts[acc.ID] = old
			} else {
				delete(accounts, acc.ID)
			}
			return nil
		}); rerr == nil {
			pl.sealed = false
		}
		return LoginResult{}, err
	}
	// A relogin replaces a session Proton already ended: nothing to revoke.
	if replaced && old.UID != acc.UID && !pl.relogin {
		l.revoke(ctx, old)
	}
	return LoginResult{State: StateReady, Username: acc.Username, AccountID: acc.ID}, nil
}

func isURI(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "otpauth://")
}
