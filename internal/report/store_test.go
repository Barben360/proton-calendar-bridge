package report_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

var ctx = context.Background()

// newStore is a store over a fresh state directory, its clock at t0 and no
// PCB_CRASH_REPORTS.
func newStore(t *testing.T) report.Store {
	t.Helper()
	return report.Store{StateDir: t.TempDir(), Env: func(string) string { return "" }, Now: func() time.Time { return t0 }}
}

// crashIn is a crash event whose crashing function is fn (a fingerprint of
// its own), built at.
func crashIn(fn string, at time.Time) report.Event {
	return report.CrashEvent(report.Crash{Kind: "panic", Frames: []report.Frame{frame(daemon+"."+fn, "internal/daemon/d.go", 1, true)}}, meta, at)
}

func pendingIDs(t *testing.T, s report.Store) []string {
	t.Helper()
	pend, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range pend {
		ids = append(ids, p.ID)
	}
	return ids
}

func save(t *testing.T, s report.Store, e report.Event) {
	t.Helper()
	if err := s.Save(ctx, e); err != nil {
		t.Fatal(err)
	}
}

func writeStore(t *testing.T, s report.Store, body string) {
	t.Helper()
	if err := os.WriteFile(report.StorePath(s.StateDir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPaths(t *testing.T) {
	if got := report.Dir("/s"); got != filepath.Join("/s", "reports") {
		t.Errorf("Dir = %q", got)
	}
	if got := report.StorePath("/s"); got != filepath.Join("/s", "reports.json") {
		t.Errorf("StorePath = %q", got)
	}
}

func TestModeDefaultsToAsk(t *testing.T) {
	if got := newStore(t).Mode(); got != report.Ask {
		t.Errorf("Mode = %q, want ask", got)
	}
}

func TestSetModeRoundTrip(t *testing.T) {
	s := newStore(t)
	for _, m := range []report.Mode{report.Always, report.Ask, report.Never} {
		if err := s.SetMode(ctx, m); err != nil {
			t.Fatal(err)
		}
		if got := s.Mode(); got != m {
			t.Errorf("after SetMode(%q): Mode = %q", m, got)
		}
	}
	if err := s.SetMode(ctx, "sometimes"); err == nil {
		t.Error("SetMode accepted an unknown mode")
	}
	if got := s.Mode(); got != report.Never {
		t.Errorf("an unknown mode changed the file: %q", got)
	}
}

func TestModeEnvOverridesFile(t *testing.T) {
	s := newStore(t)
	if err := s.SetMode(ctx, report.Always); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		env  string
		want report.Mode
	}{
		{"never", report.Never},
		{" NEVER ", report.Never},
		{"Always", report.Always},
		{"ask", report.Always}, // not an override: the file's mode
		{"off", report.Always},
		{"", report.Always},
		{"sometimes", report.Always},
	}
	for _, tc := range cases {
		env, want := tc.env, tc.want
		s.Env = func(k string) string {
			if k == report.EnvSwitch {
				return env
			}
			return ""
		}
		if got := s.Mode(); got != want {
			t.Errorf("PCB_CRASH_REPORTS=%q: Mode = %q, want %q", env, got, want)
		}
	}
}

func TestEnvSwitchName(t *testing.T) {
	if report.EnvSwitch != "PCB_CRASH_REPORTS" {
		t.Errorf("EnvSwitch = %q", report.EnvSwitch)
	}
}

// A damaged reports.json never reads as "always"; a damaged field other
// than mode keeps the mode.
func TestDamagedStoreReadsAsk(t *testing.T) {
	s := newStore(t)
	for _, body := range []string{"{not json", `{"mode":"sometimes"}`, `{"mode":5}`, `["always"]`, `"always"`} {
		writeStore(t, s, body)
		if got := s.Mode(); got != report.Ask {
			t.Errorf("%s: Mode = %q, want ask", body, got)
		}
	}
	writeStore(t, s, `{"mode":"always","seen":7,"attempts":"x","sent_count":"y"}`)
	if got := s.Mode(); got != report.Always {
		t.Errorf("damaged other fields: Mode = %q, want always", got)
	}
}

func TestEditKeepsEveryField(t *testing.T) {
	s := newStore(t)
	retry := t0.Add(time.Minute)
	if err := s.Edit(ctx, func(st *report.State) {
		st.Mode, st.RetryAfter, st.SentDay, st.SentCount, st.NoticedDay = report.Always, retry, "2026-11-19", 4, "2026-11-18"
		st.Attempts = map[string]int{"a": 2}
	}); err != nil {
		t.Fatal(err)
	}
	var got report.State
	if err := s.Edit(ctx, func(st *report.State) { got = *st }); err != nil {
		t.Fatal(err)
	}
	if got.Mode != report.Always || !got.RetryAfter.Equal(retry) || got.SentDay != "2026-11-19" || got.SentCount != 4 || got.NoticedDay != "2026-11-18" || got.Attempts["a"] != 2 {
		t.Errorf("State after a round trip: %+v", got)
	}
	b, err := os.ReadFile(report.StorePath(s.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mode", "retry_after", "sent_day", "sent_count", "attempts", "noticed_day"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("reports.json has no %q: %s", k, b)
		}
	}
}

func TestSaveKeepsTheExactEvent(t *testing.T) {
	s := newStore(t)
	e := crashIn("f", t0)
	save(t, s, e)
	pend, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 {
		t.Fatalf("%d pending, want 1", len(pend))
	}
	p := pend[0]
	if p.ID != e.ID() || p.Level != "fatal" || p.Source != "crash" || p.Path != filepath.Join(report.Dir(s.StateDir), "pending-"+e.ID()+".json") {
		t.Errorf("Pending = %+v", p)
	}
	onDisk, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := eventJSON(t, e)
	if string(p.JSON) != string(want) || string(onDisk) != string(want) {
		t.Errorf("pending JSON %s\nfile %s\nwant %s", p.JSON, onDisk, want)
	}
}

func TestNeverDeletesPendingAndSavesNothing(t *testing.T) {
	s := newStore(t)
	save(t, s, crashIn("a", t0))
	save(t, s, crashIn("b", t0))
	if n := len(pendingIDs(t, s)); n != 2 {
		t.Fatalf("%d pending before never", n)
	}
	if err := s.SetMode(ctx, report.Never); err != nil {
		t.Fatal(err)
	}
	if ids := pendingIDs(t, s); len(ids) != 0 {
		t.Errorf("never kept %v", ids)
	}
	save(t, s, crashIn("c", t0))
	if ids := pendingIDs(t, s); len(ids) != 0 {
		t.Errorf("never saved %v", ids)
	}

	// The environment's never saves nothing either, whatever the file says.
	s = newStore(t)
	s.Env = func(k string) string {
		if k == report.EnvSwitch {
			return "never"
		}
		return ""
	}
	save(t, s, crashIn("d", t0))
	if entries, _ := os.ReadDir(report.Dir(s.StateDir)); len(entries) != 0 {
		t.Errorf("PCB_CRASH_REPORTS=never left %d files", len(entries))
	}
}

func count(t *testing.T, p report.Pending) int {
	t.Helper()
	var e struct{ Extra struct{ Count int } }
	if err := json.Unmarshal(p.JSON, &e); err != nil {
		t.Fatal(err)
	}
	return e.Extra.Count
}

func TestSaveFoldsSameFingerprint(t *testing.T) {
	s := newStore(t)
	first := crashIn("loop", t0)
	save(t, s, first)
	for i := range 3 {
		save(t, s, crashIn("loop", t0.Add(time.Duration(i+1)*time.Minute)))
	}
	save(t, s, crashIn("other", t0.Add(time.Hour)))
	pend, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 2 || pend[0].ID != first.ID() {
		t.Fatalf("pending %v, want the first loop report and the other one", pendingIDs(t, s))
	}
	if n := count(t, pend[0]); n != 4 {
		t.Errorf("extra.count %d, want 4", n)
	}
	if n := count(t, pend[1]); n != 1 {
		t.Errorf("other's extra.count %d, want 1", n)
	}

	// The next day the same crash is a report of its own.
	s.Now = func() time.Time { return t0.Add(24 * time.Hour) }
	save(t, s, crashIn("loop", t0.Add(24*time.Hour)))
	if n := len(pendingIDs(t, s)); n != 3 {
		t.Errorf("%d pending the next day, want 3", n)
	}
}

// Once a report is gone (sent, discarded) the same fingerprint that day is
// dropped, not saved again.
func TestSaveDropsSameFingerprintGoneToday(t *testing.T) {
	s := newStore(t)
	save(t, s, crashIn("loop", t0))
	if err := s.Discard(ctx); err != nil {
		t.Fatal(err)
	}
	save(t, s, crashIn("loop", t0.Add(time.Hour)))
	other := crashIn("other", t0.Add(time.Hour))
	save(t, s, other)
	if ids := pendingIDs(t, s); !slices.Equal(ids, []string{other.ID()}) {
		t.Errorf("pending %v, want the repeat dropped and only %s", ids, other.ID())
	}
}

func TestPendingCapDropsOldest(t *testing.T) {
	s := newStore(t)
	var ids []string
	for i := range 21 {
		e := crashIn(fmt.Sprintf("f%d", i), t0.Add(time.Duration(i)*time.Second))
		ids = append(ids, e.ID())
		save(t, s, e)
	}
	if got := pendingIDs(t, s); !slices.Equal(got, ids[1:]) {
		t.Errorf("pending %v\nwant %v (oldest first, the first gone)", got, ids[1:])
	}
}

func TestCorruptPendingIsDropped(t *testing.T) {
	s := newStore(t)
	good := crashIn("good", t0)
	save(t, s, good)
	dir := report.Dir(s.StateDir)
	bad := map[string]string{
		"pending-0123456789abcdef0123456789abcdef.json": "{not json",
		"pending-11111111111111111111111111111111.json": `{"event_id":"11111111111111111111111111111111","server_name":"host"}`,
		"pending-22222222222222222222222222222222.json": string(eventJSON(t, crashIn("x", t0))), // another event's id
		"pending-nothex.json":                           string(eventJSON(t, crashIn("y", t0))),
	}
	for name, body := range bad {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "crash-pcb-1-2.txt")
	if err := os.WriteFile(other, []byte("panic: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); !slices.Equal(got, []string{good.ID()}) {
		t.Errorf("pending %v, want only %s", got, good.ID())
	}
	for name := range bad {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v, want it deleted", name, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a crash file was touched: %v", err)
	}
}

func TestDiscard(t *testing.T) {
	s := newStore(t)
	a, b, c := crashIn("a", t0), crashIn("b", t0.Add(time.Second)), crashIn("c", t0.Add(2*time.Second))
	for _, e := range []report.Event{a, b, c} {
		save(t, s, e)
	}
	if err := s.Edit(ctx, func(st *report.State) { st.Attempts = map[string]int{b.ID(): 2, c.ID(): 1} }); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(ctx, b.ID(), "../../etc/passwd"); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); !slices.Equal(got, []string{a.ID(), c.ID()}) {
		t.Errorf("after discarding b: %v", got)
	}
	var st report.State
	if err := s.Edit(ctx, func(x *report.State) { st = *x }); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Attempts[b.ID()]; ok || st.Attempts[c.ID()] != 1 {
		t.Errorf("attempts %v, want b's gone and c's kept", st.Attempts)
	}
	if err := s.Discard(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); len(got) != 0 {
		t.Errorf("after discarding all: %v", got)
	}
}

func TestFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes: on Windows the files take the access list of the private state directory")
	}
	s := newStore(t)
	save(t, s, crashIn("f", t0))
	if err := s.SetMode(ctx, report.Ask); err != nil {
		t.Fatal(err)
	}
	pend, err := s.Pending()
	if err != nil || len(pend) != 1 {
		t.Fatalf("pending %v, %v", pend, err)
	}
	want := map[string]os.FileMode{
		report.Dir(s.StateDir):                    0o700 | os.ModeDir,
		pend[0].Path:                              0o600,
		report.StorePath(s.StateDir):              0o600,
		filepath.Join(s.StateDir, "reports.lock"): 0o600,
	}
	for path, mode := range want {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if got := info.Mode() & (os.ModeDir | os.ModePerm); got != mode {
			t.Errorf("%s: mode %v, want %v", filepath.Base(path), got, mode)
		}
	}
}

