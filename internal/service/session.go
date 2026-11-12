package service

import (
	"context"
	"errors"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

const sessionEnded = "the Proton session has ended"

// errAdopted ends a Secrets.Update without writing: another process already
// rotated the tokens.
var errAdopted = errors.New("tokens adopted")

// stored returns the login state and the sealed account of a ready login;
// any other phase is a MustAct naming what the person must do.
func (l *Local) stored() (store.State, store.Account, error) {
	st, err := store.LoadState(l.o.Dirs.State)
	if err != nil {
		return store.State{}, store.Account{}, stateError(err)
	}
	if err := phaseError(st); err != nil {
		return st, store.Account{}, err
	}
	accounts, err := l.secrets.Load()
	if errors.Is(err, store.ErrRolledBack) {
		l.revokeHeld()
	}
	if err != nil {
		return st, store.Account{}, storeError(err, l.keyFile())
	}
	acc, ok := accounts[st.AccountID]
	if !ok {
		return st, store.Account{}, mustAct("no stored Proton session", "run pcb login")
	}
	return st, acc, nil
}

// phaseError is nil for a ready login, else what the person must do.
func phaseError(st store.State) error {
	switch st.Phase {
	case store.PhaseReady:
		return nil
	case store.PhaseNeedsCaptcha:
		if st.Captcha == nil {
			return mustAct("Proton asks for a CAPTCHA", "run pcb login")
		}
		return mustAct("Proton asks for a CAPTCHA before pcb can log in again: solve it at "+st.Captcha.URL,
			"pcb login resume --captcha-token "+shellquote.Quote(st.Captcha.Token))
	case store.PhaseNeedsLogin:
		reason := st.Reason
		if reason == "" {
			reason = sessionEnded
		}
		return mustAct(reason, "run pcb login")
	}
	return mustAct("not logged in to Proton", "run pcb login")
}

// session returns a client on the stored session, its account and the state
// it was restored from (the basis of any later needs-login decision). The
// client's refresh rotates the sealed tokens (refreshStored).
func (l *Local) session(ctx context.Context) (*api.Client, store.Account, store.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, store.Account{}, store.State{}, l.fail(err)
	}
	st, acc, err := l.stored()
	if err != nil {
		return nil, store.Account{}, store.State{}, err
	}
	return l.storedClient(acc), acc, st, nil
}

// storedClient returns the one client of acc's session. A client built for
// another account or UID is replaced. A reused client adopts the sealed
// tokens when they differ (another process rotated them). acc was read
// before the lock, so a refresh sealed in between can make the adopted tokens
// older than the client's: that costs at most one 401, healed by
// refreshStored's adoption of the sealed tokens, and never spends a refresh
// token. The lock is held for memory work only.
func (l *Local) storedClient(acc store.Account) *api.Client {
	sealed := tokensOf(acc)
	l.cmu.Lock()
	defer l.cmu.Unlock()
	if cc := l.cached; cc != nil && cc.accountID == acc.ID && cc.uid == acc.UID {
		if cc.c.Tokens() != sealed {
			cc.c.SetTokens(sealed)
		}
		return cc.c
	}
	c := l.newClient(sealed, l.refreshStored(acc.ID))
	l.cached = &cachedClient{accountID: acc.ID, uid: acc.UID, c: c}
	return c
}

func tokensOf(a store.Account) api.Tokens {
	return api.Tokens{UID: a.UID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken}
}

