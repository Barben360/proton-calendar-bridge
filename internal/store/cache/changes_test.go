package cache

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// urow is a row of UID uid; rec != 0 makes it an exception of that recurrence.
func urow(id, uid string, rec int64, v int) Row {
	r := row(id, v)
	r.UID = uid
	if rec != 0 {
		r.RecurrenceID = &rec
	}
	return r
}

func groupIDs(gs []Listed) map[string][]string {
	m := map[string][]string{}
	for _, g := range gs {
		for _, r := range g.Rows {
			m[g.UID] = append(m[g.UID], r.ID)
		}
	}
	return m
}

func snap(t *testing.T, c *Cache, cal string, fn func(View)) {
	t.Helper()
	if err := c.Snapshot(bg, cal, func(v View) error { fn(v); return nil }); err != nil {
		t.Fatal(err)
	}
}

// Fails if Snapshot used the immediate-lock handle (the writer commit would
// block then fail) or read outside one transaction (the second read would
// see the commit).
func TestSnapshotIsConsistent(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{urow("m", "u1", 0, 1), urow("x", "u1", 100, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		before, err := v.Listing()
		if err != nil {
			t.Fatal(err)
		}
		seq0 := v.MaxSeq()
		if _, err := c.ApplyDelta(bg, "cal", "c2", []Row{urow("n", "u2", 0, 1), urow("m", "u1", 0, 2)}, []string{"x"}, time.Unix(2, 0)); err != nil {
			t.Fatalf("writer commit during a snapshot: %v", err)
		}
		after, err := v.Listing()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("snapshot changed under a commit:\nbefore %+v\nafter  %+v", before, after)
		}
		if _, ok, _ := v.Group("u2"); ok {
			t.Error("Group saw a row committed after the snapshot began")
		}
		if v.MaxSeq() != seq0 {
			t.Errorf("MaxSeq moved")
		}
		if ch, del, err := v.ChangedSince(0); err != nil || len(del) != 0 || len(ch) != 1 {
			t.Errorf("ChangedSince saw the commit: %+v %v %v", ch, del, err)
		}
	})
	snap(t, c, "cal", func(v View) {
		if g := groupIDs(mustGroups(t, v)); len(g) != 2 || len(g["u1"]) != 1 {
			t.Errorf("a new snapshot sees %v, want the committed state", g)
		}
	})
}

