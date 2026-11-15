package mcp_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones the Windows mapping names, on a host with no zoneinfo

	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// lastRequest is the last request alice's agenda received.
func lastRequest(t *testing.T, hub *stubHub) service.OccurrencesRequest {
	t.Helper()
	reqs := hub.agendas["a1"].requests()
	if len(reqs) == 0 {
		t.Fatal("alice's agenda was not asked")
	}
	return reqs[len(reqs)-1]
}

// Review Focus 1: a wall-clock time or a date without an offset is read in
// the tz argument, else Options.Zone; the zone used is echoed. An RFC 3339
// time with an offset is kept as it is.
func TestNaiveTimeUsesTzThenZone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		from, to time.Time
		zone     string
	}{
		{"tz", map[string]any{"from": "2026-11-14T15:00", "to": "2026-11-14T18:30:00", "tz": "America/New_York"},
			time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 23, 30, 0, 0, time.UTC), "America/New_York"},
		{"zone", map[string]any{"from": "2026-11-14T15:00", "to": "2026-11-14T18:30:00"},
			time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 17, 30, 0, 0, time.UTC), "Europe/Paris"},
		{"dates in tz", map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "America/New_York"},
			time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 5, 0, 0, 0, time.UTC), "America/New_York"},
		{"dates in zone", map[string]any{"from": "2026-11-14", "to": "2026-11-15"},
			time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC), "Europe/Paris"},
		{"offsets kept", map[string]any{"from": "2026-11-14T15:00:00+09:00", "to": "2026-11-14T16:00:00Z", "tz": "America/New_York"},
			time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 16, 0, 0, 0, time.UTC), "America/New_York"},
	} {
		for _, tool := range []string{"events_list", "free_busy", "events_search"} {
			hub := newHub()
			cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
			args := argsFor(tool, with(tc.args, "accounts", []string{"a1"}))
			out := mustCall(t, cs, tool, args)
			req := lastRequest(t, hub)
			if !req.From.Equal(tc.from) || !req.To.Equal(tc.to) {
				t.Errorf("%s, %s: range %s – %s, want %s – %s", tc.name, tool, req.From.UTC(), req.To.UTC(), tc.from, tc.to)
			}
			if req.Loc == nil || req.Loc.String() != tc.zone {
				t.Errorf("%s, %s: agenda read in %v, want %s", tc.name, tool, req.Loc, tc.zone)
			}
			if out["zone"] != tc.zone {
				t.Errorf("%s, %s: zone %v, want %s echoed", tc.name, tool, out["zone"], tc.zone)
			}
			// The range is echoed in the zone, with its offset.
			loc, _ := time.LoadLocation(tc.zone)
			if want := tc.from.In(loc).Format(time.RFC3339); out["from"] != want {
				t.Errorf("%s, %s: from %v, want %s", tc.name, tool, out["from"], want)
			}
		}
	}
}

func TestMalformedTimeIsRefused(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		arg  string
	}{
		{map[string]any{"from": "tomorrow", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "2026-11-14", "to": "2026-13-01"}, "to"},
		{map[string]any{"from": "2026-11-14 15:00", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "2026-11-14T25:00", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "", "to": "2026-11-15"}, "from"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15T10:00:00+0100"}, "to"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "Mars/Olympus"}, "tz"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "Local"}, "tz"},
		{map[string]any{"from": "2026-11-14", "to": "2026-11-15", "tz": "+01:00"}, "tz"},
	} {
		for _, tool := range []string{"events_list", "free_busy", "events_search"} {
			hub := newHub()
			cs := newRig(t, hub).connect(t, readGrant, mcp.EntryStdio)
			terr := mustFail(t, cs, tool, argsFor(tool, tc.args), "refused")
			if !strings.HasPrefix(terr.Message, tc.arg+" ") {
				t.Errorf("%s(%v): %q does not start with the argument %s", tool, tc.args, terr.Message, tc.arg)
			}
			for id, a := range hub.agendas {
				if n := len(a.requests()); n != 0 {
					t.Errorf("%s(%v): agenda %s asked %d times", tool, tc.args, id, n)
				}
			}
		}
	}
}

