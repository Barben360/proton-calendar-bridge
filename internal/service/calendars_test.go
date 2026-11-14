package service_test

import (
	"bytes"
	"log/slog"
	"slices"
	"strconv"
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
	e.signIn(t)
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
	e.signIn(t)
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
	e.signIn(t)
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
	e.signIn(t)
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

// Ruling R9m: Proton unavailable or rate limiting on a calendar's bootstrap
// fails the whole listing (exit 20); only a deterministic refusal is reported
// on the calendar.
func TestCalendarsTransientBootstrapFailureIsBlocked(t *testing.T) {
	t.Parallel()
	for _, status := range []int{503, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, protontest.Options{})
			e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
			b := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "B"})
			e.signIn(t)
			for range 3 { // every attempt the client makes
				e.f.FailNext("GET", "/calendar/v2/"+b+"/bootstrap", status, 0, 0)
			}
			cals, err := e.svc.Calendars(t.Context())
			_ = wantExit(t, err, exitcode.Blocked)
			if cals != nil {
				t.Errorf("a partial list was returned: %+v", cals)
			}
		})
	}
}

// Observation O1: a request Proton makes wait is retried silently, so the
// service's logger gets one debug record per wait: the method, the path, the
// status and the wait, never a token, the session UID or the key passphrase.
func TestARetriedRequestIsLoggedAtDebug(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	b := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "B"})
	e.signIn(t)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	path := "/calendar/v2/" + b + "/bootstrap"
	e.f.FailNext("GET", path, 429, 85131, 7)
	if _, err := svc.Calendars(t.Context()); err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	var retries []string
	for l := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(l, "retrying a Proton request") {
			retries = append(retries, l)
		}
	}
	want := "level=DEBUG msg=\"retrying a Proton request\" method=GET path=\"" + path + "\" status=429 wait=7s"
	if len(retries) != 1 || !strings.Contains(retries[0], want) {
		t.Fatalf("retry records %q, want one with %q", retries, want)
	}
	accs := e.accounts(t)
	if len(accs) != 1 {
		t.Fatalf("%d stored sessions, want 1", len(accs))
	}
	for _, acc := range accs {
		for _, bad := range []string{acc.AccessToken, acc.RefreshToken, acc.UID, string(acc.KeyPass), "Bearer"} {
			if bad == "" || strings.Contains(buf.String(), bad) {
				t.Errorf("the log holds a secret, or the session one is empty (%d bytes): %s", len(bad), retries[0])
			}
		}
	}
}

func TestCalendarsRefusedBootstrapIsPerCalendar(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	a := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "B"})
	e.signIn(t)
	e.f.FailNext("GET", "/calendar/v2/"+a+"/bootstrap", 422, 2501, 0)
	got := e.calendars(t)
	if len(got) != 2 || got[0].Keys != "error" || got[0].KeysError == "" || got[1].Keys != "ok" {
		t.Fatalf("calendars = %+v, want A with a keys error and B ok", got)
	}
}
