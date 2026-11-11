package service

import (
	"context"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// revokeTimeout bounds the best-effort revocation of a session.
const revokeTimeout = 10 * time.Second

// Options build a Local. Sealer, HTTP, Now and Sleep default to the key file
// of Config, the API client's default, time.Now and a context-aware sleep.
type Options struct {
	Dirs   paths.Dirs
	Config config.Config
	Sealer seal.Sealer
	HTTP   *http.Client
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
}

// Local is the in-process API. Operations that change the login run one at a
// time; Status reads the files without waiting for them.
type Local struct {
	o       Options
	secrets *store.Secrets
	mgr     *gpa.Manager

	mu      sync.Mutex // serialises Login, SubmitTOTP, Logout and Close
	pending *pendingLogin
	waiting atomic.Bool // pending != nil, readable without mu
	closed  bool
}

var _ API = (*Local)(nil)

// NewLocal returns the service for o. It touches no file and no network.
func NewLocal(o Options) (*Local, error) {
	if o.Dirs.State == "" || o.Config.Proton.BaseURL == "" || o.Config.Secrets.KeyFile == "" {
		return nil, usage("the service needs the state directory, the Proton base URL and the key file", "load the configuration with config.Load")
	}
	if o.Sealer == nil {
		o.Sealer = &seal.KeyFile{Path: o.Config.Secrets.KeyFile}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	return &Local{
		o:       o,
		secrets: store.NewSecrets(o.Dirs.State, o.Sealer),
		mgr:     auth.NewManager(o.Config.Proton.BaseURL, o.Config.Proton.AppVersion),
	}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// keyFile is the path named in store errors.
func (l *Local) keyFile() string { return l.o.Config.Secrets.KeyFile }

func (l *Local) fail(err error) error { return mapError(err, l.keyFile()) }

// client returns an API client on tokens that refreshes them like the manager.
func (l *Local) client(tokens api.Tokens) *api.Client {
	return api.New(tokens, api.Options{
		BaseURL:    l.o.Config.Proton.BaseURL,
		AppVersion: l.o.Config.Proton.AppVersion,
		HTTP:       l.o.HTTP,
		Sleep:      l.o.Sleep,
		Refresh: func(ctx context.Context, stale api.Tokens) (api.Tokens, error) {
			return auth.Refresh(ctx, l.mgr, stale)
		},
	})
}

// revoke ends acc's session, best effort within revokeTimeout.
func (l *Local) revoke(ctx context.Context, acc store.Account) bool {
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	c := l.client(api.Tokens{UID: acc.UID, AccessToken: acc.AccessToken, RefreshToken: acc.RefreshToken})
	return auth.Logout(ctx, c) == nil
}

// Logout implements API.
func (l *Local) Logout(ctx context.Context) (LogoutResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropPending(ctx)
	var res LogoutResult
	accounts, err := l.secrets.Load()
	if err != nil {
		// Unreadable: nothing to revoke with, so wipe what cannot be used.
		res.WasLoggedIn = true
		if werr := l.secrets.Wipe(ctx); werr != nil {
			return LogoutResult{}, storeError(werr, l.keyFile())
		}
	} else {
		res.WasLoggedIn = len(accounts) > 0
		res.Revoked = len(accounts) > 0
		for _, acc := range accounts {
			if !l.revoke(ctx, acc) {
				res.Revoked = false
			}
		}
		if len(accounts) > 0 {
			if err := l.secrets.Update(ctx, func(a map[string]store.Account) error {
				clear(a)
				return nil
			}); err != nil {
				return LogoutResult{}, storeError(err, l.keyFile())
			}
		}
	}
	if err := store.SaveState(l.o.Dirs.State, store.State{Phase: store.PhaseLoggedOut}); err != nil {
		return LogoutResult{}, storeError(err, l.keyFile())
	}
	return res, nil
}

// Status implements API.
func (l *Local) Status(context.Context) (Status, error) {
	st, err := store.LoadState(l.o.Dirs.State)
	if err != nil {
		return Status{}, stateError(err)
	}
	s := Status{
		Phase:      string(st.Phase),
		Reason:     st.Reason,
		AccountID:  st.AccountID,
		Secrets:    "none",
		KeyFile:    l.keyFile(),
		Unattended: st.Unattended,
	}
	if st.Captcha != nil {
		s.CaptchaURL = st.Captcha.URL
	}
	if !st.LastLogin.IsZero() {
		s.LastLogin = &st.LastLogin
	}
	if !st.LastRelogin.IsZero() {
		s.LastRelogin = &st.LastRelogin
	}
	if _, err := os.Stat(l.secrets.Path); err == nil {
		accounts, err := l.secrets.Load()
		if err != nil {
			s.Secrets, s.Phase, s.Reason = "unreadable", string(store.PhaseNeedsLogin), "the stored secrets cannot be opened; see the key file"
		} else {
			s.Secrets = "sealed"
			s.Username = accounts[st.AccountID].Username
		}
	} else if st.Phase == store.PhaseReady {
		s.Phase, s.Reason = string(store.PhaseNeedsLogin), "no stored session"
	}
	if l.waiting.Load() {
		s.Phase, s.Reason = string(StateNeedsTOTP), "a login is waiting for its 2FA code"
	}
	return s, nil
}

// Calendars implements API. Task 9 fills it in.
func (l *Local) Calendars(context.Context) ([]Calendar, error) {
	return nil, blocked("listing calendars is not available yet", "wait for the next release")
}

// Close implements API: it aborts a login waiting for its code and releases
// the manager. It is idempotent.
func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	l.dropPending(context.Background())
	l.mgr.Close()
	return nil
}
