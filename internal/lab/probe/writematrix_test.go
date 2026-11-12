package probe_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// matrixRig is a lab session on a fake whose key salts need the locked
// scope (an hours-old session), with a "pcb-lab test" calendar.
func matrixRig(t *testing.T, w protontest.WriteOptions, name string) (*labtest.Front, string, *session.Session) {
	t.Helper()
	f := labtest.NewFront(t, &labtest.Front{Options: protontest.Options{LockedSalts: true, Write: w}})
	cal := f.Backend.AddCalendar(labtest.User, protontest.Calendar{Name: name})
	o := loggedIn(t, f, "")
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return f, cal, s
}

func matrixOpts(cal string) probe.MatrixOptions {
	return probe.MatrixOptions{CalendarID: cal, Username: labtest.User, Password: []byte(labtest.Pass), Limiter: rate.NewLimiter(rate.Inf, 1)}
}

// caseOf is the case name a matrix UID ends with.
var uidRE = regexp.MustCompile(`^pcb-lab-wm-[0-9a-f]{8}-(.+)$`)

func caseOf(uid string) string {
	if m := uidRE.FindStringSubmatch(uid); m != nil {
		return m[1]
	}
	return ""
}

// creates counts the creates of each UID the fake saw, so a Lab answer can
// tell a second create from the first.
type creates struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *creates) next(uid string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[uid]++
	return c.n[uid]
}

func lines(t *testing.T, res probe.MatrixResult) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range res.Cases {
		out[c.Case] = c.Line
	}
	if len(out) != 14 {
		t.Fatalf("%d cases ran, want 14: %v", len(out), out)
	}
	return out
}

func wantIn(t *testing.T, got map[string]string, want map[string][]string) {
	t.Helper()
	for c, subs := range want {
		for _, s := range subs {
			if !strings.Contains(got[c], s) {
				t.Errorf("%s: line %q lacks %q", c, got[c], s)
			}
		}
	}
}

var parisEnd = time.Date(2027, 3, 12, 8, 45, 0, 0, time.UTC).Unix() // the duration case's DTSTART + 45 min

