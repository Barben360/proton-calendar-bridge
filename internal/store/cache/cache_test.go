package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sha32(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:32]
}

var bg = context.Background()

func openAt(t *testing.T, path, account string) *Cache {
	t.Helper()
	c, err := Open(path, account)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func openNew(t *testing.T) (*Cache, string) {
	t.Helper()
	path := Path(t.TempDir())
	return openAt(t, path, "acct-1"), path
}

func row(id string, v int) Row {
	return Row{
		ID: id, UID: id + "@proton", Start: 1731000000, End: 1731003600, LastEditTime: int64(v),
		Raw: json.RawMessage(`{"ID":"` + id + `","V":` + strconv.Itoa(v) + `}`),
	}
}

func seq(t *testing.T, c *Cache) int64 {
	t.Helper()
	n, err := c.Seq(bg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ids(t *testing.T, c *Cache, cal string) map[string]Event {
	t.Helper()
	evs, err := c.Events(bg, cal)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Event{}
	for _, e := range evs {
		m[e.ID] = e
	}
	return m
}

func TestPath(t *testing.T) {
	if got := Path("/s"); got != filepath.Join("/s", "pcb.db") {
		t.Errorf("Path = %s", got)
	}
}

func TestOpenCreates0600WAL(t *testing.T) {
	c, path := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{row("e1", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := c.db.QueryRowContext(bg, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v", mode, err)
	}
	var busy int
	if err := c.db.QueryRowContext(bg, `PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != 5000 {
		t.Errorf("busy_timeout = %d, %v", busy, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, fi.Mode().Perm())
		}
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	c, path := openNew(t)
	if _, err := c.db.ExecContext(bg, `PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	_, err := Open(path, "acct-1")
	if err == nil || !strings.Contains(err.Error(), "99") {
		t.Errorf("err = %v, want one naming version 99", err)
	}
}

func TestOpenForAnotherAccountEmpties(t *testing.T) {
	c, path := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{row("e1", 1), row("e2", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyDelta(bg, "cal", "c2", nil, []string{"e2"}, time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	same := openAt(t, path, "acct-1")
	if len(ids(t, same, "cal")) != 1 {
		t.Error("reopening for the same account lost rows")
	}
	_ = same.Close()

	other := openAt(t, path, "acct-2")
	if cals, err := other.Calendars(bg); err != nil || len(cals) != 0 {
		t.Errorf("calendars = %v, %v; want none", cals, err)
	}
	if n := len(ids(t, other, "cal")); n != 0 {
		t.Errorf("%d rows survived an account change", n)
	}
	var tomb int
	if err := other.db.QueryRowContext(bg, `SELECT count(*) FROM tombstones`).Scan(&tomb); err != nil || tomb != 0 {
		t.Errorf("tombstones = %d, %v", tomb, err)
	}
	var acct string
	if err := other.db.QueryRowContext(bg, `SELECT value FROM meta WHERE key='account'`).Scan(&acct); err != nil || acct != "acct-2" {
		t.Errorf("account = %q, %v", acct, err)
	}
	info, err := Inspect(path)
	if err != nil || info.Account != "acct-2" {
		t.Errorf("Inspect = %+v, %v", info, err)
	}
}

func TestReplaceTombstonesMissingRows(t *testing.T) {
	c, _ := openNew(t)
	at := time.Unix(1000, 0)
	ch, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{row("e1", 1), row("e2", 1), row("e3", 1)}, at)
	if err != nil || ch != (Changes{Added: 3}) {
		t.Fatalf("first = %+v, %v", ch, err)
	}
	if n := seq(t, c); n != 3 {
		t.Errorf("seq = %d, want 3", n)
	}
	ch, err = c.ReplaceCalendar(bg, "cal", "c2", []Row{row("e1", 1), row("e3", 2)}, time.Unix(2000, 0))
	if err != nil || ch != (Changes{Updated: 1, Deleted: 1}) {
		t.Fatalf("second = %+v, %v", ch, err)
	}
	if n := seq(t, c); n != 5 {
		t.Errorf("seq = %d, want 5", n)
	}
	got := ids(t, c, "cal")
	if len(got) != 2 || got["e2"].ID != "" {
		t.Errorf("events = %v", got)
	}
	if got["e3"].Seq != 4 {
		t.Errorf("e3 seq = %d", got["e3"].Seq)
	}
	var tseq int64
	if err := c.db.QueryRowContext(bg, `SELECT seq FROM tombstones WHERE calendar_id='cal' AND id='e2'`).Scan(&tseq); err != nil || tseq == 0 {
		t.Errorf("tombstone e2: seq %d, %v", tseq, err)
	}
	cals, err := c.Calendars(bg)
	if err != nil || len(cals) != 1 {
		t.Fatal(cals, err)
	}
	cal := cals[0]
	if cal.Cursor != "c2" || cal.Events != 2 || !cal.FullAt.Equal(time.Unix(2000, 0)) || !cal.SyncedAt.Equal(time.Unix(2000, 0)) {
		t.Errorf("calendar = %+v", cal)
	}
	// Another calendar is untouched by a replace.
	if _, err := c.ReplaceCalendar(bg, "other", "o1", []Row{row("e1", 1)}, at); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceCalendar(bg, "cal", "c3", nil, at); err != nil {
		t.Fatal(err)
	}
	if len(ids(t, c, "other")) != 1 {
		t.Error("replace of one calendar touched another")
	}
}

func TestApplyDeltaUnchangedETagNoBump(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ApplyDelta(bg, "cal", "c1", []Row{row("e1", 1)}, nil, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	before := seq(t, c)
	ch, err := c.ApplyDelta(bg, "cal", "c2", []Row{row("e1", 1)}, []string{"never-existed"}, time.Unix(20, 0))
	if err != nil || ch != (Changes{}) {
		t.Fatalf("changes = %+v, %v", ch, err)
	}
	if got := seq(t, c); got != before {
		t.Errorf("seq %d -> %d for an unchanged row", before, got)
	}
	cals, err := c.Calendars(bg)
	if err != nil {
		t.Fatal(err)
	}
	if cals[0].Cursor != "c2" || !cals[0].SyncedAt.Equal(time.Unix(20, 0)) {
		t.Errorf("cursor/synced not advanced: %+v", cals[0])
	}
	if !cals[0].FullAt.IsZero() {
		t.Errorf("a delta set FullAt: %v", cals[0].FullAt)
	}
	// A changed row bumps.
	ch, err = c.ApplyDelta(bg, "cal", "c3", []Row{row("e1", 2)}, nil, time.Unix(30, 0))
	if err != nil || ch != (Changes{Updated: 1}) || seq(t, c) != before+1 {
		t.Errorf("changes = %+v, %v, seq %d", ch, err, seq(t, c))
	}
}

func TestApplyDeltaIsAtomic(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{row("e1", 1), row("e2", 1)}, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	before := seq(t, c)
	bad := row("", 1)
	_, err := c.ApplyDelta(bg, "cal", "c2", []Row{row("e1", 2), row("e9", 1), bad}, []string{"e2"}, time.Unix(20, 0))
	if err == nil {
		t.Fatal("a row with an empty ID was accepted")
	}
	if got := seq(t, c); got != before {
		t.Errorf("seq moved %d -> %d", before, got)
	}
	got := ids(t, c, "cal")
	if len(got) != 2 || got["e9"].ID != "" || got["e2"].ID == "" {
		t.Errorf("rows changed: %v", got)
	}
	if string(mustRaw(t, c, "cal", "e1")) != string(row("e1", 1).Raw) {
		t.Error("e1 was updated despite the failure")
	}
	cals, err := c.Calendars(bg)
	if err != nil {
		t.Fatal(err)
	}
	if cals[0].Cursor != "c1" || !cals[0].SyncedAt.Equal(time.Unix(10, 0)) {
		t.Errorf("cursor moved: %+v", cals[0])
	}
}

func mustRaw(t *testing.T, c *Cache, cal, id string) json.RawMessage {
	t.Helper()
	r, err := c.Raw(bg, cal, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecreatedIDDropsTombstone(t *testing.T) {
	c, _ := openNew(t)
	count := func() int {
		var n int
		if err := c.db.QueryRowContext(bg, `SELECT count(*) FROM tombstones`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := c.ApplyDelta(bg, "cal", "c1", []Row{row("e1", 1)}, nil, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	ch, err := c.ApplyDelta(bg, "cal", "c2", nil, []string{"e1"}, time.Unix(2, 0))
	if err != nil || ch != (Changes{Deleted: 1}) || count() != 1 {
		t.Fatalf("delete: %+v %v tombstones %d", ch, err, count())
	}
	ch, err = c.ApplyDelta(bg, "cal", "c3", []Row{row("e1", 1)}, nil, time.Unix(3, 0))
	if err != nil || ch != (Changes{Added: 1}) {
		t.Fatalf("recreate: %+v %v", ch, err)
	}
	if count() != 0 {
		t.Error("tombstone survived the re-creation")
	}
	if _, err := c.Raw(bg, "cal", "e1"); err != nil {
		t.Errorf("Raw: %v", err)
	}
	if _, err := c.Raw(bg, "cal", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Raw missing = %v, want ErrNotFound", err)
	}
}

func TestEventFieldsRoundTrip(t *testing.T) {
	c, _ := openNew(t)
	rid := int64(1731000000)
	r := Row{ID: "e1", UID: "u1", Start: 5, End: 9, FullDay: true, RecurrenceID: &rid, RRule: "FREQ=DAILY", LastEditTime: 77, Raw: json.RawMessage(`{"a":1}`)}
	plain := row("e2", 1)
	if _, err := c.ReplaceCalendar(bg, "cal", "c", []Row{r, plain}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	got := ids(t, c, "cal")
	e := got["e1"]
	wantTag, err := ETag(r.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.UID != "u1" || e.Start != 5 || e.End != 9 || !e.FullDay || e.RecurrenceID == nil || *e.RecurrenceID != rid ||
		e.RRule != "FREQ=DAILY" || e.LastEditTime != 77 || e.ETag != wantTag || e.Seq == 0 {
		t.Errorf("event = %+v", e)
	}
	if got["e2"].RecurrenceID != nil {
		t.Error("NULL RecurrenceID read back as set")
	}
	if string(mustRaw(t, c, "cal", "e1")) != `{"a":1}` {
		t.Error("Raw differs")
	}
}

func TestPutAndDropCalendar(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "cur", []Row{row("e1", 1), row("e2", 1)}, time.Unix(5, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyDelta(bg, "cal", "cur", nil, []string{"e2"}, time.Unix(6, 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.PutCalendar(bg, Calendar{ID: "cal", Name: "Work", Type: "personal", Cursor: "IGNORED", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	cals, err := c.Calendars(bg)
	if err != nil {
		t.Fatal(err)
	}
	if cals[0].Name != "Work" || cals[0].Type != "personal" || cals[0].Error != "boom" || cals[0].Cursor != "cur" || cals[0].Events != 1 ||
		!cals[0].FullAt.Equal(time.Unix(5, 0)) || !cals[0].SyncedAt.Equal(time.Unix(6, 0)) {
		t.Errorf("calendar = %+v", cals[0])
	}
	if err := c.PutCalendar(bg, Calendar{ID: "new", Name: "N"}); err != nil {
		t.Fatal(err)
	}
	if cals, _ = c.Calendars(bg); len(cals) != 2 || cals[1].Cursor != "" {
		t.Errorf("calendars = %+v", cals)
	}
	if err := c.DropCalendar(bg, "cal"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRowContext(bg, `SELECT (SELECT count(*) FROM events)+(SELECT count(*) FROM tombstones)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if cals, _ = c.Calendars(bg); len(cals) != 1 || cals[0].ID != "new" || n != 0 {
		t.Errorf("after drop: %+v, %d rows left", cals, n)
	}
}

func TestRemoveDeletesWalAndShm(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Error("Remove touched another file")
	}
	if err := Remove(path); err != nil {
		t.Errorf("missing files: %v", err)
	}
}

func TestTwoProcessesReadWhileWriting(t *testing.T) {
	w, path := openNew(t)
	if _, err := w.ReplaceCalendar(bg, "cal", "c1", []Row{row("e1", 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	tx, err := w.db.BeginTx(bg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(bg, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, "acct-1") // must not block on the writer's lock
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if got := ids(t, r, "cal"); len(got) != 1 {
		t.Errorf("reader saw %d rows during the write, want the committed 1", len(got))
	}
	if cals, err := r.Calendars(bg); err != nil || len(cals) != 1 {
		t.Errorf("Calendars = %v, %v", cals, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, r, "cal"); len(got) != 0 {
		t.Errorf("reader saw %d rows after the commit, want 0", len(got))
	}
}

func TestInspect(t *testing.T) {
	path := Path(t.TempDir())
	info, err := Inspect(path)
	if err != nil || info.Exists {
		t.Fatalf("absent: %+v, %v", info, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Inspect created the file")
	}
	c := openAt(t, path, "acct-9")
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{row("e1", 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	info, err = Inspect(path) // writer still open, WAL not checkpointed
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || info.Schema != 1 || info.Account != "acct-9" || info.QuickCheck != "ok" {
		t.Errorf("info = %+v", info)
	}
	if runtime.GOOS != "windows" && info.Mode.Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode.Perm())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if names(before) != names(after) {
		t.Errorf("Inspect changed the directory: %s -> %s", names(before), names(after))
	}

	junk := filepath.Join(t.TempDir(), "pcb.db")
	if err := os.WriteFile(junk, []byte("this is not a sqlite database at all, just text padding padding padding"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err = Inspect(junk)
	if err != nil || !info.Exists || info.QuickCheck == "ok" || info.QuickCheck == "" {
		t.Errorf("junk: %+v, %v", info, err)
	}
}

func names(es []os.DirEntry) string {
	var n []string
	for _, e := range es {
		n = append(n, e.Name())
	}
	return strings.Join(n, ",")
}

func TestOpenNotADatabase(t *testing.T) {
	path := Path(t.TempDir())
	if err := os.WriteFile(path, []byte(strings.Repeat("definitely not sqlite ", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, "a")
	if err == nil || !strings.Contains(err.Error(), "pcb sync --full") || !strings.Contains(err.Error(), "remove") {
		t.Errorf("err = %v, want one saying to remove the file and run pcb sync --full", err)
	}
}

func TestOpenMissingDirectory(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent", "pcb.db"), "a")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v", err)
	}
}
