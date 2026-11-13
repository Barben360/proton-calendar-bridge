package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// serveLockName is the root's lock of the serving process (design §4.1):
// it holds the PID as decimal text and a newline (ruling R20), the PID
// followed by a space and the process's kind since M8 (lockServe).
const serveLockName = "serve.lock"

// HubServeOptions tune Hub.Serve: the ServeOptions of every account's loop
// (Interval, LoginPoll, Logger, the latter with the account's local ID),
// with the per-account callbacks in place of ServeOptions.OnState and
// OnPass, which are ignored. Ready is called once, when Serve holds its
// locks and found what it needs to start, before the loops start.
// WaitForLogin is the process exit rule (Serve); every loop waits for a
// login whatever it says (ruling R3).
//
// The callbacks run on the account's loop goroutine: they must not wait
// for a change of the accounts (Login, SubmitTOTP, Remove, SetCalDAV,
// SetMCP, Restart), which may wait for that loop to end. OnState also gets
// ServeStopped when a loop ends: parked (AccountInfo.Stopped), paused
// (CalDAV and MCP off), removed, or Serve ending.
type HubServeOptions struct {
	ServeOptions
	OnState func(accountID string, s ServeState)
	OnPass  func(accountID string, p WatchPass)
	// Kind is the serving process's kind ("serve", "ui", "mcp"), recorded
	// in serve.lock beside its PID so a second start can say who serves.
	// "": none recorded.
	Kind string
}

// hubServe is one run of Hub.Serve.
type hubServe struct {
	ctx  context.Context // the loops' parent; cancelled when Serve ends
	o    HubServeOptions
	log  *slog.Logger
	wg   sync.WaitGroup // the loop goroutines
	wake chan struct{}  // an account's loop changed state (capacity 1)

	parked atomic.Bool // a loop parked its account since Serve last looked
}

func (sv *hubServe) poke() {
	select {
	case sv.wake <- struct{}{}:
	default: // a wake is already waiting
	}
}

// accountLoop is one account's serve loop: cancel ends it, done is closed
// once it ended.
type accountLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// stop ends the loop, if any, and waits for its end.
func (run *accountLoop) stop() {
	if run == nil {
		return
	}
	run.cancel()
	<-run.done
}

// Serve serves every account until ctx ends: one Local.Serve loop per
// account that is not pending and has either adapter (CalDAV, MCP) on,
// each in its own goroutine and waiting for a login while it has none
// (ruling R3). It holds the root's serve.lock (another holder: Blocked,
// naming the PID it wrote) and the root's sync.lock, so an older pcb
// refuses to write, for its whole run. A loop starts when its account is
// added, completes its first login or has an adapter switched on, and
// stops (Serve waits for its end) when the account is removed or its last
// adapter on is switched off (design §5.5b). A loop that
// ends on an error its context did not cause parks its account
// (AccountInfo.Stopped, OnChange runs) until Restart, a login of that
// account, or a new Hub. One account's loop never stops another's.
//
// The process exit rule (design §4.1): with WaitForLogin, Serve runs until
// ctx ends (nil). Without, it refuses to start when no account is logged
// in (MustAct, or the only account's own error), and returns once every
// account that is not pending has stopped (parked, or waiting for a login:
// the loop of one account without WaitForLogin would have ended there),
// with the error of the first to stop; with one account that is the
// single-account Serve. An account with either adapter on is served, and
// counts as any served account does; one with both off (its loop paused)
// counts as running.
// Serve returns once every loop ended, its locks released.
func (h *Hub) Serve(ctx context.Context, o HubServeOptions) error {
	if h.isClosed() {
		return errClosed()
	}
	if _, err := watchInterval(o.Interval); err != nil {
		return err
	}
	unlockServe, err := lockServe(h.root, o.Kind)
	if err != nil {
		return err
	}
	defer unlockServe()
	unlockSync, err := tryLock(h.root, syncLockName, "another pcb holds the state", "stop the running pcb first")
	if err != nil {
		return err
	}
	defer unlockSync()
	if !o.WaitForLogin {
		if err := h.anyLoggedIn(); err != nil {
			return err
		}
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sv := &hubServe{ctx: sctx, o: o, log: log, wake: make(chan struct{}, 1)}
	h.srv.Lock()
	if h.serving != nil {
		h.srv.Unlock()
		return blocked("this pcb already serves its accounts", "stop it first, or use pcb status")
	}
	h.serving = sv
	h.srv.Unlock()
	if o.Ready != nil {
		o.Ready()
	}
	h.loops()
	err = h.supervise(sv)
	h.srv.Lock()
	h.serving = nil
	h.srv.Unlock()
	cancel()
	sv.wg.Wait()
	return err
}

// supervise waits for Serve's end: ctx (nil) or, without WaitForLogin,
// every account stopped (the first stop's error). It runs OnChange after a
// loop parked its account.
func (h *Hub) supervise(sv *hubServe) error {
	for {
		select {
		case <-sv.ctx.Done():
			return nil
		case <-sv.wake:
		}
		if !sv.o.WaitForLogin {
			if err := h.allStopped(); err != nil {
				return err
			}
		}
		if sv.parked.Swap(false) {
			h.notify(true)
		}
	}
}

// lockServe takes root's serve.lock without waiting and writes this
// process's PID and kind in it ("<pid> <kind>\n"; "<pid>\n" without a
// kind); the release empties it. Held, it is Blocked naming the PID and
// kind its holder wrote, with a hint for an agent session (kind mcp).
// Off Unix there is no lock to take.
func lockServe(root, kind string) (func(), error) {
	path := filepath.Join(root, serveLockName)
	unlock, ok, err := filelock.TryLock(path)
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		return func() {}, nil
	case err != nil:
		return nil, blocked("cannot take "+serveLockName+" in "+root, "make sure "+root+" is writable by this user")
	case !ok:
		raw, _ := os.ReadFile(path)
		return nil, servingError(string(raw))
	}
	line := strconv.Itoa(os.Getpid())
	if kind != "" {
		line += " " + kind
	}
	// Written in place: the lock is on this file, a rename would drop it.
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		unlock()
		return nil, blocked("cannot write "+path, "make sure "+root+" is writable by this user")
	}
	return func() {
		_ = os.Truncate(path, 0)
		unlock()
	}, nil
}

