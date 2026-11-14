package probe_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

const feedPart = `{"Type":3,"Data":"PARTDATA-s3cr3t-0123456789","Signature":"PARTSIG-s3cr3t-0123456789","Author":"someone@example.test"}`

func feedFront(t *testing.T) *labtest.Front {
	t.Helper()
	return labtest.NewFront(t, &labtest.Front{Replies: map[string]labtest.Reply{
		"GET /calendar/v1/calA/modelevents/latest": labtest.R(200, `{"Code":1000,"CalendarModelEventID":"cursorHEAD0001"}`),
		"GET /core/v4/events/latest":               labtest.R(200, `{"Code":1000,"EventID":"coreHEAD0001"}`),
		"GET /calendar/v1/calA/modelevents/cursorHEAD0001": labtest.R(200,
			`{"Code":1000,"CalendarModelEventID":"cursorNEXT0002","More":1,"Refresh":0,"CalendarEvents":[{"ID":"ev1","Action":1},{"ID":"ev2","Action":0}]}`),
		"GET /calendar/v1/calA/modelevents/cursorNEXT0002": labtest.R(200,
			`{"Code":1000,"CalendarModelEventID":"cursorLAST0003","More":0,"Refresh":0,"CalendarEvents":[{"ID":"ev1","Action":2}]}`),
		"GET /calendar/v1/calA/events/ev1": labtest.R(200,
			`{"Code":1000,"Event":{"ID":"ev1","LastEditTime":1,"SharedEvents":[`+feedPart+`],"CalendarEvents":[`+feedPart+`]}}`),
		"GET /calendar/v1/calA/events/ev2": labtest.R(422, `{"Code":2501,"Error":"Event does not exist"}`),
		"GET /core/v4/events/coreHEAD0001": labtest.R(200, `{"Code":1000,"EventID":"coreNEXT0002","More":0,"Refresh":0,"Calendars":[{"ID":"calA","Action":2}]}`),
	}})
}

func TestProbeFeedRecordsRedacted(t *testing.T) {
	t.Parallel()
	f := feedFront(t)
	o := loggedIn(t, f, "")
	dir := t.TempDir()
	rec := &record.Recorder{Dir: dir}
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	waited := 0
	res, err := probe.Feed(t.Context(), s, rec, "calA", func(context.Context) error { waited++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if waited != 1 {
		t.Errorf("wait called %d times, want once (between the heads and the walk)", waited)
	}
	if res.Pages != 2 || res.Entries != 3 || res.Rows != 2 || res.Gone != 1 || res.Last != "cursorLAST0003" || res.CoreStart != "coreHEAD0001" {
		t.Errorf("result = %+v", res)
	}
	for _, name := range []string{"latest", "core-latest", "page-01", "page-02", "row-01", "row-02", "core-events"} {
		b, err := os.ReadFile(filepath.Join(dir, "feed-"+name+".json"))
		if err != nil {
			t.Fatalf("recording %s missing: %v", name, err)
		}
		for _, secret := range []string{"PARTDATA", "PARTSIG", "someone@example.test", o.Creds.Password} {
			if secret != "" && strings.Contains(string(b), secret) {
				t.Errorf("%s leaks %q: %s", name, secret, b)
			}
		}
	}
	page, _ := os.ReadFile(filepath.Join(dir, "feed-page-01.json"))
	if !strings.Contains(string(page), "cursorNEXT0002") || !strings.Contains(string(page), `"ev1"`) {
		t.Errorf("cursors and IDs must survive redaction: %s", page)
	}
	row, _ := os.ReadFile(filepath.Join(dir, "feed-row-01.json"))
	if !strings.Contains(string(row), "<redacted:len=") || !strings.Contains(string(row), `"Type": 3`) {
		t.Errorf("part kept as shape only (type kept, Data/Signature redacted): %s", row)
	}
	// No secret leaked into a request path.
	for _, line := range paths(f, 0) {
		if strings.Contains(line, "s3cr3t") {
			t.Errorf("secret in a request path: %s", line)
		}
	}
}

func TestProbeFeedStopsOnRefreshAndRecordsIt(t *testing.T) {
	t.Parallel()
	f := feedFront(t)
	f.Replies["GET /calendar/v1/calA/modelevents/cursorHEAD0001"] = labtest.R(200,
		`{"Code":1000,"CalendarModelEventID":"cursorNEXT0002","More":1,"Refresh":1,"CalendarEvents":[]}`)
	o := loggedIn(t, f, "")
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mark := len(f.Log)
	res, err := probe.Feed(t.Context(), s, nil, "calA", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refresh || res.Pages != 1 {
		t.Errorf("result = %+v, want a stop at the Refresh page", res)
	}
	for _, p := range paths(f, mark) {
		if strings.HasSuffix(p, "cursorNEXT0002") {
			t.Errorf("followed the cursor after Refresh: %s", p)
		}
	}
}

func TestProbeFeedWithoutHeadFailsBeforeWaiting(t *testing.T) {
	t.Parallel()
	f := feedFront(t)
	f.Replies["GET /calendar/v1/calA/modelevents/latest"] = labtest.R(404, `{"Code":2501,"Error":"no"}`)
	o := loggedIn(t, f, "")
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = probe.Feed(t.Context(), s, nil, "calA", func(context.Context) error { t.Error("waited"); return nil })
	if err == nil {
		t.Fatal("want an error when the head is unavailable")
	}
}
