package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

const (
	// pendingExpiry is how long a pending add (stopped by a CAPTCHA) is
	// kept: the first Hub open after it removes the account (design §4.3).
	pendingExpiry = 24 * time.Hour
	// abandonedAge is the age past which OpenHub removes an abandoned add's
	// directory (ruling R11): a younger one may be another process's add,
	// waiting for its 2FA code for up to pendingTTL.
	abandonedAge = pendingTTL + time.Minute
	// layoutLockName serialises the migrations of the state layout.
	layoutLockName = "layout.lock"
)

// HubOptions build a Hub: the Options every account's Local shares, with
// Dirs.State the state root. GuardDir is the root and the Limiter and the
// Sealer are one for every account.
type HubOptions struct {
	Options
	// Keyring is the desktop keyring a new state may seal with and an
	// existing secret-service state does; nil means seal.DBusKeyring("").
	Keyring Keyring
}

// Keyring is the desktop keyring (freedesktop Secret Service) the Hub
// seals with when the state's layout says secret-service: the same method
// set as seal.Keyring, so a seal.Keyring value is one, and callers that
// may not import the store (internal/ui) can pass one.
type Keyring interface {
	Get(ctx context.Context, attrs map[string]string) ([]byte, error)
	Set(ctx context.Context, label string, attrs map[string]string, secret []byte) error
	Locked(ctx context.Context) (bool, error)
}

// Hub holds every account of a state directory, one Local per account
// directory (accounts/<local-id>/). It adds an account by login, signs one
// out, removes it and switches its CalDAV adapter.
//
// An account is pending while its add has not completed a first login:
// stopped by a CAPTCHA (it expires pendingExpiry after its creation), or
// waiting for its 2FA code in this Hub. An abandoned add (no Proton user,
// never logged in, no data, no CAPTCHA and no 2FA code awaited here) is
// removed lazily (ruling R11).
type Hub struct {
	o    Options // shared by every Local; Dirs.State is the root
	root string

	logins atomic.Bool // a Login or SubmitTOTP runs: another is refused
	ops    sync.Mutex  // serialises what changes the accounts: logins, sign-outs, removals, switches, sweeps

	mu       sync.Mutex // guards accounts, closed, changes, onOpen, stops and onPark; never held across a Local call
	accounts map[string]*hubAccount
	closed   bool
	changes  []func()
	onOpen   func(*Local)    // tests only: runs on every Local the Hub opens
	stops    uint64          // the parks and waits of the serve loops so far, in order
	onPark   func(id string) // tests only: runs once a loop parked its account and cleared its loop

	// srv serialises the start and the stop of the serve loops (loops,
	// Serve's start and end). Taken after ops, before mu; never held by a
	// loop.
	srv     sync.Mutex
	serving *hubServe // Serve's run, nil outside Serve; guarded by srv
}

// hubAccount is one account of the Hub.
type hubAccount struct {
	id, dir string
	local   *Local

	// Guarded by Hub.mu: stopped and stopErr are set when the account's
	// serve loop ended on an error not caused by its context (parked);
	// state is the loop's last state ("" while it starts), waitErr why it
	// waits for a login; stopSeq orders its last park or wait among the
	// accounts' (Hub.stops).
	stopped *StopInfo
	stopErr error
	state   ServeState
	waitErr error
	stopSeq uint64

	// gate is held for reading by every call through the API Account
	// hands out, and for writing while the account is deleted (retire),
	// so no call in flight writes into a directory being removed.
	gate    sync.RWMutex
	removed bool // the account is gone: calls through its API fail; written under gate

	lmu      sync.Mutex                  // guards removing, long and loop; taken before Hub.mu when both are held
	removing bool                        // retire has begun: a long call starts already ended, no loop starts
	long     map[*int]context.CancelFunc // the long calls in flight (Serve, WatchSync), ended by retire
	loop     *accountLoop                // the Hub's serve loop of the account, nil when none runs
}

// enter admits a call through a's API, or refuses it once a is removed.
func (a *hubAccount) enter() (func(), error) {
	a.gate.RLock()
	if a.removed {
		a.gate.RUnlock()
		return nil, noAccount(a.id, "see pcb account list")
	}
	return a.gate.RUnlock, nil
}

