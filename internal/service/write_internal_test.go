package service

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
)

// holds is what confirms a write: the executor's tests reach it through the
// fake, where every edit re-seals its parts, so the parts alone already
// differ; here each field is checked alone.
func TestHoldsComparesEachFieldSent(t *testing.T) {
	packet := []byte{0xd2, 0x01, 0x02, 0x03}
	parts := events.PartList{{Type: events.PartSigned, Data: "BEGIN:VCALENDAR\r\nEND:VCALENDAR"},
		{Type: events.PartEncrypted, Data: base64.StdEncoding.EncodeToString(packet)}}
	row := func(fields map[string]any) events.Row {
		stored := []map[string]any{{"Type": 2, "Data": parts[0].Data}, {"Type": 3, "Data": base64.StdEncoding.EncodeToString(packet)}}
		m := map[string]any{"SharedEvents": stored, "CalendarEvents": []any{}, "AttendeesEvents": []any{}, "Notifications": nil, "Color": nil}
		for k, v := range fields {
			m[k] = v
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return events.Row{ID: "r", Raw: raw}
	}
	list := events.NotificationList{{Type: 1, Trigger: "-PT15M"}, {Type: 0, Trigger: "-PT1H"}}
	body := func(n events.Optional[events.NotificationList], c events.Optional[string]) events.Body {
		return events.Body{WireEvent: events.WireEvent{SharedEventContent: parts}, Notifications: n, Color: c}
	}
	reversed := []map[string]any{{"Type": 0, "Trigger": "-PT1H"}, {"Type": 1, "Trigger": "-PT15M"}}
	for _, c := range []struct {
		name string
		row  events.Row
		body events.Body
		want bool
	}{
		{"same", row(nil), body(events.Null[events.NotificationList](), events.Null[string]()), true},
		{"notifications absent: not checked", row(map[string]any{"Notifications": reversed}), body(events.Optional[events.NotificationList]{}, events.Optional[string]{}), true},
		{"null sent, a list stored", row(map[string]any{"Notifications": reversed}), body(events.Null[events.NotificationList](), events.Null[string]()), false},
		{"a list sent, null stored", row(nil), body(events.Value(list), events.Null[string]()), false},
		{"a list sent, [] stored", row(map[string]any{"Notifications": []any{}}), body(events.Value(list), events.Null[string]()), false},
		{"[] sent, null stored", row(nil), body(events.Value(events.NotificationList{}), events.Null[string]()), false},
		{"null sent, [] stored", row(map[string]any{"Notifications": []any{}}), body(events.Null[events.NotificationList](), events.Null[string]()), false},
		{"the list stored in another order", row(map[string]any{"Notifications": reversed}), body(events.Value(list), events.Null[string]()), true},
		{"another list stored", row(map[string]any{"Notifications": reversed[:1]}), body(events.Value(list), events.Null[string]()), false},
		{"colour sent and stored", row(map[string]any{"Color": "#3cbb3a"}), body(events.Null[events.NotificationList](), events.Value("#3CBB3A")), true},
		{"colour sent, another stored", row(map[string]any{"Color": "#8080FF"}), body(events.Null[events.NotificationList](), events.Value("#3CBB3A")), false},
		{"colour sent, none stored", row(nil), body(events.Null[events.NotificationList](), events.Value("#3CBB3A")), false},
		{"null colour: the row's kept", row(map[string]any{"Color": "#8080FF"}), body(events.Null[events.NotificationList](), events.Null[string]()), true},
		{"the same packet, its base64 broken across lines", row(map[string]any{"SharedEvents": []map[string]any{{"Type": 2, "Data": parts[0].Data},
			{"Type": 3, "Data": base64.StdEncoding.EncodeToString(packet)[:4] + "\n" + base64.StdEncoding.EncodeToString(packet)[4:]}}}),
			body(events.Null[events.NotificationList](), events.Null[string]()), true},
		{"another encrypted packet", row(map[string]any{"SharedEvents": []map[string]any{{"Type": 2, "Data": parts[0].Data},
			{"Type": 3, "Data": base64.StdEncoding.EncodeToString([]byte{0xd2, 9})}}}), body(events.Null[events.NotificationList](), events.Null[string]()), false},
		{"a part missing", row(map[string]any{"SharedEvents": []map[string]any{{"Type": 2, "Data": parts[0].Data}}}),
			body(events.Null[events.NotificationList](), events.Null[string]()), false},
	} {
		if got := holds(c.row, c.body); got != c.want {
			t.Errorf("%s: holds = %v, want %v", c.name, got, c.want)
		}
	}
}