// Each case's line follows what the fake was told to do: accepted, refused
// or changed (ruling L5). A probe that printed its expectation would print
// the same line three times.
func TestWriteMatrixReportsWhatProtonDid(t *testing.T) {
	t.Parallel()
	refuseCodes := map[string]int{
		"rrule-count-50": 2103, "rrule-until-2038": 2103, "rrule-bysetpos-5": 2103, "rrule-hourly": 2103, "rrule-interval-1000": 2103,
		"status-tentative": 2000, "floating-time": 2104, "non-iana-tzid": 2105, "duration": 2106, "personal-part-left-out": 2109,
	}
	refuseUpdate := map[string]int{"key-packets-on-update": 2101, "unbumped-time-edit": 2107, "notifications-absent": 2108}
	configs := map[string]func() protontest.WriteOptions{
		"accepted": func() protontest.WriteOptions {
			return protontest.WriteOptions{Lab: func(string, bool) protontest.LabAnswer { return protontest.LabAnswer{Lenient: true} }}
		},
		"refused": func() protontest.WriteOptions {
			var seen creates
			return protontest.WriteOptions{Lab: func(uid string, update bool) protontest.LabAnswer {
				c := caseOf(uid)
				switch {
				case update && refuseUpdate[c] != 0:
					return protontest.LabAnswer{Code: refuseUpdate[c]}
				case !update && c == "second-master" && seen.next(uid) == 2:
					return protontest.LabAnswer{Code: 2102}
				case !update && refuseCodes[c] != 0:
					return protontest.LabAnswer{Code: refuseCodes[c]}
				}
				return protontest.LabAnswer{Lenient: true}
			}}
		},
		"changed": func() protontest.WriteOptions {
			var seen creates
			var mu sync.Mutex
			packets := map[string]any{}
			edit := map[string]func(uid string, update bool, row map[string]any){
				"key-packets-on-update": func(uid string, update bool, row map[string]any) {
					mu.Lock()
					defer mu.Unlock()
					if !update {
						packets[uid] = row["SharedKeyPacket"]
						return
					}
					row["SharedKeyPacket"] = packets[uid] // the update's packet ignored
				},
				"second-master": func(uid string, update bool, row map[string]any) {
					if !update && seen.next(uid) == 2 {
						row["RecurrenceID"] = int64(1804000000)
					}
				},
				"status-tentative": func(_ string, _ bool, row map[string]any) {
					p := row["CalendarEvents"].([]any)[0].(map[string]any)
					p["Data"] = strings.Replace(p["Data"].(string), "STATUS:TENTATIVE", "STATUS:CONFIRMED", 1)
				},
				"floating-time": func(_ string, _ bool, row map[string]any) { row["StartTimezone"] = "UTC" },
				"non-iana-tzid": func(_ string, _ bool, row map[string]any) { row["StartTimezone"] = "Europe/Paris" },
				"duration":      func(_ string, _ bool, row map[string]any) { row["EndTime"] = parisEnd },
				"notifications-absent": func(_ string, update bool, row map[string]any) {
					if update {
						row["Notifications"] = nil
					}
				},
				"personal-part-left-out": func(_ string, update bool, row map[string]any) {
					if update {
						row["PersonalEvents"] = []any{}
					}
				},
			}
			for _, c := range []string{"rrule-count-50", "rrule-until-2038", "rrule-bysetpos-5", "rrule-hourly", "rrule-interval-1000"} {
				edit[c] = func(_ string, _ bool, row map[string]any) { row["RRule"] = "FREQ=WEEKLY;COUNT=49" }
			}
			return protontest.WriteOptions{DropUnbumpedSequence: true, Lab: func(uid string, update bool) protontest.LabAnswer {
				a := protontest.LabAnswer{Lenient: true}
				if fn := edit[caseOf(uid)]; fn != nil {
					a.Edit = func(row map[string]any) { fn(uid, update, row) }
				}
				return a
			}}
		},
	}
	want := map[string]map[string][]string{
		"accepted": {
			"key-packets-on-update":  {"create: HTTP 200, code 1000", "update with new key packets: HTTP 200, code 1000", "1 row(s): key packet the new one, parts as sent"},
			"second-master":          {"second create, same UID: HTTP 200, code 1000", "2 row(s), 2 master(s); as the first create 1, as the second 1"},
			"rrule-count-50":         {`RRULE column "FREQ=WEEKLY;COUNT=50" (as sent), parts as sent`},
			"rrule-until-2038":       {`RRULE column "FREQ=WEEKLY;UNTIL=20380106T090000Z" (as sent)`},
			"rrule-bysetpos-5":       {`(as sent)`},
			"rrule-hourly":           {`RRULE column "FREQ=HOURLY;COUNT=3" (as sent)`},
			"rrule-interval-1000":    {`(as sent)`},
			"status-tentative":       {"STATUS TENTATIVE in the calendar signed part, parts as sent"},
			"floating-time":          {`StartTimezone "", StartTime 2027-03-10T10:00:00Z, parts as sent`},
			"non-iana-tzid":          {`StartTimezone "Mars/Olympus"`},
			"duration":               {"EndTime - StartTime 0 s, DURATION PT45M, DTEND absent in the shared signed part, parts as sent"},
			"unbumped-time-edit":     {"time edit, SEQUENCE unchanged: HTTP 200, code 1000", "StartTime moved +1800 s by the update, parts as sent"},
			"notifications-absent":   {`after the create [{"Type":1,"Trigger":"-PT15M"}], after the update [{"Type":1,"Trigger":"-PT15M"}], parts as sent`},
			"personal-part-left-out": {"create with a personal part: HTTP 200, code 1000", "personal parts after the create 1, after the update 1, parts as sent"},
		},
		"refused": {
			"key-packets-on-update":  {"update with new key packets: HTTP 200, code 2101", "key packet the old one, parts differ"},
			"second-master":          {"second create, same UID: HTTP 200, code 2102", "1 row(s), 1 master(s); as the first create 1, as the second 0"},
			"rrule-count-50":         {"create: HTTP 200, code 2103", "read-back: 0 row(s)"},
			"rrule-hourly":           {"create: HTTP 200, code 2103"},
			"status-tentative":       {"create: HTTP 200, code 2000", "read-back: 0 row(s)"},
			"floating-time":          {"create: HTTP 200, code 2104", "read-back: 0 row(s)"},
			"non-iana-tzid":          {"code 2105"},
			"duration":               {"code 2106"},
			"unbumped-time-edit":     {"time edit, SEQUENCE unchanged: HTTP 200, code 2107", "StartTime unchanged by the update"},
			"notifications-absent":   {"update, Notifications absent: HTTP 200, code 2108", `after the update [{"Type":1,"Trigger":"-PT15M"}]`},
			"personal-part-left-out": {"create with a personal part: HTTP 200, code 2109", "update leaving it out: not sent", "read-back: 0 row(s)"},
		},
		"changed": {
			"key-packets-on-update":  {"update with new key packets: HTTP 200, code 1000", "key packet the old one, parts do not open with the row's keys"},
			"second-master":          {"second create, same UID: HTTP 200, code 1000", "2 row(s), 1 master(s)"},
			"rrule-count-50":         {`RRULE column "FREQ=WEEKLY;COUNT=49" (sent "FREQ=WEEKLY;COUNT=50")`},
			"rrule-hourly":           {`(sent "FREQ=HOURLY;COUNT=3")`},
			"status-tentative":       {"STATUS CONFIRMED in the calendar signed part, parts differ: calendar signed part"},
			"floating-time":          {`StartTimezone "UTC"`},
			"non-iana-tzid":          {`StartTimezone "Europe/Paris"`},
			"duration":               {"EndTime - StartTime 2700 s"},
			"unbumped-time-edit":     {"time edit, SEQUENCE unchanged: HTTP 200, code 1000", "StartTime unchanged by the update, parts differ: shared signed part"},
			"notifications-absent":   {"after the update null"},
			"personal-part-left-out": {"personal parts after the create 1, after the update 0"},
		},
	}
	got := map[string]map[string]string{}
	for name, cfg := range configs { // in turn: the comparison below needs all three
		t.Run(name, func(t *testing.T) {
			f, cal, s := matrixRig(t, cfg(), "pcb-lab test")
			res, err := probe.WriteMatrix(t.Context(), s, matrixOpts(cal))
			if err != nil {
				t.Fatal(err)
			}
			l := lines(t, res)
			wantIn(t, l, want[name])
			if !strings.HasSuffix(res.Cleanup, "left 0") || res.Stopped != "" || !strings.Contains(res.Scope, "SRP unlock") {
				t.Errorf("cleanup %q, stopped %q, salts %q: want every row deleted and the scope gained", res.Cleanup, res.Stopped, res.Scope)
			}
			c, err := s.API(nil, rate.NewLimiter(rate.Inf, 1))
			if err != nil {
				t.Fatal(err)
			}
			for _, cs := range res.Cases {
				if rows, err := events.ListByUID(t.Context(), c, cal, cs.UID); err != nil || len(rows) != 0 {
					t.Errorf("%s: %d row(s) left after the cleanup (%v)", cs.Case, len(rows), err)
				}
			}
			b, _ := json.Marshal(res)
			for _, secret := range append(f.Backend.AccessTokens(), labtest.Pass, "SUMMARY", "BEGIN:V", "PGP", "pcb-lab wm") {
				if strings.Contains(string(b), secret) {
					t.Errorf("the result holds %q: no token, password or event text may be printed", secret)
				}
			}
			got[name] = l
		})
	}
	if len(got) != 3 {
		return
	}
	for c := range got["accepted"] {
		if got["accepted"][c] == got["refused"][c] || got["accepted"][c] == got["changed"][c] || got["refused"][c] == got["changed"][c] {
			t.Errorf("%s: the same line for two different answers:\n accepted %s\n refused  %s\n changed  %s", c, got["accepted"][c], got["refused"][c], got["changed"][c])
		}
	}
}

