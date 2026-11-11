// Package events reads Proton calendar event rows and a calendar's change
// feed. Rows are returned as the plaintext columns the sync needs plus the raw
// JSON object, which the cache stores as received (nothing decrypted is
// kept). Nothing here decrypts or caches.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// PageSize is the listing page size: Proton refuses more than 100 (400/2021).
const PageSize = 100

// ErrGone means the row no longer exists (HTTP 404, or 422 with code 2501).
var ErrGone = errors.New("event no longer exists")

// Row is one event row: the plaintext columns and the row as received.
type Row struct {
	ID, CalendarID, UID string
	Start, End          int64 // unix seconds
	FullDay             bool
	RecurrenceID        *int64 // unix seconds; nil for a master or a single event
	RRule               string // "" when the row has none
	LastEditTime        int64  // unix seconds
	// Raw is the row object exactly as received (not re-encoded).
	Raw json.RawMessage
}

// wireRow is the plaintext part of a row on the wire.
type wireRow struct {
	ID           string
	CalendarID   string
	UID          string
	StartTime    int64
	EndTime      int64
	FullDay      int
	RecurrenceID *int64
	RRule        *string
	LastEditTime int64
}

func parseRow(raw json.RawMessage) (Row, error) {
	var w wireRow
	if err := json.Unmarshal(raw, &w); err != nil {
		return Row{}, fmt.Errorf("decoding an event row: %w", err)
	}
	r := Row{
		ID: w.ID, CalendarID: w.CalendarID, UID: w.UID, Start: w.StartTime, End: w.EndTime,
		FullDay: w.FullDay != 0, RecurrenceID: w.RecurrenceID, LastEditTime: w.LastEditTime,
		Raw: append(json.RawMessage(nil), raw...),
	}
	if w.RRule != nil {
		r.RRule = *w.RRule
	}
	return r, nil
}

// ListAll returns every row of the calendar: pages of PageSize from page 0
// until a page shorter than PageSize.
func ListAll(ctx context.Context, c *api.Client, calID string) ([]Row, error) {
	var rows []Row
	for page := 0; ; page++ {
		var out struct{ Events []json.RawMessage }
		q := url.Values{"Page": {strconv.Itoa(page)}, "PageSize": {strconv.Itoa(PageSize)}}
		if err := c.Get(ctx, "/calendar/v1/"+url.PathEscape(calID)+"/events", q, &out); err != nil {
			return nil, fmt.Errorf("listing events (page %d): %w", page, err)
		}
		for _, raw := range out.Events {
			r, err := parseRow(raw)
			if err != nil {
				return nil, err
			}
			rows = append(rows, r)
		}
		if len(out.Events) < PageSize {
			return rows, nil
		}
	}
}

// Get returns one row, or ErrGone when it no longer exists.
func Get(ctx context.Context, c *api.Client, calID, id string) (Row, error) {
	var out struct{ Event json.RawMessage }
	err := c.Get(ctx, "/calendar/v1/"+url.PathEscape(calID)+"/events/"+url.PathEscape(id), nil, &out)
	var ae *api.Error
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || (ae.Status == http.StatusUnprocessableEntity && ae.Code == api.CodeNotFound)) {
		return Row{}, fmt.Errorf("reading an event: %w", ErrGone)
	}
	if err != nil {
		return Row{}, fmt.Errorf("reading an event: %w", err)
	}
	if len(out.Event) == 0 || string(out.Event) == "null" {
		return Row{}, errors.New("reading an event: the answer holds no event")
	}
	return parseRow(out.Event)
}
