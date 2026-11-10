package probe_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

var t0 = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

// loggedIn logs into the fake and returns its options (the saved session is
// what the probes resume).
func loggedIn(t *testing.T, f *labtest.Front, app string) session.Options {
	t.Helper()
	o := session.Options{
		BaseURL:    f.URL,
		AppVersion: app,
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Creds:      session.Creds{Username: labtest.User, Password: labtest.Pass},
		Now:        func() time.Time { return t0 },
	}
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	s.Close()
	return o
}

func paths(f *labtest.Front, from int) []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var p []string
	for _, s := range f.Log[from:] {
		p = append(p, s.Method+" "+s.Path)
	}
	return p
}

func TestCalendarAccessProbesTheThreeEndpointsAndReportsCodes(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{Replies: map[string]labtest.Reply{
		"GET /calendar/v1":                labtest.R(200, `{"Code":1000,"Calendars":[{"ID":"calA"},{"ID":"calB"}]}`),
		"GET /calendar/v1/calA/bootstrap": labtest.R(403, `{"Code":9100,"Error":"Access token does not have sufficient scope"}`),
		"GET /settings/calendar":          labtest.R(200, `{"Code":1000,"CalendarUserSettings":{}}`),
	}})
	o := loggedIn(t, f, "web-calendar@5.0.1")
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mark := len(f.Log)

	got, err := probe.CalendarAccess(t.Context(), s, nil, "x")
	if err != nil {
		t.Fatal(err)
	}
	want := []probe.Access{
		{Path: "/calendar/v1", Status: 200, Code: 1000},
		{Path: "/calendar/v1/calA/bootstrap", Status: 403, Code: 9100},
		{Path: "/settings/calendar", Status: 200, Code: 1000},
	}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if p := strings.Join(paths(f, mark), ","); p != "GET /calendar/v1,GET /calendar/v1/calA/bootstrap,GET /settings/calendar" {
		t.Errorf("requests = %s", p)
	}
	for _, s := range f.Log[mark:] {
		if s.AppVersion != "web-calendar@5.0.1" {
			t.Errorf("%s sent x-pm-appversion %q, want the candidate under test", s.Path, s.AppVersion)
		}
	}
}

func TestCalendarAccessWithoutScopeSkipsBootstrapButKeepsGoing(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{Replies: map[string]labtest.Reply{
		"GET /calendar/v1":       labtest.R(403, `{"Code":9100,"Error":"Access token does not have sufficient scope"}`),
		"GET /settings/calendar": labtest.R(200, `{"Code":1000}`),
	}})
	o := loggedIn(t, f, "")
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mark := len(f.Log)

	got, err := probe.CalendarAccess(t.Context(), s, nil, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Code != 9100 || got[1].Skipped == "" || got[1].Status != 0 || got[2].Code != 1000 {
		t.Fatalf("results = %+v", got)
	}
	if p := strings.Join(paths(f, mark), ","); p != "GET /calendar/v1,GET /settings/calendar" {
		t.Errorf("requests = %s (no bootstrap without a calendar)", p)
	}
	if !got[0].Failed() || got[2].Failed() {
		t.Errorf("Failed() wrong: %+v", got)
	}
}

func TestCalendarAccessRecordsEachAnswerRedacted(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{Replies: map[string]labtest.Reply{
		"GET /calendar/v1":                labtest.R(200, `{"Code":1000,"Calendars":[{"ID":"calA"}],"Token":"SECRETSECRETSECRETSECRETSECRET"}`),
		"GET /calendar/v1/calA/bootstrap": labtest.R(200, `{"Code":1000}`),
		"GET /settings/calendar":          labtest.R(200, `{"Code":1000}`),
	}})
	o := loggedIn(t, f, "")
	dir := t.TempDir()
	rec := &record.Recorder{Dir: dir}
	o.Recorder = rec
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := probe.CalendarAccess(t.Context(), s, rec, "web-calendar@5.0.1"); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 3 {
		t.Fatalf("recorded files = %v, want 3", files)
	}
	for _, name := range []string{"web-calendar_5.0.1-calendar-list.json", "web-calendar_5.0.1-calendar-bootstrap.json", "web-calendar_5.0.1-settings-calendar.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(b), "SECRETSECRET") {
			t.Errorf("%s leaks a secret: %s", name, b)
		}
	}
}