// servingError is the refusal of a start while another process holds
// serve.lock, whose content is raw: "<pid> <kind>\n", or "<pid>\n" as an
// older pcb wrote it. What cannot be read is left out of the message: a
// PID that is not a positive number, a kind that is not a lower-case word.
func servingError(raw string) error {
	msg := "pcb is already serving"
	fields := strings.Fields(raw)
	pid, kind := "", ""
	if len(fields) >= 1 {
		if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
			pid = strconv.Itoa(n)
		}
	}
	if pid != "" && len(fields) == 2 && plainWord(fields[1]) {
		kind = fields[1]
	}
	switch {
	case kind != "":
		msg += " (pid " + pid + ", kind " + kind + ")"
	case pid != "":
		msg += " (pid " + pid + ")"
	}
	hint := "stop it first, or use pcb status"
	if kind == "mcp" {
		hint = "end the agent session (pcb mcp), then try again"
	}
	return blocked(msg, hint)
}

// plainWord: s is 1 to 16 lower-case ASCII letters.
func plainWord(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// anyLoggedIn is nil when an account that is not pending holds a ready
// login; else the only such account's own error (as the single-account
// Serve), or MustAct when there are none or several.
func (h *Hub) anyLoggedIn() error {
	var errs []error
	for _, a := range h.list() {
		if inspect(a).pending {
			continue
		}
		_, _, err := a.local.stored()
		if err == nil || keyringLocked(err) { // a locked keyring is waited for
			return nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return loggedOut()
}

// allStopped is the error of the account that stopped first once every
// account that is not pending has stopped (parked, or waiting for a
// login), nil while one runs (or starts, or is paused with both adapters
// off). With no such account left it is MustAct, as a start with none.
func (h *Hub) allStopped() error {
	var (
		first error
		seq   uint64
		n     int
	)
	for _, a := range h.list() {
		if inspect(a).pending {
			continue
		}
		n++
		h.mu.Lock()
		var err error
		switch {
		case a.stopped != nil:
			err = a.stopErr
		case a.state == ServeWaitingForLogin && !keyringLocked(a.waitErr):
			err = a.waitErr // a locked keyring is waited for whatever the exit rule
		}
		when := a.stopSeq
		h.mu.Unlock()
		if err == nil {
			return nil
		}
		if first == nil || when < seq {
			first, seq = err, when
		}
	}
	if n == 0 {
		return loggedOut()
	}
	return first
}

// loops makes the serve loops match the accounts while the Hub serves: a
// loop runs for every account wantsLoop names; the others' loops are
// stopped, each waited for.
func (h *Hub) loops() {
	h.srv.Lock()
	defer h.srv.Unlock()
	sv := h.serving
	if sv == nil {
		return
	}
	for _, a := range h.list() {
		if h.wantsLoop(a) {
			h.startLoop(sv, a)
			continue
		}
		a.lmu.Lock()
		run := a.loop
		a.lmu.Unlock()
		run.stop()
	}
	sv.poke() // the accounts may have changed: the exit rule looks again
}

// wantsLoop reports whether a is served: not pending, not parked, and
// either adapter on (design §5.5b: CalDAV off only refuses its clients;
// the loop pauses once MCP is off too).
func (h *Hub) wantsLoop(a *hubAccount) bool {
	d := inspect(a)
	if d.pending || (!switchOn(d.st.CalDAV) && !switchOn(d.st.MCP)) {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return a.stopped == nil
}

// startLoop starts a's loop unless it runs or a is being removed. It holds
// srv.
func (h *Hub) startLoop(sv *hubServe, a *hubAccount) {
	a.lmu.Lock()
	if a.removing || a.loop != nil {
		a.lmu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(sv.ctx)
	run := &accountLoop{cancel: cancel, done: make(chan struct{})}
	a.loop = run
	a.lmu.Unlock()
	h.mu.Lock()
	a.state, a.waitErr = "", nil
	h.mu.Unlock()
	sv.wg.Add(1)
	go h.runLoop(ctx, sv, a, run)
}

// syncLockRetry is how long a loop whose account's sync.lock another
// holds (an offline pcb sync --account, a watch) waits before it tries
// again (ruling R23): that is not a park.
const syncLockRetry = 30 * time.Second

// keyringRetry is how long a loop whose desktop keyring is locked waits
// before it asks again: that is not a park either.
const keyringRetry = 30 * time.Second

// runLoop runs a's Local.Serve until ctx ends or it stops on its own; then
// a stop its context did not cause parks a. A sync.lock held elsewhere is
// tried again every syncLockRetry, logged once per distinct cause.
//
// The loop's end reports ServeStopped first, then parks a and clears
// a.loop in one critical section (lmu, then mu): a Restart or a login
// after it finds a parked account with no loop and starts one; one
// before it finds the loop still there and leaves it, and the park
// follows.
func (h *Hub) runLoop(ctx context.Context, sv *hubServe, a *hubAccount, run *accountLoop) {
	defer sv.wg.Done()
	o := sv.o
	opts := ServeOptions{
		Interval:     o.Interval,
		LoginPoll:    o.LoginPoll,
		Logger:       sv.log.With("account", a.id),
		WaitForLogin: true,
		OnPass: func(p WatchPass) {
			if o.OnPass != nil {
				o.OnPass(a.id, p)
			}
		},
		OnState: func(s ServeState) { h.loopState(sv, a, s) },
		waitCause: func(cause error) {
			h.mu.Lock()
			h.stops++
			a.waitErr, a.stopSeq = cause, h.stops
			h.mu.Unlock()
		},
	}
	var err error
	logged := ""
	for {
		err = a.local.Serve(ctx, opts)
		if ctx.Err() != nil || !errors.Is(err, ErrSyncRunning) {
			break
		}
		if msg := err.Error(); msg != logged {
			sv.log.Warn("account not served yet: another pcb holds its sync lock", "account", a.id,
				"reason", msg, "hint", exitcode.HintOf(err), "retry", syncLockRetry.String())
			logged = msg
		}
		if a.local.o.Sleep(ctx, syncLockRetry) != nil {
			break
		}
	}
	cancelled := ctx.Err() != nil
	run.cancel()
	park := !cancelled && err != nil
	h.loopState(sv, a, ServeStopped)
	a.lmu.Lock()
	h.mu.Lock()
	if park {
		a.stopped = &StopInfo{Code: int(exitcode.From(err)), Message: err.Error(), Hint: exitcode.HintOf(err), At: h.o.Now()}
		a.stopErr = err
		h.stops++
		a.stopSeq = h.stops
	}
	if a.loop == run {
		a.loop = nil
	}
	hook := h.onPark
	h.mu.Unlock()
	a.lmu.Unlock()
	if park {
		if hook != nil {
			hook(a.id)
		}
		sv.log.Error("account stopped", "account", a.id, "reason", err.Error(), "hint", exitcode.HintOf(err))
		sv.parked.Store(true)
		sv.poke()
	}
	close(run.done)
}

// loopState records the state a's loop entered, reports it and lets the
// exit rule look again.
func (h *Hub) loopState(sv *hubServe, a *hubAccount, s ServeState) {
	h.mu.Lock()
	a.state = s
	h.mu.Unlock()
	if sv.o.OnState != nil {
		sv.o.OnState(a.id, s)
	}
	sv.poke()
}

// clearStop forgets a's park, reporting whether it was parked.
func (h *Hub) clearStop(a *hubAccount) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	was := a.stopped != nil
	a.stopped, a.stopErr = nil, nil
	return was
}

// Restart clears the account's park (AccountInfo.Stopped) and, while the
// Hub serves, starts its loop (unless it is pending or has both adapters
// off). An account that is not parked is left as it is.
func (h *Hub) Restart(ctx context.Context, selector string) (AccountInfo, error) {
	h.ops.Lock()
	a, err := h.pick(selector)
	changed := err == nil && h.clearStop(a)
	h.ops.Unlock()
	if err != nil {
		return AccountInfo{}, err
	}
	h.loops()
	h.notify(changed)
	return h.info(a), nil
}

// Health is the account's Health (Local.Health): its loop's live state,
// stopped when it runs no loop (parked, paused, or the Hub not serving).
func (h *Hub) Health(ctx context.Context, selector string) (Health, error) {
	a, err := h.pick(selector)
	if err != nil {
		return Health{}, err
	}
	leave, err := a.enter()
	if err != nil {
		return Health{}, err
	}
	defer leave()
	return a.local.Health(ctx)
}
