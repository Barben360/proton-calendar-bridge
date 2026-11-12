package service

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

// Serve's sync cadence (ruling M5-P6). Idle means no request authenticated
// by CheckClient for IdleAfter since Serve began serving: longer than
// Thunderbird's default 30-minute refresh, so a running Thunderbird keeps
// the active cadence. The first authenticated request while idle starts a
// pass at once.
const (
	ActiveInterval = 30 * time.Second
	IdleInterval   = 5 * time.Minute
	IdleAfter      = 35 * time.Minute
)

// ServeState is whether Serve is running, and whether it serves or waits
// for a login.
type ServeState string

// The serve states.
const (
	ServeStopped ServeState = "stopped"
	ServeServing ServeState = "serving"
	// ServeWaitingForLogin: Serve runs with WaitForLogin and waits for a
	// ready login (logged out, or the session ended).
	ServeWaitingForLogin ServeState = "waiting-for-login"
)

// The error classes (errorClass).
const (
	classRateLimited       = "rate-limited"
	classProtonUnavailable = "proton-unavailable"
	classNetwork           = "network"
	classSession           = "session"
	classRefused           = "refused"
	classDeferred          = "deferred"
	classCache             = "cache"
	classOther             = "other"
)

// The sources of a human verification (HumanVerification.Source).
const (
	hvWrite = "write"
	hvSync  = "sync"
)

// Health is Serve's live state, in memory only: what pcb status shows over
// the control socket. Outside Serve it is {Serve: "stopped"} and no
// calendars; while Serve waits for a login, {Serve: "waiting-for-login"},
// since when, and no calendars (pcb status lists the cached ones).
type Health struct {
	Serve ServeState `json:"serve"`
	// Since is when Serve entered this state (nil when stopped).
	Since *time.Time `json:"since,omitempty"`
	// IntervalSeconds is the wait the loop last chose before its next pass.
	IntervalSeconds int `json:"interval_seconds,omitempty"`
	// Idle: no client was authenticated for IdleAfter (see ActiveInterval).
	Idle     bool        `json:"idle"`
	LastPass *PassHealth `json:"last_pass,omitempty"`
	// Calendars are the cached calendars, in cache order.
	Calendars []CalendarHealth `json:"calendars"`
	Render    RenderCounts     `json:"render"`
	// HumanVerification lists the sources Proton asked a person to complete
	// a verification for, still unanswered, by source.
	HumanVerification []HumanVerification `json:"human_verification,omitempty"`
}

