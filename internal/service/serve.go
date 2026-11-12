package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// settingsRefresh is how often Serve reads the calendars' settings again
// from their bootstraps (ruling M3-R10), so a change of the default
// notifications or a calendar key rotation reaches the rendering without a
// restart.
const settingsRefresh = 10 * time.Minute

// ServeOptions tune Serve: the interval between sync passes while a client
// is about (0: ActiveInterval; see IdleAfter for the slower idle cadence,
// the longer of IdleInterval and Interval) and a callback after each pass, as WatchSync, with
// the render counters (WatchPass.Render).
// Ready, when set, is called once Serve holds the sync lock, a ready login
// and its cache handle, before the first pass: pcb serve binds its listener
// then, so a refused start (logged out, a sync running) never binds it.
// Logger gets what the writes served meanwhile (PutObject, DeleteObject)
// have to say: refusals worth a look at warning level, normalisations at
// debug; IDs and fixed text only, never event content (nil discards it).
type ServeOptions struct {
	Interval time.Duration
	OnPass   func(WatchPass)
	Ready    func()
	Logger   *slog.Logger
	// WaitForLogin keeps Serve running while it has no ready login (logged
	// out, or the session ended): it waits for one, checking every
	// LoginPoll (0: DefaultLoginPoll) and at once when a login completes in
	// this process.
	WaitForLogin bool
	LoginPoll    time.Duration
	// OnState, when set, gets each state Serve enters (serving, or
	// waiting-for-login), from Serve's goroutine.
	OnState func(ServeState)
}

// DefaultLoginPoll is how often a Serve waiting for a login reads
// state.json again (ruling M5-P9): a login by another pcb process.
const DefaultLoginPoll = 5 * time.Second

// Serve runs the sync loop of serve mode until ctx ends (nil) or a person
// is needed (that error), as WatchSync does: it holds the sync lock, opens
// the cache once as its writer and syncs on that handle; meanwhile the read
// API reads the same handle (ruling M3-R6). After a pass that got through,
// it refreshes the calendars' settings when settingsRefresh has passed.
//
// Without WaitForLogin it needs a ready login (M3). With it, Serve is ready
// once it holds the sync lock and waits for a login (awaitLogin); a stop
// a login fixes (loginFixes: not logged in, the session ended, needs-login
// or needs-captcha, secrets rolled back, a logout) closes the cache and
// waits again. Only ctx (nil) or another stop error, a damaged cache
// included, ends it (ruling M5-R8).
func (l *Local) Serve(ctx context.Context, o ServeOptions) error {
	if l.closed.Load() {
		return errClosed()
	}
	interval, err := watchInterval(o.Interval)
	if err != nil {
		return err
	}
	unlock, err := l.lockSync()
	if err != nil {
		return err
	}
	defer unlock()
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if !o.WaitForLogin {
		gen := l.logoutGen()
		_, acc, err := l.stored()
		if err != nil {
			return err
		}
		return l.serveOn(ctx, o, interval, acc, gen)
	}
	defer l.leaveWaiting()
	if o.Ready != nil {
		o.Ready()
	}
	var stopped error // the stop the loop waits after, nil at the start
	for {
		acc, gen, err := l.awaitLogin(ctx, o, stopped)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			return err // a stored login a login does not fix (ruling M5-R10)
		}
		if l.serveHook != nil {
			l.serveHook()
		}
		err = l.serveOn(ctx, o, interval, acc, gen)
		if err == nil || !waitsAfter(err) {
			return err // ctx ended, or a stop a login does not fix (ruling M5-R8)
		}
		stopped = err
	}
}

// waitsAfter reports whether Serve with WaitForLogin waits for a login
// after the stop err: one a login fixes (ruling M5-R8), or another
// process logged in to another account (ruling M5-R10: the next login
// check serves it).
func waitsAfter(err error) bool { return loginFixes(err) || accountSwitched(err) }