// Nothing is unlocked or written into a calendar not named pcb-lab, nor
// with a wrong password.
func TestWriteMatrixRefusesBeforeWriting(t *testing.T) {
	t.Parallel()
	f, cal, s := matrixRig(t, protontest.WriteOptions{}, "pcb-lab")
	var e *exitcode.Error
	for _, name := range []string{"Personal", "my pcb-lab", "xpcb-lab", "PCB-LAB"} { // the name must start with pcb-lab
		other := f.Backend.AddCalendar(labtest.User, protontest.Calendar{Name: name})
		_, err := probe.WriteMatrix(t.Context(), s, matrixOpts(other))
		if !errors.As(err, &e) || e.Code != exitcode.Usage || strings.Contains(err.Error(), name) {
			t.Fatalf("calendar %q: err = %v; want a usage refusal that does not print the calendar's name", name, err)
		}
	}
	if n, u := len(f.Backend.SyncCalls()), f.Backend.Calls("PUT", "/core/v4/users/unlock"); n != 0 || u != 0 {
		t.Errorf("%d writes, %d unlocks: a wrong calendar must stop before any key or write", n, u)
	}
	if _, err := probe.WriteMatrix(t.Context(), s, matrixOpts("no-such-calendar")); !errors.As(err, &e) || e.Code != exitcode.Usage {
		t.Errorf("unknown calendar: err %v, want a usage refusal", err)
	}
	o := matrixOpts(cal)
	o.Password = []byte("not-the-password")
	_, err := probe.WriteMatrix(t.Context(), s, o)
	if !errors.As(err, &e) || e.Code != exitcode.MustAct || strings.Contains(err.Error(), "not-the-password") {
		t.Fatalf("wrong password: err = %v; want must-act without the password", err)
	}
	if n := len(f.Backend.SyncCalls()); n != 0 {
		t.Errorf("%d writes after a wrong password", n)
	}
}

