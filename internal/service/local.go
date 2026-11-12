package service

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// revokeTimeout bounds the best-effort revocation of a session.
const revokeTimeout = 10 * time.Second

// wipeTimeout bounds the local wipe of a logout, which ignores cancellation.
const wipeTimeout = 10 * time.Second

func errClosed() error { return blocked("service closed", "start a new session") }

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

// Local is the in-process API. Operations that change the login (an
// unattended relogin included) run one at a time; Status reads the files
// without waiting for them.
type Local struct {
	o       Options
	secrets *store.Secrets
	mgr     *gpa.Manager

	clients *access.Store   // CalDAV app passwords, <state>/clients.json; survives logout (ruling M3-R17)
	limiter *access.Limiter // failed CalDAV password checks per remote IP

	tmu          sync.Mutex // guards touching and the start of touches
	touching     map[string]bool
	twg          sync.WaitGroup
	tctx         context.Context // ends the background touches (Close)
	tcancel      context.CancelFunc
	touchHook    func(phase, id string) // tests only
	touchTimeout time.Duration

	mu      sync.Mutex // serialises Login, SubmitTOTP, Logout, Close and relogins
	pending *pendingLogin
	waiting atomic.Bool // pending != nil, readable without mu
	closed  atomic.Bool

	cmu    sync.Mutex // guards cached; never held across a network call
	cached *cachedClient

	r renderer // keys and rendered objects of the read API, dropped with cached

	smu    sync.RWMutex // guards served; held for reading while a read uses it
	served *served      // Serve's cache handle, nil outside Serve

	lmu  sync.Mutex // guards live
	live *live      // Serve's live state, nil outside Serve (Health)

	hmu   sync.Mutex           // guards human
	human map[string]time.Time // unanswered human verifications by source, since when

	cals calLocks // one per calendar: a write and the syncer's pass of it run one at a time

	// Tests only: a write's pre-send budget when set (preSendBudget
	// otherwise), and a failure injected at a write's cache steps (fault).
	preSend      time.Duration
	writeFault   func(stage string) error
	humanHook    func()        // runs once a needs-login mark is decided, before its write
	stateTimeout time.Duration // bounds humanNeeded's wait for state.lock when set
}

// cachedClient is the one API client of a stored session, so its limiter and
// refreshed tokens last across calls.
type cachedClient struct {
	accountID, uid string
	c              *api.Client
}

