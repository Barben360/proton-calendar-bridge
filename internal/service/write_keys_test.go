package service_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// A calendar whose key was rotated lists several keys (an unopenable one, a
// retired one, the former primary and the new primary last): a row written
// now carries key packets to the primary one only, and the fake's own keys
// say which that is. A write that took the first, the last-listed or the
// retired key instead would not open.
func TestWriteOnARotatedCalendarWrapsToThePrimaryKey(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.RotateCalendarKey(cal)
	})
	r.start(t)
	body := vcal([]string{"UID:rot@x", "DTSTAMP:20270115T080000Z", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600), "SUMMARY:After the rotation"})
	if _, err := r.put(t, "rot@x.ics", body, service.Precondition{}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	rows := r.proton(t, "rot@x")
	if len(rows) != 1 {
		t.Fatalf("Proton holds %d rows of rot@x", len(rows))
	}
	var row struct{ SharedKeyPacket, CalendarKeyPacket string }
	if err := json.Unmarshal(rows[0].Raw, &row); err != nil {
		t.Fatal(err)
	}
	if row.SharedKeyPacket == "" {
		t.Fatalf("the row has no SharedKeyPacket: %s", rows[0].Raw)
	}
	opens := func(kr *crypto.KeyRing, packet string) bool {
		t.Helper()
		kp, err := base64.StdEncoding.DecodeString(packet)
		if err != nil {
			t.Fatal(err)
		}
		_, err = kr.DecryptSessionKey(kp)
		return err == nil
	}
	if !opens(r.f.CalendarKeyRing(r.cal), row.SharedKeyPacket) {
		t.Error("the SharedKeyPacket does not open with the calendar's primary key")
	}
	if opens(r.f.RetiredCalendarKeyRing(r.cal), row.SharedKeyPacket) {
		t.Error("the SharedKeyPacket opens with the retired key: it was wrapped to the wrong key")
	}
}
