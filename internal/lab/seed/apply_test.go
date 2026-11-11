package seed

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain turns the test binary into a fake proton-cal when PCB_SEED_FAKE is
// set: it records its argv, answers from the log of earlier calls and never
// touches the network.
func TestMain(m *testing.M) {
	if os.Getenv("PCB_SEED_FAKE") == "1" {
		os.Exit(fakeProtonCal(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeCall struct {
	Args      []string `json:"args"`
	ConfigDir string   `json:"config_dir"`
	EnvKeys   []string `json:"env_keys"`
	Failed    bool     `json:"failed,omitempty"`
}

func readLog(path string) []fakeCall {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var calls []fakeCall
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var c fakeCall
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			calls = append(calls, c)
		}
	}
	return calls
}

func envKeys() []string {
	var ks []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		ks = append(ks, k)
	}
	return ks
}

func isWrite(a []string) bool {
	return len(a) >= 2 && (a[0] == "create" || a[0] == "update" || a[0] == "delete")
}

func fakeProtonCal(args []string) int {
	logPath := os.Getenv("PCB_SEED_LOG")
	prev := readLog(logPath)
	call := fakeCall{Args: args, ConfigDir: os.Getenv("PROTON_CAL_CONFIG_DIR"), EnvKeys: envKeys()}
	writes := 0
	for _, c := range prev {
		if isWrite(c.Args) {
			writes++
		}
	}
	if n, err := strconv.Atoi(os.Getenv("PCB_SEED_FAIL_AFTER")); err == nil && isWrite(args) && writes >= n {
		call.Failed = true
	}
	b, _ := json.Marshal(call)
	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	if call.Failed || (os.Getenv("PCB_SEED_FAIL_READ") == args[0]) {
		fmt.Fprintln(os.Stderr, "Error: proton api error: status=429 code=0: Too Many Requests")
		return 1
	}
	cals := []map[string]any{
		{"id": "CAL-DEF", "name": "Agenda perso", "is_default": true},
		{"id": "CAL-WORK", "name": "Work", "is_default": false},
	}
	var rows []map[string]any
	creates := 0
	// The real tool renders timed starts in its own zone (Paris here) and
	// all-day starts at UTC midnight.
	paris, _ := time.LoadLocation("Europe/Paris")
	stamp := func(s, tz string) string {
		if len(s) == 10 {
			return s + "T00:00:00Z"
		}
		loc, err := time.LoadLocation(tz)
		if err != nil || tz == "" {
			loc = time.UTC
		}
		t, _ := time.ParseInLocation("2006-01-02 15:04", s, loc)
		return t.In(paris).Format(time.RFC3339)
	}
	drop := func(start string) {
		rows = slices.DeleteFunc(rows, func(r map[string]any) bool { return r["start"] == start })
	}
	for _, c := range prev {
		if c.Failed || len(c.Args) < 3 {
			continue
		}
		switch {
		case c.Args[0] == "create" && c.Args[1] == "calendar":
			cals = append(cals, map[string]any{"id": "CAL-" + strings.ToUpper(c.Args[2]), "name": c.Args[2], "is_default": false})
		case c.Args[0] == "create" && c.Args[1] == "event":
			creates++
			id := fmt.Sprintf("EV%d", creates)
			n, _ := strconv.Atoi(flagValue(c.Args, "--count"))
			if flagValue(c.Args, "--repeat") != "daily" || n < 1 {
				n = 1
			}
			t0, _ := time.Parse(time.RFC3339, stamp(flagValue(c.Args, "--start"), flagValue(c.Args, "--tz")))
			for i := 0; i < n; i++ { // the real listing expands series occurrences
				rows = append(rows, map[string]any{"id": id, "summary": c.Args[2], "start": t0.AddDate(0, 0, i).Format(time.RFC3339)})
			}
		case c.Args[0] == "update" && flagValue(c.Args, "--occurrence") != "":
			drop(stamp(flagValue(c.Args, "--occurrence"), flagValue(c.Args, "--tz")))
			rows = append(rows, map[string]any{"id": "EVE-" + c.Args[2], "summary": flagValue(c.Args, "--summary"), "start": stamp(flagValue(c.Args, "--start"), flagValue(c.Args, "--tz"))})
		case c.Args[0] == "delete" && flagValue(c.Args, "--occurrence") != "":
			drop(stamp(flagValue(c.Args, "--occurrence"), flagValue(c.Args, "--tz")))
		}
	}
	switch {
	case args[0] == "calendars":
		return say(cals)
	case args[0] == "events":
		// proton-cal filters by its --from/--days window in its own zone.
		wf, _ := time.ParseInLocation("2006-01-02", flagValue(args, "--from"), paris)
		nd, _ := strconv.Atoi(flagValue(args, "--days"))
		var in []map[string]any
		for _, r := range rows {
			t, _ := time.Parse(time.RFC3339, r["start"].(string))
			if !t.Before(wf) && t.Before(wf.AddDate(0, 0, nd)) {
				in = append(in, r)
			}
		}
		return say(in)
	case args[0] == "create" && args[1] == "calendar":
		return say(map[string]any{"id": "CAL-" + strings.ToUpper(args[2]), "name": args[2]})
	case args[0] == "create" && args[1] == "event":
		return say(map[string]any{"id": fmt.Sprintf("EV%d", creates+1), "summary": args[2]})
	}
	return say(map[string]any{"ok": true})
}

func say(v any) int {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
	return 0
}

func flagValue(args []string, name string) string {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

const smallDataset = `
seed: 7
range: {from: "2021-01-01", to: "2021-12-31"}
calendars:
  - {key: default, name: My calendar, default: true}
  - {key: work, name: Work}
  - {key: sport, name: Sport, color: fern}
timed:
  count: 3
  hours: [8, 18]
  durations_min: [30]
  timezones: [{v: UTC, w: 1}]
  calendars: [{v: default, w: 1}]
  reminders: [{v: default, w: 1}]
  colors: [{v: "", w: 1}]
  titles: {default: [Chore]}
series:
  - title: Run club
    calendar: sport
    start: "2021-03-01 07:00"
    end: "2021-03-01 08:00"
    tz: UTC
    repeat: daily
    count: 10
    edit: {occurrence: "2021-03-03 07:00", start: "2021-03-03 09:00", end: "2021-03-03 10:00"}
    delete: {occurrence: "2021-03-05 07:00"}
text:
  - {title: Note, calendar: work, start: "2021-05-04 10:00", end: "2021-05-04 11:00", tz: UTC}
`

type rig struct {
	ds     Dataset
	log    string
	runner *ExecRunner
	cfg    string
	sleeps []time.Duration
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ds, err := loadInline(t, smallDataset)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{ds: ds, log: filepath.Join(t.TempDir(), "calls.jsonl"), cfg: filepath.Join(t.TempDir(), "pcb-lab-seed")}
	for _, k := range []string{"PROTON_CAL_PASSWORD", "PROTON_CAL_USERNAME", "PROTON_CAL_TOTP", "PROTON_CAL_MCP_TOKEN", "PCB_LAB_PASSWORD", "SSH_AUTH_SOCK", "PCB_SEED_RANDOM_SENTINEL"} {
		t.Setenv(k, "must-not-reach-the-child")
	}
	t.Setenv("https_proxy", "http://proxy.invalid:3128") // allowed, in any case
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	r.runner = &ExecRunner{Bin: os.Args[0], ConfigDir: r.cfg, Env: []string{"GORACE=atexit_sleep_ms=0", "PCB_SEED_FAKE=1", "PCB_SEED_LOG=" + r.log}}
	return r
}

func (r *rig) apply(t *testing.T) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	// The runner's environment is an allowlist: the fake's knobs go through Env.
	r.runner.Env = slices.DeleteFunc(r.runner.Env, func(kv string) bool { return strings.HasPrefix(kv, "PCB_SEED_FAIL_") })
	for _, k := range []string{"PCB_SEED_FAIL_AFTER", "PCB_SEED_FAIL_READ"} {
		if v := os.Getenv(k); v != "" {
			r.runner.Env = append(r.runner.Env, k+"="+v)
		}
	}
	return Apply(ctx, r.ds, r.runner, ApplyOptions{
		Wait:  2 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error { r.sleeps = append(r.sleeps, d); return nil },
	})
}

func (r *rig) writes() []fakeCall {
	var w []fakeCall
	for _, c := range readLog(r.log) {
		if isWrite(c.Args) {
			w = append(w, c)
		}
	}
	return w
}

func TestApplyDrivesProtonCalInOrderWithIDsAndDefaultCalendar(t *testing.T) {
	r := newRig(t)
	res, err := r.apply(t)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	calls := readLog(r.log)
	w := r.writes()
	// Calendar Sport created first, then 3 timed, the series (create, edit,
	// delete) and the text case.
	if len(w) != 1+3+3+1 || res.Written != len(w) {
		t.Fatalf("%d writes (result %d), want 8", len(w), res.Written)
	}
	if got := w[0].Args; !slices.Equal(got[:3], []string{"create", "calendar", "Sport"}) {
		t.Errorf("first write = %q, want the missing Sport calendar", got)
	}
	for _, c := range calls {
		if c.ConfigDir != r.cfg {
			t.Fatalf("%v ran with PROTON_CAL_CONFIG_DIR=%q, want %q", c.Args, c.ConfigDir, r.cfg)
		}
		for _, k := range c.EnvKeys {
			if strings.HasPrefix(k, "PROTON_CAL_") && k != "PROTON_CAL_CONFIG_DIR" || k == "SSH_AUTH_SOCK" || strings.HasPrefix(k, "PCB_LAB_") || k == "PCB_SEED_RANDOM_SENTINEL" {
				t.Fatalf("%v inherited %s", c.Args, k)
			}
		}
		for _, k := range []string{"PATH", "https_proxy", "XDG_STATE_HOME", "PROTON_CAL_CONFIG_DIR"} {
			if !slices.Contains(c.EnvKeys, k) {
				t.Fatalf("%v did not get %s (keys %v)", c.Args, k, c.EnvKeys)
			}
		}
		for _, a := range c.Args {
			if strings.Contains(a, "{id}") || strings.Contains(a, "{cal}") {
				t.Fatalf("placeholder left in %q", c.Args)
			}
		}
	}
	// R5: default-key events go to the calendar flagged default, found by flag
	// not by the dataset's name "My calendar".
	if v := flagValue(w[1].Args, "--calendar"); v != "CAL-DEF" {
		t.Errorf("timed event calendar = %q, want CAL-DEF", v)
	}
	if v := flagValue(w[4].Args, "--calendar"); v != "CAL-SPORT" {
		t.Errorf("series calendar = %q, want the created CAL-SPORT", v)
	}
	if v := flagValue(w[7].Args, "--calendar"); v != "CAL-WORK" {
		t.Errorf("text event calendar = %q, want the existing CAL-WORK", v)
	}
	// R19: the edit and the delete address the event id the create returned.
	create, edit, del := w[4], w[5], w[6]
	if create.Args[0] != "create" || edit.Args[0] != "update" || del.Args[0] != "delete" {
		t.Fatalf("series order = %s %s %s", create.Args[0], edit.Args[0], del.Args[0])
	}
	wantID := "EV4" // the fourth created event: s001..s003, then the series
	if edit.Args[2] != wantID || del.Args[2] != wantID {
		t.Errorf("edit/delete target %q / %q, want the created id %q", edit.Args[2], del.Args[2], wantID)
	}
	// One wait of 2 s before every write but the first.
	if len(r.sleeps) != len(w)-1 {
		t.Errorf("%d waits for %d writes, want %d", len(r.sleeps), len(w), len(w)-1)
	}
	for _, d := range r.sleeps {
		if d != 2*time.Second {
			t.Errorf("wait = %v, want 2s", d)
		}
	}
}

func TestApplyStopsOnRateLimitAndResumes(t *testing.T) {
	r := newRig(t)
	t.Setenv("PCB_SEED_FAIL_AFTER", "5") // calendar + s001..s003 + series create succeed; the edit gets a 429
	res, err := r.apply(t)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if res.Written != 5 {
		t.Errorf("Written = %d, want 5 before the 429", res.Written)
	}
	if w := r.writes(); len(w) != 6 || !w[5].Failed || w[5].Args[0] != "update" {
		t.Fatalf("writes after the 429 = %d (want exactly one attempt, the edit, then stop)", len(w))
	}

	// Resume: the stub is healthy again; nothing is created twice.
	t.Setenv("PCB_SEED_FAIL_AFTER", "")
	before := len(r.writes())
	if _, err := r.apply(t); err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer func() { // a third run has nothing left to do
		n := len(r.writes())
		if res, err := r.apply(t); err != nil || res.Written != 0 || len(r.writes()) != n {
			t.Errorf("third run wrote %d (err %v), want a no-op", res.Written, err)
		}
	}()
	var ops []string
	for _, c := range r.writes()[before:] {
		ops = append(ops, c.Args[0]+" "+c.Args[1])
	}
	if want := []string{"update event", "delete event", "create event"}; !slices.Equal(ops, want) {
		t.Errorf("resumed writes = %v, want %v", ops, want)
	}
	if got := r.writes()[before].Args[2]; got != "EV4" {
		t.Errorf("resumed edit targets %q, want the series id EV4 learnt from the listing", got)
	}
}

func TestApplyFailsWhenNoCalendarIsDefault(t *testing.T) {
	r := newRig(t)
	r.runner.Bin = writeScript(t, `echo '[{"id":"X","name":"Only","is_default":false}]'`)
	r.runner.Env = nil
	if _, err := r.apply(t); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v, want a refusal naming the missing default calendar", err)
	}
}