// enterLong admits a long call (Serve, WatchSync) on a context retire
// cancels.
func (a *hubAccount) enterLong(ctx context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	key := new(int)
	a.lmu.Lock()
	if a.removing {
		cancel()
	}
	if a.long == nil {
		a.long = map[*int]context.CancelFunc{}
	}
	a.long[key] = cancel
	a.lmu.Unlock()
	leave, err := a.enter()
	end := func() {
		a.lmu.Lock()
		delete(a.long, key)
		a.lmu.Unlock()
		cancel()
	}
	if err != nil {
		end()
		return nil, nil, err
	}
	return ctx, func() { leave(); end() }, nil
}

// retire deletes a: it ends the long calls through its API and its serve
// loop, waiting for the loop to end (ruling R18), waits for the other
// calls, runs before (whose error leaves a as it was: gone false; the
// caller starts its loop again), then closes a's Local and removes its
// directory with no call in flight (gone true, err the removal's). Calls
// through its API then fail.
func (a *hubAccount) retire(before func() error) (gone bool, err error) {
	a.lmu.Lock()
	a.removing = true
	for _, cancel := range a.long {
		cancel()
	}
	run := a.loop
	a.lmu.Unlock()
	run.stop()
	a.gate.Lock()
	defer a.gate.Unlock()
	if before != nil {
		if err := before(); err != nil {
			a.lmu.Lock()
			a.removing = false
			a.lmu.Unlock()
			return false, err
		}
	}
	a.removed = true
	_ = a.local.Close()
	return true, os.RemoveAll(a.dir)
}

// OpenHub opens the accounts of o.Dirs.State. A state without layout.json
// (M5's, or a new one) is first moved to the per-account layout, under the
// root's layout.lock and sync.lock, both taken without waiting: an M5 pcb
// holding sync.lock is Blocked (ruling R2: with the layout in place neither
// is touched). A new state (no layout.json, no account, no legacy file, no
// interrupted migration) instead gets its sealer chosen (chooseSealer)
// and recorded in layout.json under the same locks; an existing layout is
// never probed, and o.Sealer, when set, wins. It removes the pending adds
// older than pendingExpiry and the abandoned adds older than abandonedAge,
// and opens a Local per other account directory.
func OpenHub(o HubOptions) (*Hub, error) {
	base := o.Options
	if base.Dirs.State == "" || base.Config.Proton.BaseURL == "" || base.Config.Secrets.KeyFile == "" {
		return nil, usage("the service needs the state directory, the Proton base URL and the key file", "load the configuration with config.Load")
	}
	if base.Now == nil {
		base.Now = time.Now
	}
	if base.Limiter == nil {
		base.Limiter = access.NewLimiter(base.Now)
	}
	root := base.Dirs.State
	base.GuardDir = root
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, blocked("cannot create the state directory "+root, "make sure its parent directory is writable by this user")
	}
	kr := o.Keyring
	if kr == nil {
		kr = seal.DBusKeyring("")
	}
	var choose func() (string, error)
	if base.Sealer == nil {
		choose = func() (string, error) { return chooseSealer(base.Config.Secrets.Backend, kr) }
	}
	info, err := store.ReadLayout(root)
	if err == nil && info.Version != store.LayoutVersion {
		if err = migrateLayout(root, choose); err == nil {
			info, err = store.ReadLayout(root)
		}
	}
	if err != nil {
		return nil, layoutError(err, root, info.Legacy)
	}
	if base.Sealer == nil {
		if base.Sealer, err = NewSealer(root, info.Sealer, base.Config.Secrets.KeyFile, kr); err != nil {
			return nil, err
		}
	}
	h := &Hub{o: base, root: root, accounts: map[string]*hubAccount{}}
	now := base.Now()
	for _, id := range info.Accounts {
		dir := store.AccountDir(root, id)
		switch classify(dir, now) {
		case dirRemove:
			_ = os.RemoveAll(dir)
			continue
		case dirSkip:
			continue
		}
		l, err := h.newLocal(dir)
		if err != nil {
			_ = h.Close()
			return nil, err
		}
		backfillAddress(l)
		h.accounts[id] = &hubAccount{id: id, dir: dir, local: l}
	}
	return h, nil
}