func TestNoStateDirNothingCreated(t *testing.T) {
	parent := t.TempDir()
	s := report.Store{StateDir: filepath.Join(parent, "state"), Env: func(string) string { return "" }, Now: func() time.Time { return t0 }}
	if err := s.Save(ctx, crashIn("f", t0)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Save: %v, want a missing state directory", err)
	}
	if err := s.SetMode(ctx, report.Always); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SetMode: %v", err)
	}
	if err := s.Edit(ctx, func(*report.State) {}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Edit: %v", err)
	}
	if err := s.Discard(ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Discard: %v", err)
	}
	if pend, err := s.Pending(); err != nil || len(pend) != 0 {
		t.Errorf("Pending = %v, %v", pend, err)
	}
	if got := s.Mode(); got != report.Ask {
		t.Errorf("Mode = %q", got)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("created %v", entries)
	}
}

// A Store without a state directory never reads nor writes relative to the
// working directory.
func TestEmptyStateDir(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("reports.json", []byte(`{"mode":"always"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("reports", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("reports", "pending-x.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := report.Store{Env: func(string) string { return "" }}
	if got := s.Mode(); got != report.Ask {
		t.Errorf("Mode = %q, want ask", got)
	}
	if pend, err := s.Pending(); err != nil || len(pend) != 0 {
		t.Errorf("Pending = %v, %v", pend, err)
	}
	if err := s.Save(ctx, crashIn("f", t0)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join("reports", "pending-x.json")); err != nil {
		t.Errorf("a file of the working directory was touched: %v", err)
	}
}

// The zero Store reads the real environment and clock.
func TestStoreDefaults(t *testing.T) {
	t.Setenv(report.EnvSwitch, "always")
	s := report.Store{StateDir: t.TempDir()}
	if got := s.Mode(); got != report.Always {
		t.Errorf("Mode = %q, want the environment's always", got)
	}
	t.Setenv(report.EnvSwitch, "")
	save(t, s, crashIn("f", time.Now()))
	if n := len(pendingIDs(t, s)); n != 1 {
		t.Errorf("%d pending", n)
	}
}
