package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// pendingLogin is a login that passed SRP and waits for its 2FA code. It
// lives in memory only: a second SRP is never needed.
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
	expires    time.Time   // when it stops waiting for its code (pendingTTL after Login)
	stop       func() bool // stops its expiry timer; nil until it waits
}

// pendingTTL is how long a login waits for its 2FA code (ruling M5-Rpre4):
// then expire revokes its session and zeroes its password.
const pendingTTL = 5 * time.Minute

// release ends pl for good: its expiry timer stopped, so a stale timer
// never touches a newer login, and its password zeroed.
func (pl *pendingLogin) release() {
	if pl.stop != nil {
		pl.stop()
	}
	clear(pl.password)
}

// loginWho is whom a login waiting for its code is for: the username it
// was given and the Proton user ID SRP answered.
type loginWho struct{ username, userID string }

// hold makes pl the login waiting for its code. It holds mu. The first time,
// it arms the expiry timer; a rejected code keeps the first deadline.
func (l *Local) hold(pl *pendingLogin) {
	l.pending = pl
	exp := pl.expires
	l.waiting.Store(&exp)
	l.who.Store(&loginWho{username: pl.username, userID: pl.p.UserID})
	if pl.stop == nil {
		pl.stop = l.o.AfterFunc(pendingTTL, func() { l.expire(pl) })
	}
}

// unhold forgets the login waiting for its code. It holds mu.
func (l *Local) unhold() {
	l.pending = nil
	l.waiting.Store(nil)
	l.who.Store(nil)
}

// waitingLogin is whom the login waiting for its code is for, nil if none.
func (l *Local) waitingLogin() *loginWho { return l.who.Load() }

// expire ends pl once pendingTTL has passed: its half session revoked
// (best effort, within revokeTimeout), its password zeroed, and SubmitTOTP
// then says it expired. A timer whose login completed, failed or was
// replaced meanwhile finds another pending login, or none, and does nothing.
func (l *Local) expire(pl *pendingLogin) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != pl || l.closed.Load() {
		return
	}
	l.dropPending(context.Background())
	l.expired = true
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
	l.expired = false

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
	pl := &pendingLogin{p: p, username: req.Username, password: []byte(req.Password), unattended: req.UnattendedRelogin, last: last}
	if isURI(req.TOTP) {
		pl.totpURI = strings.TrimSpace(req.TOTP)
	}
	if p.NeedsTOTP && totp == nil {
		pl.expires = l.o.Now().Add(pendingTTL)
		l.hold(pl)
		exp := pl.expires
		return LoginResult{State: StateNeedsTOTP, Username: req.Username, AccountID: p.UserID, LoginExpires: &exp}, nil
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
	if l.pending == nil && l.expired {
		return LoginResult{}, mustAct("the login waiting for its 2FA code expired after 5 minutes", "run pcb login again")
	}
	if l.pending == nil {
		return LoginResult{}, mustAct("no login is waiting for a 2FA code", "run pcb login first")
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
	if err := os.MkdirAll(l.o.GuardDir, 0o700); err != nil {
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

// finish runs what follows SRP: the 2FA code, the keys, the sealing. A failure
// aborts the session it opened and saves nothing, except a rejected 2FA code:
// the login then stays pending for up to maxTOTPTries codes in all.
func (l *Local) finish(ctx context.Context, pl *pendingLogin, totp *otp.TOTP, now time.Time) (LoginResult, error) {
	res, err := l.complete(ctx, pl, totp, now)
	if err == nil {
		pl.p.Close()
		pl.release()
		res.DefaultClientCreated = l.createDefaultClient(ctx)
		l.wakeLogin() // a Serve waiting for a login serves at once (ruling M5-P9)
		return res, nil
	}
	if errors.Is(err, auth.ErrTOTPRejected) {
		if pl.tries++; pl.tries < maxTOTPTries {
			l.hold(pl)
			return LoginResult{}, mustAct("the 2FA code was rejected by Proton",
				fmt.Sprintf("enter the current code again (%d tries left)", maxTOTPTries-pl.tries))
		}
		l.abort(ctx, pl)
		pl.release()
		return LoginResult{}, mustAct("the 2FA code was rejected "+strconv.Itoa(maxTOTPTries)+" times and the login was cancelled",
			"run pcb login again")
	}
	if !pl.sealed {
		l.abort(ctx, pl)
	}
	pl.release()
	return LoginResult{}, l.fail(err)
}

func (l *Local) complete(ctx context.Context, pl *pendingLogin, totp *otp.TOTP, now time.Time) (LoginResult, error) {
	p := pl.p
	if p.NeedsTOTP {
		if wait := totp.Wait(now); wait > 0 {
			if err := l.o.Sleep(ctx, wait); err != nil {
				return LoginResult{}, err
			}
			now = now.Add(wait)
		}
		if err := p.SubmitTOTP(ctx, totp.Code(now)); err != nil {
			return LoginResult{}, err
		}
	}
	c := l.client(p.Tokens)
	pl.live = c
	scope := auth.Scope{M: l.mgr, C: c, Username: pl.username, Password: pl.password}
	salts, err := keys.FetchSalts(ctx, c, scope)
	if err != nil {
		return LoginResult{}, err
	}
	user, addrs, err := keys.Fetch(ctx, c)
	if err != nil {
		return LoginResult{}, err
	}
	keyPass, err := keys.DeriveKeyPass(user, addrs, salts, pl.password)
	if err != nil {
		return LoginResult{}, err
	}
	defer clear(keyPass) // sealed below; the account copy shares it
	if l.keyPassHook != nil {
		l.keyPassHook(keyPass)
	}
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
	err = l.writeLogin(ctx, pl.last, func(cur store.State) store.State {
		st := store.State{
			AccountID: acc.ID, Phase: store.PhaseReady, Unattended: pl.unattended,
			LastLogin: now, LastRelogin: pl.last.LastRelogin,
			Address: loginAddress(cur, pl.username, pl.relogin), CalDAV: cur.CalDAV, MCP: cur.MCP, Created: createdOr(cur, now),
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
