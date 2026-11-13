package mcp

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Time arguments (design §5.7, Review Focus 1): an RFC 3339 time with an
// offset is an instant as it is; a wall clock without offset or a bare date
// is read in the call's tz argument, else the server's zone ([mcp] timezone,
// else the process's). The zone used is echoed in the result. Anything
// else is refused, naming the argument: never a silent UTC.

const (
	wallLayoutSeconds = "2006-01-02T15:04:05"
	wallLayoutMinutes = "2006-01-02T15:04"
)

// callZone is the zone a call reads its times in: tz, else def.
func callZone(tz string, def *time.Location) (*time.Location, error) {
	if tz == "" {
		return def, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "Local" {
		return nil, refusedErr("tz is not an IANA time zone: "+quote(tz), "give a zone such as Europe/Paris or America/New_York, or leave tz out")
	}
	return loc, nil
}

// parseTime reads the time argument name.
func parseTime(name, v string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{wallLayoutSeconds, wallLayoutMinutes, time.DateOnly} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, refusedErr(name+" is not a time: "+quote(v),
		"give an RFC 3339 time (2026-11-14T15:00:00+01:00), a wall clock (2026-11-14T15:00) or a date (2026-11-14), read in tz")
}

// quote shows an argument the agent gave, cut short.
func quote(s string) string {
	if len(s) > 64 {
		s = cutUTF8(s, 64) + "…"
	}
	return `"` + s + `"`
}

// localZone is the process's zone under its IANA name when one is found
// (TZ, else /etc/localtime's link), so that results echo a name an agent
// can use; else time.Local.
func localZone() *time.Location {
	name := strings.TrimPrefix(os.Getenv("TZ"), ":")
	if name == "" {
		if target, err := os.Readlink("/etc/localtime"); err == nil {
			if _, after, ok := strings.Cut(target, "zoneinfo/"); ok {
				name = after
			}
		}
	}
	if name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.Local
}

// timeView is a time in a result: an instant with its zone's TZID and UTC
// offset (at in RFC 3339 with that offset), a floating wall clock marked
// floating, or an all-day date (an end date is exclusive). Never a zero
// time.
type timeView struct {
	At       string `json:"at,omitempty" jsonschema:"RFC 3339 time with the offset of its zone"`
	Date     string `json:"date,omitempty" jsonschema:"all-day: YYYY-MM-DD; an end date is exclusive"`
	TZID     string `json:"tzid,omitempty" jsonschema:"IANA zone of the time (UTC for a UTC time)"`
	Offset   string `json:"offset,omitempty" jsonschema:"UTC offset of at, +hh:mm"`
	Floating bool   `json:"floating,omitempty" jsonschema:"a wall clock in no zone: the same clock time wherever it is read"`
}

// instantIn is the instant t in loc, with its TZID (name) and offset.
func instantIn(t time.Time, loc *time.Location, name string) timeView {
	w := t.In(loc)
	return timeView{At: w.Format(time.RFC3339), TZID: name, Offset: w.Format("-07:00")}
}

var zones sync.Map // TZID → *time.Location

// zoneOf is the location of an IANA TZID, nil if unknown.
func zoneOf(tzid string) *time.Location {
	if v, ok := zones.Load(tzid); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(tzid)
	if err != nil {
		return nil
	}
	zones.Store(tzid, loc)
	return loc
}

// occurrenceTimes are an occurrence's start and end: dates when all-day; a
// floating one read in loc (the call's zone) and marked; a zoned one in
// its TZID; a UTC one in UTC.
func occurrenceTimes(o service.Occurrence, loc *time.Location) (start, end timeView) {
	switch {
	case o.AllDay:
		return timeView{Date: o.StartDate}, timeView{Date: o.EndDate}
	case o.Floating:
		start, end = instantIn(o.Start, loc, ""), instantIn(o.End, loc, "")
		start.Floating, end.Floating = true, true
		return start, end
	case o.TZID != "":
		if z := zoneOf(o.TZID); z != nil {
			return instantIn(o.Start, z, o.TZID), instantIn(o.End, z, o.TZID)
		}
	}
	return instantIn(o.Start, time.UTC, "UTC"), instantIn(o.End, time.UTC, "UTC")
}

// eventTime is a stored DTSTART, DTEND, EXDATE or RECURRENCE-ID: a date, a
// floating wall clock (no offset), or an instant in its TZID or UTC.
func eventTime(t service.EventTime) timeView {
	switch {
	case t.Date != "":
		return timeView{Date: t.Date}
	case t.At.IsZero():
		return timeView{}
	case t.Floating:
		return timeView{At: t.At.Format(wallLayoutSeconds), Floating: true}
	case t.TZID != "":
		if z := zoneOf(t.TZID); z != nil {
			return instantIn(t.At, z, t.TZID)
		}
	}
	return instantIn(t.At, time.UTC, "UTC")
}
