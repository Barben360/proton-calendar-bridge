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

// A pin outlives its row, whichever path deletes it (ruling M4-R35):
// Proton never reuses an event ID, so a row hidden once and served again
// keeps its first packets as its pin.
func TestPinsOutliveTheirRow(t *testing.T) {
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
	if _, err := c.ApplyWrite(bg, "cal", "u1", nil, time.Unix(130, 0)); err != nil {
		t.Fatal(err)
	}
	want := map[string]Pin{"a": {Shared: "S1"}, "b": {Shared: "SB"}, "x": {Shared: "SX"}}
	got := pins(t, c, "cal", "a", "b", "x")
	for id, p := range want {
		if got[id] != p {
			t.Errorf("pin of %s after its row's deletion = %+v, want %+v", id, got[id], p)
		}
	}
	// Served again with other packets: the first stay.
	if _, err := c.ApplyDelta(bg, "cal", "c4", []Row{prow("a", "S2", "C2", "", 2)}, nil, time.Unix(140, 0)); err != nil {
		t.Fatal(err)
	}
	if p := pins(t, c, "cal", "a")["a"]; p != want["a"] {
		t.Errorf("pin of a row served again = %+v, want %+v", p, want["a"])
	}
}

// Pins outlive their calendar's drop, and die with a new epoch (another
// account).
func TestPinsDieWithTheEpochOnly(t *testing.T) {
	c, path := openNew(t)
	for i, cal := range []string{"cal", "cal2"} {
		if _, err := c.ReplaceCalendar(bg, cal, "c1", []Row{prow("a", "S"+strconv.Itoa(i), "", "", 1)}, time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DropCalendar(bg, "cal2"); err != nil {
		t.Fatal(err)
	}
	if p := pins(t, c, "cal2", "a")["a"]; p.Shared != "S1" {
		t.Errorf("a dropped calendar's pin = %+v, want S1", p)
	}
	if _, err := c.ReplaceCalendar(bg, "cal2", "c2", []Row{prow("a", "S9", "", "", 2)}, time.Unix(110, 0)); err != nil {
		t.Fatal(err)
	}
	if p := pins(t, c, "cal2", "a")["a"]; p.Shared != "S1" {
		t.Errorf("the pin of a calendar listed again = %+v, want S1", p)
	}
	if p := pins(t, c, "cal", "a")["a"]; p.Shared != "S0" {
		t.Errorf("cal's pin = %+v, want S0", p)
	}
	_ = c.Close()
	other := openAt(t, path, "acct-2")
	var n int
	if err := other.db.QueryRowContext(bg, `SELECT count(*) FROM pins`).Scan(&n); err != nil || n != 0 {
		t.Errorf("pins after an account switch = %d, %v", n, err)
	}
}

// A write pins the rows it created with the packets it sent, not the ones
// echoed (ruling M4-R36); a row already pinned keeps its pin.
func TestApplyWritePinnedPinsWhatWasSent(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{prow("a", "S1", "", "", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	sent := map[string]Pin{"a": {Shared: "SENT-A"}, "n": {Shared: "SENT-N", Calendar: "SENT-C"}}
	if _, err := c.ApplyWritePinned(bg, "cal", "u1", []Row{prow("a", "S1", "", "", 2), prow("n", "ECHO", "ECHO-C", "", 1)}, sent, time.Unix(110, 0)); err != nil {
		t.Fatal(err)
	}
	got := pins(t, c, "cal", "a", "n")
	if got["a"] != (Pin{Shared: "S1"}) {
		t.Errorf("an already pinned row: %+v", got["a"])
	}
	if got["n"] != sent["n"] {
		t.Errorf("a created row's pin = %+v, want what was sent %+v", got["n"], sent["n"])
	}
}

func TestSamePacket(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{"wcBMA0", "wcBMA0", true},
		{"AAECAwQ=", "AAEC\r\nAwQ", true},
		{"AAECAwQ=", "AAECAwU=", false},
		{"", "AAECAwQ=", false},
		{"not base64!", "not base64!", true},
		{"not base64!", "not base64?", false},
	} {
		if got := SamePacket(c.a, c.b); got != c.want {
			t.Errorf("SamePacket(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if !(Pin{Shared: "AAECAwQ="}).Same(Pin{Shared: "AAECAwQ"}) || (Pin{Address: "AAECAwQ="}).Same(Pin{}) {
		t.Error("Pin.Same compares the wrong fields")
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

// A Sync answer may name any row ID: pinning by it must never replace the pin
// of a row already seen (first seen wins, ruling M4-R30/R37).
func TestPinRowsNeverReplacesAPin(t *testing.T) {
	c, _ := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{prow("a", "S1", "C1", "", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.PinRows(bg, "cal", map[string]Pin{"a": {Shared: "EVIL", Calendar: "EVIL-C"}, "n": {Shared: "SENT-N"}}); err != nil {
		t.Fatal(err)
	}
	got := pins(t, c, "cal", "a", "n")
	if got["a"] != (Pin{Shared: "S1", Calendar: "C1"}) {
		t.Errorf("an existing pin was replaced: %+v", got["a"])
	}
	if got["n"] != (Pin{Shared: "SENT-N"}) {
		t.Errorf("a new ID's pin = %+v, want the given one", got["n"])
	}
}