// A write transaction holding the write lock does not block a Snapshot, which
// sees the committed state only.
func TestSnapshotNotBlockedByAWriteTransaction(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{urow("m", "u1", 0, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	tx, err := c.db.BeginTx(bg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(bg, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		if g := mustGroups(t, v); len(g) != 1 {
			t.Errorf("snapshot saw %d groups during an uncommitted write, want 1", len(g))
		}
	})
}

func mustGroups(t *testing.T, v View) []Listed {
	t.Helper()
	g, err := v.Listing()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Final review, minor 1: a listing carries each UID's rows (master, then by
// recurrence ID, then by ID) with their IDs and ETags, as Group has them,
// and no payload (Listed has no Raw: a listing of 10k events does not load
// their ciphertext).
func TestListingIsGroupsWithoutPayload(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{
		urow("x2", "u1", 200, 1), urow("m", "u1", 0, 1), urow("x1", "u1", 100, 3), urow("o", "u2", 0, 2),
	}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		list := mustGroups(t, v)
		if len(list) != 2 || list[0].UID != "u1" || list[1].UID != "u2" {
			t.Fatalf("listing = %+v, want u1 then u2", list)
		}
		for _, l := range list {
			g, ok, err := v.Group(l.UID)
			if err != nil || !ok {
				t.Fatalf("Group(%s): %v %v", l.UID, ok, err)
			}
			if want := g.Listed(); !reflect.DeepEqual(l, want) {
				t.Errorf("listing of %s = %+v, want %+v (Group's IDs and ETags, in its order)", l.UID, l, want)
			}
			for _, r := range l.Rows {
				if r.ID == "" || r.ETag == "" || r.ID == r.ETag {
					t.Errorf("%s: row %+v", l.UID, r)
				}
			}
		}
	})
}

func TestChangedSinceGroupsByUID(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{
		urow("m", "u1", 0, 1), urow("x2", "u1", 200, 1), urow("x1", "u1", 100, 1), urow("o", "u2", 0, 1),
	}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	since := seq(t, c)
	if _, err := c.ApplyDelta(bg, "cal", "c2", []Row{urow("x1", "u1", 100, 2)}, nil, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		ch, del, err := v.ChangedSince(since)
		if err != nil || len(del) != 0 {
			t.Fatalf("changed %+v deleted %v err %v", ch, del, err)
		}
		if len(ch) != 1 || ch[0].UID != "u1" {
			t.Fatalf("changed = %+v, want only u1", ch)
		}
		var got []string
		for _, r := range ch[0].Rows {
			got = append(got, r.ID)
		}
		if want := []string{"m", "x1", "x2"}; !reflect.DeepEqual(got, want) { // master, then by recurrence ID
			t.Errorf("rows = %v, want the whole UID %v", got, want)
		}
		if g, ok, err := v.Group("u2"); err != nil || !ok || len(g.Rows) != 1 || g.Rows[0].ETag == "" || len(g.Rows[0].Raw) == 0 {
			t.Errorf("Group(u2) = %+v %v %v", g, ok, err)
		}
		if _, ok, _ := v.Group("nope"); ok {
			t.Error("Group of an unknown UID found")
		}
	})
}

// Final review, minor 3: a stored row carries its plaintext RRULE, which a
// placeholder master needs.
func TestGroupCarriesTheRRule(t *testing.T) {
	c, _ := openNew(t)
	m := urow("m", "u1", 0, 1)
	m.RRule = "FREQ=WEEKLY;BYDAY=FR"
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{m, urow("x", "u1", 100, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		g, ok, err := v.Group("u1")
		if err != nil || !ok || len(g.Rows) != 2 || g.Rows[0].RRule != "FREQ=WEEKLY;BYDAY=FR" || g.Rows[1].RRule != "" {
			t.Errorf("Group(u1) = %+v %v %v; want the master's RRULE only", g, ok, err)
		}
	})
}

func TestDeletedUIDOnlyWhenNoLiveRow(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{
		urow("m", "u1", 0, 1), urow("x", "u1", 100, 1), urow("a", "u2", 0, 1), urow("b", "u3", 0, 1),
	}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	since := seq(t, c)
	// u1 loses an exception (master lives), u2 loses its only row, u3 is replaced under a new row ID.
	if _, err := c.ApplyDelta(bg, "cal", "c2", []Row{urow("b2", "u3", 0, 1)}, []string{"x", "a", "b"}, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		ch, del, err := v.ChangedSince(since)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(del, []string{"u2"}) {
			t.Errorf("deleted = %v, want only u2", del)
		}
		// u1 lost an exception while its master lives: changed, with the master.
		if got, want := groupIDs(ch), map[string][]string{"u1": {"m"}, "u3": {"b2"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("changed = %v, want %v", got, want)
		}
	})
}