// PassHealth is the last sync pass. OK is true for a pass that got through
// (no error, or a partial one: the calendars carry their own errors); an
// ErrorClass and Error (secret-free) are set otherwise.
type PassHealth struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	ErrorClass string    `json:"error_class,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// CalendarHealth is one cached calendar. SyncedAt is its last successful
// pass and LagSeconds its age (nil: never synced). Error is its last sync
// failure with its ErrorClass (a class of the last pass of this process, or
// "other" for a failure the cache holds from an earlier one). Gate is the
// write gate, open, closed, undecided or unknown (keys not loaded yet), and
// GateReason says why unless it is open (M4-R39, M4-R40). KeysError is why
// the keys did not unlock, when they did not.
type CalendarHealth struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Events     int        `json:"events"`
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
	LagSeconds *int       `json:"lag_seconds,omitempty"`
	ErrorClass string     `json:"error_class,omitempty"`
	Error      string     `json:"error,omitempty"`
	Gate       string     `json:"gate"`
	GateReason string     `json:"gate_reason,omitempty"`
	KeysError  string     `json:"keys_error,omitempty"`
}

// HumanVerification is a verification Proton asked for: Source "write" (a
// write's call was answered 9001) or "sync" (a pass's call was).
type HumanVerification struct {
	Source string    `json:"source"`
	Since  time.Time `json:"since"`
}

// The write gate's texts, in the order Access.Gate decides them.
const (
	gtOpen      = "open"
	gtClosed    = "closed"
	gtUndecided = "undecided"
	gtUnknown   = "unknown"

	reasonNotNormal    = "not a normal calendar (subscribed or holidays)"
	reasonNoWrite      = "our member lacks the Write permission"
	reasonBadSignature = "the member passphrase signature does not verify"
	reasonNoUnlock     = "the calendar keys did not unlock"
	reasonNoCheck      = "the member passphrase signature cannot be checked (none served, or our address key is locked)"
	reasonNoPrimary    = "no usable primary calendar key (none unlocked, or two flagged primary)"
	reasonNoSigner     = "our member's address has no unlocked primary key"
	reasonNotLoaded    = "the keys were not loaded yet"
)

// gateOf is the gate and its reason for a calendar whose keys were tried
// (ck), or unknown when they were not.
func gateOf(ck *calKeys) (gate, reason string) {
	if ck == nil {
		return gtUnknown, reasonNotLoaded
	}
	a := ck.access
	switch ck.gate {
	case calendar.GateOpen:
		return gtOpen, ""
	case calendar.GateClosed:
		switch {
		case a == nil, a.Type != calendar.Normal:
			return gtClosed, reasonNotNormal // only the type closes a calendar with no keys
		case a.Permissions&calendar.PermissionWrite == 0:
			return gtClosed, reasonNoWrite
		}
		return gtClosed, reasonBadSignature
	}
	switch {
	case a == nil:
		return gtUndecided, reasonNoUnlock
	case !a.PassphraseChecked:
		return gtUndecided, reasonNoCheck
	case a.Primary == nil:
		return gtUndecided, reasonNoPrimary
	}
	return gtUndecided, reasonNoSigner
}

// errorClass sorts a failure for Health: rate-limited (429),
// proton-unavailable (5xx), network, session (a person must act, a log in),
// refused (another Proton 4xx), deferred (a full listing waits, M2-R11),
// cache, other. It looks at types only, never at text. Errors the service
// mapped (pass level) carry their class; the raw ones of a calendar's
// failure are read as they are.
func errorClass(err error) string {
	var (
		de *syncer.ResyncDeferredError
		d  *detail
		ee *exitcode.Error
		ae *api.Error
		ne net.Error
		ue *url.Error
	)
	switch {
	case err == nil:
		return ""
	case errors.As(err, &de):
		return classDeferred
	case errors.As(err, &d) && d.class != "":
		return d.class
	case cacheFailure(err) != nil:
		return classCache
	case errors.Is(err, api.ErrSessionEnded), errors.As(err, &ee) && ee.Code == exitcode.MustAct:
		return classSession
	case errors.As(err, &ae):
		switch {
		case ae.Status == http.StatusTooManyRequests:
			return classRateLimited
		case ae.Status >= 500:
			return classProtonUnavailable
		}
		return classRefused
	case errors.As(err, &ne), errors.As(err, &ue):
		return classNetwork
	}
	return classOther
}

// live is what Serve keeps while it runs, in memory only.
type live struct {
	mu       sync.Mutex
	since    time.Time
	active   time.Duration // the interval after a pass when clients are about
	idleIv   time.Duration // and when none is
	interval time.Duration // the last wait chosen
	activity time.Time     // the last authenticated request; zero: none yet
	last     *PassHealth
	classes  map[string]string // calendar ID → error class, as of its last pass
	wake     chan struct{}     // wakes an idle loop (capacity 1)
	wakes    atomic.Int64      // wakes asked for (tests)
}

func newLive(now time.Time, active time.Duration) *live {
	return &live{since: now, active: active, idleIv: max(IdleInterval, active), interval: active,
		classes: map[string]string{}, wake: make(chan struct{}, 1)}
}

// idleAt reports whether no client was authenticated for IdleAfter, counted
// from the later of Serve's start and the last request; v.mu held.
func (v *live) idleAt(now time.Time) bool {
	ref := v.since
	if v.activity.After(ref) {
		ref = v.activity
	}
	return now.Sub(ref) >= IdleAfter
}

// next is the wait after a pass that got through.
func (v *live) next(now time.Time) time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.idleAt(now) {
		return v.idleIv
	}
	return v.active
}

// notePass records a pass's outcome, its wait and each calendar's class.
func (v *live) notePass(at time.Time, p WatchPass) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ph := &PassHealth{At: at, OK: p.Err == nil || exitcode.From(p.Err) == exitcode.Partial}
	if !ph.OK {
		ph.ErrorClass, ph.Error = errorClass(p.Err), p.Err.Error()
	}
	v.last = ph
	if p.Next > 0 {
		v.interval = p.Next
	}
	for _, c := range p.Result.Calendars {
		if c.ErrorClass == "" {
			delete(v.classes, c.ID)
		} else {
			v.classes[c.ID] = c.ErrorClass
		}
	}
}

func (l *Local) liveState() *live {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	return l.live
}

// noteActivity records an authenticated request; when the loop was idle it
// is woken, so a pass starts at once.
func (l *Local) noteActivity() {
	v := l.liveState()
	if v == nil {
		return
	}
	now := l.o.Now()
	v.mu.Lock()
	wasIdle := v.idleAt(now)
	if now.After(v.activity) {
		v.activity = now
	}
	v.mu.Unlock()
	if wasIdle {
		v.wakes.Add(1)
		select {
		case v.wake <- struct{}{}:
		default: // a wake is already waiting
		}
	}
}

// noteHuman records that Proton asked for a human verification (a 9001) for
// source, keeping the time of the first ask.
func (l *Local) noteHuman(source string) {
	l.hmu.Lock()
	defer l.hmu.Unlock()
	if _, ok := l.human[source]; ok {
		return
	}
	if l.human == nil {
		l.human = map[string]time.Time{}
	}
	l.human[source] = l.o.Now()
}

// clearHuman forgets source's verification: a write confirmed, or a pass
// that got through.
func (l *Local) clearHuman(source string) {
	l.hmu.Lock()
	defer l.hmu.Unlock()
	delete(l.human, source)
}

// Health implements API.
func (l *Local) Health(ctx context.Context) (Health, error) {
	if l.closed.Load() {
		return Health{}, errClosed()
	}
	if since := l.waitingSince(); since != nil {
		h := Health{Serve: ServeWaitingForLogin, Since: since, Calendars: []CalendarHealth{}, Render: l.RenderCounts()}
		if h.Render.KeysErrors == nil {
			h.Render.KeysErrors = []string{}
		}
		return h, nil
	}
	v := l.liveState()
	if v == nil {
		return Health{Serve: ServeStopped, Calendars: []CalendarHealth{}, Render: RenderCounts{KeysErrors: []string{}}}, nil
	}
	now := l.o.Now()
	v.mu.Lock()
	since := v.since
	h := Health{Serve: ServeServing, Since: &since, IntervalSeconds: int(v.interval / time.Second), Idle: v.idleAt(now), Calendars: []CalendarHealth{}}
	if v.last != nil {
		last := *v.last
		h.LastPass = &last
	}
	classes := maps.Clone(v.classes)
	v.mu.Unlock()

	h.Render = l.RenderCounts()
	if h.Render.KeysErrors == nil {
		h.Render.KeysErrors = []string{}
	}
	l.hmu.Lock()
	for _, src := range []string{hvWrite, hvSync} {
		if t, ok := l.human[src]; ok {
			h.HumanVerification = append(h.HumanVerification, HumanVerification{Source: src, Since: t})
		}
	}
	l.hmu.Unlock()

	db, release, err := l.readCache()
	if err != nil {
		return Health{}, err
	}
	defer release()
	if db == nil {
		return h, nil // the cache is no longer this login's (a logout meanwhile)
	}
	cals, err := db.Calendars(ctx)
	if err != nil {
		return Health{}, l.fail(cacheError(err))
	}
	for _, c := range cals {
		ch := CalendarHealth{ID: c.ID, Name: c.Name, Events: c.Events, Error: c.Error}
		if c.Error != "" {
			if ch.ErrorClass = classes[c.ID]; ch.ErrorClass == "" {
				ch.ErrorClass = classOther
			}
		}
		if !c.SyncedAt.IsZero() {
			t := c.SyncedAt
			lag := max(int(now.Sub(t)/time.Second), 0)
			ch.SyncedAt, ch.LagSeconds = &t, &lag
		}
		ck := l.r.calKeys(c.ID)
		ch.Gate, ch.GateReason = gateOf(ck)
		if ck != nil {
			ch.KeysError = ck.err
		}
		h.Calendars = append(h.Calendars, ch)
	}
	return h, nil
}
