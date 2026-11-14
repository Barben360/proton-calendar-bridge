package events_test

import (
	"encoding/json"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
)

// The sync endpoint refuses null part lists (research A.2) and an update
// must not carry key packets (research B.5): the zero WireEvent encodes
// three [] and no key packet field, and the field names are the endpoint's.
func TestWireEventEncoding(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(events.WireEvent{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"SharedEventContent":[],"CalendarEventContent":[],"AttendeesEventContent":[]}`
	if string(got) != want {
		t.Errorf("zero WireEvent encodes as\n%s\nwant\n%s", got, want)
	}

	full := events.WireEvent{
		SharedKeyPacket: "c2s=", CalendarKeyPacket: "Y2s=",
		SharedEventContent:    events.PartList{{Type: events.PartSigned, Data: "d", Signature: "s"}},
		CalendarEventContent:  events.PartList{},
		AttendeesEventContent: events.PartList{{Type: events.PartEncrypted, Data: "e", Signature: "t"}},
	}
	got, err = json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"SharedKeyPacket":"c2s=","CalendarKeyPacket":"Y2s=",` +
		`"SharedEventContent":[{"Type":2,"Data":"d","Signature":"s"}],` +
		`"CalendarEventContent":[],` +
		`"AttendeesEventContent":[{"Type":3,"Data":"e","Signature":"t"}]}`
	if string(got) != want {
		t.Errorf("WireEvent encodes as\n%s\nwant\n%s", got, want)
	}

	// Embedded in a larger Event (Task 5 adds Notifications, Color...), the
	// lists keep their encoding and the outer fields are not swallowed.
	type entry struct {
		events.WireEvent
		Permissions int
	}
	got, err = json.Marshal(entry{Permissions: 1})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"SharedEventContent":[],"CalendarEventContent":[],"AttendeesEventContent":[],"Permissions":1}`
	if string(got) != want {
		t.Errorf("embedded WireEvent encodes as\n%s\nwant\n%s", got, want)
	}
}