// A deleted exception is a change of its UID's object: u1 must be in changed.
func TestDeletedExceptionChangesItsUID(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{urow("m", "u1", 0, 1), urow("x", "u1", 100, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	since := seq(t, c)
	if _, err := c.ApplyDelta(bg, "cal", "c2", nil, []string{"x"}, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		ch, del, err := v.ChangedSince(since)
		if err != nil {
			t.Fatal(err)
		}
		if len(del) != 0 {
			t.Errorf("deleted = %v, want none (the master lives)", del)
		}
		if got := groupIDs(ch); !reflect.DeepEqual(got, map[string][]string{"u1": {"m"}}) {
			t.Errorf("changed = %v, want u1 with its master", got)
		}
	})
}

func TestMaxSeqPerCalendar(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "a", "c", []Row{urow("a1", "ua", 0, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceCalendar(bg, "b", "c", []Row{urow("b1", "ub", 0, 1), urow("b2", "ub2", 0, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "a", func(v View) {
		if v.MaxSeq() != 1 {
			t.Errorf("MaxSeq(a) = %d, want 1 (not the global 3)", v.MaxSeq())
		}
		if e, err := c.Epoch(bg); err != nil || v.Epoch() != e {
			t.Errorf("Epoch = %q, want %q (%v)", v.Epoch(), e, err)
		}
	})
	if _, err := c.ApplyDelta(bg, "a", "c", nil, []string{"a1"}, time.Unix(2, 0)); err != nil { // tombstone counts
		t.Fatal(err)
	}
	snap(t, c, "a", func(v View) {
		if v.MaxSeq() != 4 {
			t.Errorf("MaxSeq(a) after a tombstone = %d, want 4", v.MaxSeq())
		}
	})
	snap(t, c, "empty", func(v View) {
		if v.MaxSeq() != 0 || v.MinTombstoneSeq() != 0 {
			t.Errorf("unknown calendar: %d %d, want 0 0", v.MaxSeq(), v.MinTombstoneSeq())
		}
	})
}

func TestMinTombstoneSeq(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c", []Row{urow("e1", "u1", 0, 1), urow("e2", "u2", 0, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceCalendar(bg, "other", "c", []Row{urow("o1", "uo", 0, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		if v.MinTombstoneSeq() != 0 {
			t.Errorf("no tombstone: MinTombstoneSeq = %d, want 0", v.MinTombstoneSeq())
		}
	})
	if _, err := c.ApplyDelta(bg, "other", "c", nil, []string{"o1"}, time.Unix(2, 0)); err != nil { // seq 4
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		if v.MinTombstoneSeq() != 0 {
			t.Errorf("another calendar's tombstone leaked: %d", v.MinTombstoneSeq())
		}
	})
	if _, err := c.ApplyDelta(bg, "cal", "c", nil, []string{"e1"}, time.Unix(3, 0)); err != nil { // seq 5
		t.Fatal(err)
	}
	if _, err := c.ApplyDelta(bg, "cal", "c", nil, []string{"e2"}, time.Unix(4, 0)); err != nil { // seq 6
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		if v.MinTombstoneSeq() != 5 {
			t.Errorf("MinTombstoneSeq = %d, want 5", v.MinTombstoneSeq())
		}
	})
}

func TestSnapshotOnReadOnlyCacheAndCallbackErrors(t *testing.T) {
	c, path := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c", []Row{urow("e1", "u1", 0, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path, "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	snap(t, ro, "cal", func(v View) {
		if len(mustGroups(t, v)) != 1 {
			t.Error("read-only snapshot misses the row")
		}
	})
	if err := c.Snapshot(bg, "", func(View) error { return nil }); err == nil {
		t.Error("empty calendar ID accepted")
	}
	wantErr := errors.New("callback failed")
	if err := c.Snapshot(bg, "cal", func(View) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Errorf("callback error = %v, want it returned as is", err)
	}
	// The failed callback's transaction was rolled back: a write is not blocked.
	if _, err := c.ApplyDelta(bg, "cal", "c2", []Row{urow("e2", "u2", 0, 1)}, nil, time.Unix(2, 0)); err != nil {
		t.Errorf("write after a failed snapshot: %v", err)
	}
}

func TestViewAfterSnapshotEnds(t *testing.T) {
	c, _ := openNew(t)
	var kept View
	snap(t, c, "cal", func(v View) { kept = v })
	if _, err := kept.Listing(); !errors.Is(err, errEnded) {
		t.Errorf("Groups after the snapshot = %v, want errEnded", err)
	}
	if _, _, err := kept.ChangedSince(0); !errors.Is(err, errEnded) {
		t.Errorf("ChangedSince after the snapshot = %v, want errEnded", err)
	}
}

