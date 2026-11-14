package mcp

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestEventIDRoundTrip(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	at := time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC)
	for name, ref := range map[string]service.EventRef{
		"single":   {CalendarID: "cal/1==", UID: "uid with spaces@x"},
		"date":     {CalendarID: "c", UID: "u", RecurrenceID: &service.EventTime{Date: "2026-11-14"}},
		"floating": {CalendarID: "c", UID: "u", RecurrenceID: &service.EventTime{At: at, Floating: true}},
		"zoned":    {CalendarID: "c", UID: "u", RecurrenceID: &service.EventTime{At: at.In(paris), TZID: "Europe/Paris"}},
		"utc":      {CalendarID: "c", UID: "u", RecurrenceID: &service.EventTime{At: at}},
	} {
		id := encodeEventID("acct-1", ref)
		if strings.ContainsAny(id, "=+/") {
			t.Errorf("%s: %q is not unpadded base64url", name, id)
		}
		raw, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil || !strings.HasPrefix(string(raw), "acct-1\x00"+ref.CalendarID+"\x00"+ref.UID) {
			t.Errorf("%s: %q decodes to %q, %v", name, id, raw, err)
		}
		account, got, err := decodeEventID(id)
		if err != nil || account != "acct-1" || got.CalendarID != ref.CalendarID || got.UID != ref.UID {
			t.Fatalf("%s: decode = %q, %+v, %v", name, account, got, err)
		}
		switch w, g := ref.RecurrenceID, got.RecurrenceID; {
		case w == nil && g == nil:
		case w == nil || g == nil:
			t.Errorf("%s: recurrence ID %+v, want %+v", name, g, w)
		case g.Date != w.Date || g.Floating != w.Floating || !g.At.Equal(w.At):
			t.Errorf("%s: recurrence ID %+v, want %+v", name, *g, *w)
		case w.Floating && g.At.Format(time.DateTime) != "2026-03-29 02:30:00":
			t.Errorf("%s: floating wall clock %s, want 02:30 kept", name, g.At)
		}
	}
	// A floating ID carries no offset, an instant one does.
	floating, _ := base64.RawURLEncoding.DecodeString(encodeEventID("a", service.EventRef{CalendarID: "c", UID: "u", RecurrenceID: &service.EventTime{At: at, Floating: true}}))
	if !strings.HasSuffix(string(floating), "\x002026-03-29T02:30:00") {
		t.Errorf("floating ID %q", floating)
	}
	utc, _ := base64.RawURLEncoding.DecodeString(encodeEventID("a", service.EventRef{CalendarID: "c", UID: "u", RecurrenceID: &service.EventTime{At: at.In(paris), TZID: "Europe/Paris"}}))
	if !strings.HasSuffix(string(utc), "\x002026-03-29T02:30:00Z") {
		t.Errorf("zoned ID %q, want its instant in RFC 3339", utc)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{"", "!!", enc("a\x00c"), enc("a\x00c\x00u\x00not a time"), enc("\x00c\x00u"),
		enc("a\x00\x00u"), enc("a\x00c\x00"), enc("a\x00c\x00u\x002026-11-14\x00x"), enc("a\x00c\x00u\x002026-11-14") + "=="} {
		if a, ref, err := decodeEventID(bad); err == nil {
			t.Errorf("decodeEventID(%q) = %q, %+v; want an error", bad, a, ref)
		}
	}
}