// M8-F1 (live 2026-11-14): with [mcp] timezone empty the daemons pass
// time.Local, which Go names "Local" when TZ is unset; status and every
// result that echoes the zone must name the IANA zone the process runs in
// instead. (time.Local itself is process-wide: a zone named "Local" stands
// in for it, and the resolution is the one TestLocalZoneResolution checks.)
func TestResultsNameTheLocalZoneByItsIANAName(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	defer mcp.SetLocalZone(func() *time.Location { return ny })()
	r := newRigWith(t, newHub(), func(o *mcp.Options) { o.Zone = time.FixedZone("Local", -5*3600) })
	cs := r.connect(t, writeGrant, mcp.EntryStdio)
	out := mustCall(t, cs, "status", nil)
	if out["zone"] != "America/New_York" || field(out, "now.zone") != "America/New_York" ||
		field(out, "now.at") != "2026-11-14T05:00:00-05:00" || field(out, "now.offset") != "-05:00" {
		t.Errorf("status zone %v, now %v; want America/New_York, 05:00 -05:00", out["zone"], out["now"])
	}
	list := mustCall(t, cs, "events_list", map[string]any{"from": "2026-11-16", "to": "2026-11-17"})
	if list["zone"] != "America/New_York" || list["from"] != "2026-11-16T00:00:00-05:00" {
		t.Errorf("events_list zone %v from %v; want America/New_York, midnight there", list["zone"], list["from"])
	}
	created := mustCall(t, cs, "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "x",
		"start": "2026-11-20T15:00", "end": "2026-11-20T16:00"})
	if created["zone"] != "America/New_York" {
		t.Errorf("event_create zone %v, want America/New_York", created["zone"])
	}
	updated := mustCall(t, cs, "event_update", map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series",
		"changes": map[string]any{"summary": "y"}})
	if updated["zone"] != "America/New_York" {
		t.Errorf("event_update zone %v, want America/New_York", updated["zone"])
	}
	// What is written carries the name too, never UTC for want of one.
	w := r.hub.agendas["a1"].writes()
	if s := w.creates[len(w.creates)-1].f.Start; s == nil || s.TZID != "America/New_York" {
		t.Errorf("created start %+v, want TZID America/New_York", s)
	}
}

// Review I1: with TZ a zone file's path, Go names time.Local by that path,
// not "Local"; the daemons pass time.Local itself. Through NewServer as
// the daemons call it (a child process, since time.Local is read once per
// process): a zoneinfo path gets its IANA name; a file with no name keeps
// the local zone for reading wall clocks (never UTC) and is labelled by
// its offset.
func TestProcessZoneUnderAPathTZ(t *testing.T) {
	if mode := os.Getenv("PCB_MCP_ZONE_CHILD"); mode != "" {
		zoneChild(t, mode)
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Go reads no TZ on Windows")
	}
	const src = "/usr/share/zoneinfo/America/New_York"
	b, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("no zoneinfo on this host: %v", err)
	}
	unnamed := filepath.Join(t.TempDir(), "myzone")
	if err := os.WriteFile(unnamed, b, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, tz, mode string }{
		{"a zoneinfo path", src, "named"},
		{"a file with no name", ":" + unnamed, "unnamed"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessZoneUnderAPathTZ$", "-test.count=1")
		cmd.Env = append(os.Environ(), "TZ="+tc.tz, "PCB_MCP_ZONE_CHILD="+tc.mode)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("TZ %s: %v\n%s", tc.name, err, out)
		}
	}
}

// zoneChild is TestProcessZoneUnderAPathTZ in its child process.
func zoneChild(t *testing.T, mode string) {
	zone, tzid := "America/New_York", "America/New_York"
	if mode == "unnamed" {
		zone, tzid = "UTC-05:00", ""
	}
	r := newRigWith(t, newHub(), func(o *mcp.Options) { o.Zone = time.Local })
	cs := r.connect(t, writeGrant, mcp.EntryStdio)
	if out := mustCall(t, cs, "status", nil); out["zone"] != zone || field(out, "now.at") != "2026-11-14T05:00:00-05:00" {
		t.Errorf("time.Local named %q: status zone %v, now %v; want %s, 05:00 -05:00", time.Local.String(), out["zone"], out["now"], zone)
	}
	created := mustCall(t, cs, "event_create", map[string]any{"account": "a1", "calendar": "cal-a", "summary": "x",
		"start": "2026-11-20T15:00", "end": "2026-11-20T16:00"})
	if created["zone"] != zone {
		t.Errorf("event_create zone %v, want %s", created["zone"], zone)
	}
	w := r.hub.agendas["a1"].writes()
	s := w.creates[len(w.creates)-1].f.Start
	if s == nil || !s.At.Equal(time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC)) || s.TZID != tzid {
		t.Errorf("created start %+v, want 15:00 New York (20:00Z) with TZID %q", s, tzid)
	}
}