// backfillAddress writes into l's state.json the address an M5 state
// lacks (only an M6 login writes it): the username of its sealed account
// (ruling R16). Best effort: without a readable session it stays empty.
func backfillAddress(l *Local) {
	dir := l.o.Dirs.State
	st, err := store.LoadState(dir)
	if err != nil || st.Address != "" || !exists(l.secrets.Path) {
		return
	}
	accounts, err := l.secrets.Load()
	if err != nil {
		return
	}
	acc, ok := accounts[st.AccountID]
	if !ok || acc.Username == "" {
		return
	}
	_ = store.UpdateState(dir, func(s *store.State) error {
		if s.Address != "" || s.AccountID != st.AccountID {
			return store.ErrStateUnchanged
		}
		s.Address = acc.Username
		return nil
	})
}

// migrateLayout moves root to the per-account layout under its layout.lock
// and its sync.lock, neither waited for. A new state (see OpenHub) is
// instead created with the sealer choose names, when choose is not nil
// (ruling P2); Migrate then finds version 2 and does nothing.
func migrateLayout(root string, choose func() (string, error)) error {
	unlockLayout, err := tryLock(root, layoutLockName, "another pcb is moving the state to the per-account layout", "run the command again in a moment")
	if err != nil {
		return err
	}
	defer unlockLayout()
	unlockSync, err := tryLock(root, syncLockName, "another pcb holds the state", "stop the running pcb first")
	if err != nil {
		return err
	}
	defer unlockSync()
	if choose != nil {
		// Read again under the lock: another pcb may have created it.
		if info, err := store.ReadLayout(root); err == nil && newState(info) {
			name, err := choose()
			if err != nil {
				return err
			}
			if err := store.CreateLayout(root, name); err != nil {
				return blocked("cannot create the state layout in "+root, "make sure "+root+" is writable by this user")
			}
		}
	}
	_, _, err = store.Migrate(root, func() (string, error) {
		return store.NewLocalID(func(id string) bool { return exists(store.AccountDir(root, id)) })
	})
	return err
}

// newState reports whether info is a state nothing was ever written to.
func newState(info store.LayoutInfo) bool {
	return info.Version == 0 && len(info.Accounts) == 0 && len(info.Legacy) == 0 && info.Migrating == ""
}

// ProbeTimeout is how long a new state waits for the desktop keyring to
// answer before it counts as absent.
const ProbeTimeout = 2 * time.Second

// chooseSealer is the sealer a new state records, by [secrets] backend:
// keyfile, secret-service (refused, exit 10, when the keyring cannot be
// used) or auto (secret-service when the keyring answers within
// ProbeTimeout and is unlocked, else keyfile).
func chooseSealer(backend string, kr Keyring) (string, error) {
	if backend == "keyfile" {
		return SealerKeyFile, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	err := seal.Probe(ctx, kr)
	switch {
	case err == nil:
		return SealerSecretService, nil
	case backend == "secret-service":
		return "", mustAct("[secrets] backend is secret-service but the desktop keyring cannot be used: "+err.Error(),
			"unlock the keyring and start a Secret Service (GNOME Keyring, KWallet), or set [secrets] backend to keyfile")
	}
	return SealerKeyFile, nil
}

// NewSealer is the sealer layout.json names for the state root: the key
// file's, or the desktop keyring's with its one key for the whole root
// (its absolute path names the keyring item). pcb doctor builds the
// sealer through it too.
func NewSealer(root, name, keyFile string, kr Keyring) (seal.Sealer, error) {
	if name != SealerSecretService {
		return &seal.KeyFile{Path: keyFile}, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, blocked("cannot resolve the state directory "+root, "give an absolute state directory")
	}
	return &seal.SecretService{Keyring: kr, StateDir: abs}, nil
}

// tryLock takes dir/name without waiting; held, it is Blocked with msg and
// hint. Off Unix there is no lock to take.
func tryLock(dir, name, msg, hint string) (func(), error) {
	unlock, ok, err := filelock.TryLock(filepath.Join(dir, name))
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		return func() {}, nil
	case err != nil:
		return nil, blocked("cannot take "+name+" in "+dir, "make sure "+dir+" is writable by this user")
	case !ok:
		return nil, blocked(msg, hint)
	}
	return unlock, nil
}

// exists reports whether path exists; a doubt counts as existing.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// What OpenHub does with an account directory.
type dirAction int

const (
	dirOpen   dirAction = iota // an account
	dirRemove                  // an expired pending add, or an old abandoned one
	dirSkip                    // a young abandoned add: maybe another process's in flight
)

