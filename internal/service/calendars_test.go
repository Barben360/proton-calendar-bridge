package service_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (e *env) calendars(t *testing.T) []service.Calendar {
	t.Helper()
	cals, err := e.svc.Calendars(t.Context())
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	return cals
}

func TestCalendarsListsWithFlags(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work", Description: "the job", Color: "#3F51B5"})
	feed := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Feed", Type: 1})
	hol := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Holidays in France", Type: 2})
	e.f.SetDefaultCalendar(e.f.Username, work)
	e.login(t, e.req())
	want := []service.Calendar{
		{ID: work, Name: "Work", Description: "the job", Color: "#3F51B5", Type: "normal", ReadOnly: false, Default: true, Keys: "ok"},
		{ID: feed, Name: "Feed", Type: "subscribed", ReadOnly: true, Keys: "ok"},
		{ID: hol, Name: "Holidays in France", Type: "holidays", ReadOnly: true, Keys: "ok"},
	}
	if got := e.calendars(t); !slices.Equal(got, want) {
		t.Fatalf("calendars =\n%+v\nwant\n%+v", got, want)
	}
}

func TestCalendarsReportsBrokenKeysPerCalendar(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	broken := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Broken", BrokenPassphrase: true})
	good := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Good"})
	e.login(t, e.req())
	got := e.calendars(t)
	if len(got) != 2 || got[0].ID != broken || got[1].ID != good {
		t.Fatalf("calendars = %+v, want Broken then Good", got)
	}
	if got[0].Keys != "error" || got[0].KeysError == "" {
		t.Errorf("broken calendar: keys %q, error %q; want error with a reason", got[0].Keys, got[0].KeysError)
	}
	if got[1].Keys != "ok" || got[1].KeysError != "" {
		t.Errorf("good calendar: keys %q, error %q; want ok", got[1].Keys, got[1].KeysError)
	}
	a := e.account(t)
	for _, s := range []string{e.f.Password, a.AccessToken, a.RefreshToken, string(a.KeyPass)} {
		if strings.Contains(got[0].KeysError, s) {
			t.Errorf("keys error %q contains a secret", got[0].KeysError)
		}
	}
}

func TestCalendarsNoDefaultWhenUnset(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "B"})
	e.login(t, e.req())
	for _, c := range e.calendars(t) {
		if c.Default {
			t.Errorf("calendar %q is default with no default set", c.Name)
		}
	}
}

// The default calendar ID may point to a calendar the list does not have:
// no calendar is marked default then, and nothing fails.
func TestCalendarsNoDefaultWhenUnlisted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "B"})
	e.f.SetDefaultCalendar(e.f.Username, "a-calendar-not-in-the-list")
	e.login(t, e.req())
	got := e.calendars(t)
	if len(got) != 2 {
		t.Fatalf("calendars = %+v", got)
	}
	for _, c := range got {
		if c.Default {
			t.Errorf("calendar %q is default; the default ID is not listed", c.Name)
		}
	}
}

func TestCalendarsWhenLoggedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	_, err := e.svc.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "pcb login") {
		t.Errorf("hint = %q, want pcb login", ee.Hint)
	}
	if n := e.f.Calls("", "/"); n != 0 {
		t.Errorf("%d requests while logged out", n)
	}
}
