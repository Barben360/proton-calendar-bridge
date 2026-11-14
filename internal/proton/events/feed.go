package events

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// MaxFeedPages bounds one Changes call: past it the caller must resync.
const MaxFeedPages = 50

// Delta is what changed in a calendar since a cursor.
type Delta struct {
	Cursor  string   // where to resume; the input cursor when nothing is new
	IDs     []string // changed event IDs, deduplicated, in first-appearance order
	Refresh bool     // the cache must be rebuilt (Proton asked, or too many pages)
	TooLong bool     // Refresh because the feed ran past MaxFeedPages, not because Proton asked
}

// Latest returns the calendar's current head of the change feed.
func Latest(ctx context.Context, c *api.Client, calID string) (string, error) {
	var out struct{ CalendarModelEventID string }
	if err := c.Get(ctx, "/calendar/v1/"+url.PathEscape(calID)+"/modelevents/latest", nil, &out); err != nil {
		return "", fmt.Errorf("reading the change feed head: %w", err)
	}
	if out.CalendarModelEventID == "" {
		return "", errors.New("reading the change feed head: the answer holds no cursor")
	}
	return out.CalendarModelEventID, nil
}

// Changes reads the feed after cursor, following More for up to MaxFeedPages
// pages. Action and the embedded event are ignored: every ID must be re-read.
// A cursor Proton rejects comes back as the *api.Error (a 4xx other than 429
// means the caller should resync).
func Changes(ctx context.Context, c *api.Client, calID, cursor string) (Delta, error) {
	d := Delta{Cursor: cursor}
	seen := map[string]bool{}
	for page := 1; ; page++ {
		var out struct {
			CalendarModelEventID string
			More, Refresh        int
			CalendarEvents       []struct{ ID string }
		}
		if err := c.Get(ctx, "/calendar/v1/"+url.PathEscape(calID)+"/modelevents/"+url.PathEscape(d.Cursor), nil, &out); err != nil {
			return Delta{}, fmt.Errorf("reading the change feed: %w", err)
		}
		for _, e := range out.CalendarEvents {
			if e.ID != "" && !seen[e.ID] {
				seen[e.ID] = true
				d.IDs = append(d.IDs, e.ID)
			}
		}
		if out.CalendarModelEventID != "" {
			d.Cursor = out.CalendarModelEventID
		}
		if out.Refresh != 0 {
			d.Refresh = true
		}
		if out.More == 0 {
			return d, nil
		}
		if page == MaxFeedPages {
			d.Refresh, d.TooLong = true, true
			return d, nil
		}
	}
}