func classify(dir string, now time.Time) dirAction {
	st, err := store.LoadState(dir)
	switch {
	case err != nil:
		return dirOpen
	case neverLoggedIn(st) && st.Phase == store.PhaseNeedsCaptcha:
		if !st.Created.IsZero() && now.Sub(st.Created) > pendingExpiry {
			return dirRemove
		}
		return dirOpen
	case abandonedState(dir, st):
		fi, err := os.Stat(dir)
		if err != nil {
			return dirOpen
		}
		if now.Sub(fi.ModTime()) > abandonedAge {
			return dirRemove
		}
		return dirSkip
	}
	return dirOpen
}

// neverLoggedIn reports whether st is an account's that never completed a
// login: no Proton user and no secrets ever sealed (a sign-out keeps the
// sealed generation, so a signed-out account is not one).
func neverLoggedIn(st store.State) bool { return st.AccountID == "" && st.SecretsGeneration == 0 }

// abandonedState reports whether the account directory dir, whose
// state.json is st, is an abandoned add (ruling R11): never logged in, not
// stopped by a CAPTCHA, and holding no data (sealed secrets, CalDAV
// clients, event cache). Whether a 2FA code is awaited is the caller's.
func abandonedState(dir string, st store.State) bool {
	if !neverLoggedIn(st) || st.Phase == store.PhaseNeedsCaptcha {
		return false
	}
	db := cache.Path(dir)
	for _, p := range []string{filepath.Join(dir, "secrets.sealed"), filepath.Join(dir, "clients.json"), db, db + "-wal", db + "-shm"} {
		if exists(p) {
			return false
		}
	}
	return true
}

// newLocal opens the Local of the account directory dir.
func (h *Hub) newLocal(dir string) (*Local, error) {
	o := h.o
	o.Dirs.State = dir
	l, err := NewLocal(o)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	hook := h.onOpen
	h.mu.Unlock()
	if hook != nil {
		hook(l)
	}
	return l, nil
}

// list is the Hub's accounts, none once closed.
func (h *Hub) list() []*hubAccount {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*hubAccount, 0, len(h.accounts))
	for _, a := range h.accounts {
		out = append(out, a)
	}
	return out
}

