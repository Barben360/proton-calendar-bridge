package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

// LaterFor is how long Later hides the update panel. It comes back after,
// for the same version too: a fix only protects those who install it.
const LaterFor = 48 * time.Hour

// The preferences Updates keeps (Fyne's, per user).
const (
	prefNotified = "updateNotified"   // the version already notified
	prefLater    = "updateLaterUntil" // RFC 3339: the panel hidden until then
)

// Prefs is where pcb-ui keeps what only it uses: fyne.Preferences.
type Prefs interface {
	String(key string) string
	SetString(key, value string)
}

// UpdateNotice is what the window and the tray show of an update.
// Available: a newer stable release exists (the tray line); Panel: the
// window's panel too (not within Later). The zero notice shows nothing.
type UpdateNotice struct {
	Available                     bool
	Version, Installed, URL, Hint string
	Panel                         bool
}

// Updates decides, from update.json's cache and the preferences, what
// pcb-ui shows of a newer release and when it notifies.
type Updates struct {
	Checker update.Checker
	Path    string // update.json
	Env     func(string) string
	Prefs   Prefs
	Hint    string // update.Hint, "" where the release page is the hint
	Now     func() time.Time
}

// Enabled reports whether the check runs (the switch and PCB_UPDATE_CHECK).
func (u *Updates) Enabled() bool { return update.Enabled(u.Env, update.Load(u.Path)) }

// SetEnabled flips the switch in update.json.
func (u *Updates) SetEnabled(ctx context.Context, on bool) error {
	return update.Edit(ctx, u.Path, func(s *update.State) { s.Off = !on })
}

// Later hides the panel for LaterFor.
func (u *Updates) Later() { u.Prefs.SetString(prefLater, u.Now().Add(LaterFor).Format(time.RFC3339)) }

// MarkNotified records that version's notification was delivered: Refresh
// offers it again until then, so a failed send is retried.
func (u *Updates) MarkNotified(version string) { u.Prefs.SetString(prefNotified, version) }

// Refresh is the notice now, and a notification while the version's has not
// been delivered (see MarkNotified). Switched off or a dev build: nothing. A
// failed check keeps update.json's last answer, so the panel does not come
// and go with the network.
func (u *Updates) Refresh(ctx context.Context) (UpdateNotice, *Notification) {
	if !u.Enabled() {
		return UpdateNotice{}, nil
	}
	now := u.Now()
	res := u.Checker.Cached(ctx, u.Path, now)
	if res.Err != nil {
		rel, ok := u.Checker.Last(u.Path)
		if !ok || !update.Newer(u.Checker.Installed, rel.Version) {
			return UpdateNotice{}, nil
		}
		res = update.Result{Latest: rel, Available: true}
	}
	if !res.Available {
		return UpdateNotice{}, nil
	}
	n := UpdateNotice{Available: true, Version: res.Latest.Version, Installed: u.Checker.Installed, URL: res.Latest.URL, Hint: u.Hint, Panel: true}
	// A deadline more than LaterFor ahead was saved under a wrong clock:
	// it does not count, or the panel could stay hidden for years.
	if until, err := time.Parse(time.RFC3339, u.Prefs.String(prefLater)); err == nil && now.Before(until) && !until.After(now.Add(LaterFor)) {
		n.Panel = false
	}
	var note *Notification
	if u.Prefs.String(prefNotified) != n.Version {
		note = &Notification{Code: "update-available", Title: "pcb", Body: fmt.Sprintf("pcb %s is available (you have %s).", n.Version, n.Installed)}
	}
	return n, note
}

// Run refreshes at start, every interval and at each poke (Later, the
// switch), showing each notice and notifying with the release page, until
// ctx ends. A version is marked notified only once notify returned nil: a
// failed one is sent again at the next refresh.
//
// A check takes up to its HTTP timeout, so Run watches the pokes meanwhile:
// once the switch is off, the notice is cleared at once and the result of
// the check in flight is dropped (never shown, notified or marked). Only
// Run's goroutine calls show, so a stale notice cannot follow the clearing.
func (u *Updates) Run(ctx context.Context, every time.Duration, poke <-chan struct{}, show func(UpdateNotice), notify func(Notification, string) error) {
	t := time.NewTicker(every)
	defer t.Stop()
	type result struct {
		n    UpdateNotice
		note *Notification
	}
	for {
		done := make(chan result, 1)
		go func() {
			n, note := u.Refresh(ctx)
			done <- result{n, note}
		}()
		again := false // a poke came in during the check: look again after it
		var r result
	wait:
		for {
			select {
			case r = <-done:
				break wait
			case <-poke:
				if !u.Enabled() {
					show(UpdateNotice{})
				}
				again = true
			case <-ctx.Done():
				return
			}
		}
		if !u.Enabled() { // switched off during the check
			r = result{}
		}
		show(r.n)
		if r.note != nil && notify(*r.note, r.n.URL) == nil {
			u.MarkNotified(r.n.Version)
		}
		if again {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-poke:
		}
	}
}
