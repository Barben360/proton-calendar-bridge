package seed

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const datasetPath = "../../../testdata/seed/dataset.yaml"

func load(t *testing.T) Dataset {
	t.Helper()
	ds, err := Load(datasetPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return ds
}

// loadInline loads a data set written by the test.
func loadInline(t *testing.T, body string) (Dataset, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ds.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestDatasetIsDeterministic(t *testing.T) {
	a, b := Plan(load(t), nil), Plan(load(t), nil)
	if len(a) < 350 {
		t.Fatalf("plan has %d commands, want at least the 348 creates", len(a))
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two loads of the same file gave different plans")
	}
	other := load(t)
	other.Seed = 43
	if reflect.DeepEqual(a, Plan(other, nil)) {
		t.Fatal("changing the seed did not change the plan: the generator ignores it")
	}
}

func TestSeedPlanSkipsExisting(t *testing.T) {
	ds := load(t)
	full := Plan(ds, nil)
	if !slices.ContainsFunc(full, func(c Command) bool { return c.ID == "s042" && c.Op == OpCreate }) {
		t.Fatal("the full plan has no create for s042; the test below would pass vacuously")
	}
	got := Plan(ds, []Existing{{ID: "s042", EventID: "EV42"}})
	if len(got) != len(full)-1 {
		t.Fatalf("plan has %d commands, want %d (the full plan minus s042)", len(got), len(full)-1)
	}
	for _, c := range got {
		if c.ID == "s042" || strings.Contains(strings.Join(c.Args, " "), "pcb-seed s042 ") {
			t.Fatalf("command for existing s042 still planned: %v", c.Args)
		}
	}
}

func TestSeedPlanResumesHalfSeededSeries(t *testing.T) {
	ds := load(t)
	var series Entry
	for _, e := range ds.Expand() {
		if e.Kind == KindSeries && e.Delete != nil && e.Delete.Occurrence == "2024-03-09 08:30" {
			series = e
		}
	}
	if series.ID == "" {
		t.Fatal("daily series not found")
	}
	// Created, nothing else done: the occurrence to delete is still listed.
	row := func(occ string) Existing {
		ts, err := time.ParseInLocation(timedLayout, occ, mustLoc(t, series.TZ))
		if err != nil {
			t.Fatal(err)
		}
		return Existing{ID: series.ID, EventID: "EV7", Start: ts}
	}
	created := Plan(ds, []Existing{row(series.Start), row(series.Delete.Occurrence)})
	var ops []string
	for _, c := range created {
		if c.ID == series.ID || c.Ref == series.ID {
			ops = append(ops, c.Op+":"+c.ID)
		}
	}
	want := []string{OpEdit + ":" + series.ID + ".e", OpDelete + ":" + series.ID + ".d"}
	if !slices.Equal(ops, want) {
		t.Errorf("created series: ops %v, want %v (no second create)", ops, want)
	}
	// Created and deleted (the occurrence is gone from the listing), edit missing.
	deleted := Plan(ds, []Existing{row(series.Start)})
	ops = nil
	for _, c := range deleted {
		if c.ID == series.ID || c.Ref == series.ID {
			ops = append(ops, c.Op+":"+c.ID)
		}
	}
	// Plan cannot tell the delete is done from a listing that never had the row;
	// it must only skip it when the series is listed without that occurrence.
	if want := []string{OpEdit + ":" + series.ID + ".e"}; !slices.Equal(ops, want) {
		t.Errorf("deleted series: ops %v, want %v", ops, want)
	}
	// Everything done: the edit shows up under its own id.
	done := Plan(ds, []Existing{row(series.Start), {ID: series.ID + ".e", EventID: "EV8"}})
	for _, c := range done {
		if c.ID == series.ID || c.Ref == series.ID {
			t.Errorf("fully seeded series still plans %s %s", c.Op, c.ID)
		}
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSeedCommandsCarryNoSecrets(t *testing.T) {
	t.Setenv("PCB_LAB_USERNAME", "lab-user-sentinel@example.test")
	t.Setenv("PCB_LAB_PASSWORD", "lab-password-sentinel")
	t.Setenv("PCB_LAB_TOTP", "otpauth://totp/sentinel?secret=ABCD")
	for _, c := range Plan(load(t), nil) {
		for _, a := range c.Args {
			for _, bad := range []string{"pass://", "PCB_LAB_", "lab-password-sentinel", "lab-user-sentinel", "otpauth://", "PROTON_CAL_"} {
				if strings.Contains(a, bad) {
					t.Fatalf("command %s %s carries %q in %q", c.Op, c.ID, bad, a)
				}
			}
		}
	}
}

func TestLoadRefusesSecretLookingValues(t *testing.T) {
	for _, bad := range []string{"pass://pcb-dev/x/password", "PCB_LAB_PASSWORD=abc"} {
		_, err := loadInline(t, `
seed: 1
range: {from: "2021-01-01", to: "2021-12-31"}
calendars: [{key: default, name: Main, default: true}]
text:
  - {title: "leak `+bad+`", calendar: default, start: "2021-02-03 10:00", end: "2021-02-03 11:00", tz: UTC}
`)
		if err == nil || !strings.Contains(err.Error(), "secret") {
			t.Errorf("Load accepted %q: err = %v", bad, err)
		}
	}
}

func TestDatasetCoverage(t *testing.T) {
	ds := load(t)
	entries := ds.Expand()
	count := map[string]int{}
	ids := map[string]bool{}
	var edits, dels, dst int
	tzs := map[string]int{}
	rem := map[string]bool{}
	colors := map[string]bool{}
	cals := map[string]bool{}
	var multi, single int
	from, to := "2021-01-01", "2027-06-30"
	for _, e := range entries {
		count[e.Kind]++
		if ids[e.ID] {
			t.Fatalf("duplicate id %s", e.ID)
		}
		ids[e.ID] = true
		cals[e.Calendar] = true
		for _, r := range e.Reminders {
			rem[r] = true
		}
		if e.Color != "" {
			colors[e.Color] = true
		}
		day := e.Start[:10]
		if day < from || day > to {
			t.Errorf("%s starts %s, outside %s..%s", e.ID, day, from, to)
		}
		switch e.Kind {
		case KindTimed:
			tzs[e.TZ]++
			loc := mustLoc(t, e.TZ)
			s, _ := time.ParseInLocation(timedLayout, e.Start, loc)
			en, _ := time.ParseInLocation(timedLayout, e.End, loc)
			if d := en.Sub(s); d < 15*time.Minute || d > 3*time.Hour {
				t.Errorf("%s lasts %v, want 15 min to 3 h", e.ID, d)
			}
		case KindAllDay:
			if e.End != "" && e.End != e.Start {
				multi++
			} else {
				single++
			}
		case KindSeries:
			if e.Edit != nil {
				edits++
			}
			if e.Delete != nil {
				dels++
			}
			if e.DSTCrossing {
				dst += crossesDST(t, e)
			}
		}
	}
	for kind, want := range map[string]int{KindTimed: 300, KindAllDay: 40, KindSeries: 8} {
		if count[kind] != want {
			t.Errorf("%d %s entries, want %d", count[kind], kind, want)
		}
	}
	if edits != 8 || dels != 8 {
		t.Errorf("%d edits and %d deletions, want 8 and 8", edits, dels)
	}
	if dst < 1 {
		t.Error("no series verifiably crosses a DST change")
	}
	if multi == 0 || single == 0 {
		t.Errorf("all-day: %d multi-day, %d single-day, want both", multi, single)
	}
	if share := float64(tzs["Europe/Paris"]) / 300; share < 0.6 || share > 0.8 {
		t.Errorf("Europe/Paris share %.2f, want about 0.70", share)
	}
	for _, tz := range []string{"America/New_York", "Asia/Tokyo", "UTC"} {
		if tzs[tz] == 0 {
			t.Errorf("no timed event in %s", tz)
		}
	}
	for _, r := range []string{"none", "15m", "1d", "email:1h"} {
		if !rem[r] {
			t.Errorf("no event with reminder %q", r)
		}
	}
	if len(colors) < 3 {
		t.Errorf("colours %v, want at least 3", colors)
	}
	if len(cals) != 3 {
		t.Errorf("calendars used %v, want default, work, sport", cals)
	}
	checkSeriesOccurrences(t, entries)
	checkTextCases(t, entries)
}

// crossesDST reports 1 when the last occurrence of a weekly counted series has
// another UTC offset than the first.
func crossesDST(t *testing.T, e Entry) int {
	t.Helper()
	if e.Repeat != "weekly" || e.Count < 2 {
		t.Fatalf("%s: dst_crossing is only verifiable for weekly series with a count", e.ID)
	}
	first, err := time.ParseInLocation(timedLayout, e.Start, mustLoc(t, e.TZ))
	if err != nil {
		t.Fatal(err)
	}
	last := first.AddDate(0, 0, 7*e.Every1()*(e.Count-1))
	_, o1 := first.Zone()
	_, o2 := time.Date(last.Year(), last.Month(), last.Day(), 12, 0, 0, 0, first.Location()).Zone()
	if o1 != o2 {
		return 1
	}
	return 0
}

// checkSeriesOccurrences pins that each edited or deleted occurrence is a real
// occurrence: at the series' time of day, after its start, never the same one.
func checkSeriesOccurrences(t *testing.T, entries []Entry) {
	t.Helper()
	for _, e := range entries {
		if e.Kind != KindSeries {
			continue
		}
		if e.Edit.Occurrence == e.Delete.Occurrence {
			t.Errorf("%s edits and deletes the same occurrence", e.ID)
		}
		for _, occ := range []string{e.Edit.Occurrence, e.Delete.Occurrence} {
			if e.AllDay {
				if occ <= e.Start {
					t.Errorf("%s: occurrence %s is not after the start %s", e.ID, occ, e.Start)
				}
				continue
			}
			if occ <= e.Start || occ[11:] != e.Start[11:] {
				t.Errorf("%s: occurrence %q is not a later occurrence at %s", e.ID, occ, e.Start[11:])
			}
		}
		switch {
		case e.Repeat == "daily" && e.Every1() == 1 && e.Count > 0:
			for _, occ := range []string{e.Edit.Occurrence, e.Delete.Occurrence} {
				d, _ := time.Parse("2006-01-02", occ[:10])
				s, _ := time.Parse("2006-01-02", e.Start[:10])
				if n := int(d.Sub(s).Hours() / 24); n >= e.Count {
					t.Errorf("%s: occurrence %s is beyond the %d occurrences", e.ID, occ, e.Count)
				}
			}
		case e.Repeat == "weekly":
			for _, occ := range []string{e.Edit.Occurrence, e.Delete.Occurrence} {
				d, _ := time.Parse("2006-01-02", occ[:10])
				s, _ := time.Parse("2006-01-02", e.Start[:10])
				if d.Weekday() != s.Weekday() || int(d.Sub(s).Hours()/24/7)%e.Every1() != 0 {
					t.Errorf("%s: occurrence %s is not on the weekly rhythm", e.ID, occ)
				}
			}
		}
	}
}

func checkTextCases(t *testing.T, entries []Entry) {
	t.Helper()
	var emoji, big, seps bool
	for _, e := range entries {
		if e.Kind != KindText {
			continue
		}
		for _, r := range e.Title {
			if r >= 0x1F000 {
				emoji = true
			}
		}
		if len(e.Description) == 2048 {
			big = true
		}
		if strings.ContainsAny(e.Location, ",;") && strings.Contains(e.Location, `\`) && strings.Contains(e.Location, "\n") {
			seps = true
		}
	}
	if !emoji || !big || !seps {
		t.Errorf("text cases: emoji title %v, 2 KB description %v, location with ,;\\ and newline %v", emoji, big, seps)
	}
}

// TestCommandShapes pins the exact proton-cal argv of each kind of command.
func TestCommandShapes(t *testing.T) {
	ds, err := loadInline(t, `
seed: 1
range: {from: "2021-01-01", to: "2021-12-31"}
calendars:
  - {key: default, name: Main, default: true}
  - {key: work, name: Work}
series:
  - title: Payday
    calendar: work
    start: "2021-03-05 09:00"
    end: "2021-03-05 09:05"
    tz: Europe/Paris
    rrule: "FREQ=MONTHLY;BYSETPOS=-1;BYDAY=FR"
    edit: {occurrence: "2021-04-30 09:00", start: "2021-04-30 10:00", end: "2021-04-30 10:05"}
    delete: {occurrence: "2021-05-28 09:00"}
  - title: Party
    calendar: default
    all_day: true
    start: "2021-06-12"
    repeat: yearly
    count: 3
    edit: {occurrence: "2022-06-12", start: "2022-06-13"}
    delete: {occurrence: "2023-06-12"}
text:
  - {title: Odd, calendar: default, start: "2021-02-03 10:00", end: "2021-02-03 11:00", tz: UTC, location: "A, B;C\\D", description: "x", reminders: [none], color: pacific}
  - {title: Many, calendar: default, start: "2021-02-04 10:00", end: "2021-02-04 11:00", tz: UTC, reminders: [15m, "email:1h"]}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"create", "event", "pcb-seed s001 Payday", "--start", "2021-03-05 09:00", "--end", "2021-03-05 09:05", "--tz", "Europe/Paris", "--rrule", "FREQ=MONTHLY;BYSETPOS=-1;BYDAY=FR", "--calendar", "{cal}", "-o", "json"},
		{"update", "event", "{id}", "--occurrence", "2021-04-30 09:00", "--start", "2021-04-30 10:00", "--end", "2021-04-30 10:05", "--summary", "pcb-seed s001.e Payday (edited)", "--tz", "Europe/Paris", "--calendar", "{cal}", "-o", "json"},
		{"delete", "event", "{id}", "--occurrence", "2021-05-28 09:00", "--tz", "Europe/Paris", "--calendar", "{cal}", "-o", "json"},
		{"create", "event", "pcb-seed s002 Party", "--all-day", "--start", "2021-06-12", "--repeat", "yearly", "--count", "3", "--calendar", "{cal}", "-o", "json"},
		{"update", "event", "{id}", "--occurrence", "2022-06-12", "--start", "2022-06-13", "--summary", "pcb-seed s002.e Party (edited)", "--calendar", "{cal}", "-o", "json"},
		{"delete", "event", "{id}", "--occurrence", "2023-06-12", "--calendar", "{cal}", "-o", "json"},
		{"create", "event", "pcb-seed s003 Odd", "--start", "2021-02-03 10:00", "--end", "2021-02-03 11:00", "--tz", "UTC", "--description", "x", "--location", "A, B;C\\D", "--no-reminders", "--color", "pacific", "--calendar", "{cal}", "-o", "json"},
		{"create", "event", "pcb-seed s004 Many", "--start", "2021-02-04 10:00", "--end", "2021-02-04 11:00", "--tz", "UTC", "--reminder", "15m", "--reminder", "email:1h", "--calendar", "{cal}", "-o", "json"},
	}
	cmds := Plan(ds, nil)
	if len(cmds) != len(want) {
		t.Fatalf("%d commands, want %d: %v", len(cmds), len(want), cmds)
	}
	for i, c := range cmds {
		if !slices.Equal(c.Args, want[i]) {
			t.Errorf("command %d:\n got %q\nwant %q", i, c.Args, want[i])
		}
	}
	if cmds[1].Ref != "s001" || cmds[2].Ref != "s001" || cmds[0].Ref != "" {
		t.Errorf("refs = %q %q %q, want edit and delete to reference s001 and create none", cmds[0].Ref, cmds[1].Ref, cmds[2].Ref)
	}
	if cmds[0].Calendar != "work" || cmds[3].Calendar != "default" {
		t.Errorf("calendars = %q %q", cmds[0].Calendar, cmds[3].Calendar)
	}
}
