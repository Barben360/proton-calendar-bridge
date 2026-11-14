package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

// Access is the outcome of one calendar-access request.
type Access struct {
	Path    string `json:"path"`
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Skipped string `json:"skipped,omitempty"` // why the request was not made
}

// Failed reports a skipped request or an answer other than HTTP 200 / 1000.
func (a Access) Failed() bool { return a.Skipped != "" || a.Status != http.StatusOK || a.Code != 1000 }

// CalendarAccess asks, with the current identity, whether calendar endpoints
// answer: the calendar list, the first calendar's bootstrap and the calendar
// settings. A 403/9100 means the identity yields no calendar scope. label names
// the candidate in the recorded files.
func CalendarAccess(ctx context.Context, s *session.Session, rec *record.Recorder, label string) ([]Access, error) {
	var out []Access
	step := func(name, path string) (session.Response, error) {
		r, err := s.Get(ctx, path)
		if err != nil {
			return r, err
		}
		out = append(out, Access{Path: path, Status: r.Status, Code: r.Code})
		return r, save(rec, label, name, http.MethodGet, path, r)
	}

	list, err := step("calendar-list", "/calendar/v1")
	if err != nil {
		return out, err
	}
	var body struct{ Calendars []struct{ ID string } }
	if list.Status == http.StatusOK {
		_ = json.Unmarshal(list.Body, &body)
	}
	if len(body.Calendars) > 0 && body.Calendars[0].ID != "" {
		if _, err := step("calendar-bootstrap", fmt.Sprintf("/calendar/v1/%s/bootstrap", body.Calendars[0].ID)); err != nil {
			return out, err
		}
	} else {
		out = append(out, Access{Path: "/calendar/v1/{first}/bootstrap", Skipped: "no calendar in the list"})
	}
	_, err = step("settings-calendar", "/settings/calendar")
	return out, err
}
