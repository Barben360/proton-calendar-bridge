package cache

import (
	"encoding/json"
	"testing"
	"time"
)

// wrow is a row of UID uid with a chosen ID and a version baked into Raw.
func wrow(id, uid string, v int) Row {
	r := row(id, v)
	r.UID = uid
	return r
}

func cursorOf(t *testing.T, c *Cache, cal string) string {
	t.Helper()
	cals, err := c.Calendars(bg)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range cals {
		if x.ID == cal {
			return x.Cursor
		}
	}
	t.Fatalf("no calendar %q", cal)
	return ""
}

func TestApplyWriteReplacesTheGroup(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{
		wrow("m", "u1", 1), wrow("x1", "u1", 1), wrow("x2", "u1", 1), wrow("o", "u2", 1),
	}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	before := ids(t, c, "cal")
	ch, err := c.ApplyWrite(bg, "cal", "u1", []Row{
		wrow("m", "u1", 1),  // equal: no bump
		wrow("x1", "u1", 2), // changed
		wrow("x3", "u1", 1), // new
	}, time.Unix(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if ch != (Changes{Added: 1, Updated: 1, Deleted: 1}) {
		t.Errorf("Changes = %+v, want 1 added, 1 updated, 1 deleted", ch)
	}
	after := ids(t, c, "cal")
	if _, ok := after["x2"]; ok {
		t.Error("x2 survived: it is absent from the written rows")
	}
	if after["m"].Seq != before["m"].Seq {
		t.Errorf("equal row m bumped: seq %d -> %d", before["m"].Seq, after["m"].Seq)
	}
	if after["x1"].Seq <= before["x1"].Seq || after["x3"].Seq <= before["x1"].Seq {
		t.Errorf("changed or new rows not bumped: x1 %d -> %d, x3 %d", before["x1"].Seq, after["x1"].Seq, after["x3"].Seq)
	}
	if after["o"].Seq != before["o"].Seq {
		t.Error("another UID's row was touched")
	}
	chs, err := c.Changes(bg, "cal", before["o"].Seq)
	if err != nil {
		t.Fatal(err)
	}
	var tomb *Change
	for i := range chs {
		if chs[i].Deleted && chs[i].ID == "x2" {
			tomb = &chs[i]
		}
	}
	if tomb == nil || tomb.UID != "u1" {
		t.Errorf("removed row is not a tombstone carrying the UID: %+v", chs)
	}

	// An empty write deletes the UID, and only it.
	ch, err = c.ApplyWrite(bg, "cal", "u1", nil, time.Unix(300, 0))
	if err != nil || ch.Deleted != 3 {
		t.Fatalf("delete: %+v, %v; want 3 deleted", ch, err)
	}
	if got := ids(t, c, "cal"); len(got) != 1 || got["o"].ID == "" {
		t.Errorf("rows after deleting u1 = %v, want only o", got)
	}
}

func TestApplyWriteRefusesAForeignUIDAndWritesNothing(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{wrow("a", "u1", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	n := seq(t, c)
	_, err := c.ApplyWrite(bg, "cal", "u1", []Row{wrow("a", "u1", 2), wrow("b", "u2", 1)}, time.Unix(200, 0))
	if err == nil {
		t.Fatal("a row of another UID was accepted")
	}
	if got := ids(t, c, "cal"); len(got) != 1 || seq(t, c) != n || got["a"].LastEditTime != 1 {
		t.Errorf("a refused write changed the cache: %v, seq %d -> %d", got, n, seq(t, c))
	}
	if _, err := c.ApplyWrite(bg, "cal", "", nil, time.Unix(200, 0)); err == nil {
		t.Error("empty UID accepted")
	}
}

func TestApplyWriteKeepsTheCursor(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{wrow("a", "u1", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyWrite(bg, "cal", "u1", []Row{wrow("a", "u1", 2)}, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	if got := cursorOf(t, c, "cal"); got != "c1" {
		t.Errorf("cursor = %q after a write, want c1", got)
	}
	cals, _ := c.Calendars(bg)
	if !cals[0].SyncedAt.Equal(time.Unix(100, 0)) || !cals[0].FullAt.Equal(time.Unix(100, 0)) {
		t.Errorf("a write moved SyncedAt/FullAt: %+v", cals[0])
	}
}

func TestApplyWriteToAnUnknownCalendarCreatesItLikeApplyDelta(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ApplyWrite(bg, "new", "u1", []Row{wrow("a", "u1", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if len(ids(t, c, "new")) != 1 {
		t.Error("row not stored")
	}
	if cursorOf(t, c, "new") != "" {
		t.Error("an unknown calendar got a cursor")
	}
	err := c.Snapshot(bg, "new", func(v View) error {
		if v.Floor() == 0 {
			t.Error("the created calendar has no floor")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFeedRereadOfAWrittenRowIsANoop(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{wrow("a", "u1", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyWrite(bg, "cal", "u1", []Row{wrow("a", "u1", 2), wrow("b", "u1", 1)}, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	n := seq(t, c)
	// The feed now reports the same rows, with the Raw bytes laid out differently.
	a := wrow("a", "u1", 2)
	a.Raw = json.RawMessage(`{ "V": 2,  "ID": "a" }`)
	ch, err := c.ApplyDelta(bg, "cal", "c2", []Row{a, wrow("b", "u1", 1)}, nil, time.Unix(300, 0))
	if err != nil {
		t.Fatal(err)
	}
	if ch != (Changes{}) {
		t.Errorf("feed re-read changed %+v, want nothing", ch)
	}
	if seq(t, c) != n {
		t.Errorf("seq %d -> %d: the feed re-read bumped it", n, seq(t, c))
	}
	if got := cursorOf(t, c, "cal"); got != "c2" {
		t.Errorf("cursor = %q, the feed must still advance it", got)
	}
}

func aliases(t *testing.T, c *Cache, cal string) (map[string]string, map[string]string) {
	t.Helper()
	byUID := map[string]string{}
	byName := map[string]string{}
	err := c.Snapshot(bg, cal, func(v View) error {
		var err error
		if byUID, err = v.AliasesFor(); err != nil {
			return err
		}
		for _, n := range []string{"n1.ics", "n2.ics", "n3.ics"} {
			if uid, ok, err := v.AliasOf(n); err != nil {
				return err
			} else if ok {
				byName[n] = uid
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return byUID, byName
}

func TestAliasesAreOneNamePerUID(t *testing.T) {
	c, _ := openNew(t)
	for _, a := range [][3]string{{"cal", "n1.ics", "u1"}, {"cal", "n2.ics", "u1"}, {"cal", "n3.ics", "u2"}, {"other", "n1.ics", "u9"}} {
		if err := c.PutAlias(bg, a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	byUID, byName := aliases(t, c, "cal")
	if len(byUID) != 2 || byUID["u1"] != "n2.ics" || byUID["u2"] != "n3.ics" {
		t.Errorf("AliasesFor = %v, want u1->n2.ics (the latest), u2->n3.ics", byUID)
	}
	if _, ok := byName["n1.ics"]; ok || byName["n2.ics"] != "u1" {
		t.Errorf("AliasOf = %v: the replaced name must be gone", byName)
	}
	// Another UID taking a name takes it from the first.
	if err := c.PutAlias(bg, "cal", "n2.ics", "u2"); err != nil {
		t.Fatal(err)
	}
	byUID, _ = aliases(t, c, "cal")
	if _, ok := byUID["u1"]; ok || byUID["u2"] != "n2.ics" {
		t.Errorf("AliasesFor after a name changed hands = %v", byUID)
	}
	if o, _ := aliases(t, c, "other"); o["u9"] != "n1.ics" || len(o) != 1 {
		t.Errorf("another calendar's aliases = %v", o)
	}
	for _, a := range [][3]string{{"", "n", "u"}, {"c", "", "u"}, {"c", "n", ""}} {
		if err := c.PutAlias(bg, a[0], a[1], a[2]); err == nil {
			t.Errorf("PutAlias%v accepted", a)
		}
	}
}

func TestAliasesSurviveReopenAndDieWithTheEpoch(t *testing.T) {
	c, path := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{wrow("a", "u1", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.PutAlias(bg, "cal", "n1.ics", "u1"); err != nil {
		t.Fatal(err)
	}
	// Deleting the UID keeps its alias: the tombstone is reported at that href.
	if _, err := c.ApplyWrite(bg, "cal", "u1", nil, time.Unix(150, 0)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	c = openAt(t, path, "acct-1")
	if byUID, _ := aliases(t, c, "cal"); byUID["u1"] != "n1.ics" {
		t.Fatalf("alias after reopen and deletion = %v", byUID)
	}
	// A second calendar's alias goes with DropCalendar only for that calendar.
	if err := c.PutAlias(bg, "cal2", "n2.ics", "u2"); err != nil {
		t.Fatal(err)
	}
	if err := c.DropCalendar(bg, "cal2"); err != nil {
		t.Fatal(err)
	}
	if byUID, _ := aliases(t, c, "cal2"); len(byUID) != 0 {
		t.Errorf("a dropped calendar kept aliases: %v", byUID)
	}
	if byUID, _ := aliases(t, c, "cal"); byUID["u1"] != "n1.ics" {
		t.Errorf("dropping cal2 removed cal's alias: %v", byUID)
	}
	_ = c.Close()

	// Another account: a new epoch, no alias.
	other := openAt(t, path, "acct-2")
	if byUID, _ := aliases(t, other, "cal"); len(byUID) != 0 {
		t.Errorf("aliases survived an account switch: %v", byUID)
	}
	var n int
	if err := other.db.QueryRowContext(bg, `SELECT count(*) FROM aliases`).Scan(&n); err != nil || n != 0 {
		t.Errorf("alias rows after the switch = %d, %v", n, err)
	}
}

func TestAliasesDieWithARemovedFile(t *testing.T) {
	c, path := openNew(t)
	if err := c.PutAlias(bg, "cal", "n1.ics", "u1"); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	c = openAt(t, path, "acct-1")
	if byUID, _ := aliases(t, c, "cal"); len(byUID) != 0 {
		t.Errorf("aliases of a removed file came back: %v", byUID)
	}
}

// buildM3File leaves a file as an M3 build did: version 1, no aliases table.
// way an M3 build left it, with no aliases table.
func buildM3File(t *testing.T, path string) {
	t.Helper()
	c := openAt(t, path, "acct-1")
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{wrow("a", "u1", 1), wrow("b", "u1", 2)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyDelta(bg, "cal", "c2", nil, []string{"b"}, time.Unix(110, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(bg, `DROP TABLE aliases`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(bg, `PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func TestMigratesAnM3File(t *testing.T) {
	path := Path(t.TempDir())
	buildM3File(t, path)
	info, err := Inspect(path)
	if err != nil || info.Schema != 1 || info.Account != "acct-1" {
		t.Fatalf("setup: %+v, %v; want a version 1 file", info, err)
	}

	c := openAt(t, path, "acct-1")
	if info, _ := Inspect(path); info.Schema != 2 {
		t.Errorf("schema after open = %d, want 2", info.Schema)
	}
	got := ids(t, c, "cal")
	if len(got) != 1 || got["a"].UID != "u1" {
		t.Errorf("rows after the migration = %v", got)
	}
	if ch, err := c.Changes(bg, "cal", 0); err != nil || len(ch) != 2 {
		t.Errorf("rows and tombstones after the migration = %+v, %v", ch, err)
	}
	if got := cursorOf(t, c, "cal"); got != "c2" {
		t.Errorf("cursor after the migration = %q", got)
	}
	if err := c.PutAlias(bg, "cal", "n1.ics", "u1"); err != nil {
		t.Fatalf("PutAlias on the migrated file: %v", err)
	}
	if byUID, _ := aliases(t, c, "cal"); byUID["u1"] != "n1.ics" {
		t.Errorf("alias on the migrated file = %v", byUID)
	}
	_ = c.Close()
	// And the migrated file reopens as any v2 file.
	c = openAt(t, path, "acct-1")
	if byUID, _ := aliases(t, c, "cal"); byUID["u1"] != "n1.ics" {
		t.Errorf("alias after reopening the migrated file = %v", byUID)
	}
}

// A read-only handle never upgrades: on an M3 file it reads the rows and
// reports no aliases.
func TestReadOnlyReadsAnM3File(t *testing.T) {
	path := Path(t.TempDir())
	buildM3File(t, path)
	ro, err := OpenReadOnly(path, "acct-1")
	if err != nil {
		t.Fatalf("OpenReadOnly on a version 1 file: %v", err)
	}
	defer func() { _ = ro.Close() }()
	if len(ids(t, ro, "cal")) != 1 {
		t.Error("rows of an M3 file not readable")
	}
	if byUID, byName := aliases(t, ro, "cal"); len(byUID) != 0 || len(byName) != 0 {
		t.Errorf("aliases on an M3 file: %v %v", byUID, byName)
	}
	if info, _ := Inspect(path); info.Schema != 1 {
		t.Errorf("the read-only open upgraded the file to %d", info.Schema)
	}
}

func TestAliasReadsRefuseAnEndedView(t *testing.T) {
	c, _ := openNew(t)
	var kept View
	if err := c.Snapshot(bg, "cal", func(v View) error { kept = v; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := kept.AliasOf("n1.ics"); err == nil {
		t.Error("AliasOf worked after the snapshot ended")
	}
	if _, err := kept.AliasesFor(); err == nil {
		t.Error("AliasesFor worked after the snapshot ended")
	}
}
