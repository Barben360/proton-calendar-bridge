package protontest_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// rawGet sends one GET with tk and decodes the answer into out; it returns the
// status and the Proton code.
func rawGet(t *testing.T, f *protontest.Fake, tk api.Tokens, path string, out any) (status, code int) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("x-pm-uid", tk.UID)
	req.Header.Set("Authorization", "Bearer "+tk.AccessToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	var env struct{ Code int }
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &env)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, env.Code
}

type feedAnswer struct {
	CalendarModelEventID string
	More, Refresh        int
	CalendarEvents       []struct {
		ID     string
		Action int
	}
}

func latest(t *testing.T, f *protontest.Fake, tk api.Tokens, cal string) string {
	t.Helper()
	var out struct{ CalendarModelEventID string }
	if st, _ := rawGet(t, f, tk, "/calendar/v1/"+cal+"/modelevents/latest", &out); st != 200 {
		t.Fatalf("latest: HTTP %d", st)
	}
	return out.CalendarModelEventID
}

func feed(t *testing.T, f *protontest.Fake, tk api.Tokens, cal, cursor string) (feedAnswer, int, int) {
	t.Helper()
	var out feedAnswer
	st, code := rawGet(t, f, tk, "/calendar/v1/"+cal+"/modelevents/"+url.PathEscape(cursor), &out)
	return out, st, code
}

func TestFakeFeedRecordsEveryChange(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	tk := f.Session(t, f.Username)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	start := latest(t, f, tk, cal)

	a := f.AddEvent(cal, protontest.Event{UID: "u1", Start: 1000, End: 2000})
	b := f.AddEvent(cal, protontest.Event{UID: "u2", Start: 1000, End: 2000})
	f.UpdateEvent(cal, a, protontest.Event{UID: "u1", Start: 1500, End: 2500})
	f.DeleteEvent(cal, b)

	got, st, _ := feed(t, f, tk, cal, start)
	if st != 200 {
		t.Fatalf("feed: HTTP %d", st)
	}
	want := []struct {
		id     string
		action int
	}{{a, 1}, {b, 1}, {a, 2}, {b, 0}}
	if len(got.CalendarEvents) != len(want) {
		t.Fatalf("feed has %d entries, want %d: %+v", len(got.CalendarEvents), len(want), got.CalendarEvents)
	}
	for i, w := range want {
		if e := got.CalendarEvents[i]; e.ID != w.id || e.Action != w.action {
			t.Errorf("entry %d = %+v, want %+v", i, e, w)
		}
	}
	if got.More != 0 || got.Refresh != 0 {
		t.Errorf("More/Refresh = %d/%d, want 0/0", got.More, got.Refresh)
	}
	if got.CalendarModelEventID != latest(t, f, tk, cal) {
		t.Errorf("returned cursor %q is not the head %q", got.CalendarModelEventID, latest(t, f, tk, cal))
	}
	again, _, _ := feed(t, f, tk, cal, got.CalendarModelEventID)
	if len(again.CalendarEvents) != 0 || again.CalendarModelEventID != got.CalendarModelEventID {
		t.Errorf("nothing new must answer no entries and the same cursor, got %+v", again)
	}
}

func TestFakeFeedPagesAndRefresh(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	tk := f.Session(t, f.Username)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	f.SetFeedPageSize(2)
	start := latest(t, f, tk, cal)
	for range 5 {
		f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	}
	p1, _, _ := feed(t, f, tk, cal, start)
	if len(p1.CalendarEvents) != 2 || p1.More != 1 {
		t.Fatalf("page 1 = %d entries, More %d; want 2, 1", len(p1.CalendarEvents), p1.More)
	}
	p2, _, _ := feed(t, f, tk, cal, p1.CalendarModelEventID)
	p3, _, _ := feed(t, f, tk, cal, p2.CalendarModelEventID)
	if len(p3.CalendarEvents) != 1 || p3.More != 0 || p2.More != 1 {
		t.Errorf("pages 2/3 = More %d/%d, last has %d entries; want 1/0 and 1", p2.More, p3.More, len(p3.CalendarEvents))
	}

	f.ForceRefresh(cal)
	if r, _, _ := feed(t, f, tk, cal, start); r.Refresh != 1 {
		t.Errorf("ForceRefresh: Refresh = %d, want 1", r.Refresh)
	}
	if r, _, _ := feed(t, f, tk, cal, start); r.Refresh != 0 {
		t.Errorf("Refresh must be one-shot, got %d", r.Refresh)
	}
}