// refreshStored returns the refresh of account id's sealed session. It runs
// under the secrets lock, so the processes sharing the state directory
// refresh once (refresh tokens are single-use, R23b): when the sealed refresh
// token is no longer the stale one, another process already rotated it, and
// its tokens are adopted with no network call; else Proton rotates them and
// they are sealed before the refresh returns.
//
// If sealing fails after Proton rotated the tokens, the new tokens are lost
// and the old refresh token is spent: the error is returned (Blocked), and
// the next run finds the session ended (relogin, or needs-login).
func (l *Local) refreshStored(id string) func(context.Context, api.Tokens) (api.Tokens, error) {
	return func(ctx context.Context, stale api.Tokens) (api.Tokens, error) {
		var fresh api.Tokens
		var refreshErr error
		err := l.secrets.Update(ctx, func(accounts map[string]store.Account) error {
			acc, ok := accounts[id]
			if !ok {
				return mustAct("pcb was logged out meanwhile", "run pcb login")
			}
			if acc.RefreshToken != stale.RefreshToken {
				fresh = tokensOf(acc)
				return errAdopted
			}
			t, err := auth.Refresh(ctx, l.mgr, stale)
			if err != nil {
				refreshErr = err
				return err
			}
			acc.AccessToken, acc.RefreshToken = t.AccessToken, t.RefreshToken
			accounts[id] = acc
			fresh = t
			return nil
		})
		switch {
		case errors.Is(err, errAdopted):
			return fresh, nil
		case refreshErr != nil:
			return api.Tokens{}, refreshErr
		case err != nil:
			return api.Tokens{}, storeError(err, l.keyFile())
		}
		return fresh, nil
	}
}

// errKeysChanged is returned by an operation when the sealed key passphrase
// no longer unlocks the account keys.
var errKeysChanged = errors.New("account keys no longer unlock")

// withSession runs op on the stored session. When Proton says the session
// has ended, the login is repaired once (relogin) and op runs again; if the
// repaired session ends too, the login is marked needs-login, so later runs
// do not relogin again without a person.
func (l *Local) withSession(ctx context.Context, op func(context.Context, *api.Client, store.Account) error) error {
	c, acc, st, err := l.session(ctx)
	if err != nil {
		return err
	}
	err = op(ctx, c, acc)
	if !errors.Is(err, api.ErrSessionEnded) {
		return l.opError(ctx, st, err)
	}
	if c, acc, st, err = l.relogin(ctx, c.Tokens()); err != nil {
		return err
	}
	err = op(ctx, c, acc)
	if errors.Is(err, api.ErrSessionEnded) {
		return l.humanNeeded(ctx, st, mustAct(sessionEnded, "run pcb login"))
	}
	return l.opError(ctx, st, err)
}

// opError maps what an operation returned; keys that no longer unlock mark
// the login needs-login.
func (l *Local) opError(ctx context.Context, st store.State, err error) error {
	if errors.Is(err, errKeysChanged) {
		return l.humanNeeded(ctx, st, mustAct(keysChanged, "run pcb login"))
	}
	return l.fail(err)
}

// relogin repairs a session that ended with the tokens ended. When the store
// already holds another session (another process relogged in), that one is
// used. Otherwise, with unattended relogin, it logs in once with the sealed
// credentials; without, the login is marked needs-login.
func (l *Local) relogin(ctx context.Context, ended api.Tokens) (*api.Client, store.Account, store.State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return nil, store.Account{}, store.State{}, errClosed()
	}
	st, acc, err := l.stored()
	if err != nil {
		return nil, store.Account{}, store.State{}, err
	}
	if acc.UID != ended.UID || acc.RefreshToken != ended.RefreshToken {
		return l.session(ctx)
	}
	if !st.Unattended || acc.Relogin == nil {
		return nil, store.Account{}, store.State{}, l.humanNeeded(ctx, st, mustAct(sessionEnded, "run pcb login"))
	}
	if err := l.reloginOnce(ctx, st, acc); err != nil {
		return nil, store.Account{}, store.State{}, err
	}
	return l.session(ctx)
}