// errLoggedOut ends a serving phase that a Logout stops (ruling M5-P10).
var errLoggedOut = errors.New("logged out during serve")

func loggedOut() error { return loginNeeded("not logged in to Proton", "run pcb login") }

// servePhase is Serve serving on a cache handle: cancel stops it, done is
// closed once its handle is closed.
type servePhase struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// logoutGen is the count of the logouts begun in this process: Serve reads
// it before it reads the stored login, and serveOn refuses a login a
// logout began after.
func (l *Local) logoutGen() uint64 {
	l.gmu.Lock()
	defer l.gmu.Unlock()
	return l.logouts
}

// serveOn serves acc, read when the logout count was gen: it opens the
// cache, runs the sync loop on it until ctx ends (nil), a pass needs a
// person (that error) or a Logout stops it (loggedOut), then closes the
// cache. A logout running, or begun since gen, refuses it before the cache
// is opened (loggedOut), so no empty pcb.db outlives a wipe. Ready is
// called once the cache is open, unless WaitForLogin called it already.
// With WaitForLogin, a stop Serve waits after (waitsAfter) marks Serve
// waiting before its handle is cleared and closed: a read never opens its
// own handle in between, and a Logout returns with Health already waiting.
// Until the first pass that gets through, an unknown calendar is pending,
// not missing (ruling M5-R9).
func (l *Local) serveOn(ctx context.Context, o ServeOptions, interval time.Duration, acc store.Account, gen uint64) (err error) {
	pctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ph := &servePhase{cancel: cancel, done: make(chan struct{})}
	l.gmu.Lock()
	if l.halts > 0 || l.logouts != gen { // a logout is wiping, or wiped since the login was read
		l.gmu.Unlock()
		if o.WaitForLogin {
			l.enterWaiting(o)
		}
		return loggedOut()
	}
	l.phase = ph
	l.waitSince = nil
	l.gmu.Unlock()
	waitNext := func() {
		if o.WaitForLogin && ctx.Err() == nil && waitsAfter(err) {
			l.enterWaiting(o) // Serve waits next: say so before the handle goes
		}
	}
	defer func() {
		waitNext() // no-op when done already; covers a cache that did not open
		l.gmu.Lock()
		l.phase = nil
		l.gmu.Unlock()
		close(ph.done)
	}()
	db, err := cache.Open(cache.Path(l.o.Dirs.State), acc.ID)
	if err != nil {
		return l.fail(cacheError(err))
	}
	v := newLive(l.o.Now(), interval)
	l.hmu.Lock()
	l.human = nil
	l.hmu.Unlock()
	l.unlisted.Store(db)
	l.smu.Lock()
	l.served = &served{db: db, account: acc.ID, log: o.Logger}
	l.smu.Unlock()
	l.lmu.Lock()
	l.live = v
	l.lmu.Unlock()
	defer func() {
		waitNext()
		l.lmu.Lock()
		l.live = nil
		l.lmu.Unlock()
		l.smu.Lock()
		l.served = nil
		l.smu.Unlock()
		l.unlisted.CompareAndSwap(db, nil)
		_ = db.Close()
	}()
	if o.Ready != nil && !o.WaitForLogin {
		o.Ready()
	}
	if o.OnState != nil {
		o.OnState(ServeServing)
	}
	onPass := func(p WatchPass) {
		v.notePass(l.o.Now(), p) // before the callback: Health reads it there
		if o.OnPass != nil {
			counts := l.RenderCounts()
			p.Render = &counts
			o.OnPass(p)
		}
	}
	pace := watchPacing{next: func() time.Duration { return v.next(l.o.Now()) }, wake: v.wake}
	err = l.watch(pctx, SyncRequest{}, interval, onPass, func(ctx context.Context, req SyncRequest) (SyncResult, error) {
		res, err := l.syncPassOn(ctx, req, db, acc.ID)
		if err == nil || exitcode.From(err) == exitcode.Partial {
			l.unlisted.CompareAndSwap(db, nil) // the calendars are listed: an unknown one is missing now
			if cals, cerr := db.Calendars(ctx); cerr == nil {
				live := make(map[string]calendar.Type, len(cals))
				for _, c := range cals {
					live[c.ID] = calendar.ParseType(c.Type)
				}
				l.refreshSettings(ctx, live)
			}
		}
		return res, err
	}, pace)
	if err == nil && ctx.Err() == nil && errors.Is(context.Cause(pctx), errLoggedOut) {
		return loggedOut()
	}
	return err
}

