package events_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// setup returns a fake with one calendar and a client on a Session (no SRP).
func setup(t *testing.T) (*protontest.Fake, *api.Client, string) {
	t.Helper()
	f := protontest.New(t, protontest.Options{})
	c := api.New(f.Session(t, f.Username), api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	})
	return f, c, f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
}

func TestListAllPagesUntilShortPage(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	for i := range 250 {
		f.AddEvent(cal, protontest.Event{UID: "uid" + strconv.Itoa(i), Start: int64(1000 + i), End: int64(2000 + i)})
	}
	rows, err := events.ListAll(t.Context(), c, cal)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 250 {
		t.Fatalf("got %d rows, want 250", len(rows))
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/events"); n != 3 {
		t.Errorf("%d listing requests, want 3 (100+100+50)", n)
	}
	for i, r := range rows {
		if r.UID != "uid"+strconv.Itoa(i) || r.Start != int64(1000+i) || r.End != int64(2000+i) || r.CalendarID != cal {
			t.Fatalf("row %d = %+v", i, r)
		}
	}

	// Raw is byte-equal to the object the server sent.
	served := rawListing(t, f, c, cal, 2)
	if len(served) != 50 {
		t.Fatalf("raw page 2 has %d rows", len(served))
	}
	for i, raw := range served {
		if !bytes.Equal(rows[200+i].Raw, raw) {
			t.Fatalf("row %d: Raw differs from the served object:\n got %s\nwant %s", 200+i, rows[200+i].Raw, raw)
		}
	}
}

// rawListing fetches page n of the listing with the client's own tokens.
func rawListing(t *testing.T, f *protontest.Fake, c *api.Client, cal string, page int) []json.RawMessage {
	t.Helper()
	tk := c.Tokens()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+"/calendar/v1/"+cal+"/events?PageSize=100&Page="+strconv.Itoa(page), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-uid", tk.UID)
	req.Header.Set("Authorization", "Bearer "+tk.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	data, _ := io.ReadAll(resp.Body)
	var out struct{ Events []json.RawMessage }
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out.Events
}

func TestRowDecodesPlaintextFields(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	rid := int64(777)
	id := f.AddEvent(cal, protontest.Event{UID: "u1", Start: 1000, End: 2000, FullDay: true, RRule: "FREQ=WEEKLY", RecurrenceID: &rid})
	plain := f.AddEvent(cal, protontest.Event{UID: "u2", Start: 5, End: 6})

	r, err := events.Get(t.Context(), c, cal, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id || r.CalendarID != cal || r.UID != "u1" || r.Start != 1000 || r.End != 2000 || !r.FullDay || r.RRule != "FREQ=WEEKLY" ||
		r.RecurrenceID == nil || *r.RecurrenceID != 777 || r.LastEditTime == 0 {
		t.Errorf("row = %+v", r)
	}
	var obj map[string]any
	if err := json.Unmarshal(r.Raw, &obj); err != nil || obj["ID"] != id {
		t.Errorf("Raw is not the row object: %v %s", err, r.Raw)
	}
	p, err := events.Get(t.Context(), c, cal, plain)
	if err != nil {
		t.Fatal(err)
	}
	if p.FullDay || p.RecurrenceID != nil || p.RRule != "" {
		t.Errorf("plain row = %+v, want no full day, nil RecurrenceID, empty RRule", p)
	}
}

func TestGetGone(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	id := f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	path := "/calendar/v1/" + cal + "/events/" + url.PathEscape(id)
	for _, tc := range []struct {
		name         string
		status, code int
		gone         bool
	}{
		{"404", 404, 2000, true},
		{"422/2501", 422, 2501, true},
		{"422/other", 422, 2000, false},
		{"403", 403, 9101, false},
	} {
		f.FailNext("GET", path, tc.status, tc.code, 0)
		_, err := events.Get(t.Context(), c, cal, id)
		if err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
		if got := errorsIsGone(err); got != tc.gone {
			t.Errorf("%s: ErrGone = %v, want %v (%v)", tc.name, got, tc.gone, err)
		}
	}
	// And for real: a deleted row.
	f.DeleteEvent(cal, id)
	if _, err := events.Get(t.Context(), c, cal, id); !errorsIsGone(err) {
		t.Errorf("deleted row: %v, want ErrGone", err)
	}
}

func TestPathsEscaped(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	_, err := events.Get(t.Context(), c, cal, "x/modelevents/latest")
	if !errorsIsGone(err) {
		t.Errorf("an ID holding slashes: %v, want ErrGone (an unknown row)", err)
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/latest"); n != 0 {
		t.Errorf("the feed route was reached %d times through an event ID", n)
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/events/x/modelevents/latest"); n != 1 {
		t.Errorf("the ID must travel as one escaped segment (decoded path seen %d times, want 1)", n)
	}
}
