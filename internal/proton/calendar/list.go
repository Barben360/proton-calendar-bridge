// Package calendar lists a user's Proton calendars, resolves the default one
// and unlocks a calendar's keys from the bootstrap snapshot. Nothing is
// cached: every call fetches fresh state, so a calendar list never goes stale.
package calendar

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// Type is Proton's calendar type.
type Type int

// Known calendar types.
const (
	Normal     Type = 0
	Subscribed Type = 1
	Holidays   Type = 2
)

// String returns normal, subscribed, holidays, or "type N" for an unknown
// type.
func (t Type) String() string {
	switch t {
	case Normal:
		return "normal"
	case Subscribed:
		return "subscribed"
	case Holidays:
		return "holidays"
	}
	return "type " + strconv.Itoa(int(t))
}

// ReadOnly reports whether the calendar must not be written: true for every
// type but Normal, so an unknown type fails closed.
func (t Type) ReadOnly() bool { return t != Normal }

// Calendar is one calendar with its display metadata resolved.
type Calendar struct {
	ID, Name, Description, Color string
	Type                         Type
	// Our member in the calendar, and the address it uses.
	MemberID, AddressID, Email string
	Permissions                int
}

// member is our member entry of a calendar, in the list and the bootstrap.
type member struct {
	ID          string
	AddressID   string
	Email       string
	Name        string
	Description string
	Color       string
	Permissions int
}

// List returns the calendars in API order. Name, description and colour come
// from our member entry; the legacy top-level fields are only a fallback, and
// nothing is invented when both are empty.
func List(ctx context.Context, c *api.Client) ([]Calendar, error) {
	var out struct {
		Calendars []struct {
			ID          string
			Type        int
			Name        string
			Description string
			Color       string
			Members     []member
		}
	}
	if err := c.Get(ctx, "/calendar/v1", nil, &out); err != nil {
		return nil, fmt.Errorf("listing calendars: %w", err)
	}
	cals := make([]Calendar, 0, len(out.Calendars))
	for _, w := range out.Calendars {
		cal := Calendar{ID: w.ID, Type: Type(w.Type), Name: w.Name, Description: w.Description, Color: w.Color}
		if len(w.Members) > 0 {
			m := w.Members[0]
			cal.MemberID, cal.AddressID, cal.Email, cal.Permissions = m.ID, m.AddressID, m.Email, m.Permissions
			cal.Name = firstNonEmpty(m.Name, cal.Name)
			cal.Description = firstNonEmpty(m.Description, cal.Description)
			cal.Color = firstNonEmpty(m.Color, cal.Color)
		}
		cals = append(cals, cal)
	}
	return cals, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// DefaultID returns the account's default calendar ID, "" when unset.
func DefaultID(ctx context.Context, c *api.Client) (string, error) {
	var out struct {
		CalendarUserSettings struct{ DefaultCalendarID string }
	}
	if err := c.Get(ctx, "/settings/calendar", nil, &out); err != nil {
		return "", fmt.Errorf("fetching the calendar settings: %w", err)
	}
	return out.CalendarUserSettings.DefaultCalendarID, nil
}