func (h *Hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// described is what an account's files and its Local say of it, with no
// network call and no secret opened.
type described struct {
	st       store.State
	readable bool      // state.json could be read
	held     *loginWho // the login waiting for its 2FA code, nil if none
	address  string
	pending  bool
}

func inspect(a *hubAccount) described {
	d := described{held: a.local.waitingLogin()}
	st, err := store.LoadState(a.dir)
	d.st, d.readable = st, err == nil
	d.address = st.Address
	if d.address == "" && d.held != nil {
		d.address = d.held.username
	}
	d.pending = d.readable && neverLoggedIn(st) && (st.Phase == store.PhaseNeedsCaptcha || d.held != nil)
	return d
}

// info is a's AccountInfo. Its phase is Status's; a state.json that
// cannot be read is needs-login, as Status says of unreadable secrets.
func (h *Hub) info(a *hubAccount) AccountInfo {
	d := inspect(a)
	info := AccountInfo{ID: a.id, Address: d.address, Pending: d.pending, CalDAV: d.st.CalDAV == nil || *d.st.CalDAV}
	h.mu.Lock()
	if a.stopped != nil {
		s := *a.stopped
		info.Stopped = &s
	}
	h.mu.Unlock()
	if s, err := a.local.Status(context.Background()); err == nil {
		info.Phase = s.Phase
	} else {
		info.Phase = string(store.PhaseNeedsLogin)
	}
	return info
}

func (h *Hub) infos(list []*hubAccount) []AccountInfo {
	out := make([]AccountInfo, 0, len(list))
	for _, a := range list {
		out = append(out, h.info(a))
	}
	slices.SortFunc(out, func(x, y AccountInfo) int {
		if c := strings.Compare(strings.ToLower(x.Address), strings.ToLower(y.Address)); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

// Accounts lists the accounts, pending ones included, sorted by address.
func (h *Hub) Accounts() []AccountInfo {
	h.sweep()
	return h.infos(h.list())
}

// Account chooses an account: selector is its local ID or its address
// (case-insensitive), a pending account included; empty, it is the only
// account that is not pending. Unknown or several are Usage errors, none
// ready MustAct (SelectAccount); several carry a *SeveralAccountsError
// listing them.
func (h *Hub) Account(selector string) (API, AccountInfo, error) {
	h.sweep()
	a, err := h.pick(selector)
	if err != nil {
		return nil, AccountInfo{}, err
	}
	return accountAPI{a}, h.info(a), nil
}

func (h *Hub) pick(selector string) (*hubAccount, error) {
	if h.isClosed() {
		return nil, errClosed()
	}
	list := h.list()
	info, err := SelectAccount(lightInfos(list), selector)
	var several *SeveralAccountsError
	if errors.As(err, &several) {
		return nil, severalAccounts(h.infos(byIDs(list, several.Accounts)))
	}
	if err != nil {
		return nil, err
	}
	return byIDs(list, []AccountInfo{info})[0], nil
}

// SelectAccount is the account of list that selector names: its local ID,
// else its address (case-insensitively), a pending account included.
// Empty, it is the only account of list that is not pending. It refuses
// a selector that names none (Usage) and an empty one with no account
// ready (MustAct, ruling R38), both with ErrNoAccount as cause, and an
// empty one with several ready (Usage), with a *SeveralAccountsError
// listing them in list's order.
// Hub.Account chooses with it; so does the CLI, over a daemon's list.
func SelectAccount(list []AccountInfo, selector string) (AccountInfo, error) {
	if selector != "" {
		for _, info := range list {
			if info.ID == selector {
				return info, nil
			}
		}
		for _, info := range list {
			if strings.EqualFold(info.Address, selector) {
				return info, nil
			}
		}
		return AccountInfo{}, noAccount(selector, "see pcb account list")
	}
	var ready []AccountInfo
	pending := 0
	for _, info := range list {
		if info.Pending {
			pending++
		} else {
			ready = append(ready, info)
		}
	}
	switch len(ready) {
	case 1:
		return ready[0], nil
	case 0:
		if pending > 0 {
			return AccountInfo{}, noReadyAccount("finish the pending login (pcb login resume), or add an account with pcb login")
		}
		return AccountInfo{}, noReadyAccount("add one with pcb login")
	}
	return AccountInfo{}, severalAccounts(ready)
}

// lightInfos is what SelectAccount needs of each account of list (its
// local ID, address and pending flag), read without asking its Local.
func lightInfos(list []*hubAccount) []AccountInfo {
	out := make([]AccountInfo, 0, len(list))
	for _, a := range list {
		d := inspect(a)
		out = append(out, AccountInfo{ID: a.id, Address: d.address, Pending: d.pending})
	}
	return out
}

// byIDs is the accounts of list that infos name, in infos' order.
func byIDs(list []*hubAccount, infos []AccountInfo) []*hubAccount {
	out := make([]*hubAccount, 0, len(infos))
	for _, info := range infos {
		for _, a := range list {
			if a.id == info.ID {
				out = append(out, a)
			}
		}
	}
	return out
}

// sweep removes the abandoned adds, unless an operation that changes the
// accounts runs (a login sweeps itself).
func (h *Hub) sweep() {
	if !h.ops.TryLock() {
		return
	}
	changed := h.sweepLocked()
	h.ops.Unlock()
	h.notify(changed)
}

// sweepLocked removes the abandoned adds. It holds ops.
func (h *Hub) sweepLocked() bool {
	changed := false
	for _, a := range h.list() {
		if abandoned(a) {
			h.drop(a)
			changed = true
		}
	}
	return changed
}

// abandoned reports whether a is an abandoned add (ruling R11).
func abandoned(a *hubAccount) bool {
	if a.local.waitingLogin() != nil {
		return false
	}
	st, err := store.LoadState(a.dir)
	return err == nil && abandonedState(a.dir, st)
}

// drop takes a out of the Hub and deletes its directory.
func (h *Hub) drop(a *hubAccount) {
	h.mu.Lock()
	delete(h.accounts, a.id)
	h.mu.Unlock()
	discard(a)
}

// discard closes a's Local and deletes its directory (retire), best
// effort: what is left is removed by a later OpenHub.
func discard(a *hubAccount) {
	_, _ = a.retire(nil)
}

// insert adds a to the Hub; closed meanwhile, it closes a's Local instead
// and reports false.
func (h *Hub) insert(a *hubAccount) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		_ = a.local.Close()
		return false
	}
	h.accounts[a.id] = a
	return true
}

// OnChange makes f run after an account is added (pending included),
// completes its first login, is signed out, is removed, has its CalDAV
// switched, or is parked (from Serve's goroutine) or restarted. f runs on
// the goroutine of the change, outside the Hub's locks: it may call the
// Hub.
func (h *Hub) OnChange(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, f)
}

func (h *Hub) notify(changed bool) {
	if !changed {
		return
	}
	h.mu.Lock()
	callbacks := slices.Clone(h.changes)
	h.mu.Unlock()
	for _, f := range callbacks {
		f()
	}
}

// begin takes the login slot and ops: a second login is refused at once,
// as ctl's oneLogin does.
func (h *Hub) begin() (func(), error) {
	if !h.logins.CompareAndSwap(false, true) {
		return nil, mustAct("a login is already in progress", "wait for it to end, then see pcb status")
	}
	h.ops.Lock()
	return func() {
		h.ops.Unlock()
		h.logins.Store(false)
	}, nil
}

// Login logs in to the account whose address is req.Username
// (case-insensitive), pending or not; with none it adds an account: a new
// directory, removed again if the login fails, kept as a pending account
// when Proton asks for a CAPTCHA or a 2FA code. A Proton user that is
// already another account's is refused (MustAct naming it): the session
// the login opened is revoked and an add's directory removed. One login
// runs at a time. The result's Account is the local ID.
func (h *Hub) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	res, changed, err := h.login(ctx, req)
	h.loops()
	h.notify(changed)
	return res, err
}

