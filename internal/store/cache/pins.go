package cache

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Pin is a row's key packets as the cache first stored them (ruling
// M4-R30), or as pcb sent them for a row it created (M4-R36): Proton never
// changes a row's key packets (research B.5), so a refreshed row whose
// packets differ from its pin is not trusted for a write. "" stands for a
// packet the row lacks. A pin outlives its row and its calendar, and dies
// only with the epoch (M4-R35): Proton never reuses an event ID.
type Pin struct {
	Shared, Calendar, Address string
}

// PinOf reads the key packets of raw, a row as Proton served it: a field
// absent or null is "", a string its value, anything else its JSON text. A
// row that does not decode has none.
func PinOf(raw json.RawMessage) Pin {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	field := func(name string) string {
		v := bytes.TrimSpace(m[name])
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		if len(v) == 0 || string(v) == "null" {
			return ""
		}
		return string(v)
	}
	return Pin{Shared: field("SharedKeyPacket"), Calendar: field("CalendarKeyPacket"), Address: field("AddressKeyPacket")}
}

// SamePacket reports whether two spellings of a key packet are the same
// packet: equal once decoded from base64, whitespace and padding aside
// (Proton may wrap or re-pad what it echoes); compared as written when
// either does not decode.
func SamePacket(a, b string) bool {
	da, okA := decodePacket(a)
	db, okB := decodePacket(b)
	if okA && okB {
		return bytes.Equal(da, db)
	}
	return a == b
}

func decodePacket(s string) ([]byte, bool) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	return b, err == nil
}

// Same reports whether p and q hold the same packets (SamePacket).
func (p Pin) Same(q Pin) bool {
	return SamePacket(p.Shared, q.Shared) && SamePacket(p.Calendar, q.Calendar) && SamePacket(p.Address, q.Address)
}

// pin records r's key packets unless its row already has a pin: the first
// seen wins.
func (w *writer) pin(r Row) error {
	return w.pinAs(r.ID, PinOf(r.Raw))
}

// pinAs records p as row id's pin unless it already has one.
func (w *writer) pinAs(id string, p Pin) error {
	_, err := w.tx.ExecContext(w.ctx, `INSERT OR IGNORE INTO pins (calendar_id, id, shared, calendar, address) VALUES (?, ?, ?, ?, ?)`,
		w.cal, id, p.Shared, p.Calendar, p.Address)
	return err
}

// PinRows pins the rows of pins (row ID to packets) that have no pin yet,
// whether or not they are stored: a create's row, by the ID Proton
// answered, with the packets pcb sent, before its read-back (ruling
// M4-R37).
func (c *Cache) PinRows(ctx context.Context, calID string, pins map[string]Pin) error {
	if len(pins) == 0 {
		return nil
	}
	return c.classify(c.tx(ctx, func(tx *sql.Tx) error {
		w, err := c.begin(ctx, tx, calID)
		if err != nil {
			return err
		}
		for id, p := range pins {
			if err := w.pinAs(id, p); err != nil {
				return err
			}
		}
		return nil
	}))
}

// hasTable reports whether the file has table name.
func (c *Cache) hasTable(ctx context.Context, name string) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0, err
}

// backfillPins creates the pins table and pins every stored row that has
// none (an upgrade: the stored rows are the first seen).
func backfillPins(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV2); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT calendar_id, id, raw FROM events`)
	if err != nil {
		return err
	}
	type stored struct {
		cal, id string
		pin     Pin
	}
	var all []stored
	for rows.Next() {
		var s stored
		var raw []byte
		if err := rows.Scan(&s.cal, &s.id, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		s.pin = PinOf(raw)
		all = append(all, s)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range all {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pins (calendar_id, id, shared, calendar, address) VALUES (?, ?, ?, ?, ?)`,
			s.cal, s.id, s.pin.Shared, s.pin.Calendar, s.pin.Address); err != nil {
			return err
		}
	}
	return nil
}

// Pins implements View.
func (v *view) Pins(ids []string) (map[string]Pin, error) {
	if v.ended {
		return nil, errEnded
	}
	out := map[string]Pin{}
	if v.c.noPins || v.c.noAliases || len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, v.cal)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := v.tx.QueryContext(v.ctx, `SELECT id, shared, calendar, address FROM pins WHERE calendar_id = ? AND id IN (?`+
		strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, v.c.classify(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var p Pin
		if err := rows.Scan(&id, &p.Shared, &p.Calendar, &p.Address); err != nil {
			return nil, v.c.classify(err)
		}
		out[id] = p
	}
	return out, v.c.classify(rows.Err())
}