func TestFakeFeedRefusesUnknownAndExpiredCursors(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	tk := f.Session(t, f.Username)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	old := latest(t, f, tk, cal)
	f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	if _, st, code := feed(t, f, tk, cal, "nonsense"); st != 422 || code != 2001 {
		t.Errorf("unknown cursor: HTTP %d code %d, want 422/2001", st, code)
	}
	f.ExpireFeed(cal)
	if _, st, code := feed(t, f, tk, cal, old); st != 422 || code != 2001 {
		t.Errorf("expired cursor: HTTP %d code %d, want 422/2001", st, code)
	}
	fresh := latest(t, f, tk, cal)
	if fresh == old {
		t.Fatal("the head must move on after ExpireFeed")
	}
	if _, st, _ := feed(t, f, tk, cal, fresh); st != 200 {
		t.Errorf("a cursor read after ExpireFeed must work, got HTTP %d", st)
	}
}

func TestFakeListingRefusesLargePages(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	tk := f.Session(t, f.Username)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	for range 3 {
		f.AddEvent(cal, protontest.Event{UID: "u", Start: 1, End: 2})
	}
	if st, code := rawGet(t, f, tk, "/calendar/v1/"+cal+"/events?Page=0&PageSize=101", nil); st != 400 || code != 2021 {
		t.Errorf("PageSize 101: HTTP %d code %d, want 400/2021", st, code)
	}
	var out struct {
		Events []json.RawMessage
		Total  int
	}
	if st, _ := rawGet(t, f, tk, "/calendar/v1/"+cal+"/events?Page=0&PageSize=100", &out); st != 200 || len(out.Events) != 3 || out.Total != 3 {
		t.Errorf("PageSize 100: HTTP %d, %d rows, Total %d; want 200, 3, 3", st, len(out.Events), out.Total)
	}
	if st, _ := rawGet(t, f, tk, "/calendar/v1/"+cal+"/events?Page=1&PageSize=2", &out); st != 200 || len(out.Events) != 1 {
		t.Errorf("page 1 of size 2: HTTP %d, %d rows; want 200, 1", st, len(out.Events))
	}
}

func TestFakeRowFieldsAndGet(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	tk := f.Session(t, f.Username)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	other := f.AddCalendar(f.Username, protontest.Calendar{Name: "B"})
	rid := int64(4242)
	id := f.AddEvent(cal, protontest.Event{UID: "u1", Start: 1000, End: 2000, FullDay: true, RRule: "FREQ=DAILY", RecurrenceID: &rid})
	plain := f.AddEvent(cal, protontest.Event{UID: "u2", Start: 1, End: 2})

	var got struct{ Event map[string]any }
	if st, _ := rawGet(t, f, tk, "/calendar/v1/"+cal+"/events/"+url.PathEscape(id), &got); st != 200 {
		t.Fatalf("get: HTTP %d", st)
	}
	e := got.Event
	for k, want := range map[string]any{"ID": id, "CalendarID": cal, "UID": "u1", "StartTime": 1000.0, "EndTime": 2000.0, "FullDay": 1.0, "RRule": "FREQ=DAILY", "RecurrenceID": 4242.0} {
		if e[k] != want {
			t.Errorf("%s = %v, want %v", k, e[k], want)
		}
	}
	for _, k := range []string{"SharedEventID", "CreateTime", "ModifyTime", "LastEditTime", "StartTimezone", "EndTimezone", "IsOrganizer", "Exdates", "Author", "Permissions", "Color", "Notifications", "SharedKeyPacket", "CalendarKeyPacket", "SharedEvents", "CalendarEvents", "PersonalEvents", "AttendeesEvents", "Attendees"} {
		if _, ok := e[k]; !ok {
			t.Errorf("row lacks field %s", k)
		}
	}
	var p struct{ Event map[string]any }
	rawGet(t, f, tk, "/calendar/v1/"+cal+"/events/"+url.PathEscape(plain), &p)
	if p.Event["RRule"] != nil || p.Event["RecurrenceID"] != nil || p.Event["FullDay"] != 0.0 {
		t.Errorf("plain row: RRule %v, RecurrenceID %v, FullDay %v; want null, null, 0", p.Event["RRule"], p.Event["RecurrenceID"], p.Event["FullDay"])
	}

	before := e["LastEditTime"].(float64)
	f.UpdateEvent(cal, id, protontest.Event{UID: "u1", Start: 1000, End: 2000})
	rawGet(t, f, tk, "/calendar/v1/"+cal+"/events/"+url.PathEscape(id), &got)
	if got.Event["LastEditTime"].(float64) <= before {
		t.Errorf("UpdateEvent must bump LastEditTime (%v -> %v)", before, got.Event["LastEditTime"])
	}

	if st, code := rawGet(t, f, tk, "/calendar/v1/"+other+"/events/"+url.PathEscape(id), nil); st != 422 || code != 2501 {
		t.Errorf("row of another calendar: HTTP %d code %d, want 422/2501", st, code)
	}
	f.DeleteEvent(cal, id)
	if st, code := rawGet(t, f, tk, "/calendar/v1/"+cal+"/events/"+url.PathEscape(id), nil); st != 422 || code != 2501 {
		t.Errorf("deleted row: HTTP %d code %d, want 422/2501", st, code)
	}
}

