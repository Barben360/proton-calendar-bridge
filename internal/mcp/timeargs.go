package mcp

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/winzone"
)

// Time arguments (design §5.7, Review Focus 1): an RFC 3339 time with an
// offset is an instant as it is; a wall clock without offset or a bare date
// is read in the call's tz argument, else the server's zone ([mcp] timezone,
// else the process's). The zone used is echoed in the result. Anything
// else is refused, naming the argument: never a silent UTC. A wall clock in
// a DST gap or overlap reads as the iCalendar codec reads it (parseTime).

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
	// A wall clock is resolved as the codec resolves a stored one: the first
	// of two in a DST overlap, the offset before a gap (time.ParseInLocation
	// gives other instants for both).
	for _, layout := range []string{wallLayoutSeconds, wallLayoutMinutes, time.DateOnly} {
		if w, err := time.Parse(layout, v); err == nil {
			return service.ResolveWall(w, loc), nil
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

// localZone is the process's zone under its IANA name when one is found,
// so that results echo a name an agent can pass back as tz and new times
// are written with that TZID (M8-F1: Go names the zone "Local" when TZ is
// unset, or by a zone file's path); else time.Local itself, so wall clocks
// are still read in the local zone, never in UTC for want of a name
// (review M1). A variable for tests.
var localZone = func() *time.Location { return localZoneFrom(runtime.GOOS, os.LookupEnv, os.Readlink, os.ReadFile) }

// localZoneFrom names the zone Go reads as time.Local on goos. On Unix,
// TZ's name (a zone file's path by its zoneinfo/ part, or the link it is),
// else, TZ unset, the /etc/localtime link, else /etc/timezone (Debian's
// copy of the name when /etc/localtime is no link). TZ set but empty or
// unknown is time.Local (Go's UTC then); so is no name found. On Windows,
// where Go reads no TZ, the zone is the IANA zone CLDR maps the Windows zone
// key to (windowsZone). zoneLabel names an unnamed time.Local by its
// offset.
func localZoneFrom(goos string, env func(string) (string, bool), readlink func(string) (string, error), readFile func(string) ([]byte, error)) *time.Location {
	if goos == "windows" {
		return windowsZone(winzone.Key)
	}
	tz, set := env("TZ")
	tz = strings.TrimPrefix(tz, ":")
	file := "/etc/localtime"
	switch {
	case set && !strings.HasPrefix(tz, "/"):
		if loc := namedZone(tz); loc != nil {
			return loc
		}
		return time.Local
	case set:
		if loc := namedZone(zoneinfoName(tz)); loc != nil {
			return loc
		}
		file = tz
	}
	if target, err := readlink(file); err == nil {
		if loc := namedZone(zoneinfoName(target)); loc != nil {
			return loc
		}
	}
	if file == "/etc/localtime" {
		if b, err := readFile("/etc/timezone"); err == nil {
			if loc := namedZone(strings.TrimSpace(string(b))); loc != nil {
				return loc
			}
		}
	}
	return time.Local
}

// windowsZone is the IANA zone of the Windows zone key, time.Local itself
// when there is no key, CLDR does not map it (pcb doctor reports that) or
// the zone cannot be loaded: wall clocks are then still read in the local
// zone, labelled by its offset.
func windowsZone(key func() (string, bool)) *time.Location {
	k, ok := key()
	if !ok {
		return time.Local
	}
	name, ok := winzone.IANA(k)
	if !ok {
		return time.Local
	}
	if loc := namedZone(name); loc != nil {
		return loc
	}
	return time.Local
}

// zoneinfoName is the zone name of a zone file's path (what follows
// zoneinfo/), "" when it has none.
func zoneinfoName(path string) string {
	_, after, _ := strings.Cut(path, "zoneinfo/")
	return after
}

// namedZone is the zone of an IANA name, nil for none ("Local" and paths
// are no names).
func namedZone(name string) *time.Location {
	if name == "" || name == "Local" || strings.HasPrefix(name, "/") {
		return nil
	}
	return zoneOf(name)
}

// zoneLabel is how results name loc: its IANA name, or for a zone with
// none (time.Local named "Local" or by a path) its UTC offset at t, as
// UTC+hh:mm.
func zoneLabel(loc *time.Location, t time.Time) string {
	if name := loc.String(); namedZone(name) != nil {
		return name
	}
	return "UTC" + t.In(loc).Format("-07:00")
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
		if loc, ok := v.(*time.Location); ok {
			return loc
		}
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
