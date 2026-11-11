package cards_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// me is the author the cheap tests sign as; own knows it.
const me = "me@pcb.test"

// fullCard holds a property of every part, each line written once; its
// VALARM reaches the personal part with PersonalAlarms only.
const fullCard = "UID:full@pcb.test\r\n" +
	"DTSTAMP:20261111T090000Z\r\n" +
	"DTSTART:20261112T090000Z\r\n" +
	"DTEND:20261112T100000Z\r\n" +
	"RRULE:FREQ=WEEKLY\r\n" +
	"SEQUENCE:2\r\n" +
	"ORGANIZER;CN=Me:mailto:me@pcb.test\r\n" +
	"SUMMARY:Lunch\r\n" +
	"DESCRIPTION:Bring the\\nnotes\r\n" +
	"LOCATION:Canteen\r\n" +
	"X-MOZ-GENERATION:3\r\n" +
	"STATUS:CONFIRMED\r\n" +
	"TRANSP:OPAQUE\r\n" +
	"EXDATE:20261119T090000Z\r\n" +
	"COMMENT:private note\r\n" +
	"ATTENDEE;CN=Ann;PARTSTAT=ACCEPTED;X-PM-TOKEN=tokA:mailto:ann@pcb.test\r\n" +
	"ATTENDEE;CN=Bob;PARTSTAT=TENTATIVE;X-PM-TOKEN=tokB:mailto:bob@pcb.test\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT10M\r\nDESCRIPTION:Soon\r\nEND:VALARM\r\n"

// rig is a fake with one calendar read through a Session (no SRP), and the
// keys the fake holds.
type rig struct {
	f     *protontest.Fake
	c     *api.Client
	cal   string
	calKR *crypto.KeyRing
	addr  *crypto.KeyRing
}

func newRig(t testing.TB) *rig {
	t.Helper()
	f := protontest.New(t, protontest.Options{})
	c := api.New(f.Session(t, f.Username), api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	})
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	return &rig{f: f, c: c, cal: cal, calKR: f.CalendarKeyRing(cal), addr: f.AddressKeyRing()}
}

// own knows me only.
func (r *rig) own(email string) *crypto.KeyRing {
	if email == me {
		return r.addr
	}
	return nil
}

