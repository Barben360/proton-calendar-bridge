package events_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// nonCanonical is a row whose bytes json.Marshal would never produce: spaces,
// unsorted keys, an escaped é, a big integer.
const nonCanonical = `{ "UID" : "café", "ID":"e1",  "LastEditTime": 1731000000123, "StartTime":1,"EndTime":2, "CalendarID":"c", "Extra": [1,  2] }`

func stub(t *testing.T, list string) *api.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = fmt.Fprint(w, list)
		default:
			_, _ = fmt.Fprintf(w, `{"Code":1000,"Event":%s}`, nonCanonical)
		}
	}))
	t.Cleanup(ts.Close)
	return api.New(api.Tokens{UID: "u", AccessToken: "a", RefreshToken: "r"}, api.Options{
		BaseURL: ts.URL, Limiter: rate.NewLimiter(rate.Inf, 1), HTTP: &http.Client{Timeout: 5 * time.Second},
	})
}

func TestRawIsByteExact(t *testing.T) {
	t.Parallel()
	c := stub(t, `{"Code":1000,"Total":1,"Events":[`+nonCanonical+`]}`)
	rows, err := events.ListAll(t.Context(), c, "c")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListAll: %v, %d rows", err, len(rows))
	}
	if !bytes.Equal(rows[0].Raw, []byte(nonCanonical)) {
		t.Errorf("ListAll Raw = %s, want %s", rows[0].Raw, nonCanonical)
	}
	if rows[0].UID != "café" || rows[0].LastEditTime != 1731000000123 {
		t.Errorf("typed fields = %+v", rows[0])
	}
	g, err := events.Get(t.Context(), c, "c", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(g.Raw, []byte(nonCanonical)) {
		t.Errorf("Get Raw = %s, want %s", g.Raw, nonCanonical)
	}
}

func TestListAllStopsAtTotal(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	for range 100 {
		f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	}
	if _, err := events.ListAll(t.Context(), c, cal); err != nil {
		t.Fatal(err)
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/events"); n != 1 {
		t.Errorf("%d requests, want 1 (Total reached on a full page)", n)
	}
}

func TestListAllGivesUpOnEndlessPages(t *testing.T) {
	restore := events.SetMaxPages(3)
	defer restore()
	var rows []string
	for i := range events.PageSize {
		rows = append(rows, fmt.Sprintf(`{"ID":"e%d","UID":"u"}`, i))
	}
	c := stub(t, `{"Code":1000,"Events":[`+strings.Join(rows, ",")+`]}`)
	got, err := events.ListAll(t.Context(), c, "c")
	if err == nil || got != nil {
		t.Errorf("endless full pages: %d rows, %v; want an error and no rows", len(got), err)
	}
}

func TestGetRefusesEmptyID(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	_, err := events.Get(t.Context(), c, cal, "")
	if err == nil || errorsIsGone(err) {
		t.Errorf("empty ID: %v, want a non-ErrGone error", err)
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/events/"); n != 0 {
		t.Errorf("%d requests sent for an empty ID", n)
	}
}

// Without Total, a page shorter than PageSize ends the listing.
func TestListAllStopsOnShortPageWithoutTotal(t *testing.T) {
	t.Parallel()
	page := func(from, n int) string {
		rows := make([]string, n)
		for i := range rows {
			rows[i] = fmt.Sprintf(`{"ID":"e%d","UID":"u"}`, from+i)
		}
		return `{"Code":1000,"Events":[` + strings.Join(rows, ",") + `]}`
	}
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("Page") {
		case "0":
			_, _ = fmt.Fprint(w, page(0, events.PageSize))
		case "1":
			_, _ = fmt.Fprint(w, page(events.PageSize, 50))
		default:
			http.Error(w, `{"Code":2000}`, http.StatusBadRequest)
		}
	}))
	t.Cleanup(ts.Close)
	c := api.New(api.Tokens{UID: "u", AccessToken: "a", RefreshToken: "r"}, api.Options{
		BaseURL: ts.URL, Limiter: rate.NewLimiter(rate.Inf, 1), HTTP: &http.Client{Timeout: 5 * time.Second},
	})
	rows, err := events.ListAll(t.Context(), c, "c")
	if err != nil || len(rows) != events.PageSize+50 {
		t.Fatalf("ListAll: %v, %d rows; want %d", err, len(rows), events.PageSize+50)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests, want 2 (the short page ends the listing)", n)
	}
}
