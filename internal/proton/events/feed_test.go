package events_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

func TestLatestAndNothingNew(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	cur, err := events.Latest(t.Context(), c, cal)
	if err != nil || cur == "" {
		t.Fatalf("Latest = %q, %v", cur, err)
	}
	d, err := events.Changes(t.Context(), c, cal, cur)
	if err != nil {
		t.Fatal(err)
	}
	if d.Cursor != cur || len(d.IDs) != 0 || d.Refresh {
		t.Errorf("nothing new: %+v, want the same cursor %q, no IDs, no refresh", d, cur)
	}
	id := f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	d, err = events.Changes(t.Context(), c, cal, cur)
	if err != nil {
		t.Fatal(err)
	}
	if d.Cursor == cur || !slices.Equal(d.IDs, []string{id}) {
		t.Errorf("one change: %+v, want a new cursor and [%s]", d, id)
	}
}

func TestChangesFollowsMore(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	start, err := events.Latest(t.Context(), c, cal)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range 100 { // 100 creations of 100 rows ...
		ids = append(ids, f.AddEvent(cal, protontest.Event{UID: "u" + strconv.Itoa(i), Start: 1, End: 2}))
	}
	for _, id := range ids[:20] { // ... plus 20 updates of the first 20: 120 entries, 100 distinct
		f.UpdateEvent(cal, id, protontest.Event{UID: "changed", Start: 1, End: 2})
	}
	before := f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/")
	d, err := events.Changes(t.Context(), c, cal, start)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/") - before; n != 3 {
		t.Errorf("%d feed requests, want 3 (50+50+20)", n)
	}
	if !slices.Equal(d.IDs, ids) {
		t.Errorf("IDs = %d entries, want the 100 distinct IDs in first-appearance order", len(d.IDs))
	}
	if head, _ := events.Latest(t.Context(), c, cal); d.Cursor != head {
		t.Errorf("final cursor %q, want the head %q", d.Cursor, head)
	}
	if d.Refresh {
		t.Error("Refresh set without cause")
	}
}

func TestChangesRefresh(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	start, _ := events.Latest(t.Context(), c, cal)
	f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	f.ForceRefresh(cal)
	d, err := events.Changes(t.Context(), c, cal, start)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Refresh || d.TooLong {
		t.Errorf("Refresh %v, TooLong %v; want Proton's Refresh, not a long feed: %+v", d.Refresh, d.TooLong, d)
	}
}

// afterFirstFeedPage raises the calendar's Refresh flag once the first feed
// page has been served, so only a later page carries it.
type afterFirstFeedPage struct {
	f    *protontest.Fake
	cal  string
	done bool
}

func (a *afterFirstFeedPage) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && !a.done && strings.Contains(r.URL.Path, "/modelevents/") && !strings.HasSuffix(r.URL.Path, "/latest") {
		a.done = true
		a.f.ForceRefresh(a.cal)
	}
	return resp, err
}

func TestChangesRefreshOnLaterPage(t *testing.T) {
	t.Parallel()
	f, _, cal := setup(t)
	c := api.New(f.Session(t, f.Username), api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 5 * time.Second, Transport: &afterFirstFeedPage{f: f, cal: cal}},
	})
	f.SetFeedPageSize(1)
	start, _ := events.Latest(t.Context(), c, cal)
	f.AddEvent(cal, protontest.Event{UID: "a", Start: 1, End: 2})
	f.AddEvent(cal, protontest.Event{UID: "b", Start: 1, End: 2})
	d, err := events.Changes(t.Context(), c, cal, start)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Refresh || len(d.IDs) != 2 {
		t.Errorf("%+v, want Refresh from the second page and both IDs", d)
	}
}

func TestChangesTooManyPagesMeansRefresh(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	f.SetFeedPageSize(1)
	start, _ := events.Latest(t.Context(), c, cal)
	for range 60 {
		f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	}
	before := f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/")
	d, err := events.Changes(t.Context(), c, cal, start)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/") - before; n != 50 {
		t.Errorf("%d feed requests, want the cap of 50", n)
	}
	if !d.Refresh || !d.TooLong {
		t.Errorf("more than 50 pages must set Refresh and TooLong: %+v", d)
	}
}

func TestChangesExactly50PagesIsNotRefresh(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	f.SetFeedPageSize(1)
	start, _ := events.Latest(t.Context(), c, cal)
	for range 50 {
		f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	}
	d, err := events.Changes(t.Context(), c, cal, start)
	if err != nil {
		t.Fatal(err)
	}
	if d.Refresh || d.TooLong || len(d.IDs) != 50 {
		t.Errorf("50 pages ending with More 0: Refresh %v, %d IDs; want false, 50", d.Refresh, len(d.IDs))
	}
}

func TestChangesUnusableCursorIsAnAPIError(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	old, _ := events.Latest(t.Context(), c, cal)
	f.ExpireFeed(cal)
	_, err := events.Changes(t.Context(), c, cal, old)
	if err == nil || !apiCode(err, 2001) {
		t.Errorf("expired cursor: %v, want an API error with code 2001 (the caller decides to resync)", err)
	}
}
