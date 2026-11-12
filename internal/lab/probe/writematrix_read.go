package probe

// The write matrix's read-back: each row of a UID as structural facts,
// compared with what was sent (never the text).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
)

// readRow is a row read back, by its structural facts.
type readRow struct {
	id, keyPacket string
	start, end    int64
	startTZ       string
	rrule         *string
	recurrence    bool
	notifications json.RawMessage
	personal      int
	open          *cards.Event
	parts         map[cards.Kind][]byte
	openErr       error
}

// readBack lists the rows of uid (exactly that UID) and opens each.
func (m *matrix) readBack(ctx context.Context, uid string) ([]readRow, error) {
	rows, err := events.ListByUID(ctx, m.c, m.calID, uid)
	if err != nil {
		return nil, err
	}
	var out []readRow
	for _, r := range rows {
		if r.UID != uid {
			continue
		}
		var w struct {
			SharedKeyPacket string
			StartTimezone   string
			RRule           *string
			RecurrenceID    *int64
			Notifications   json.RawMessage
			PersonalEvents  []json.RawMessage
		}
		_ = json.Unmarshal(r.Raw, &w)
		rr := readRow{
			id: r.ID, keyPacket: w.SharedKeyPacket, start: r.Start, end: r.End, startTZ: w.StartTimezone,
			rrule: w.RRule, recurrence: w.RecurrenceID != nil, notifications: compact(w.Notifications), personal: len(w.PersonalEvents),
		}
		ev, err := cards.Open(r.Raw, m.calKR, m.addrs, m.own)
		if err != nil {
			rr.openErr = err
		} else {
			rr.open = &ev
			rr.parts = map[cards.Kind][]byte{}
			for _, p := range ev.Parts {
				if p.Kind != cards.Personal {
					rr.parts[p.Kind] = p.Text
				}
			}
		}
		out = append(out, rr)
	}
	return out, nil
}

func compact(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("absent")
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

const asSent = "parts as sent"

// partsFact compares r's parts with sent, by kind: the text is never shown.
func partsFact(r readRow, sent map[ical.PartKind][]byte) string {
	if r.openErr != nil {
		return "parts do not open with the row's keys"
	}
	var diff []string
	for k := ical.SharedSigned; k <= ical.Attendees; k++ {
		got, have := r.parts[cards.Kind(k)]
		want, sentIt := sent[k]
		switch {
		case have && !sentIt:
			diff = append(diff, "extra "+k.String())
		case sentIt && !have:
			diff = append(diff, "no "+k.String())
		case sentIt && !bytes.Equal(got, want):
			diff = append(diff, k.String())
		}
	}
	if len(diff) == 0 {
		return asSent
	}
	return "parts differ: " + strings.Join(diff, ", ")
}

// partLine is the value of the first name: line of r's part k, or "absent".
func partLine(r readRow, k ical.PartKind, name string) string {
	for _, l := range strings.Split(string(r.parts[cards.Kind(k)]), "\r\n") {
		if v, ok := strings.CutPrefix(l, name+":"); ok {
			return v
		}
		if strings.HasPrefix(l, name+";") {
			return l[len(name):]
		}
	}
	return "absent"
}

func utc(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

// readFailed is the read-back fact of a failed listing.
func readFailed(err error) string { return "listing failed: " + answerOf(err).String() }

// one reads uid back expecting one openable row for a follow-up write; it
// returns the row, or the read-back fact that stops the case.
func (m *matrix) one(ctx context.Context, uid string) (readRow, string) {
	rows, err := m.readBack(ctx, uid)
	switch {
	case err != nil:
		return readRow{}, readFailed(err)
	case len(rows) != 1:
		return readRow{}, fmt.Sprintf("%d row(s)", len(rows))
	case rows[0].openErr != nil:
		return readRow{}, "1 row, parts do not open with the row's keys"
	}
	return rows[0], ""
}