// In order on one rig: --keep leaves the rows, every UID written carries the
// run's prefix and the writes are paced by the interval; then a 429 stops
// every further write.
func TestWriteMatrixKeepPaceAndStop(t *testing.T) {
	t.Parallel()
	f, cal, s := matrixRig(t, protontest.WriteOptions{}, "pcb-lab")
	o := matrixOpts(cal)
	o.Keep, o.Interval = true, 2*time.Second
	var sleeps []time.Duration
	o.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	res, err := probe.WriteMatrix(t.Context(), s, o)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^pcb-lab-wm-[0-9a-f]{8}-$`).MatchString(res.UIDPrefix) || !strings.HasPrefix(res.Cleanup, "kept") {
		t.Errorf("prefix %q, cleanup %q", res.UIDPrefix, res.Cleanup)
	}
	calls := f.Backend.SyncCalls()
	if len(calls) != res.Writes || res.Writes < 14 {
		t.Fatalf("%d sync calls, %d writes reported", len(calls), res.Writes)
	}
	for _, c := range calls {
		for _, e := range c.Body.Events {
			if e.Event == nil {
				t.Errorf("a delete was sent despite --keep")
				continue
			}
			for _, p := range e.Event.SharedEventContent {
				if p.Type == 2 && !strings.Contains(p.Data, "\r\nUID:"+res.UIDPrefix) {
					t.Errorf("a write without the run's UID prefix %q", res.UIDPrefix)
				}
			}
		}
	}
	if len(sleeps) != res.Writes-1 {
		t.Errorf("%d pauses for %d writes, want one before each write but the first", len(sleeps), res.Writes)
	}
	for _, d := range sleeps {
		if d != 2*time.Second {
			t.Errorf("pause %s, want the interval", d)
		}
	}
	writes := res.Writes
	path := "/calendar/v1/" + cal + "/events/sync"
	before := f.Backend.Calls("PUT", path)
	f.Backend.FailNext("PUT", path, 429, 429, 30)
	o.Keep, o.Sleep = false, nil
	o.Interval = 0
	res, err = probe.WriteMatrix(t.Context(), s, o)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.Backend.Calls("PUT", path) - before; n != 1 || res.Writes != 1 {
		t.Errorf("%d writes reached the fake (%d reported) after a 429, want only the one answered 429 (the kept run made %d)", n, res.Writes, writes)
	}
	if !strings.Contains(res.Stopped, "429") || len(res.Cases) != 1 || !strings.Contains(res.Cases[0].Line, "HTTP 429, code 429") || !strings.HasPrefix(res.Cleanup, "not tried") {
		t.Errorf("stopped %q, cases %d, cleanup %q, first line %q", res.Stopped, len(res.Cases), res.Cleanup, res.Cases[0].Line)
	}
}

// Proton's ?UID= may match more than the UID (HUMAN-CHECKS W3): a neighbour
// whose UID only starts with a case's is neither read back nor deleted.
func TestWriteMatrixReadsAndDeletesItsExactUIDsOnly(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{Options: protontest.Options{UIDMatch: strings.HasPrefix}})
	cal := f.Backend.AddCalendar(labtest.User, protontest.Calendar{Name: "pcb-lab"})
	s, err := session.Resume(t.Context(), loggedIn(t, f, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	neighbour := "pcb-lab-wm-0badcafe-rrule-hourly-neighbour"
	f.Backend.AddEvent(cal, protontest.Event{UID: neighbour, Start: 1804000000, End: 1804003600})
	o := matrixOpts(cal)
	o.RunID = "0badcafe"
	res, err := probe.WriteMatrix(t.Context(), s, o)
	if err != nil {
		t.Fatal(err)
	}
	if l := lines(t, res)["rrule-hourly"]; !strings.Contains(l, "read-back: 1 row(s);") {
		t.Errorf("rrule-hourly: %q; want only its own row read back", l)
	}
	c, err := s.API(nil, rate.NewLimiter(rate.Inf, 1))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := events.ListByUID(t.Context(), c, cal, neighbour)
	if err != nil || len(rows) != 1 || !strings.HasSuffix(res.Cleanup, "left 0") {
		t.Errorf("neighbour rows %d (%v), cleanup %q; want the neighbour kept and ours gone", len(rows), err, res.Cleanup)
	}
}

func TestWriteMatrixStopsWhenTheSessionEnds(t *testing.T) {
	t.Parallel()
	f, cal, s := matrixRig(t, protontest.WriteOptions{}, "pcb-lab")
	path := "/calendar/v1/" + cal + "/events/sync"
	f.Mu.Lock()
	f.Replies = map[string]labtest.Reply{"PUT " + path: labtest.R(401, `{"Code":401,"Error":"Invalid access token"}`)}
	f.Mu.Unlock()
	res, err := probe.WriteMatrix(t.Context(), s, matrixOpts(cal))
	if err != nil {
		t.Fatal(err)
	}
	if res.Writes != 1 || !strings.Contains(res.Stopped, "session ended") || len(res.Cases) != 1 || !strings.HasPrefix(res.Cleanup, "not tried") {
		t.Errorf("writes %d, stopped %q, cases %d, cleanup %q; want one write, then a stop", res.Writes, res.Stopped, len(res.Cases), res.Cleanup)
	}
}