// add adds e (signed as me unless e.Author is set) and returns the row as served.
func (r *rig) add(t testing.TB, e protontest.Event) events.Row {
	t.Helper()
	if e.Author == "" {
		e.Author = me
	}
	id := r.f.AddEvent(r.cal, e)
	row, err := events.Get(t.Context(), r.c, r.cal, id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (r *rig) open(t *testing.T, raw json.RawMessage) cards.Event {
	t.Helper()
	ev, err := cards.Open(raw, r.calKR, nil, r.own)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return ev
}

func part(t *testing.T, ev cards.Event, k cards.Kind) cards.Part {
	t.Helper()
	for _, p := range ev.Parts {
		if p.Kind == k {
			return p
		}
	}
	t.Fatalf("no %s among %d parts", k, len(ev.Parts))
	return cards.Part{}
}

// has reports whether text holds line as a whole content line.
func has(text []byte, line string) bool {
	return bytes.Contains(text, []byte("\r\n"+line+"\r\n"))
}

// editRaw decodes raw, lets fn change it and encodes it again.
func editRaw(t *testing.T, raw json.RawMessage, fn func(m map[string]any)) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func partsOf(m map[string]any, group string) []any { return m[group].([]any) }

func field(m map[string]any, group string, i int, name string) string {
	return partsOf(m, group)[i].(map[string]any)[name].(string)
}

func setField(m map[string]any, group string, i int, name string, v any) {
	partsOf(m, group)[i].(map[string]any)[name] = v
}

func TestOpenDecryptsEveryPart(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	raw := r.add(t, protontest.Event{UID: "full@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true}).Raw
	// Proton's array order is not promised: the encrypted part listed first
	// still comes after the signed one.
	ev := r.open(t, editRaw(t, raw, func(m map[string]any) {
		s := partsOf(m, "SharedEvents")
		s[0], s[1] = s[1], s[0]
	}))

	common := []string{"UID:full@pcb.test", "DTSTAMP:20261111T090000Z"}
	want := []struct {
		kind  cards.Kind
		lines []string
	}{
		{cards.SharedSigned, []string{"DTSTART:20261112T090000Z", "DTEND:20261112T100000Z", "RRULE:FREQ=WEEKLY", "SEQUENCE:2", "ORGANIZER;CN=Me:mailto:me@pcb.test", "EXDATE:20261119T090000Z"}},
		{cards.SharedEncrypted, []string{"SUMMARY:Lunch", `DESCRIPTION:Bring the\nnotes`, "LOCATION:Canteen", "X-MOZ-GENERATION:3"}},
		{cards.CalendarSigned, []string{"STATUS:CONFIRMED", "TRANSP:OPAQUE", "EXDATE:20261119T090000Z"}},
		{cards.CalendarEncrypted, []string{"COMMENT:private note"}},
		{cards.Attendees, []string{"ATTENDEE;CN=Ann;PARTSTAT=ACCEPTED;X-PM-TOKEN=tokA:mailto:ann@pcb.test", "ATTENDEE;CN=Bob;PARTSTAT=TENTATIVE;X-PM-TOKEN=tokB:mailto:bob@pcb.test"}},
		{cards.Personal, []string{"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT10M\r\nDESCRIPTION:Soon\r\nEND:VALARM"}},
	}
	if len(ev.Parts) != len(want) || ev.SkippedParts != 0 {
		t.Fatalf("%d parts, %d skipped; want %d, 0", len(ev.Parts), ev.SkippedParts, len(want))
	}
	for i, w := range want {
		p := ev.Parts[i]
		if p.Kind != w.kind {
			t.Fatalf("part %d is %s, want %s (parts are ordered by kind)", i, p.Kind, w.kind)
		}
		if !p.Signed || !p.Verified || p.Author != me {
			t.Errorf("%s: Signed %v Verified %v Author %q; want true, true, %q", p.Kind, p.Signed, p.Verified, p.Author, me)
		}
		if !has(p.Text, common[0]) {
			t.Errorf("%s lacks %q", p.Kind, common[0])
		}
		for _, l := range w.lines {
			if !has(p.Text, l) {
				t.Errorf("%s lacks %q in\n%q", p.Kind, l, p.Text)
			}
		}
		// Every line lives in its own part only; EXDATE, in both signed
		// parts (research B.1), is listed in each.
		for j, o := range want {
			if j == i {
				continue
			}
			for _, l := range o.lines {
				if has(p.Text, l) && !slices.Contains(w.lines, l) {
					t.Errorf("%s holds %q, a line of the %s", p.Kind, l, o.kind)
				}
			}
		}
	}
}

// TestOpenCardlessEvent covers the M2 events (no Card): the fake makes a
// minimal card from the row's columns.
func TestOpenCardlessEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ev := r.open(t, r.add(t, protontest.Event{UID: "m2@pcb.test", Start: 1762851600, End: 1762855200}).Raw)
	ss := part(t, ev, cards.SharedSigned)
	for _, l := range []string{"UID:m2@pcb.test", "DTSTART:20251111T090000Z", "DTEND:20251111T100000Z"} {
		if !has(ss.Text, l) {
			t.Errorf("shared signed part lacks %q in\n%q", l, ss.Text)
		}
	}
	if !ss.Verified {
		t.Error("the minimal card must be signed too")
	}
}

func TestOpenVerifiesOwnSignatures(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	raw := r.add(t, protontest.Event{UID: "v@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true}).Raw
	// Signed with the address key, but named as someone not ours.
	stranger := r.add(t, protontest.Event{UID: "s@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, Author: "stranger@pcb.test"}).Raw
	// Named as ours, signed with another key.
	forged := r.add(t, protontest.Event{UID: "f@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, ForeignKey: true}).Raw
	other, err := crypto.GenerateKey("Other", "other@pcb.test", "x25519", 0)
	if err != nil {
		t.Fatal(err)
	}
	otherKR, err := crypto.NewKeyRing(other)
	if err != nil {
		t.Fatal(err)
	}
	// Our address keys are at hand: they must be used through own only.
	addrs := map[string]*crypto.KeyRing{"addr1": r.addr}

	type want map[cards.Kind]bool // kind → Verified; kinds absent: true
	cases := []struct {
		name string
		raw  json.RawMessage
		own  func(string) *crypto.KeyRing
		want want
		all  bool // every part unverified
	}{
		{name: "untouched", raw: raw, own: r.own},
		{name: "tampered cleartext", raw: editRaw(t, raw, func(m map[string]any) {
			d := field(m, "SharedEvents", 0, "Data")
			setField(m, "SharedEvents", 0, "Data", strings.Replace(d, "DTEND:20261112T100000Z", "DTEND:20261112T230000Z", 1))
		}), own: r.own, want: want{cards.SharedSigned: false}},
		{name: "signature of another part", raw: editRaw(t, raw, func(m map[string]any) {
			setField(m, "SharedEvents", 1, "Signature", field(m, "SharedEvents", 0, "Signature"))
		}), own: r.own, want: want{cards.SharedEncrypted: false}},
		{name: "garbage signature", raw: editRaw(t, raw, func(m map[string]any) {
			setField(m, "CalendarEvents", 0, "Signature", "not armored")
		}), own: r.own, want: want{cards.CalendarSigned: false}},
		{name: "part without Author falls back to the row's", raw: editRaw(t, raw, func(m map[string]any) {
			delete(partsOf(m, "SharedEvents")[0].(map[string]any), "Author")
		}), own: r.own},
		{name: "signature by another key over the same text", raw: editRaw(t, raw, func(m map[string]any) {
			sig, err := otherKR.SignDetached(crypto.NewPlainMessage([]byte(field(m, "SharedEvents", 0, "Data"))))
			if err != nil {
				t.Fatal(err)
			}
			armored, err := sig.GetArmored()
			if err != nil {
				t.Fatal(err)
			}
			setField(m, "SharedEvents", 0, "Signature", armored)
		}), own: r.own, want: want{cards.SharedSigned: false}},
		{name: "every part signed by another key", raw: forged, own: r.own, all: true},
		{name: "part Author not ours, row Author ours", raw: editRaw(t, raw, func(m map[string]any) {
			setField(m, "SharedEvents", 0, "Author", "stranger@pcb.test")
		}), own: r.own, want: want{cards.SharedSigned: false}},
		{name: "part Author ours, row Author not ours", raw: editRaw(t, raw, func(m map[string]any) {
			m["Author"] = "stranger@pcb.test"
		}), own: r.own},
		{name: "author not ours", raw: stranger, own: r.own, all: true},
		{name: "no own function", raw: raw, own: nil, all: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, err := cards.Open(c.raw, r.calKR, addrs, c.own)
			if err != nil {
				t.Fatalf("Open: %v (a bad signature is not an error)", err)
			}
			if len(ev.Parts) != 6 {
				t.Fatalf("%d parts, want 6", len(ev.Parts))
			}
			for _, p := range ev.Parts {
				verified, listed := c.want[p.Kind]
				if !listed {
					verified = !c.all
				}
				if !p.Signed || p.Verified != verified {
					t.Errorf("%s: Signed %v Verified %v, want true, %v", p.Kind, p.Signed, p.Verified, verified)
				}
			}
		})
	}
	// The tampered text is served as it came: verification marks, never edits.
	ev, _ := cards.Open(cases[1].raw, r.calKR, nil, r.own)
	if !has(part(t, ev, cards.SharedSigned).Text, "DTEND:20261112T230000Z") {
		t.Error("the tampered cleartext must be returned as is")
	}
	if got := part(t, r.open(t, stranger), cards.SharedSigned).Author; got != "stranger@pcb.test" {
		t.Errorf("Author = %q, want the part's", got)
	}
}

func TestOpenAddressKeyPacket(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	raw := r.add(t, protontest.Event{UID: "inv@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, AddressKeyPacket: true}).Raw
	var keysOf struct{ SharedKeyPacket, AddressKeyPacket *string }
	if err := json.Unmarshal(raw, &keysOf); err != nil {
		t.Fatal(err)
	}
	if keysOf.SharedKeyPacket != nil || keysOf.AddressKeyPacket == nil || *keysOf.AddressKeyPacket == "" {
		t.Fatalf("the fake must serve an AddressKeyPacket and no SharedKeyPacket, got %+v", keysOf)
	}

	ev, err := cards.Open(raw, r.calKR, map[string]*crypto.KeyRing{"addr1": r.addr}, r.own)
	if err != nil {
		t.Fatalf("Open with the address keys: %v", err)
	}
	if !has(part(t, ev, cards.SharedEncrypted).Text, "SUMMARY:Lunch") || !has(part(t, ev, cards.Attendees).Text, "ATTENDEE;CN=Ann;PARTSTAT=ACCEPTED;X-PM-TOKEN=tokA:mailto:ann@pcb.test") {
		t.Error("the shared session key from the address key packet must open the shared encrypted and attendees parts")
	}
	if !has(part(t, ev, cards.CalendarEncrypted).Text, "COMMENT:private note") {
		t.Error("the calendar part still opens with the calendar key")
	}

	_, err = cards.Open(raw, r.calKR, nil, r.own)
	if !errors.Is(err, cards.ErrUndecryptable) || !strings.Contains(err.Error(), "shared encrypted part") {
		t.Errorf("without address keys: %v, want ErrUndecryptable naming the shared encrypted part", err)
	}

	// Both packets served: an address packet that does not open falls back
	// to the shared packet and the calendar key.
	plain := r.add(t, protontest.Event{UID: "p@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true}).Raw
	both := editRaw(t, plain, func(m map[string]any) { m["AddressKeyPacket"] = "bm90IGEga2V5IHBhY2tldA==" })
	if _, err := cards.Open(both, r.calKR, map[string]*crypto.KeyRing{"addr1": r.addr}, r.own); err != nil {
		t.Errorf("an address packet that does not open must fall back to the shared packet: %v", err)
	}
}

func TestOpenBrokenPartNamesThePart(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	cases := []struct {
		broken protontest.Part
		kind   cards.Kind
		group  string
		index  int
	}{
		{protontest.SharedEncryptedPart, cards.SharedEncrypted, "SharedEvents", 1},
		{protontest.CalendarEncryptedPart, cards.CalendarEncrypted, "CalendarEvents", 1},
		{protontest.AttendeesPart, cards.Attendees, "AttendeesEvents", 0},
	}
	for _, c := range cases {
		row := r.add(t, protontest.Event{UID: "b@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, BrokenPart: c.broken})
		t.Run(c.kind.String(), func(t *testing.T) {
			_, err := cards.Open(row.Raw, r.calKR, nil, r.own)
			if !errors.Is(err, cards.ErrUndecryptable) {
				t.Fatalf("err = %v, want ErrUndecryptable", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.kind.String()) || !strings.Contains(msg, row.ID) {
				t.Errorf("%q must name the %s and the event %s", msg, c.kind, row.ID)
			}
			var m map[string]any
			_ = json.Unmarshal(row.Raw, &m)
			data := field(m, c.group, c.index, "Data")
			for _, leak := range []string{data[:24], "Lunch", "private note", "BEGIN:VCALENDAR", "PGP"} {
				if strings.Contains(msg, leak) {
					t.Errorf("%q leaks %q", msg, leak)
				}
			}
		})
	}

	raw := r.add(t, protontest.Event{UID: "k@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true}).Raw
	for name, open := range map[string]func() error{
		"no calendar key": func() error { _, err := cards.Open(raw, nil, nil, r.own); return err },
		"another calendar key": func() error {
			_, err := cards.Open(raw, r.f.CalendarKeyRing(r.f.AddCalendar(r.f.Username, protontest.Calendar{})), nil, r.own)
			return err
		},
		"garbage key packet": func() error {
			_, err := cards.Open(editRaw(t, raw, func(m map[string]any) { m["SharedKeyPacket"] = "not base64!" }), r.calKR, nil, r.own)
			return err
		},
	} {
		if err := open(); !errors.Is(err, cards.ErrUndecryptable) || !strings.Contains(err.Error(), "shared encrypted part") {
			t.Errorf("%s: %v, want ErrUndecryptable naming the shared encrypted part", name, err)
		}
	}
}

// TestOpenSkipsUndecryptablePersonalPart: ruling M3-R7, a personal part that
// does not decrypt is left out and counted, never fatal.
func TestOpenSkipsUndecryptablePersonalPart(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ev, err := cards.Open(r.add(t, protontest.Event{UID: "p@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, BrokenPart: protontest.PersonalPart}).Raw, r.calKR, nil, r.own)
	if err != nil {
		t.Fatalf("Open: %v, want the event without its personal part", err)
	}
	if ev.SkippedParts != 1 || len(ev.Parts) != 5 {
		t.Fatalf("SkippedParts %d, %d parts; want 1, 5", ev.SkippedParts, len(ev.Parts))
	}
	for _, p := range ev.Parts {
		if p.Kind == cards.Personal {
			t.Error("the undecryptable personal part must be left out")
		}
	}
	if !has(part(t, ev, cards.SharedEncrypted).Text, "SUMMARY:Lunch") {
		t.Error("the other parts must still open")
	}
}

func TestOpenKeepsUTF8AndCRLF(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	card := "UID:u@pcb.test\r\n" +
		"DTSTART:20261112T090000Z\r\n" +
		"ORGANIZER;CN=Zoë Ü:mailto:zoe@pcb.test\r\n" +
		"SUMMARY:Café ☕ 🎉\r\n" +
		"DESCRIPTION:a long line that a client folded\r\n  here\r\n" +
		"X-RAW:\xff\xfe not UTF-8\r\n"
	ev := r.open(t, r.add(t, protontest.Event{UID: "u@pcb.test", Start: 1, End: 2, Card: card}).Raw)
	se := part(t, ev, cards.SharedEncrypted).Text
	for _, l := range []string{"SUMMARY:Café ☕ 🎉", "DESCRIPTION:a long line that a client folded\r\n  here", "X-RAW:\xff\xfe not UTF-8"} {
		if !has(se, l) {
			t.Errorf("shared encrypted part lacks %q byte for byte in\n%q", l, se)
		}
	}
	if !has(part(t, ev, cards.SharedSigned).Text, "ORGANIZER;CN=Zoë Ü:mailto:zoe@pcb.test") {
		t.Error("the shared signed part must keep UTF-8")
	}
	for _, p := range ev.Parts {
		if bytes.Count(p.Text, []byte("\n")) != bytes.Count(p.Text, []byte("\r\n")) {
			t.Errorf("%s has a bare LF: CRLF must survive", p.Kind)
		}
	}
}

func TestOpenRowFields(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	rid := int64(1762851600)
	ev := r.open(t, r.add(t, protontest.Event{
		UID: "f@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, RecurrenceID: &rid, Color: "#EC3E7C",
		Notifications: []protontest.Notification{{Type: 1, Trigger: "-PT15M"}},
	}).Raw)
	if ev.Color != "#EC3E7C" || ev.RecurrenceID == nil || *ev.RecurrenceID != rid {
		t.Errorf("Color %q RecurrenceID %v", ev.Color, ev.RecurrenceID)
	}
	if got := string(ev.Notifications); got != `[{"Type":1,"Trigger":"-PT15M"}]` {
		t.Errorf("Notifications = %s", got)
	}
	want := []cards.AttendeeStatus{{Token: "tokA", Status: 3}, {Token: "tokB", Status: 1}}
	if len(ev.Attendees) != len(want) {
		t.Fatalf("Attendees = %+v", ev.Attendees)
	}
	for i, w := range want {
		if a := ev.Attendees[i]; a.Token != w.Token || a.Status != w.Status || a.ID == "" {
			t.Errorf("attendee %d = %+v, want token %s status %d and an ID", i, a, w.Token, w.Status)
		}
	}

	// Notifications keep Proton's tri-state: null = calendar defaults, [] = none.
	for _, c := range []struct {
		n    []protontest.Notification
		want string
	}{{nil, "null"}, {[]protontest.Notification{}, "[]"}} {
		ev := r.open(t, r.add(t, protontest.Event{UID: "n@pcb.test", Start: 1, End: 2, Notifications: c.n}).Raw)
		if string(ev.Notifications) != c.want || ev.Color != "" || ev.RecurrenceID != nil {
			t.Errorf("Notifications %s Color %q RecurrenceID %v, want %s, empty, nil", ev.Notifications, ev.Color, ev.RecurrenceID, c.want)
		}
	}
}

func TestOpenRefusesWhatIsNotARow(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"ID":`, `null`, `{}`, `[1]`} {
		if _, err := cards.Open(json.RawMessage(raw), nil, nil, nil); err == nil {
			t.Errorf("Open(%s) succeeded", raw)
		}
	}
}

// TestOpenWithLoggedInKeys runs the real chain once: login, address keys,
// calendar unlock, rows read with that session. The fake's default author and
// its signatures must verify with the address keys Proton serves, and an
// address key packet must open with them.
func TestOpenWithLoggedInKeys(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := auth.NewManager(f.URL, "Other")
	t.Cleanup(m.Close)
	p, err := auth.Begin(t.Context(), m, f.Username, []byte(f.Password), "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(p.Close)
	c := api.New(p.Tokens, api.Options{BaseURL: f.URL, Limiter: rate.NewLimiter(rate.Inf, 1), HTTP: &http.Client{Timeout: 10 * time.Second}})
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	salts, err := keys.FetchSalts(t.Context(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password))
	if err != nil {
		t.Fatal(err)
	}
	u, err := keys.Unlock(user, addrs, pass)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(u.Close)
	calID := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	a, err := calendar.Unlock(t.Context(), c, u, calendar.Calendar{ID: calID})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	own := func(email string) *crypto.KeyRing {
		for _, ad := range u.Addresses {
			if strings.EqualFold(ad.Email, email) {
				return u.AddrKRs[ad.ID]
			}
		}
		return nil
	}

	for i, e := range []protontest.Event{
		{UID: "l0@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true},
		{UID: "l1@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, AddressKeyPacket: true},
	} {
		row, err := events.Get(t.Context(), c, calID, f.AddEvent(calID, e))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := cards.Open(row.Raw, a.KR, u.AddrKRs, own)
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if len(ev.Parts) != 6 {
			t.Fatalf("event %d: %d parts", i, len(ev.Parts))
		}
		for _, p := range ev.Parts {
			if !p.Verified || p.Author != u.Addresses[0].Email {
				t.Errorf("event %d %s: Verified %v Author %q; want true, %q", i, p.Kind, p.Verified, p.Author, u.Addresses[0].Email)
			}
		}
	}
}

func BenchmarkOpen(b *testing.B) {
	r := newRig(b)
	const n = 1000
	for i := range n {
		uid := "bench" + strconv.Itoa(i) + "@pcb.test"
		r.f.AddEvent(r.cal, protontest.Event{UID: uid, Start: 1, End: 2, Author: me, PersonalAlarms: true,
			Card: strings.Replace(fullCard, "UID:full@pcb.test", "UID:"+uid, 1)})
	}
	rows, err := events.ListAll(b.Context(), r.c, r.cal)
	if err != nil || len(rows) != n {
		b.Fatalf("ListAll: %d rows, %v", len(rows), err)
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := cards.Open(rows[i%n].Raw, r.calKR, nil, r.own); err != nil {
			b.Fatal(err)
		}
	}
}

// Ruling M3-R36: Own tells a part authored by one of our addresses (the
// part's Author, else the row's), whether or not it is signed; nil own
// knows no address.
func TestOpenTellsOwnAuthoredParts(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	raw := r.add(t, protontest.Event{UID: "o@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true}).Raw
	stranger := r.add(t, protontest.Event{UID: "s@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, Author: "stranger@pcb.test"}).Raw
	unsigned := r.add(t, protontest.Event{UID: "u@pcb.test", Start: 1, End: 2, Card: fullCard, PersonalAlarms: true, Unsigned: protontest.SharedSignedPart}).Raw
	addrs := map[string]*crypto.KeyRing{"addr1": r.addr}
	for _, c := range []struct {
		name string
		raw  json.RawMessage
		own  func(string) *crypto.KeyRing
		want bool
	}{
		{"ours", raw, r.own, true},
		{"a stranger's", stranger, r.own, false},
		{"no own function", raw, nil, false},
	} {
		ev, err := cards.Open(c.raw, r.calKR, addrs, c.own)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, p := range ev.Parts {
			if p.Own != c.want {
				t.Errorf("%s: %s Own = %v, want %v", c.name, p.Kind, p.Own, c.want)
			}
		}
	}
	ev, err := cards.Open(unsigned, r.calKR, addrs, r.own)
	if err != nil {
		t.Fatal(err)
	}
	ss := part(t, ev, cards.SharedSigned)
	if !ss.Own || ss.Signed || ss.Verified {
		t.Errorf("the unsigned shared part = Own %v, Signed %v, Verified %v; want ours, unsigned, unverified", ss.Own, ss.Signed, ss.Verified)
	}
}
