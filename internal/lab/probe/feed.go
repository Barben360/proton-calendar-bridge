package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

// maxFeedPages bounds one feed walk (the engine's own limit is also 50).
const maxFeedPages = 50

// FeedResult summarises one `probe feed` run (the raw answers are in the
// recordings, not here).
type FeedResult struct {
	Cursor    string   `json:"cursor"`            // calendar feed head taken at the start
	CoreStart string   `json:"core_cursor"`       // core event head taken at the start
	Pages     int      `json:"feed_pages"`        // modelevents pages read
	Entries   int      `json:"feed_entries"`      // entries over all pages
	Refresh   bool     `json:"refresh"`           // a page asked for a full resync
	Rows      int      `json:"rows"`              // rows re-read
	Gone      int      `json:"gone"`              // re-reads answered 404/422
	Statuses  []string `json:"statuses"`          // "<step> HTTP n code c" per exchange
	Last      string   `json:"last_cursor"`       // cursor returned by the last page
	CoreMore  bool     `json:"core_more"`         // the core page said More
	Stopped   string   `json:"stopped,omitempty"` // why the walk ended early
}

// Feed is E04/E05/E07's live probe, to be run while the author edits the
// calendar in the web app. It records, through the fail-closed redaction:
// the calendar feed head and the core event head (before the edits), then,
// once wait returns, every modelevents page from the first cursor, each
// listed row re-read by ID, and /core/v4/events/{core head}. The raw JSON is
// what the experiments need (real shapes, Action values, which columns move).
// Nothing is decrypted; Data and Signature are redacted by the recorder.
func Feed(ctx context.Context, s *session.Session, rec *record.Recorder, calendarID string, wait func(context.Context) error) (FeedResult, error) {
	var res FeedResult
	cal := "/calendar/v1/" + url.PathEscape(calendarID)
	step := func(name, path string) (session.Response, error) {
		r, err := s.Get(ctx, path)
		if err != nil {
			return r, err
		}
		res.Statuses = append(res.Statuses, fmt.Sprintf("%s HTTP %d code %d", name, r.Status, r.Code))
		return r, save(rec, "feed", name, http.MethodGet, path, r)
	}

	latest, err := step("latest", cal+"/modelevents/latest")
	if err != nil {
		return res, err
	}
	var head struct{ CalendarModelEventID string }
	_ = json.Unmarshal(latest.Body, &head)
	if !latest.OK() || head.CalendarModelEventID == "" {
		return res, fmt.Errorf("feed head unavailable: HTTP %d, Proton code %d", latest.Status, latest.Code)
	}
	res.Cursor = head.CalendarModelEventID

	core, err := step("core-latest", "/core/v4/events/latest")
	if err != nil {
		return res, err
	}
	var coreHead struct{ EventID string }
	_ = json.Unmarshal(core.Body, &coreHead)
	res.CoreStart = coreHead.EventID

	if err := wait(ctx); err != nil {
		return res, err
	}

	cursor := res.Cursor
	var ids []string
	for res.Pages < maxFeedPages {
		res.Pages++
		r, err := step(fmt.Sprintf("page-%02d", res.Pages), cal+"/modelevents/"+url.PathEscape(cursor))
		if err != nil {
			return res, err
		}
		var page struct {
			CalendarModelEventID string
			More, Refresh        int
			CalendarEvents       []struct{ ID string }
		}
		if !r.OK() || json.Unmarshal(r.Body, &page) != nil {
			res.Stopped = fmt.Sprintf("feed page %d: HTTP %d, Proton code %d", res.Pages, r.Status, r.Code)
			break
		}
		res.Entries += len(page.CalendarEvents)
		for _, e := range page.CalendarEvents {
			ids = append(ids, e.ID)
		}
		res.Last = page.CalendarModelEventID
		if page.Refresh != 0 {
			res.Refresh = true
			res.Stopped = "the feed asked for a full resync (Refresh)"
			break
		}
		if page.More == 0 || page.CalendarModelEventID == "" || page.CalendarModelEventID == cursor {
			break
		}
		cursor = page.CalendarModelEventID
	}
	if res.Stopped == "" && res.Pages == maxFeedPages && res.Last != "" && res.Last != cursor {
		res.Stopped = "page limit reached"
	}

	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		res.Rows++
		r, err := step(fmt.Sprintf("row-%02d", res.Rows), cal+"/events/"+url.PathEscape(id))
		if err != nil {
			return res, err
		}
		if r.Status == http.StatusNotFound || r.Status == http.StatusUnprocessableEntity {
			res.Gone++
		}
	}

	if res.CoreStart != "" {
		r, err := step("core-events", "/core/v4/events/"+url.PathEscape(res.CoreStart))
		if err != nil {
			return res, err
		}
		var more struct{ More int }
		_ = json.Unmarshal(r.Body, &more)
		res.CoreMore = more.More != 0
	} else {
		if res.Stopped != "" {
			res.Stopped += "; "
		}
		res.Stopped += "core event head unavailable"
	}
	return res, nil
}