// reloginOnce is the login flow with the sealed password and the TOTP code
// of the sealed URI, under the login guard. A CAPTCHA marks the login
// needs-captcha and a failure a person must fix (MustAct) needs-login, so no
// further automatic attempt is made; a transient failure (Blocked: too soon,
// Proton down) leaves the state as it was. That includes a 429 whose
// Retry-After is too long to wait: nothing is persisted but the guard's
// stamp, so the next run retries once the guard allows it.
func (l *Local) reloginOnce(ctx context.Context, st store.State, acc store.Account) error {
	var totp *otp.TOTP
	if uri := acc.Relogin.TOTPURI; uri != "" {
		var err error
		if totp, err = otp.Parse(uri); err != nil {
			return l.humanNeeded(ctx, st, mustAct("the stored TOTP secret cannot be read", "run pcb login"))
		}
	}
	release, now, err := l.acquire(ctx, false, l.o.Now())
	if err != nil {
		return err
	}
	defer release()
	password := []byte(acc.Relogin.Password)
	defer clear(password)
	p, err := auth.Begin(ctx, l.mgr, acc.Username, password, "")
	var ce *auth.CaptchaError
	switch {
	case errors.As(err, &ce):
		if _, err := l.needsCaptcha(ctx, ce, st, now, acc.Username); err != nil {
			return err
		}
		return phaseError(store.State{Phase: store.PhaseNeedsCaptcha, Captcha: &store.Captcha{URL: ce.URL, Token: ce.Token}})
	case err != nil:
		return l.humanNeeded(ctx, st, l.fail(err))
	}
	switch {
	case p.UserID != acc.ID:
		p.Abort(ctx)
		return l.humanNeeded(ctx, st, mustAct("the stored credentials now open another Proton account", "run pcb login"))
	case p.NeedsTOTP && totp == nil:
		p.Abort(ctx)
		return l.humanNeeded(ctx, st, mustAct("Proton now asks for a 2FA code and no TOTP secret is stored", "run pcb login"))
	}
	pl := &pendingLogin{p: p, username: acc.Username, password: password, unattended: true, relogin: true, totpURI: acc.Relogin.TOTPURI, last: st}
	if _, err := l.complete(ctx, pl, totp, now); err != nil {
		if !pl.sealed {
			l.abort(ctx, pl)
		}
		return l.humanNeeded(ctx, st, l.fail(err))
	}
	p.Close()
	return nil
}

// humanNeeded returns err; when err is a MustAct, the login is first marked
// needs-login with err's message as the reason (it is secret-free: the
// service wrote it). st is the state the decision was based on: the mark is
// written only if state.json still says the same, so a login that finished
// meanwhile (in another process) is never overwritten. The check and the
// write are one store.UpdateState, under state.lock, which every writer of
// state.json takes (R10m): no login lands between them. The lock is held for
// the file's load and write only, never across a login waiting for its 2FA
// code.
//
// The mark is a durable decision (it stops further relogins), so it is
// written even when ctx is cancelled; the wait for the lock is bounded by
// its timeout (stateTimeout in tests).
func (l *Local) humanNeeded(ctx context.Context, st store.State, err error) error {
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.MustAct {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if l.stateTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.stateTimeout)
		defer cancel()
	}
	loaded := false
	uerr := store.UpdateStateContext(ctx, l.o.Dirs.State, func(cur *store.State) error {
		loaded = true
		if !sameLogin(*cur, st) {
			return store.ErrStateUnchanged
		}
		if l.humanHook != nil {
			l.humanHook()
		}
		cur.Phase, cur.Reason, cur.Captcha = store.PhaseNeedsLogin, ee.Err.Error(), nil
		return nil
	})
	switch {
	case uerr == nil:
		return err
	case errors.Is(uerr, store.ErrStateLock):
		return stateLockError(uerr, l.o.Dirs.State)
	case !loaded:
		return stateError(uerr) // state.json could not be read
	}
	return storeError(uerr, l.keyFile())
}

// sameLogin reports whether a and b describe the same login: no login,
// relogin or logout happened between them.
func sameLogin(a, b store.State) bool {
	return a.Phase == b.Phase && a.AccountID == b.AccountID &&
		a.LastLogin.Equal(b.LastLogin) && a.LastRelogin.Equal(b.LastRelogin)
}
