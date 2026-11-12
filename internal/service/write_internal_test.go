package service

import (
	"encoding/json"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
)

// fieldsHold is the row-field half of a write's confirmation. Through the
// fake every edit re-seals its parts, so the parts alone already differ
// there; here each field is checked alone. The parts half (partsHold) is
// covered by the executor's tests.
func TestFieldsHoldComparesEachFieldSent(t *testing.T) {
	row := func(fields map[string]any) json.RawMessage {
		m := map[string]any{"Notifications": nil, "Color": nil}
		for k, v := range fields {
			m[k] = v
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	list := events.NotificationList{{Type: 1, Trigger: "-PT15M"}, {Type: 0, Trigger: "-PT1H"}}
	body := func(n events.Optional[events.NotificationList], c events.Optional[string]) events.Body {
		return events.Body{Notifications: n, Color: c}
	}
	none, noColour := events.Null[events.NotificationList](), events.Null[string]()
	reversed := []map[string]any{{"Type": 0, "Trigger": "-PT1H"}, {"Type": 1, "Trigger": "-PT15M"}}
	for _, c := range []struct {
		name string
		row  json.RawMessage
		body events.Body
		want bool
	}{
		{"same", row(nil), body(none, noColour), true},
		{"notifications absent: not checked", row(map[string]any{"Notifications": reversed}), body(events.Optional[events.NotificationList]{}, events.Optional[string]{}), true},
		{"null sent, a list stored", row(map[string]any{"Notifications": reversed}), body(none, noColour), false},
		{"a list sent, null stored", row(nil), body(events.Value(list), noColour), false},
		{"a list sent, [] stored", row(map[string]any{"Notifications": []any{}}), body(events.Value(list), noColour), false},
		{"[] sent, null stored", row(nil), body(events.Value(events.NotificationList{}), noColour), false},
		{"null sent, [] stored", row(map[string]any{"Notifications": []any{}}), body(none, noColour), false},
		{"the list stored in another order", row(map[string]any{"Notifications": reversed}), body(events.Value(list), noColour), true},
		{"another list stored", row(map[string]any{"Notifications": reversed[:1]}), body(events.Value(list), noColour), false},
		{"colour sent and stored", row(map[string]any{"Color": "#3cbb3a"}), body(none, events.Value("#3CBB3A")), true},
		{"colour sent, another stored", row(map[string]any{"Color": "#8080FF"}), body(none, events.Value("#3CBB3A")), false},
		{"colour sent, none stored", row(nil), body(none, events.Value("#3CBB3A")), false},
		{"null colour: the row's kept", row(map[string]any{"Color": "#8080FF"}), body(none, noColour), true},
		{"not a row", json.RawMessage(`[1]`), body(none, noColour), false},
	} {
		if got := fieldsHold(c.row, c.body); got != c.want {
			t.Errorf("%s: fieldsHold = %v, want %v", c.name, got, c.want)
		}
	}
}
