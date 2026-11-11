package service

import (
	"context"
	"errors"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

const keysChanged = "account keys no longer unlock (password changed?)"

// Calendars implements API. Each calendar's keys are unlocked to report
// whether they work; a calendar whose keys fail is listed with Keys "error".
// Default marks only the calendar whose ID is the account's default: none
// when the default is unset or not in the list (no fallback is invented).
func (l *Local) Calendars(ctx context.Context) ([]Calendar, error) {
	if l.closed.Load() {
		return nil, errClosed()
	}
	var out []Calendar
	err := l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		var err error
		out, err = l.calendars(ctx, c, acc)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *Local) calendars(ctx context.Context, c *api.Client, acc store.Account) ([]Calendar, error) {
	user, addrs, err := keys.Fetch(ctx, c)
	if err != nil {
		return nil, err
	}
	u, err := keys.Unlock(user, addrs, acc.KeyPass)
	if errors.Is(err, keys.ErrKeysLocked) {
		st, serr := store.LoadState(l.o.Dirs.State)
		if serr != nil {
			return nil, stateError(serr)
		}
		return nil, l.humanNeeded(st, mustAct(keysChanged, "run pcb login"))
	}
	if err != nil {
		return nil, err
	}
	defer u.Close()
	cals, err := calendar.List(ctx, c)
	if err != nil {
		return nil, err
	}
	def, err := calendar.DefaultID(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]Calendar, 0, len(cals))
	for _, cal := range cals {
		sc := Calendar{
			ID: cal.ID, Name: cal.Name, Description: cal.Description, Color: cal.Color,
			Type: cal.Type.String(), ReadOnly: cal.Type.ReadOnly(),
			Default: def != "" && cal.ID == def,
			Keys:    "ok",
		}
		a, err := calendar.Unlock(ctx, c, u, cal)
		switch {
		case err == nil:
			a.Close()
		case sessionWide(ctx, err):
			return nil, err
		default:
			sc.Keys, sc.KeysError = "error", keysError(err)
		}
		out = append(out, sc)
	}
	return out, nil
}

// sessionWide reports whether err stops the whole listing rather than one
// calendar: the session ended, the caller gave up, or the service already
// decided (a store failure during a refresh).
func sessionWide(ctx context.Context, err error) bool {
	var ee *exitcode.Error
	return ctx.Err() != nil || errors.Is(err, api.ErrSessionEnded) || errors.As(err, &ee)
}