func (h *Hub) login(ctx context.Context, req LoginRequest) (LoginResult, bool, error) {
	end, err := h.begin()
	if err != nil {
		return LoginResult{}, false, err
	}
	defer end()
	if h.isClosed() {
		return LoginResult{}, false, errClosed()
	}
	if req.Username == "" || req.Password == "" {
		return LoginResult{}, false, usage("a username and a password are required", "pass the username and the password (PCB_USERNAME, PCB_PASSWORD or the prompt)")
	}
	swept := h.sweepLocked()
	var a *hubAccount
	for _, b := range h.list() {
		if strings.EqualFold(knownAddress(b), req.Username) {
			a = b
			break
		}
	}
	if a == nil {
		a = addressless(h.list())
	}
	added := a == nil
	if added {
		if a, err = h.add(); err != nil {
			return LoginResult{}, swept, err
		}
	}
	fresh := added || freshDir(a.dir)
	res, err := a.local.Login(ctx, req)
	res, changed, err := h.settle(ctx, a, added, fresh, res, err)
	return res, swept || changed, err
}

// addressless is the only account of list signed out with no address,
// nil if there are none or several: an M5 account migrated signed out,
// whose state.json never had one (ruling R16; M5 held one account). An
// account whose state.json cannot be read, or that is logged in, is never
// one (ruling R37): a login into it would fail.
func addressless(list []*hubAccount) *hubAccount {
	var found *hubAccount
	for _, a := range list {
		if d := inspect(a); d.readable && d.st.AccountID == "" && d.address == "" {
			if found != nil {
				return nil
			}
			found = a
		}
	}
	return found
}

// knownAddress is a's address as inspect reads it, else, when a's
// state.json cannot be read, the username of its sealed account (ruling
// R37): the login of that address then reaches a and meets its fault,
// instead of adding a second account for the same Proton user.
func knownAddress(a *hubAccount) string {
	d := inspect(a)
	if d.readable || d.address != "" {
		return d.address
	}
	for _, acc := range sealedAccounts(a) {
		return acc.Username
	}
	return ""
}

// sealedAccounts is what a's sealed secrets hold when it is one account,
// nil otherwise or when they cannot be read. It opens them: only for an
// account whose state.json cannot be read.
func sealedAccounts(a *hubAccount) map[string]store.Account {
	accounts, err := a.local.secrets.Load()
	if err != nil || len(accounts) != 1 {
		return nil
	}
	return accounts
}

// freshDir reports whether the account directory dir never completed a login.
func freshDir(dir string) bool {
	st, err := store.LoadState(dir)
	return err == nil && neverLoggedIn(st)
}