// awaitLogin returns the ready login Serve can serve and the logout count
// read before it (serveOn), checking at once (after a stop: at the next
// poll or wake, as the stop's cause is usually still there), then every
// o.LoginPoll and at once when a login completes in this process. While
// no login is ready, or a logout is wiping, Serve waits: Health says so,
// the reads answer ErrWaitingForLogin, and the wait is logged once with
// its reason. A stored login a login does not fix (an unreadable state
// file, a missing key file, a generation out of range) is returned as a
// MustAct with its own hint (ruling M5-R10). ctx's end returns its error.
func (l *Local) awaitLogin(ctx context.Context, o ServeOptions, stopped error) (store.Account, uint64, error) {
	poll := o.LoginPoll
	if poll <= 0 {
		poll = DefaultLoginPoll
	}
	logged := false
	wait := stopped != nil
	if stopped != nil {
		o.Logger.Warn("waiting for a login", "reason", stopped.Error(), "hint", exitcode.HintOf(stopped))
		logged = true
	}
	for {
		if wait {
			tick, stop := l.pollTimer(poll)
			select {
			case <-ctx.Done():
				stop()
				return store.Account{}, 0, ctx.Err()
			case <-l.loginWake:
			case <-tick:
			}
			stop()
		}
		wait = true
		if l.checkHook != nil {
			l.checkHook()
		}
		l.gmu.Lock()
		halted, gen := l.halts > 0, l.logouts
		l.gmu.Unlock()
		var err error
		if !halted {
			var acc store.Account
			_, acc, err = l.stored()
			switch {
			case err == nil:
				return acc, gen, nil
			case !loginFixes(err):
				return store.Account{}, 0, persistent(err)
			}
		}
		l.enterWaiting(o)
		if !logged && err != nil {
			o.Logger.Warn("waiting for a login", "reason", err.Error(), "hint", exitcode.HintOf(err))
			logged = true
		}
	}
}

// persistent is err, a failure to read the stored login that no login and
// no retry fixes (files only), as a MustAct with its message and hint: pcb
// serve exits 10 and is not restarted in a loop (ruling M5-R10).
func persistent(err error) error {
	var ee *exitcode.Error
	if !errors.As(err, &ee) {
		return err
	}
	return &exitcode.Error{Code: exitcode.MustAct, Err: ee.Err, Hint: ee.Hint}
}