// The process's zone: TZ (its ':' prefix or a zoneinfo path read as the
// name), else the /etc/localtime link, else /etc/timezone; TZ set but
// empty or unknown, or no name at all, is time.Local, as Go reads it, which
// results label by its UTC offset, never "Local" nor a path. On Windows Go
// reads no TZ: time.Local (review M1).
func TestLocalZoneResolution(t *testing.T) {
	noLink := func(string) (string, error) { return "", errors.New("not a link") }
	noFile := func(string) ([]byte, error) { return nil, errors.New("no file") }
	unset := func(string) (string, bool) { return "", false }
	tz := func(v string) func(string) (string, bool) {
		return func(k string) (string, bool) { return v, k == "TZ" }
	}
	link := func(target string) func(string) (string, error) {
		return func(string) (string, error) { return target, nil }
	}
	for _, tc := range []struct {
		name string
		env  func(string) (string, bool)
		link func(string) (string, error)
		file func(string) ([]byte, error)
		want string
	}{
		{"TZ", tz("Europe/Paris"), link("/usr/share/zoneinfo/Asia/Tokyo"), noFile, "Europe/Paris"},
		{"TZ with colon", tz(":Asia/Tokyo"), noLink, noFile, "Asia/Tokyo"},
		{"TZ as a path", tz("/usr/share/zoneinfo/America/New_York"), noLink, noFile, "America/New_York"},
		{"link", unset, link("/usr/share/zoneinfo/Europe/Paris"), noFile, "Europe/Paris"},
		{"relative link", unset, link("../usr/share/zoneinfo/America/New_York"), noFile, "America/New_York"},
		{"macOS link", unset, link("/var/db/timezone/zoneinfo/Europe/Berlin"), noFile, "Europe/Berlin"},
		{"/etc/timezone", unset, noLink, func(p string) ([]byte, error) {
			if p != "/etc/timezone" {
				return nil, errors.New("unexpected " + p)
			}
			return []byte("Europe/Madrid\n"), nil
		}, "Europe/Madrid"},
	} {
		if got := mcp.LocalZoneFrom("linux", tc.env, tc.link, tc.file).String(); got != tc.want {
			t.Errorf("%s: zone %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		goos string
		env  func(string) (string, bool)
		link func(string) (string, error)
	}{
		{"no name", "linux", unset, noLink},
		{"TZ a path with no name", "linux", tz("/etc/localtime"), noLink},
		{"TZ empty", "linux", tz(""), link("/usr/share/zoneinfo/Asia/Tokyo")},
		{"TZ unknown", "linux", tz("Mars/Olympus"), link("/usr/share/zoneinfo/Asia/Tokyo")},
	} {
		if loc := mcp.LocalZoneFrom(tc.goos, tc.env, tc.link, noFile); loc != time.Local {
			t.Errorf("%s: zone %v, want the process's time.Local", tc.name, loc)
		}
	}
	if got := mcp.LocalZoneFrom("linux", tz("/etc/localtime"), link("/usr/share/zoneinfo/Europe/Paris"), noFile).String(); got != "Europe/Paris" {
		t.Errorf("TZ a path linked to a zone: %q, want Europe/Paris", got)
	}
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if got := mcp.ZoneLabel(time.FixedZone("Local", 3600), at); got != "UTC+01:00" {
		t.Errorf("label of an unnamed zone %q, want UTC+01:00", got)
	}
	if got := mcp.ZoneLabel(time.FixedZone("/srv/zones/mine", -5*3600), at); got != "UTC-05:00" {
		t.Errorf("label of a zone named by a path %q, want UTC-05:00", got)
	}
	if got := mcp.ZoneLabel(paris, at); got != "Europe/Paris" {
		t.Errorf("label of Europe/Paris %q", got)
	}
}

// On Windows the process's zone is the IANA zone of the Windows zone key;
// a key CLDR does not map, or none, keeps time.Local (wall clocks read in
// it, labelled by offset), never UTC.
func TestWindowsZoneNamesTheMappedZone(t *testing.T) {
	key := func(k string, ok bool) func() (string, bool) { return func() (string, bool) { return k, ok } }
	if got := mcp.WindowsZone(key("W. Europe Standard Time", true)).String(); got != "Europe/Berlin" {
		t.Errorf("W. Europe Standard Time: zone %q, want Europe/Berlin", got)
	}
	if got := mcp.WindowsZone(key("Tokyo Standard Time", true)).String(); got != "Asia/Tokyo" {
		t.Errorf("Tokyo Standard Time: zone %q, want Asia/Tokyo", got)
	}
	for name, k := range map[string]func() (string, bool){
		"unmapped key":            key("Mars Standard Time", true),
		"no key":                  key("", false),
		"key ignored when not ok": key("W. Europe Standard Time", false),
	} {
		if loc := mcp.WindowsZone(k); loc != time.Local {
			t.Errorf("%s: zone %v, want the process's time.Local", name, loc)
		}
	}
}