func TestUnnamedTombstoneSeq(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c", []Row{urow("e1", "u1", 0, 1)}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		if v.UnnamedTombstoneSeq() != 0 {
			t.Errorf("no unnamed tombstone: %d, want 0", v.UnnamedTombstoneSeq())
		}
	})
	// A tombstone as an early M2 build wrote it: no UID.
	if _, err := c.db.ExecContext(bg, `INSERT INTO tombstones (calendar_id, id, uid, seq) VALUES ('cal', 'old', '', 7)`); err != nil {
		t.Fatal(err)
	}
	snap(t, c, "cal", func(v View) {
		if v.UnnamedTombstoneSeq() != 7 {
			t.Errorf("UnnamedTombstoneSeq = %d, want 7", v.UnnamedTombstoneSeq())
		}
		if _, del, err := v.ChangedSince(0); err != nil || len(del) != 0 {
			t.Errorf("an unnamed tombstone must not be named: %v %v", del, err)
		}
	})
}

func TestEventsUIDIndexExistsAlsoOnOldFiles(t *testing.T) {
	c, path := openNew(t)
	has := func(c *Cache) bool {
		var n int
		if err := c.db.QueryRowContext(bg, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='events_uid'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !has(c) {
		t.Fatal("new file lacks events_uid")
	}
	if _, err := c.db.ExecContext(bg, `DROP INDEX events_uid`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c2 := openAt(t, path, "acct-1")
	if !has(c2) {
		t.Error("opening an existing file did not add events_uid")
	}
}

// Ruling M3-R12: a calendar row records, when it is created, a floor past
// every counter value already used, so a calendar dropped and created again
// under the same ID never answers a position of its former life.
func TestFloorIsSetWhenTheCalendarIsCreated(t *testing.T) {
	c, _ := openNew(t)
	if err := c.PutCalendar(bg, Calendar{ID: "cal", Name: "Work"}); err != nil {
		t.Fatal(err)
	}
	var floor0 int64
	snap(t, c, "cal", func(v View) { floor0 = v.Floor() })
	if got := seq(t, c); got != floor0 || got == 0 {
		t.Errorf("Seq after a calendar creation = %d, want the floor it took (%d)", got, floor0)
	}
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{urow("e1", "u1", 0, 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.PutCalendar(bg, Calendar{ID: "cal", Name: "Renamed"}); err != nil {
		t.Fatal(err)
	}
	var before, floor1 int64
	snap(t, c, "cal", func(v View) { before, floor1 = v.MaxSeq(), v.Floor() })
	if floor1 != floor0 || floor0 >= before {
		t.Errorf("floor %d then %d (max seq %d): an update must keep the floor set at creation, below the rows", floor0, floor1, before)
	}
	if err := c.DropCalendar(bg, "cal"); err != nil {
		t.Fatal(err)
	}
	if err := c.PutCalendar(bg, Calendar{ID: "cal", Name: "Work"}); err != nil {
		t.Fatal(err)
	}
	var again int64
	snap(t, c, "cal", func(v View) { again = v.Floor() })
	if again <= before {
		t.Errorf("floor after re-creation = %d, want above %d (every value of the former life)", again, before)
	}
	// A calendar first created by a write (no PutCalendar) gets one too.
	if _, err := c.ReplaceCalendar(bg, "new", "c1", []Row{urow("n1", "u1", 0, 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	var nf, nmax int64
	snap(t, c, "new", func(v View) { nf, nmax = v.Floor(), v.MaxSeq() })
	if nf <= again || nf > nmax {
		t.Errorf("floor of a calendar created by its first write = %d, want above %d and at most its row's %d", nf, again, nmax)
	}
}
