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
// Proton answers 2501 for a calendar that no longer exists too: the syncer
// re-reads the calendar list first, so a missing calendar is handled there
// before a row is read.
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

// Listing is a calendar's full listing.
type Listing struct {
	// Rows are unique by ID, in first-appearance order; a row served twice
	// (the listing shifted while it was read) keeps its later copy.
	Rows []Row
	// Total is the row count the first page announced, -1 when it had none.
	// Rows shorter than Total means the listing shifted under a concurrent
	// edit and may have skipped a row.
	Total int
}

// ListAll returns every row of the calendar (List's rows).
func ListAll(ctx context.Context, c *api.Client, calID string) ([]Row, error) {
	l, err := List(ctx, c, calID)
	return l.Rows, err
}

// List reads every row of the calendar: pages of PageSize from page 0
// until a page shorter than PageSize, or until Total rows when the answer
// carries it. A server that never ends is cut off after maxPages pages.
// Pages are offsets, so a row created or deleted while the listing runs can
// make a later page repeat a row or skip one: the caller checks Total and
// the feed head.
func List(ctx context.Context, c *api.Client, calID string) (Listing, error) {
	return list(ctx, c, calID, nil)
}

// list is List with extra query parameters (UID= for ListByUID).
func list(ctx context.Context, c *api.Client, calID string, extra url.Values) (Listing, error) {
	l := Listing{Total: -1}
	at := map[string]int{} // ID -> index in l.Rows
	served := 0
	for page := 0; page < maxPages; page++ {
		var out struct {
			Events []json.RawMessage
			Total  *int
		}
		q := url.Values{"Page": {strconv.Itoa(page)}, "PageSize": {strconv.Itoa(PageSize)}}
		for k, v := range extra {
			q[k] = v
		}
		if err := c.Get(ctx, "/calendar/v1/"+url.PathEscape(calID)+"/events", q, &out); err != nil {
			return Listing{}, fmt.Errorf("listing events (page %d): %w", page, err)
		}
		if page == 0 && out.Total != nil {
			l.Total = *out.Total
		}
		for _, raw := range out.Events {
			r, err := parseRow(raw)
			if err != nil {
				return Listing{}, err
			}
			served++
			if i, dup := at[r.ID]; dup {
				l.Rows[i] = r
				continue
			}
			at[r.ID] = len(l.Rows)
			l.Rows = append(l.Rows, r)
		}
		if len(out.Events) < PageSize || (out.Total != nil && served >= *out.Total) {
			return l, nil
		}
	}
	return Listing{}, fmt.Errorf("listing events: more than %d pages, giving up", maxPages)
}

// maxPages bounds ListAll (a variable for the tests): 10000 pages is a million
// rows, far beyond a calendar.
var maxPages = 10000

// Get returns one row, or ErrGone when it no longer exists.
func Get(ctx context.Context, c *api.Client, calID, id string) (Row, error) {
	if id == "" {
		return Row{}, errors.New("reading an event: empty event ID")
	}
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
