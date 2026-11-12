package cache

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// prow is row id of UID u1 at version v, with key packets shared and
// calendar (and an AddressKeyPacket when address is set).
func prow(id, shared, calendar, address string, v int) Row {
	m := map[string]any{"ID": id, "V": v, "SharedKeyPacket": shared, "CalendarKeyPacket": nil, "AddressKeyPacket": nil}
	if calendar != "" {
		m["CalendarKeyPacket"] = calendar
	}
	if address != "" {
		m["AddressKeyPacket"] = address
	}
	raw, _ := json.Marshal(m)
	return Row{ID: id, UID: "u1", Start: 1731000000, End: 1731003600, LastEditTime: int64(v), Raw: raw}
}

func pins(t *testing.T, c *Cache, cal string, ids ...string) map[string]Pin {
	t.Helper()
	var out map[string]Pin
	if err := c.Snapshot(bg, cal, func(v View) error {
		var err error
		out, err = v.Pins(ids)
		return err
	}); err != nil {
		t.Fatalf("Pins: %v", err)
	}
	return out
}

func TestPinOf(t *testing.T) {
	for raw, want := range map[string]Pin{
		`{"SharedKeyPacket":"s","CalendarKeyPacket":"c","AddressKeyPacket":"a"}`: {"s", "c", "a"},
		`{"SharedKeyPacket":null,"CalendarKeyPacket":""}`:                        {},
		`{}`:                           {},
		`{"AddressKeyPacket":{"k":1}}`: {Address: `{"k":1}`},
		`not json`:                     {},
	} {
		if got := PinOf(json.RawMessage(raw)); got != want {
			t.Errorf("PinOf(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// The first packets stored for a row are its pin, whichever path stores it
// later (ruling M4-R30); a row first seen through any path is pinned.
func TestPinsKeepTheFirstPacketsSeen(t *testing.T) {
	c, path := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{prow("a", "S1", "C1", "", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyDelta(bg, "cal", "c2", []Row{prow("a", "S2", "C2", "A2", 2), prow("b", "SB", "", "", 1)}, nil, time.Unix(110, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceCalendar(bg, "cal", "c3", []Row{prow("a", "S3", "C3", "", 3), prow("b", "SB2", "", "", 2)}, time.Unix(120, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyWrite(bg, "cal", "u1", []Row{prow("a", "S4", "C4", "", 4), prow("b", "SB3", "", "", 3), prow("w", "SW", "CW", "", 1)}, time.Unix(130, 0)); err != nil {
		t.Fatal(err)
	}
	want := map[string]Pin{"a": {"S1", "C1", ""}, "b": {"SB", "", ""}, "w": {"SW", "CW", ""}}
	got := pins(t, c, "cal", "a", "b", "w", "nope")
	if len(got) != len(want) {
		t.Fatalf("pins %+v, want %+v", got, want)
	}
	for id, p := range want {
		if got[id] != p {
			t.Errorf("pin of %s = %+v, want %+v", id, got[id], p)
		}
	}
	// A pin survives a reopen.
	_ = c.Close()
	c = openAt(t, path, "acct-1")
	if p := pins(t, c, "cal", "a")["a"]; p != want["a"] {
		t.Errorf("pin of a after a reopen = %+v", p)
	}
	if got := pins(t, c, "other", "a"); len(got) != 0 {
		t.Errorf("another calendar's pins: %+v", got)
	}
}

// A pin dies with its row, whichever path deletes it: the row first seen
// again afterwards is pinned anew.
func TestPinsDieWithTheirRow(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{prow("a", "S1", "", "", 1), prow("b", "SB", "", "", 1), prow("x", "SX", "", "", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyDelta(bg, "cal", "c2", nil, []string{"a"}, time.Unix(110, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceCalendar(bg, "cal", "c3", []Row{prow("x", "SX", "", "", 1)}, time.Unix(120, 0)); err != nil {
		t.Fatal(err)
	}
	if got := pins(t, c, "cal", "a", "b", "x"); len(got) != 1 || got["x"].Shared != "SX" {
		t.Fatalf("pins after deletions %+v, want x only", got)
	}
	if _, err := c.ApplyWrite(bg, "cal", "u1", nil, time.Unix(130, 0)); err != nil {
		t.Fatal(err)
	}
	if got := pins(t, c, "cal", "x"); len(got) != 0 {
		t.Fatalf("pin of a row ApplyWrite deleted: %+v", got)
	}
	if _, err := c.ApplyDelta(bg, "cal", "c4", []Row{prow("a", "S2", "", "", 2)}, nil, time.Unix(140, 0)); err != nil {
		t.Fatal(err)
	}
	if p := pins(t, c, "cal", "a")["a"]; p.Shared != "S2" {
		t.Errorf("pin of a row seen again after its deletion = %+v", p)
	}
}

// Pins die with their calendar and with a new epoch (another account).
func TestPinsDieWithTheCalendarAndTheEpoch(t *testing.T) {
	c, path := openNew(t)
	for i, cal := range []string{"cal", "cal2"} {
		if _, err := c.ReplaceCalendar(bg, cal, "c1", []Row{prow("a", "S"+strconv.Itoa(i), "", "", 1)}, time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DropCalendar(bg, "cal2"); err != nil {
		t.Fatal(err)
	}
	if got := pins(t, c, "cal2", "a"); len(got) != 0 {
		t.Errorf("a dropped calendar kept its pins: %+v", got)
	}
	if p := pins(t, c, "cal", "a")["a"]; p.Shared != "S0" {
		t.Errorf("dropping cal2 removed cal's pin: %+v", p)
	}
	_ = c.Close()
	other := openAt(t, path, "acct-2")
	var n int
	if err := other.db.QueryRowContext(bg, `SELECT count(*) FROM pins`).Scan(&n); err != nil || n != 0 {
		t.Errorf("pins after an account switch = %d, %v", n, err)
	}
}

// The upgrade creates the table and pins the stored rows: a version 1 file
// and a version 2 file written before the pins existed.
func TestUpgradePinsTheStoredRows(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run("version "+strconv.Itoa(version), func(t *testing.T) {
			path := Path(t.TempDir())
			c := openAt(t, path, "acct-1")
			if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{prow("a", "S1", "C1", "", 1)}, time.Unix(100, 0)); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{`DROP TABLE pins`, `PRAGMA user_version = ` + strconv.Itoa(version)} {
				if _, err := c.db.ExecContext(bg, q); err != nil {
					t.Fatal(err)
				}
			}
			if version == 1 {
				if _, err := c.db.ExecContext(bg, `DROP TABLE aliases`); err != nil {
					t.Fatal(err)
				}
			}
			_ = c.Close()

			ro, err := OpenReadOnly(path, "acct-1")
			if err != nil {
				t.Fatal(err)
			}
			if got := pins(t, ro, "cal", "a"); len(got) != 0 {
				t.Errorf("read-only pins of a file without the table: %+v", got)
			}
			_ = ro.Close()

			c = openAt(t, path, "acct-1")
			if ok, err := c.hasTable(bg, "pins"); err != nil || !ok {
				t.Fatalf("pins table after the upgrade: %v, %v", ok, err)
			}
			if p := pins(t, c, "cal", "a")["a"]; p != (Pin{"S1", "C1", ""}) {
				t.Errorf("pin of a stored row after the upgrade = %+v", p)
			}
			if _, err := c.ApplyDelta(bg, "cal", "c2", []Row{prow("a", "S2", "C2", "", 2)}, nil, time.Unix(110, 0)); err != nil {
				t.Fatal(err)
			}
			if p := pins(t, c, "cal", "a")["a"]; p.Shared != "S1" {
				t.Errorf("the upgrade's pin was replaced: %+v", p)
			}
		})
	}
}
