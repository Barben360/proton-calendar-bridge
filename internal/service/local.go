package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
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

// SessionsPage is where a person ends a Proton session pcb cannot revoke.
const SessionsPage = "the Proton web app (Settings → Security → Sessions)"

func errClosed() error { return blocked("service closed", "start a new session") }

// Options build a Local. Sealer, HTTP, Now, Sleep and AfterFunc default to
// the key file of Config, the API client's default, time.Now, a
// context-aware sleep and time.AfterFunc. AfterFunc runs f once d has
// passed, unless stop (true: it will not run) came first; it ends a login
// left waiting for its 2FA code (pendingTTL). Logger gets what the service
// says outside Serve (Serve's own logger wins while it runs); nil discards
// it. Dirs.State is the account's own directory. GuardDir holds the login
// guard (login.lock, last-login) and is shared by every account of a
// process; empty, it is Dirs.State. Limiter counts failed CalDAV password
// checks per remote IP and is shared likewise; nil, the service gets its own.
type Options struct {
	GuardDir  string
	Limiter   *access.Limiter
	Dirs      paths.Dirs
	Config    config.Config
	Sealer    seal.Sealer
	HTTP      *http.Client
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration) error
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
	Logger    *slog.Logger
}

// Local is the in-process API. Operations that change the login (an
// unattended relogin included) run one at a time; Status reads the files
// without waiting for them.
type Local struct {
	o       Options
	secrets *store.Secrets
	mgr     *gpa.Manager

	clients *access.Store               // CalDAV app passwords, <state>/clients.json, sealed by o.Sealer; survives logout (ruling M3-R17)
	slog    atomic.Pointer[slog.Logger] // Serve's logger while it runs (log)
	limiter *access.Limiter             // failed CalDAV password checks per remote IP

	xmu        sync.Mutex // guards clientsErr
	clientsErr string     // the clients.json load failure last logged

	tmu          sync.Mutex // guards touching and the start of touches
	touching     map[string]bool
	twg          sync.WaitGroup
	tctx         context.Context // ends the background touches (Close)
	tcancel      context.CancelFunc
	touchHook    func(phase, id string) // tests only
	touchTimeout time.Duration

	mu      sync.Mutex // serialises Login, SubmitTOTP, Logout, Close and relogins
	pending *pendingLogin
	waiting atomic.Pointer[time.Time] // pending's expiry while pending != nil, readable without mu
	who     atomic.Pointer[loginWho]  // pending's username and user ID while pending != nil, readable without mu
	expired bool                      // the last pending login expired (SubmitTOTP says so); guarded by mu
	closed  atomic.Bool

	cmu    sync.Mutex // guards cached; never held across a network call
	cached *cachedClient

	r renderer // keys and rendered objects of the read API, dropped with cached

	smu    sync.RWMutex // guards served; held for reading while a read uses it
	served *served      // Serve's cache handle, nil outside Serve

	lmu  sync.Mutex // guards live
	live *live      // Serve's live state, nil outside Serve (Health)

	gmu       sync.Mutex    // guards waitSince, halts, logouts and phase
	waitSince *time.Time    // when Serve began waiting for a login, nil when it does not wait
	halts     int           // logouts in progress: Serve does not start serving meanwhile
	logouts   uint64        // logouts begun in this process (serveOn refuses a login read before one)
	phase     *servePhase   // Serve's serving phase, nil when it does not serve
	loginWake chan struct{} // a login completed in this process (capacity 1)

	// unlisted is Serve's cache handle until its first pass that got
	// through: an unknown calendar is pending meanwhile (ruling M5-R9).
	unlisted atomic.Pointer[cache.Cache]

	hmu   sync.Mutex           // guards human
	human map[string]time.Time // unanswered human verifications by source, since when

	cals calLocks // one per calendar: a write and the syncer's pass of it run one at a time

	syncHeld atomic.Int32 // sync locks this process holds (lockSync): Serve, a sync or a watch

	// Tests only: a write's pre-send budget when set (preSendBudget
	// otherwise), and a failure injected at a write's cache steps (fault).
	preSend      time.Duration
	writeFault   func(stage string) error
	humanHook    func()                                 // runs once a needs-login mark is decided, before its write
	keyPassHook  func([]byte)                           // sees a login's key passphrase before it is sealed
	skipSRP      func(username string) *auth.Pending    // replaces Login's SRP step when set
	stateTimeout time.Duration                          // bounds humanNeeded's wait for state.lock when set
	checkHook    func()                                 // runs at each login check of a Serve waiting for a login
	wipeHook     func()                                 // runs in Logout just before the wipe
	serveHook    func()                                 // runs between a ready login check and serveOn
	pollHook     func(d time.Duration) <-chan time.Time // replaces the login poll's timer

	// skipRelogin replaces an unattended relogin's SRP step when set.
	skipRelogin func(username string) (*auth.Pending, error)
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

// NewLocal returns the service for o. It touches no network, and no file
// but the temporary files a crash left (atomicfile.RemoveStale, older than
// an hour), which it removes: .tmp-* and .key-* in the state and config
// directories, .key-* only in the key file's directory.
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
	if o.AfterFunc == nil {
		o.AfterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if o.GuardDir == "" {
		o.GuardDir = o.Dirs.State
	}
	if o.Limiter == nil {
		o.Limiter = access.NewLimiter(o.Now)
	}
	l := &Local{
		o:       o,
		secrets: store.NewSecrets(o.Dirs.State, o.Sealer),
		mgr:     auth.NewManager(o.Config.Proton.BaseURL, o.Config.Proton.AppVersion),
		clients: access.NewStore(o.Dirs.State, o.Now),
		limiter: o.Limiter,

		touching:     map[string]bool{},
		touchTimeout: defaultTouchTimeout,
		loginWake:    make(chan struct{}, 1),
	}
	l.clients.Sealer = o.Sealer
	l.tctx, l.tcancel = context.WithCancel(context.Background())
	l.r.lru = newLRU(lruCapacity)
	l.secrets.Log = l.serveLog
	for _, dir := range []string{o.Dirs.State, o.Dirs.Config} {
		if dir != "" {
			atomicfile.RemoveStale(dir, o.Now(), atomicfile.TempPrefix, atomicfile.KeyPrefix)
		}
	}
	// The key file's directory is the person's choice and may be shared
	// (a home directory): only the key file's own temporary files go.
	atomicfile.RemoveStale(filepath.Dir(o.Config.Secrets.KeyFile), o.Now(), atomicfile.KeyPrefix)
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

// log is Serve's logger while Serve runs, else Options.Logger, else a
// discarding one.
func (l *Local) log() *slog.Logger {
	if lg := l.slog.Load(); lg != nil {
		return lg
	}
	if l.o.Logger != nil {
		return l.o.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// keyFile is the path named in store errors.
// The sealers' names (seal.Sealer.Name, layout.json's "sealer").
const (
	SealerKeyFile       = "keyfile"
	SealerSecretService = "secret-service"
)

// keyringRef stands for the desktop keyring where the error mappers take a
// key file's path: the key of a secret-service state is no file.
const keyringRef = "<the desktop keyring>"

// keyFile is the key file's path, or keyringRef for a secret-service state;
// it is only a name for the messages (Status.KeyFile reads the config).
func (l *Local) keyFile() string {
	if l.o.Sealer.Name() == SealerSecretService {
		return keyringRef
	}
	return l.o.Config.Secrets.KeyFile
}

// keyFix is the hint for a key that cannot be used, by where it lives.
func (l *Local) keyFix() string {
	if l.keyFile() == keyringRef {
		return keyringFailFix
	}
	return keyFix
}

// keyWhat names where the sealing key lives, for a message: "the key file
// <path>" or "the desktop keyring".
func (l *Local) keyWhat() string {
	if f := l.keyFile(); f != keyringRef {
		return "the key file " + f
	}
	return "the desktop keyring"
}

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
		Logger:     l.log(),
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
//
// During Serve, the sync loop is stopped and its cache handle closed first
// (ruling M5-P10), before mu is taken: a pass may need mu to end. The event
// cache is removed under the sync lock, so no sync starts meanwhile; when
// another process holds it (pcb sync, its watch, or pcb serve), a pass of
// it that read the session before the wipe may write the cache again, and
// the result says so (SyncRunning, state Minor).
func (l *Local) Logout(ctx context.Context, req LogoutRequest) (LogoutResult, error) {
	release := l.haltServe()
	defer release()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return LogoutResult{}, errClosed()
	}
	accounts, err := l.secrets.Load()
	var ue *store.UnsupportedError
	if errors.As(err, &ue) && !req.Discard {
		// A newer pcb could still read it: never discarded unasked, and the
		// refusal changes nothing, a login waiting for its code included
		// (state-I1).
		return LogoutResult{}, mustAct(ue.Explain()+", so pcb cannot revoke the Proton session",
			"end the session in "+SessionsPage+", then run pcb logout --yes")
	}
	l.dropPending(ctx)
	l.expired = false
	if errors.Is(err, store.ErrRolledBack) {
		l.revokeHeld()
	}
	l.dropClient()
	if l.wipeHook != nil {
		l.wipeHook()
	}
	wipe, cancel := context.WithTimeout(context.WithoutCancel(ctx), wipeTimeout)
	defer cancel()
	var res LogoutResult
	var next uint64 // the mirror the wipe moved to: seeds state.json below
	switch {
	case errors.Is(err, seal.ErrNoKey) || errors.Is(err, seal.ErrCorrupt) || errors.Is(err, store.ErrRolledBack) ||
		errors.Is(err, store.ErrUnsupported):
		// Unopenable for good, restored (its tokens are not the current
		// session's, and may be someone else's to keep), or one this pcb
		// cannot read that the person chose to discard: nothing to revoke
		// with, so wipe what cannot be used. The wipe moves the mirror past
		// it, so the file is refused if restored again.
		res.WasLoggedIn = true
		var werr error
		if next, werr = l.secrets.Wipe(wipe); werr != nil {
			return LogoutResult{}, storeError(werr, l.keyFile())
		}
	case err != nil:
		// Unreadable for another reason (permissions): keep it.
		msg := "cannot read the stored secrets, so they were kept"
		if cause := keyCause(err); cause != "" {
			msg = "cannot read the stored secrets (" + cause + "), so they were kept"
		}
		return LogoutResult{}, keyFailure(err, msg, "make secrets.sealed in "+l.o.Dirs.State+" readable by the user pcb runs as, then run pcb logout again")
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
	unlockSync, held := l.logoutSyncLock()
	err = cache.Remove(cache.Path(l.o.Dirs.State))
	unlockSync()
	if err != nil {
		return LogoutResult{}, blocked("the secrets are gone but the event cache could not be removed",
			"remove pcb.db (and its -wal and -shm files) from "+l.o.Dirs.State+", then run pcb logout again")
	}
	res.SyncRunning = held
	// Seeded with the wiped generation + 1: over an unreadable state.json
	// (the reset logout is) the mirror write failed and SaveState cannot
	// read the old one, so the mirror must not restart at 0.
	// The address, the CalDAV switch and the creation time are not login
	// fields: they stay. A state.json too damaged to read loses them.
	kept, _ := store.LoadState(l.o.Dirs.State)
	reset := store.State{Phase: store.PhaseLoggedOut, SecretsGeneration: next, Address: kept.Address, CalDAV: kept.CalDAV, Created: kept.Created}
	if err := store.SaveStateContext(wipe, l.o.Dirs.State, reset); err != nil {
		return LogoutResult{}, l.stateWriteError(err)
	}
	return res, nil
}

// logoutSyncLock takes the sync lock for a logout's removal of the cache,
// without waiting: unless this process holds it (its Serve, whose passes
// haltServe stopped). held reports that another process holds it. Off Unix,
// or when the lock cannot be created, there is nothing to take.
func (l *Local) logoutSyncLock() (unlock func(), held bool) {
	if l.syncHeld.Load() > 0 {
		return func() {}, false
	}
	unlock, ok, err := filelock.TryLock(filepath.Join(l.o.Dirs.State, syncLockName))
	switch {
	case err != nil:
		return func() {}, false
	case !ok:
		return func() {}, true
	}
	return unlock, false
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
		Sealer:     l.o.Sealer.Name(),
		Unattended: st.Unattended,
		Address:    st.Address,
	}
	if s.Sealer == SealerKeyFile {
		s.KeyFile = l.o.Config.Secrets.KeyFile
		// The keyring's sealer has no file to be absent.
		if _, err := os.Stat(s.KeyFile); errors.Is(err, os.ErrNotExist) {
			s.KeyFileAbsent = true
		}
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
		var (
			rb *store.RolledBackError
			ue *store.UnsupportedError
		)
		unreadable := "the stored secrets cannot be opened; see the key file"
		if l.keyFile() == keyringRef {
			unreadable = "the stored secrets cannot be opened with the key in the desktop keyring"
		}
		switch {
		case errors.As(err, &rb):
			s.Secrets, s.Phase, s.Reason = "rolled-back", string(store.PhaseNeedsLogin), rb.Explain()
		case errors.As(err, &ue):
			s.Secrets, s.Phase, s.Reason = SecretsUnsupported, string(store.PhaseNeedsLogin), ue.Explain()
		case errors.Is(err, seal.ErrLocked):
			s.Secrets = "locked" // the keyring unlocks with the desktop session: nothing is lost
		case errors.Is(err, seal.ErrNoService):
			s.Secrets = SecretsKeyringUnavailable // no keyring answers here: nothing is lost either (R15)
		case err != nil:
			s.Secrets, s.Phase, s.Reason = "unreadable", string(store.PhaseNeedsLogin), unreadable
		default:
			s.Secrets = "sealed"
			s.Username = accounts[st.AccountID].Username
		}
	} else if st.Phase == store.PhaseReady {
		s.Phase, s.Reason = string(store.PhaseNeedsLogin), "no stored session"
	}
	if exp := l.waiting.Load(); exp != nil {
		s.Phase, s.Reason = string(StateNeedsTOTP), "a login is waiting for its 2FA code"
		t := *exp
		s.LoginExpires = &t
	}
	s.Sync = l.syncStatus()
	if l.clients != nil {
		if _, err := l.clients.List(); err != nil {
			s.ClientsFile = filepath.Join(l.o.Dirs.State, "clients.json")
		}
	}
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
