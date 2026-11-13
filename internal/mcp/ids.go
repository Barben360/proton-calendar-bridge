package mcp

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// An event_id is opaque to agents (design §5.7): the unpadded base64url of
// localID NUL calendarID NUL uid, then NUL and the recurrence ID for one
// occurrence. The recurrence ID keeps its value type (ruling R10): a date
// (2026-11-14), a floating wall clock without offset (2026-11-14T15:00:00),
// or the instant in RFC 3339 UTC; the service matches an occurrence by
// that, never by its TZID.

const floatingLayout = "2006-01-02T15:04:05"

var errBadEventID = errors.New("not an event ID")

func encodeEventID(localID string, ref service.EventRef) string {
	parts := []string{localID, ref.CalendarID, ref.UID}
	if rid := ref.RecurrenceID; rid != nil {
		switch {
		case rid.Date != "":
			parts = append(parts, rid.Date)
		case rid.Floating:
			parts = append(parts, rid.At.Format(floatingLayout))
		default:
			parts = append(parts, rid.At.UTC().Format(time.RFC3339))
		}
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x00")))
}

func decodeEventID(id string) (localID string, ref service.EventRef, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", service.EventRef{}, errBadEventID
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) < 3 || len(parts) > 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", service.EventRef{}, errBadEventID
	}
	ref = service.EventRef{CalendarID: parts[1], UID: parts[2]}
	if len(parts) == 4 {
		rid, ok := parseRID(parts[3])
		if !ok {
			return "", service.EventRef{}, errBadEventID
		}
		ref.RecurrenceID = &rid
	}
	return parts[0], ref, nil
}

func parseRID(s string) (service.EventTime, bool) {
	if d, err := time.Parse(time.DateOnly, s); err == nil && d.Format(time.DateOnly) == s {
		return service.EventTime{Date: s}, true
	}
	if t, err := time.Parse(floatingLayout, s); err == nil && t.Format(floatingLayout) == s {
		return service.EventTime{At: t, Floating: true}, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil && strings.HasSuffix(s, "Z") {
		return service.EventTime{At: t.UTC()}, true
	}
	return service.EventTime{}, false
}