func TestParseExisting(t *testing.T) {
	in := `[
	 {"id":"A1","summary":"pcb-seed s001 Run","start":"2021-03-01T07:00:00Z"},
	 {"id":"A2","summary":"pcb-seed s341.e Standup (edited)","start":"2024-03-07T10:00:00+01:00"},
	 {"id":"B","summary":"Somebody's real event","start":"2021-03-01T07:00:00Z"},
	 {"id":"C","summary":"pcb-seed","start":"2021-03-01T07:00:00Z"}
	]`
	got, err := ParseExisting(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "s001" || got[0].EventID != "A1" || got[1].ID != "s341.e" {
		t.Fatalf("got %+v, want only the two pcb-seed rows with ids s001 and s341.e", got)
	}
	if !got[1].Start.Equal(time.Date(2024, 3, 7, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v, want the instant parsed from RFC 3339", got[1].Start)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

const edgeDataset = `
seed: 1
range: {from: "2021-01-01", to: "2027-06-30"}
calendars: [{key: default, name: Main, default: true}]
text:
  - {title: First edge, calendar: default, start: "2021-01-01 07:00", end: "2021-01-01 08:00", tz: Asia/Tokyo}
  - {title: Last edge, calendar: default, start: "2027-06-30 23:00", end: "2027-06-30 23:30", tz: America/New_York}
`

// The edge events fall on 2020-12-31 and 2027-07-01 in the listing's zone
// (Paris): a window of exactly the range would not list them and a rerun would
// create them twice.
func TestApplyRecognisesEdgeEventsListedInAnotherTimezone(t *testing.T) {
	r := newRig(t)
	ds, err := loadInline(t, edgeDataset)
	if err != nil {
		t.Fatal(err)
	}
	r.ds = ds
	if _, err := r.apply(t); err != nil {
		t.Fatal(err)
	}
	if got := len(r.writes()); got != 2 {
		t.Fatalf("first run: %d writes, want 2", got)
	}
	if res, err := r.apply(t); err != nil || res.Written != 0 || len(r.writes()) != 2 {
		t.Fatalf("rerun wrote %d (err %v), %d writes in all: the edge events were not recognised", res.Written, err, len(r.writes()))
	}
}

func TestListingWindowCoversRangeWithAMarginEachSide(t *testing.T) {
	ds := load(t)
	from, days := ListingWindow(ds)
	f, _ := time.Parse(dateLayout, from)
	end := f.AddDate(0, 0, days)
	if from != "2020-12-31" || end.Format(dateLayout) != "2027-07-02" {
		t.Errorf("window %s + %d days ends %s, want 2020-12-31 to 2027-07-02 (range 2021-01-01..2027-06-30, one day each side)", from, days, end.Format(dateLayout))
	}
}

func TestApplyStopsOnRateLimitOfAListing(t *testing.T) {
	for _, listing := range []string{"calendars", "events"} {
		r := newRig(t)
		t.Setenv("PCB_SEED_FAIL_READ", listing)
		res, err := r.apply(t)
		eventWrites := 0 // creating the missing calendar before the events listing is fine
		for _, w := range r.writes() {
			if w.Args[1] == "event" {
				eventWrites++
			}
		}
		if !errors.Is(err, ErrRateLimited) || eventWrites != 0 {
			t.Errorf("%s 429: err = %v (%d result writes), %d event writes; want ErrRateLimited and no event write", listing, err, res.Written, eventWrites)
		}
	}
}

func TestRateLimitedMatchesProtonCalsErrorOnly(t *testing.T) {
	for stderr, want := range map[string]bool{
		"Error: proton api error: status=429 code=0: Too Many Requests": true,
		"proton api error: status=429 code=2028: slow down":             true,
		"Error: too many requests, retry later":                         true,
		"proton api error: status=4290 code=1: x":                       false,
		"Error: event 1429 not found":                                   false,
		"Error: calendar \"Room 429\" not found":                        false,
		"proton api error: status=422 code=2001: bad start":             false,
	} {
		if got := rateLimited(&RunError{Stderr: stderr}); got != want {
			t.Errorf("rateLimited(%q) = %v, want %v", stderr, got, want)
		}
	}
}

func TestAllDayOccurrenceMatchesByCalendarDate(t *testing.T) {
	ds, err := loadInline(t, `
seed: 1
range: {from: "2021-01-01", to: "2027-06-30"}
calendars: [{key: default, name: Main, default: true}]
series:
  - {title: Party, calendar: default, all_day: true, start: "2021-06-12", repeat: yearly, count: 5, edit: {occurrence: "2022-06-12", start: "2022-06-13"}, delete: {occurrence: "2024-06-12"}}
`)
	if err != nil {
		t.Fatal(err)
	}
	has := func(listed string) bool {
		ts, err := time.Parse(time.RFC3339, listed)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range Plan(ds, []Existing{{ID: "s001", EventID: "E", Start: ts}, {ID: "s001.e", EventID: "F"}}) {
			if c.Op == OpDelete {
				return true
			}
		}
		return false
	}
	for _, listed := range []string{"2024-06-12T00:00:00+02:00", "2024-06-12T00:00:00Z", "2024-06-12T00:00:00-05:00"} {
		if !has(listed) {
			t.Errorf("all-day occurrence listed at %s not seen as still present, so the delete would be skipped", listed)
		}
	}
	if has("2024-06-13T00:00:00Z") {
		t.Error("a listing of another date counted as the occurrence")
	}
}