func TestFakeEventsOfAnotherUserAreHidden(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	f.AddUser("pcbother", "pw-other")
	mine := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	id := f.AddEvent(mine, protontest.Event{UID: "u", Start: 1, End: 2})
	tk := f.Session(t, "pcbother")
	for _, p := range []string{"/events", "/events/" + id, "/modelevents/latest"} {
		if st, code := rawGet(t, f, tk, "/calendar/v1/"+mine+p, nil); st != 422 || code != 2501 {
			t.Errorf("GET %s as another user: HTTP %d code %d, want 422/2501", p, st, code)
		}
	}
}

func TestSessionTokensFollowTheFakesRules(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	tk := f.Session(t, f.Username)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	path := "/calendar/v1/" + cal + "/events"
	if st, _ := rawGet(t, f, tk, path, nil); st != 200 {
		t.Fatalf("valid session: HTTP %d", st)
	}
	bad := tk
	bad.AccessToken = "fakeaccforged"
	if st, _ := rawGet(t, f, bad, path, nil); st != 401 {
		t.Errorf("forged token: HTTP %d, want 401", st)
	}
	wrongUID := tk
	wrongUID.UID = "other"
	if st, _ := rawGet(t, f, wrongUID, path, nil); st != 401 {
		t.Errorf("token with another UID: HTTP %d, want 401", st)
	}
	if st, _ := rawGet(t, f, tk, "/calendar/v1", nil); st != 401 {
		t.Errorf("calendar list with a Session (backend needed): HTTP %d, want 401", st)
	}
	f.ExpireAccessTokens()
	if st, _ := rawGet(t, f, tk, path, nil); st != 401 {
		t.Errorf("expired session: HTTP %d, want 401", st)
	}
	tk2 := f.Session(t, f.Username)
	f.RevokeSessions()
	if st, _ := rawGet(t, f, tk2, path, nil); st != 401 {
		t.Errorf("revoked session: HTTP %d, want 401", st)
	}
}

func TestFakeOnListPageRunsBeforeThePageIsSent(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	tk := f.Session(t, f.Username)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	first := f.AddEvent(cal, protontest.Event{UID: "a", Start: 1, End: 2})
	var added string
	var pages []int
	f.OnListPage(func(calID string, page int) {
		pages = append(pages, page)
		if calID == cal && added == "" {
			added = f.AddEvent(cal, protontest.Event{UID: "b", Start: 1, End: 2})
		}
	})
	type listing struct{ Events []struct{ ID string } }
	var out listing
	if st, _ := rawGet(t, f, tk, "/calendar/v1/"+cal+"/events?Page=0&PageSize=100", &out); st != 200 {
		t.Fatalf("list: HTTP %d", st)
	}
	f.OnListPage(nil) // takes the fake's lock: the hook's writes are visible after it
	if len(out.Events) != 1 || out.Events[0].ID != first {
		t.Fatalf("page served while the hook added a row = %+v, want only %s", out.Events, first)
	}
	if added == "" || len(pages) != 1 || pages[0] != 0 {
		t.Fatalf("hook ran for pages %v (added %q), want once for page 0", pages, added)
	}
	out = listing{}
	if st, _ := rawGet(t, f, tk, "/calendar/v1/"+cal+"/events?Page=0&PageSize=100", &out); st != 200 {
		t.Fatalf("second list: HTTP %d", st)
	}
	if len(out.Events) != 2 || len(pages) != 1 {
		t.Errorf("after removing the hook: %d rows, hook calls %v; want 2 rows, no new call", len(out.Events), pages)
	}
}
