package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// pendingLogin is a login that passed SRP and waits for its 2FA code. It
// lives in memory only: a second SRP is never needed.
type pendingLogin struct {
	p          *auth.Pending
	username   string
	password   []byte
	unattended bool
	totpURI    string      // the otpauth:// URI, when one was given
	last       store.State // state.json when the login began
}

const unattendedHint = "unattended relogin needs the TOTP secret as an otpauth:// URI"

// Login implements API.
func (l *Local) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	l.dropPending(ctx)

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

	p, err := auth.Begin(ctx, l.mgr, req.Username, []byte(req.Password), req.CaptchaToken)
	if err != nil {
		var ce *auth.CaptchaError
		if errors.As(err, &ce) {
			return l.needsCaptcha(ce, last, now, req.Username)
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
		l.pending = pl
		l.waiting.Store(true)
		return LoginResult{State: StateNeedsTOTP, Username: req.Username, AccountID: p.UserID}, nil
	}
	return l.finish(ctx, pl, totp, now)
}

// SubmitTOTP implements API. The code is a 6-8 digit code or an otpauth://
// URI (required when the login asked for unattended relogin).
func (l *Local) SubmitTOTP(ctx context.Context, code string) (LoginResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	l.pending = nil
	l.waiting.Store(false)
	return l.finish(ctx, pl, totp, l.o.Now())
}

// dropPending aborts a login waiting for its code. It holds mu.
func (l *Local) dropPending(ctx context.Context) {
	if l.pending == nil {
		return
	}
	l.pending.p.Abort(ctx)
	clear(l.pending.password)
	l.pending = nil
	l.waiting.Store(false)
}

// acquire takes the login guard. With a CAPTCHA token the user already
// waited for Proton's challenge, so a TooSoon is waited out instead of
// refused; the returned time is the one the attempt is stamped with.
func (l *Local) acquire(ctx context.Context, wait bool, now time.Time) (func(), time.Time, error) {
	if err := os.MkdirAll(l.o.Dirs.State, 0o700); err != nil {
		return nil, now, acquireError(err, l.o.Dirs.State)
	}
	release, err := loginguard.Acquire(l.o.Dirs.State, loginguard.MinInterval, now)
	if d, ok := loginguard.WaitFor(err); ok && wait {
		if err := l.o.Sleep(ctx, d); err != nil {
			return nil, now, l.fail(err)
		}
		now = now.Add(d)
		release, err = loginguard.Acquire(l.o.Dirs.State, loginguard.MinInterval, now)
	}
	if err != nil {
		return nil, now, acquireError(err, l.o.Dirs.State)
	}
	return release, now, nil
}

func (l *Local) needsCaptcha(ce *auth.CaptchaError, last store.State, now time.Time, username string) (LoginResult, error) {
	st := store.State{
		AccountID:   last.AccountID,
		Phase:       store.PhaseNeedsCaptcha,
		Reason:      "Proton asks for a CAPTCHA",
		Captcha:     &store.Captcha{URL: ce.URL, Token: ce.Token, At: now},
		Unattended:  last.Unattended,
		LastLogin:   last.LastLogin,
		LastRelogin: last.LastRelogin,
	}
	if err := store.SaveState(l.o.Dirs.State, st); err != nil {
		return LoginResult{}, storeError(err, l.keyFile())
	}
	return LoginResult{State: StateNeedsCaptcha, Username: username, CaptchaURL: ce.URL, CaptchaToken: ce.Token}, nil
}

// finish runs what follows SRP: the 2FA code, the keys, the sealing. Any
// failure aborts the session it opened and saves nothing.
func (l *Local) finish(ctx context.Context, pl *pendingLogin, totp *otp.TOTP, now time.Time) (LoginResult, error) {
	defer clear(pl.password)
	res, err := l.complete(ctx, pl, totp, now)
	if err != nil {
		pl.p.Abort(ctx)
		return LoginResult{}, l.fail(err)
	}
	pl.p.Close()
	return res, nil
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
	acc := store.Account{
		ID: p.UserID, Username: pl.username,
		UID: p.Tokens.UID, AccessToken: p.Tokens.AccessToken, RefreshToken: p.Tokens.RefreshToken,
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
	if replaced && old.UID != acc.UID {
		l.revoke(ctx, old)
	}
	st := store.State{
		AccountID: acc.ID, Phase: store.PhaseReady, Unattended: pl.unattended,
		LastLogin: now, LastRelogin: pl.last.LastRelogin,
	}
	if err := store.SaveState(l.o.Dirs.State, st); err != nil {
		return LoginResult{}, storeError(err, l.keyFile())
	}
	return LoginResult{State: StateReady, Username: acc.Username, AccountID: acc.ID}, nil
}

func isURI(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "otpauth://")
}