// dropClient forgets the cached session client, the unlocked keys and the
// rendered objects (login, relogin, logout, Close).
func (l *Local) dropClient() {
	l.cmu.Lock()
	l.cached = nil
	l.cmu.Unlock()
	l.r.drop()
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
	l := &Local{
		o:       o,
		secrets: store.NewSecrets(o.Dirs.State, o.Sealer),
		mgr:     auth.NewManager(o.Config.Proton.BaseURL, o.Config.Proton.AppVersion),
		clients: access.NewStore(o.Dirs.State, o.Now),
		limiter: access.NewLimiter(o.Now),

		touching:     map[string]bool{},
		touchTimeout: defaultTouchTimeout,
	}
	l.tctx, l.tcancel = context.WithCancel(context.Background())
	l.r.lru = newLRU(lruCapacity)
	l.secrets.Log = l.serveLog
	return l, nil
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

// client returns an API client on tokens that refreshes them in memory only:
// for a session not (or no longer) sealed, such as a login in progress or a
// session being revoked. The stored session uses session (refreshStored).
func (l *Local) client(tokens api.Tokens) *api.Client {
	return l.newClient(tokens, func(ctx context.Context, stale api.Tokens) (api.Tokens, error) {
		return auth.Refresh(ctx, l.mgr, stale)
	})
}

func (l *Local) newClient(tokens api.Tokens, refresh func(context.Context, api.Tokens) (api.Tokens, error)) *api.Client {
	return api.New(tokens, api.Options{
		BaseURL:    l.o.Config.Proton.BaseURL,
		AppVersion: l.o.Config.Proton.AppVersion,
		HTTP:       l.o.HTTP,
		Sleep:      l.o.Sleep,
		Refresh:    refresh,
	})
}

// revokeHeld ends the session of the cached client, best effort within
// revokeTimeout, and drops the client. It runs when secrets.sealed is found
// rolled back: that session is pcb's newest, sealed only in the file the
// restore replaced, so it is revoked rather than left live and forgotten
// (ruling M5-R3). The restored file's own session is not revoked. The
// revoke runs on a client of the held tokens that refreshes in memory only
// (client): the cached client's refresh goes through the rolled-back store,
// which refuses it, so an expired access token would leave the session alive.
func (l *Local) revokeHeld() {
	l.cmu.Lock()
	cc := l.cached
	l.cmu.Unlock()
	l.dropClient()
	if cc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	_ = auth.Logout(ctx, l.client(cc.c.Tokens()))
}

// revoke ends acc's session, best effort within revokeTimeout.
func (l *Local) revoke(ctx context.Context, acc store.Account) bool {
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	c := l.client(api.Tokens{UID: acc.UID, AccessToken: acc.AccessToken, RefreshToken: acc.RefreshToken})
	return auth.Logout(ctx, c) == nil
}

// Logout implements API. The local wipe runs even when ctx is cancelled, so
// a cancelled caller cannot leave secrets behind; only the revocation (best
// effort) follows ctx.
func (l *Local) Logout(ctx context.Context) (LogoutResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return LogoutResult{}, errClosed()
	}
	l.dropPending(ctx)
	accounts, err := l.secrets.Load()
	if errors.Is(err, store.ErrRolledBack) {
		l.revokeHeld()
	}
	l.dropClient()
	wipe, cancel := context.WithTimeout(context.WithoutCancel(ctx), wipeTimeout)
	defer cancel()
	var res LogoutResult
	var next uint64 // the mirror the wipe moved to: seeds state.json below
	switch {
	case errors.Is(err, seal.ErrNoKey) || errors.Is(err, seal.ErrCorrupt) || errors.Is(err, store.ErrRolledBack):
		// Unopenable for good, or restored (its tokens are not the current
		// session's, and may be someone else's to keep): nothing to revoke
		// with, so wipe what cannot be used. The wipe moves the mirror past
		// it, so the file is refused if restored again.
		res.WasLoggedIn = true
		var werr error
		if next, werr = l.secrets.Wipe(wipe); werr != nil {
			return LogoutResult{}, storeError(werr, l.keyFile())
		}
	case err != nil:
		// Unreadable for another reason (permissions, a newer format): keep it.
		return LogoutResult{}, blocked("the stored secrets cannot be read, so they were kept",
			"check the permissions and the format of secrets.sealed in the state directory, then run pcb logout again")
	default:
		res.WasLoggedIn = len(accounts) > 0
		res.Revoked = len(accounts) > 0
		for _, acc := range accounts {
			if !l.revoke(ctx, acc) {
				res.Revoked = false
			}
		}
		if len(accounts) > 0 {
			var werr error
			if next, werr = l.secrets.Wipe(wipe); werr != nil {
				return LogoutResult{}, storeError(werr, l.keyFile())
			}
		}
	}
	if err := cache.Remove(cache.Path(l.o.Dirs.State)); err != nil {
		return LogoutResult{}, blocked("the secrets are gone but the event cache could not be removed",
			"remove pcb.db (and its -wal and -shm files) from "+l.o.Dirs.State+", then run pcb logout again")
	}
	// Seeded with the wiped generation + 1: over an unreadable state.json
	// (the reset logout is) the mirror write failed and SaveState cannot
	// read the old one, so the mirror must not restart at 0.
	if err := store.SaveStateContext(wipe, l.o.Dirs.State, store.State{Phase: store.PhaseLoggedOut, SecretsGeneration: next}); err != nil {
		return LogoutResult{}, l.stateWriteError(err)
	}
	return res, nil
}

// Status implements API.
func (l *Local) Status(context.Context) (Status, error) {
	if l.closed.Load() {
		return Status{}, errClosed()
	}
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
		var rb *store.RolledBackError
		switch {
		case errors.As(err, &rb):
			s.Secrets, s.Phase, s.Reason = "rolled-back", string(store.PhaseNeedsLogin), rb.Explain()
		case err != nil:
			s.Secrets, s.Phase, s.Reason = "unreadable", string(store.PhaseNeedsLogin), "the stored secrets cannot be opened; see the key file"
		default:
			s.Secrets = "sealed"
			s.Username = accounts[st.AccountID].Username
		}
	} else if st.Phase == store.PhaseReady {
		s.Phase, s.Reason = string(store.PhaseNeedsLogin), "no stored session"
	}
	if l.waiting.Load() {
		s.Phase, s.Reason = string(StateNeedsTOTP), "a login is waiting for its 2FA code"
	}
	s.Sync = l.syncStatus()
	return s, nil
}

// Close implements API: it aborts a login waiting for its code and releases
// the manager. It is idempotent.
func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Swap(true) {
		return nil
	}
	l.dropPending(context.Background())
	l.dropClient()
	l.tmu.Lock() // no touch starts after this: touch checks closed under tmu
	l.tcancel()
	l.tmu.Unlock()
	l.twg.Wait()
	l.mgr.Close()
	return nil
}