// add makes a new account directory and its Local, not in the Hub yet.
func (h *Hub) add() (*hubAccount, error) {
	id, err := store.NewLocalID(func(id string) bool {
		h.mu.Lock()
		_, taken := h.accounts[id]
		h.mu.Unlock()
		return taken || exists(store.AccountDir(h.root, id)) || exists(store.AccountDir(h.root, id)+".migrating")
	})
	if err != nil {
		return nil, blocked("cannot draw a local account ID", "run the command again")
	}
	dir := store.AccountDir(h.root, id)
	if err := os.MkdirAll(store.AccountsDir(h.root), 0o700); err != nil {
		return nil, blocked("cannot create "+store.AccountsDir(h.root), "make sure "+h.root+" is writable by this user")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, blocked("cannot create the account directory "+dir, "make sure "+h.root+" is writable by this user")
	}
	l, err := h.newLocal(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &hubAccount{id: id, dir: dir, local: l}, nil
}

// settle applies the Hub's rules to what a's Login or SubmitTOTP answered.
// added: a is a new directory, not in the Hub yet; fresh: a had never
// completed a login. It reports whether the accounts changed.
func (h *Hub) settle(ctx context.Context, a *hubAccount, added, fresh bool, res LoginResult, err error) (LoginResult, bool, error) {
	if err != nil {
		switch {
		case added && a.local.waitingLogin() != nil:
			// A code given with the login was rejected: the login waits
			// for the next one.
			return LoginResult{}, h.insert(a), err
		case added:
			discard(a)
		case fresh && abandoned(a):
			h.drop(a)
			return LoginResult{}, true, err
		}
		return LoginResult{}, false, err
	}
	if res.AccountID != "" {
		if other := h.userAccount(res.AccountID, a); other != "" {
			// Ruling R12: the session this login opened is revoked, then
			// an add's directory goes. A logout that fails keeps the
			// directory: it holds the session's only tokens.
			out, lerr := a.local.Logout(context.WithoutCancel(ctx), LogoutRequest{})
			if lerr != nil {
				return LoginResult{}, added && h.insert(a), lerr
			}
			changed := false
			switch {
			case added:
				discard(a)
			case fresh:
				h.drop(a)
				changed = true
			}
			hint := "use that account, or remove it first (pcb account remove --account " + other + ")"
			if !out.Revoked {
				hint += "; pcb could not revoke the session this login opened: end the session in " + SessionsPage
			}
			return LoginResult{}, changed, mustAct("this Proton account is already added as "+other, hint)
		}
	}
	changed := fresh && res.State == StateReady
	if res.State == StateReady && h.clearStop(a) {
		changed = true
	}
	if added {
		if !h.insert(a) {
			return LoginResult{}, false, errClosed()
		}
		changed = true
	}
	res.Account = a.id
	return res, changed, nil
}

// userAccount is the address (else the local ID) of the account other than
// except whose Proton user is userID, "" if none. An account whose
// state.json cannot be read is its sealed account's user (ruling R37).
func (h *Hub) userAccount(userID string, except *hubAccount) string {
	for _, b := range h.list() {
		if b == except {
			continue
		}
		d := inspect(b)
		sealed := false
		if !d.readable {
			_, sealed = sealedAccounts(b)[userID]
		}
		if d.st.AccountID == userID || (d.held != nil && d.held.userID == userID) || sealed {
			if d.address != "" {
				return d.address
			}
			return b.id
		}
	}
	return ""
}

// SubmitTOTP finishes the login of the account selector names that waits
// for its 2FA code, with the rules of Login. An empty selector is the only
// account whose login waits for a code.
func (h *Hub) SubmitTOTP(ctx context.Context, selector, code string) (LoginResult, error) {
	res, changed, err := h.submitTOTP(ctx, selector, code)
	h.loops()
	h.notify(changed)
	return res, err
}

func (h *Hub) submitTOTP(ctx context.Context, selector, code string) (LoginResult, bool, error) {
	end, err := h.begin()
	if err != nil {
		return LoginResult{}, false, err
	}
	defer end()
	a, err := h.pickWaiting(selector)
	if err != nil {
		return LoginResult{}, false, err
	}
	fresh := freshDir(a.dir)
	res, err := a.local.SubmitTOTP(ctx, code)
	return h.settle(ctx, a, false, fresh, res, err)
}

func (h *Hub) pickWaiting(selector string) (*hubAccount, error) {
	if h.isClosed() {
		return nil, errClosed()
	}
	list := h.list()
	if selector != "" {
		info, err := SelectAccount(lightInfos(list), selector)
		if err != nil {
			return nil, err
		}
		return byIDs(list, []AccountInfo{info})[0], nil
	}
	var waiting []*hubAccount
	for _, a := range list {
		if a.local.waitingLogin() != nil {
			waiting = append(waiting, a)
		}
	}
	switch len(waiting) {
	case 0:
		return nil, mustAct("no login is waiting for a 2FA code", "run pcb login first")
	case 1:
		return waiting[0], nil
	}
	return nil, severalAccounts(h.infos(waiting))
}

// SignOut signs the account out (Local.Logout): its session revoked, its
// secrets and event cache gone; its directory, address and CalDAV clients
// stay.
func (h *Hub) SignOut(ctx context.Context, selector string, req LogoutRequest) (LogoutResult, error) {
	res, err := h.signOut(ctx, selector, req)
	h.notify(err == nil)
	return res, err
}

func (h *Hub) signOut(ctx context.Context, selector string, req LogoutRequest) (LogoutResult, error) {
	h.ops.Lock()
	defer h.ops.Unlock()
	a, err := h.pick(selector)
	if err != nil {
		return LogoutResult{}, err
	}
	return a.local.Logout(ctx, req)
}

// Remove stops the account's serve loop and waits for its end (ruling
// R18), signs the account out, then deletes its directory. A sign-out it
// refuses leaves the account as it was, its loop started again.
func (h *Hub) Remove(ctx context.Context, selector string, req LogoutRequest) (AccountInfo, error) {
	info, changed, err := h.remove(ctx, selector, req)
	h.loops() // a removal Logout refused: its loop starts again
	h.notify(changed)
	return info, err
}

func (h *Hub) remove(ctx context.Context, selector string, req LogoutRequest) (AccountInfo, bool, error) {
	h.ops.Lock()
	defer h.ops.Unlock()
	a, err := h.pick(selector)
	if err != nil {
		return AccountInfo{}, false, err
	}
	var info AccountInfo
	gone, err := a.retire(func() error {
		if _, err := a.local.Logout(ctx, req); err != nil {
			return err
		}
		info = h.info(a)
		return nil
	})
	if !gone {
		return AccountInfo{}, false, err
	}
	h.mu.Lock()
	delete(h.accounts, a.id)
	h.mu.Unlock()
	if err != nil {
		return info, true, blocked("the account is signed out but its directory could not be removed", "remove "+a.dir)
	}
	return info, true, nil
}

// SetCalDAV switches the account's CalDAV adapter, in its state.json; while
// the Hub serves, off stops the account's loop (it returns once the loop
// ended) and on starts it, with a pass at once. It is refused while a
// login of the account waits for its 2FA code (that login would write the
// switch back as it found it).
func (h *Hub) SetCalDAV(ctx context.Context, selector string, on bool) (AccountInfo, error) {
	info, err := h.setCalDAV(ctx, selector, on)
	h.loops()
	h.notify(err == nil)
	return info, err
}

func (h *Hub) setCalDAV(ctx context.Context, selector string, on bool) (AccountInfo, error) {
	h.ops.Lock()
	defer h.ops.Unlock()
	a, err := h.pick(selector)
	if err != nil {
		return AccountInfo{}, err
	}
	if a.local.waitingLogin() != nil {
		return AccountInfo{}, mustAct("a login of this account waits for its 2FA code", "finish it (pcb login resume) or let it expire, then switch CalDAV again")
	}
	if _, err := store.LoadState(a.dir); err != nil {
		return AccountInfo{}, stateError(err)
	}
	if err := store.UpdateStateContext(ctx, a.dir, func(s *store.State) error {
		s.CalDAV = &on
		return nil
	}); err != nil {
		return AccountInfo{}, a.local.stateWriteError(err)
	}
	return h.info(a), nil
}

// Close closes every account's Local, aborting the logins waiting for a
// code. It is idempotent; the Hub then refuses its calls.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	list := make([]*hubAccount, 0, len(h.accounts))
	for _, a := range h.accounts {
		list = append(list, a)
	}
	clear(h.accounts)
	h.mu.Unlock()
	for _, a := range list {
		_ = a.local.Close()
	}
	return nil
}