// pollTimer is the wait between two login checks: a timer of d, or the
// test's (SetLoginPollTimer). stop releases it.
func (l *Local) pollTimer(d time.Duration) (tick <-chan time.Time, stop func()) {
	if l.pollHook != nil {
		return l.pollHook(d), func() {}
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// enterWaiting marks Serve waiting for a login, reporting it to OnState
// when it was not already.
func (l *Local) enterWaiting(o ServeOptions) {
	l.gmu.Lock()
	entered := l.waitSince == nil
	if entered {
		now := l.o.Now()
		l.waitSince = &now
	}
	l.gmu.Unlock()
	if entered && o.OnState != nil {
		o.OnState(ServeWaitingForLogin)
	}
}

// leaveWaiting ends the wait when Serve returns.
func (l *Local) leaveWaiting() {
	l.gmu.Lock()
	l.waitSince = nil
	l.gmu.Unlock()
}

// waitingSince is when Serve began waiting for a login, nil when it does
// not wait.
func (l *Local) waitingSince() *time.Time {
	l.gmu.Lock()
	defer l.gmu.Unlock()
	if l.waitSince == nil {
		return nil
	}
	t := *l.waitSince
	return &t
}

// waitingErr is ErrWaitingForLogin while Serve waits for a login, else nil.
func (l *Local) waitingErr() error {
	if l.waitingSince() == nil {
		return nil
	}
	return &exitcode.Error{Code: exitcode.Blocked, Err: ErrWaitingForLogin, Hint: "run pcb login"}
}

// wakeLogin tells a Serve waiting for a login that one completed in this
// process.
func (l *Local) wakeLogin() {
	select {
	case l.loginWake <- struct{}{}:
	default: // a wake is already waiting
	}
}

// haltServe stops Serve's serving phase, if any, and waits until its cache
// handle is closed; until release, Serve does not serve again (ruling
// M5-P10: a logout wipes nothing a pass still writes). It holds no lock
// while it waits: a pass may need mu (a relogin) to end.
func (l *Local) haltServe() (release func()) {
	l.gmu.Lock()
	l.halts++
	l.logouts++
	ph := l.phase
	l.gmu.Unlock()
	if ph != nil {
		ph.cancel(errLoggedOut)
		<-ph.done
	}
	return func() {
		l.gmu.Lock()
		l.halts--
		l.gmu.Unlock()
		l.wakeLogin()
	}
}

// refreshSettings reads again the bootstrap of every calendar whose keys
// are held and that the cache still has (live), once settingsRefresh has
// passed since they were last read (one Proton call per calendar); the keys
// of a calendar the cache no longer has are dropped. A calendar whose settings, key IDs or write
// gate changed, or whose keys failed and now unlock, gets its new keys: its ETags, CTag
// and token generation change, so clients fetch its objects again. A
// failure keeps what is held (a transient one is tried at the next pass).
func (l *Local) refreshSettings(ctx context.Context, live map[string]calendar.Type) {
	l.r.kmu.RLock()
	due := l.r.ks != nil && !l.o.Now().Before(l.r.refreshedAt.Add(settingsRefresh))
	l.r.kmu.RUnlock()
	if !due {
		return
	}
	l.r.load.Lock()
	defer l.r.load.Unlock()
	_ = l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		gen, ks := l.r.acquire()
		defer l.r.release(ks)
		if ks == nil || ks.account != acc.ID {
			return nil
		}
		// ks.cals changes only under load, which is held.
		fresh := map[string]*calKeys{}
		var gone []string
		discard := func() {
			for _, ck := range fresh {
				ck.access.Close()
			}
		}
		for _, id := range slices.Sorted(maps.Keys(ks.cals)) {
			typ, ok := live[id]
			if !ok {
				gone = append(gone, id)
				continue
			}
			a, err := calendar.Unlock(ctx, c, ks.u, calendar.Calendar{ID: id, Type: typ})
			switch {
			case err == nil:
				ck := newCalKeys(id, a, ks.addrTags)
				// The gate too: undecided and closed both render read only
				// (same settings), yet answer a write differently (ruling
				// M4-R39).
				if old := ks.cals[id]; old.access != nil && bytes.Equal(old.settings, ck.settings) && old.gate == ck.gate {
					a.Close()
					continue
				}
				fresh[id] = ck
			case api.SessionWide(ctx, err):
				discard()
				return err
			}
		}
		l.r.kmu.Lock()
		defer l.r.kmu.Unlock()
		if l.closed.Load() || l.r.gen != gen {
			discard()
			return nil
		}
		for id, ck := range fresh {
			old := ks.cals[id]
			ks.cals[id] = ck
			old.access.Close() // no render holds it: they hold kmu for reading
		}
		l.r.noteGates(ks.account, fresh)
		for _, id := range gone {
			ks.cals[id].access.Close()
			delete(ks.cals, id)
		}
		l.r.refreshedAt = l.o.Now()
		return nil
	})
}
