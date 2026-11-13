package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// The planner is pure, so its tests build rows themselves: each stored row
// is sealed with cards.Seal under test keys (our address key signs, the
// calendar key receives the key packets), served in Proton's row shape and
// opened with OpenGroup, as the executor will. Outcomes are checked by
// opening the planned parts again with cards.Open and the same keys: the
// plaintext Proton would store, every signature verified. Keys are made
// here (x25519, fast) rather than borrowed from protontest, whose fake
// starts a Proton backend these tests never call.

const (
	planUID       = "u1@pcb.test"
	meEmail       = "me@pcb.test"
	strangerEmail = "stranger@pcb.test"
	meAddrID      = "addr-me"
	stamp         = "DTSTAMP:20261101T090000Z"
)

type planKeySet struct{ me, stranger, cal, otherCal *crypto.Key }

var planKeys = sync.OnceValues(func() (planKeySet, error) {
	gen := func(name, email string) (*crypto.Key, error) {
		k, err := crypto.GenerateKey(name, email, "x25519", 0)
		if err == nil {
			_, err = k.Serialize() // warm: go-crypto caches on the first serialisation
		}
		return k, err
	}
	var s planKeySet
	var err error
	for _, k := range []struct {
		dst         **crypto.Key
		name, email string
	}{
		{&s.me, "Me", meEmail}, {&s.stranger, "Stranger", strangerEmail},
		{&s.cal, "Calendar", "cal@pcb.test"}, {&s.otherCal, "Other calendar", "other@pcb.test"},
	} {
		if *k.dst, err = gen(k.name, k.email); err != nil {
			return s, err
		}
	}
	return s, nil
})

func ring(t *testing.T, k *crypto.Key) *crypto.KeyRing {
	t.Helper()
	kr, err := crypto.NewKeyRing(k)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func keySet(t *testing.T) planKeySet {
	t.Helper()
	s, err := planKeys()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func groupKeys(t *testing.T) GroupKeys {
	s := keySet(t)
	me := ring(t, s.me)
	return GroupKeys{
		Calendar:  ring(t, s.cal),
		Addresses: map[string]*crypto.KeyRing{meAddrID: me},
		Own: func(email string) *crypto.KeyRing {
			if strings.EqualFold(email, meEmail) {
				return me
			}
			return nil
		},
	}
}

var testNow = time.Date(2026, 11, 12, 8, 30, 0, 0, time.UTC)

func planDefaults(t *testing.T) Defaults {
	s := keySet(t)
	return Defaults{
		PartDay: []ical.Notification{{Type: 1, Trigger: "-PT15M"}},
		FullDay: []ical.Notification{{Type: 1, Trigger: "-PT15H"}},
		Signer:  ring(t, s.me), CalendarPrimary: ring(t, s.cal), Now: testNow,
	}
}

// frag is a part in Proton's fragment form.
func frag(lines ...string) string {
	return "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\nEND:VCALENDAR"
}

// seed is a stored row to build.
type seed struct {
	id        string
	uidColumn string // the row's UID column; "" is planUID
	rid       *int64
	fullDay   bool
	lastEdit  int64
	parts     map[ical.PartKind]string
	// Faults: parts served unsigned, signed by the stranger's key and
	// authored by the stranger, or with our signature over other bytes.
	unsigned, foreign, badSig []ical.PartKind
	// clear replaces a signed part's text after sealing (Seal refuses to
	// sign a misrouted line), signed by us.
	clear map[ical.PartKind]string
	// otherCal seals the key packets to another calendar key: nothing opens.
	otherCal bool
	// invitation wraps the shared session key to our address key
	// (AddressKeyPacket) instead of the calendar key; strangerInvitation to
	// the stranger's, so that the row does not open.
	invitation, strangerInvitation bool
	// personal adds a personal part: "own" (ours, signed, opens),
	// "own-unsigned" (ours, unsigned, opens) or "undecryptable" (encrypted
	// to the stranger).
	personal                        string
	notifications, color, attendees string // raw JSON; "" is null, null, []
	isOrganizer                     *int
	// dup serves the part of each kind twice; fields set (or, nil, drop)
	// top-level fields of the row last.
	dup    []ical.PartKind
	fields map[string]any
}

// kindOf is the kind of a sealed part, from its list and Type.
func kindOf(list string, typ int) ical.PartKind {
	enc := typ&1 != 0
	switch {
	case list == "AttendeesEvents":
		return ical.Attendees
	case list == "SharedEvents" && enc:
		return ical.SharedEncrypted
	case list == "SharedEvents":
		return ical.SharedSigned
	case enc:
		return ical.CalendarEncrypted
	}
	return ical.CalendarSigned
}

func sign(t *testing.T, kr *crypto.KeyRing, text []byte) string {
	t.Helper()
	sig, err := kr.SignDetached(crypto.NewPlainMessage(text))
	if err != nil {
		t.Fatal(err)
	}
	armored, err := sig.GetArmored()
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

func has(kinds []ical.PartKind, k ical.PartKind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// row builds s as Proton serves it.
func (s seed) row(t *testing.T) cache.StoredRow {
	t.Helper()
	ks := keySet(t)
	me, stranger := ring(t, ks.me), ring(t, ks.stranger)
	cal := ring(t, ks.cal)
	if s.otherCal {
		cal = ring(t, ks.otherCal)
	}
	parts := map[ical.PartKind][]byte{}
	for k, text := range s.parts {
		parts[k] = []byte(text)
	}
	we, err := cards.Seal(parts, cards.SealKeys{Signer: me, CalendarPrimary: cal})
	if err != nil {
		t.Fatal(err)
	}
	r := map[string]any{
		"ID": s.id, "CalendarID": "cal1", "UID": planUID, "Author": meEmail, "AddressID": meAddrID,
		"SharedKeyPacket": we.SharedKeyPacket, "CalendarKeyPacket": we.CalendarKeyPacket,
		"PersonalEvents": []any{}, "Notifications": json.RawMessage("null"), "Color": nil,
		"Attendees": json.RawMessage("[]"), "RecurrenceID": s.rid, "LastEditTime": s.lastEdit,
	}
	if s.uidColumn != "" {
		r["UID"] = s.uidColumn
	}
	if s.isOrganizer != nil {
		r["IsOrganizer"] = *s.isOrganizer
	}
	if s.notifications != "" {
		r["Notifications"] = json.RawMessage(s.notifications)
	}
	if s.color != "" {
		r["Color"] = s.color
	}
	if s.attendees != "" {
		r["Attendees"] = json.RawMessage(s.attendees)
	}
	for list, ps := range map[string]events.PartList{
		"SharedEvents": we.SharedEventContent, "CalendarEvents": we.CalendarEventContent, "AttendeesEvents": we.AttendeesEventContent,
	} {
		out := []map[string]any{}
		for _, p := range ps {
			k := kindOf(list, p.Type)
			e := map[string]any{"Type": p.Type, "Data": p.Data, "Signature": p.Signature, "Author": meEmail}
			text := parts[k]
			if c, ok := s.clear[k]; ok {
				text = []byte(c)
				e["Data"], e["Signature"] = c, sign(t, me, text)
			}
			switch {
			case has(s.unsigned, k):
				e["Type"], e["Signature"] = p.Type&^2, ""
			case has(s.foreign, k):
				e["Signature"], e["Author"] = sign(t, stranger, text), strangerEmail
			case has(s.badSig, k):
				e["Signature"] = sign(t, me, append(append([]byte(nil), text...), 'x'))
			}
			out = append(out, e)
			if has(s.dup, k) {
				out = append(out, e)
			}
		}
		r[list] = out
	}
	if s.invitation || s.strangerInvitation {
		kp, err := base64.StdEncoding.DecodeString(we.SharedKeyPacket)
		if err != nil {
			t.Fatal(err)
		}
		sk, err := cal.DecryptSessionKey(kp)
		if err != nil {
			t.Fatal(err)
		}
		to := me
		if s.strangerInvitation {
			to = stranger
		}
		wrapped, err := to.EncryptSessionKey(sk)
		if err != nil {
			t.Fatal(err)
		}
		r["AddressKeyPacket"], r["SharedKeyPacket"] = base64.StdEncoding.EncodeToString(wrapped), ""
	}
	if s.personal != "" {
		to, author := cal, meEmail
		if s.personal == "undecryptable" {
			to = stranger
		}
		text := []byte(frag("UID:"+planUID, stamp, "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "END:VALARM"))
		msg, err := to.Encrypt(crypto.NewPlainMessage(text), nil)
		if err != nil {
			t.Fatal(err)
		}
		armored, err := msg.GetArmored()
		if err != nil {
			t.Fatal(err)
		}
		part := map[string]any{"Type": 1, "Data": armored, "Signature": "", "Author": author}
		if s.personal == "own" {
			part["Type"], part["Signature"] = 3, sign(t, me, text)
		}
		r["PersonalEvents"] = []map[string]any{part}
	}
	for k, v := range s.fields {
		if v == nil {
			delete(r, k)
			continue
		}
		r[k] = v
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return cache.StoredRow{ID: s.id, UID: planUID, FullDay: s.fullDay, RecurrenceID: s.rid, LastEdit: s.lastEdit, Raw: raw}
}

func group(t *testing.T, seeds ...seed) Group {
	t.Helper()
	g := cache.Group{UID: planUID}
	for _, s := range seeds {
		g.Rows = append(g.Rows, s.row(t))
	}
	return OpenGroup(g, groupKeys(t))
}

// opened is a planned body as Proton would store it: its parts, opened
// with the test keys (every signature verified), and its JSON fields.
type opened struct {
	parts  map[ical.PartKind]string
	fields map[string]json.RawMessage
}

// open opens op's parts. An update must carry no key packet: its parts open
// with the stored row's key packets (stored), as Proton keeps them.
func open(t *testing.T, op events.Op, stored *cache.StoredRow) opened {
	t.Helper()
	var body events.Body
	r := map[string]any{"ID": "planned", "UID": planUID, "Author": meEmail}
	switch {
	case op.Create != nil:
		body = *op.Create
		r["SharedKeyPacket"], r["CalendarKeyPacket"] = body.SharedKeyPacket, body.CalendarKeyPacket
	case op.Update != nil:
		body = op.Update.Body
		if body.SharedKeyPacket != "" || body.CalendarKeyPacket != "" {
			t.Fatalf("an update carries a key packet")
		}
		if stored == nil {
			t.Fatal("open: an update needs its stored row")
		}
		var kp struct{ SharedKeyPacket, CalendarKeyPacket string }
		if err := json.Unmarshal(stored.Raw, &kp); err != nil {
			t.Fatal(err)
		}
		r["SharedKeyPacket"], r["CalendarKeyPacket"] = kp.SharedKeyPacket, kp.CalendarKeyPacket
	default:
		t.Fatalf("open: not a create or an update")
	}
	for list, ps := range map[string]events.PartList{
		"SharedEvents": body.SharedEventContent, "CalendarEvents": body.CalendarEventContent, "AttendeesEvents": body.AttendeesEventContent,
	} {
		out := []map[string]any{}
		for _, p := range ps {
			out = append(out, map[string]any{"Type": p.Type, "Data": p.Data, "Signature": p.Signature, "Author": meEmail})
		}
		r[list] = out
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	k := groupKeys(t)
	ev, err := cards.Open(raw, k.Calendar, k.Addresses, k.Own)
	if err != nil {
		t.Fatalf("the planned parts do not open: %v", err)
	}
	o := opened{parts: map[ical.PartKind]string{}}
	for _, p := range ev.Parts {
		if !p.Verified {
			t.Errorf("the planned %s does not verify", p.Kind)
		}
		o.parts[ical.PartKind(p.Kind)] = string(p.Text)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &o.fields); err != nil {
		t.Fatal(err)
	}
	return o
}

// field is a JSON field of the body, "<absent>" when left out.
func (o opened) field(name string) string {
	v, ok := o.fields[name]
	if !ok {
		return "<absent>"
	}
	return string(v)
}

// cal is a PUT body holding the VEVENTs, each a list of content lines.
func cal(vevents ...[]string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n")
	for _, ev := range vevents {
		b.WriteString("BEGIN:VEVENT\r\n" + strings.Join(ev, "\r\n") + "\r\nEND:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

func parse(t *testing.T, body string) ical.Incoming {
	t.Helper()
	in, err := ical.ParseWrite([]byte(body))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	return in
}

// with is lines with each old line replaced by new ("" new drops it), and
// extra lines appended; an old line that is missing fails the test.
func with(t *testing.T, lines []string, swaps map[string]string, extra ...string) []string {
	t.Helper()
	out := append([]string(nil), lines...)
	for old, repl := range swaps {
		found := false
		for i, l := range out {
			if l == old {
				out[i], found = repl, true
			}
		}
		if !found {
			t.Fatalf("with: no line %q", old)
		}
	}
	kept := out[:0]
	for _, l := range out {
		if l != "" {
			kept = append(kept, l)
		}
	}
	return append(kept, extra...)
}

var alarm15 = []string{"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15M", "DESCRIPTION:Reminder", "END:VALARM"}

// The stored master of most tests: a timed event, SEQUENCE 2, no calendar
// encrypted part, Notifications null (the calendar's defaults). Its lines
// are stored in another order than Thunderbird sends them (masterBody), as
// another client may write them: an edit must keep the stored order.
func masterParts() map[ical.PartKind]string {
	return map[ical.PartKind]string{
		ical.SharedSigned:    frag("UID:"+planUID, stamp, "SEQUENCE:2", "DTSTART;TZID=Europe/Paris:20261115T100000", "DTEND;TZID=Europe/Paris:20261115T110000"),
		ical.SharedEncrypted: frag("UID:"+planUID, stamp, "DESCRIPTION:Daily sync", "SUMMARY:Standup", "X-MOZ-GENERATION:3"),
		ical.CalendarSigned:  frag("UID:"+planUID, stamp, "STATUS:CONFIRMED", "TRANSP:OPAQUE"),
	}
}

// masterBody is what Thunderbird sends back for masterParts unchanged: the
// rendered event, with the defaults as its VALARM.
func masterBody() []string {
	return append([]string{
		"UID:" + planUID, stamp, "DTSTART;TZID=Europe/Paris:20261115T100000", "DTEND;TZID=Europe/Paris:20261115T110000",
		"SEQUENCE:2", "SUMMARY:Standup", "DESCRIPTION:Daily sync", "X-MOZ-GENERATION:3", "STATUS:CONFIRMED", "TRANSP:OPAQUE",
	}, alarm15...)
}

func plan(t *testing.T, g Group, body string) WritePlan {
	t.Helper()
	p, err := Plan(g, parse(t, body), planDefaults(t))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

// refusedWith asserts err is a *WriteRefusal with code.
func refusedWith(t *testing.T, err error, code string) {
	t.Helper()
	var r *WriteRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *WriteRefusal %s", err, code)
	}
	if r.Code != code {
		t.Fatalf("refusal code = %s (%s), want %s", r.Code, r.Reason, code)
	}
}

func onlyUpdate(t *testing.T, p WritePlan, id string) events.Op {
	t.Helper()
	if len(p.Ops) != 1 || p.Ops[0].Update == nil || p.Ops[0].Update.ID != id {
		t.Fatalf("ops = %s, want one update of %s", describe(p), id)
	}
	return p.Ops[0]
}

func onlyCreate(t *testing.T, p WritePlan) events.Op {
	t.Helper()
	if len(p.Ops) != 1 || p.Ops[0].Create == nil {
		t.Fatalf("ops = %s, want one create", describe(p))
	}
	return p.Ops[0]
}

// describe lists the ops as "update:ID", "create", "delete:ID".
func describe(p WritePlan) string {
	var out []string
	for _, op := range p.Ops {
		switch {
		case op.Create != nil:
			out = append(out, "create")
		case op.Update != nil:
			out = append(out, "update:"+op.Update.ID)
		case op.Delete != nil:
			out = append(out, "delete:"+op.Delete.ID)
		}
	}
	return "[" + strings.Join(out, " ") + "]"
}

func lineOf(part, name string) string {
	for _, l := range strings.Split(part, "\r\n") {
		if strings.HasPrefix(l, name+":") || strings.HasPrefix(l, name+";") {
			return l
		}
	}
	return ""
}

// A title edit patches the SUMMARY line in place: every other part comes
// back byte for byte, the other lines of its part too, SEQUENCE unbumped,
// Notifications still null (research B.5, constraints).
func TestTitleEditChangesOnlyThatLine(t *testing.T) {
	stored := seed{id: "row-m", parts: masterParts()}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	p := plan(t, g, cal(with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"})))
	if p.Created {
		t.Error("Created on an update")
	}
	got := open(t, onlyUpdate(t, p, "row-m"), &stored)
	want := masterParts()
	want[ical.SharedEncrypted] = strings.Replace(want[ical.SharedEncrypted], "SUMMARY:Standup", "SUMMARY:Planning", 1)
	if len(got.parts) != len(want) {
		t.Errorf("parts = %d kinds, want %d", len(got.parts), len(want))
	}
	for k, text := range want {
		if got.parts[k] != text {
			t.Errorf("%s =\n%q\nwant\n%q", k, got.parts[k], text)
		}
	}
	if n := got.field("Notifications"); n != "null" {
		t.Errorf("Notifications = %s, want null", n)
	}
}

// Research D: the alarms Thunderbird sends back are compared, by meaning,
// with the set the render served; the calendar's defaults (a null row)
// stay null, so a later change of the defaults still reaches the event.
func TestUnchangedDefaultAlarmsKeepNull(t *testing.T) {
	stored := seed{id: "row-m", parts: masterParts()}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	title := map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}
	noAlarm := with(t, masterBody(), map[string]string{
		"BEGIN:VALARM": "", "ACTION:DISPLAY": "", "TRIGGER:-PT15M": "", "DESCRIPTION:Reminder": "", "END:VALARM": "",
	})
	for _, c := range []struct {
		name string
		body []string
		want string
	}{
		{"the defaults as rendered", with(t, masterBody(), title), "null"},
		{"the defaults spelt otherwise", with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning", "TRIGGER:-PT15M": "TRIGGER:-P0DT0H15M0S"}), "null"},
		{"another alarm", with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning", "TRIGGER:-PT15M": "TRIGGER:-PT30M"}), `[{"Type":1,"Trigger":"-PT30M"}]`},
		{"no alarm", with(t, noAlarm, title), "[]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := open(t, onlyUpdate(t, plan(t, g, cal(c.body)), "row-m"), &stored)
			if n := got.field("Notifications"); n != c.want {
				t.Errorf("Notifications = %s, want %s", n, c.want)
			}
		})
	}
	t.Run("an unchanged body plans nothing", func(t *testing.T) {
		if p := plan(t, g, cal(masterBody())); len(p.Ops) != 0 {
			t.Errorf("ops = %s, want none", describe(p))
		}
	})
	t.Run("an alarm-only change updates the row", func(t *testing.T) {
		p := plan(t, g, cal(with(t, masterBody(), map[string]string{"TRIGGER:-PT15M": "TRIGGER:-PT1H"})))
		if got := open(t, onlyUpdate(t, p, "row-m"), &stored); got.field("Notifications") != `[{"Type":1,"Trigger":"-PT1H"}]` {
			t.Errorf("Notifications = %s", got.field("Notifications"))
		}
	})
	t.Run("a row without Notifications", func(t *testing.T) {
		// The render serves the parts' VALARMs (none here) and the update
		// leaves the field out, so the row keeps its own (review M3).
		bare := seed{id: "row-n", parts: masterParts(), fields: map[string]any{"Notifications": nil}}.row(t)
		gn := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{bare}}, groupKeys(t))
		if p := plan(t, gn, cal(noAlarm)); len(p.Ops) != 0 {
			t.Errorf("unchanged body: ops = %s, want none", describe(p))
		}
		if got := open(t, onlyUpdate(t, plan(t, gn, cal(with(t, noAlarm, title))), "row-n"), &bare); got.field("Notifications") != "<absent>" {
			t.Errorf("title edit: Notifications = %s, want it left out", got.field("Notifications"))
		}
		if got := open(t, onlyUpdate(t, plan(t, gn, cal(masterBody())), "row-n"), &bare); got.field("Notifications") != `[{"Type":1,"Trigger":"-PT15M"}]` {
			t.Errorf("an alarm added: Notifications = %s", got.field("Notifications"))
		}
	})
	t.Run("a custom list read back is kept as stored", func(t *testing.T) {
		custom := seed{id: "row-c", parts: masterParts(), notifications: `[{"Type":0,"Trigger":"-PT1H"}]`}.row(t)
		gc := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{custom}}, groupKeys(t))
		body := with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning", "ACTION:DISPLAY": "ACTION:EMAIL", "TRIGGER:-PT15M": "TRIGGER:-P0DT1H"})
		if got := open(t, onlyUpdate(t, plan(t, gc, cal(body)), "row-c"), &custom); got.field("Notifications") != `[{"Type":0,"Trigger":"-PT1H"}]` {
			t.Errorf("Notifications = %s", got.field("Notifications"))
		}
	})
	for _, c := range []struct {
		name string
		body []string
		want string
	}{
		{"a create with the defaults", with(t, masterBody(), nil), "null"},
		{"a create without alarms", noAlarm, "[]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := plan(t, Group{UID: planUID}, cal(c.body))
			if len(p.Ops) != 1 || p.Ops[0].Create == nil {
				t.Fatalf("ops = %s, want one create", describe(p))
			}
			if n := open(t, p.Ops[0], nil).field("Notifications"); n != c.want {
				t.Errorf("Notifications = %s, want %s", n, c.want)
			}
		})
	}
}

// Email reminders are Proton-side (Fix 4, choice A; live 2026-11-13,
// HUMAN-CHECKS 6.3 D4): with the calendar defaults of a new Proton
// calendar, an email and a display reminder, Thunderbird sends back the
// DISPLAY alarm only. The update maps the client's display alarms alone:
// a null row stays null while they are the defaults' display part, and
// otherwise gets them plus the defaults' email part; an explicit row
// keeps its email entries. An incoming EMAIL VALARM is ignored.
func TestEmailNotificationsStayProtonSide(t *testing.T) {
	d := planDefaults(t)
	d.PartDay = []ical.Notification{{Type: 0, Trigger: "-PT15M"}, {Type: 1, Trigger: "-PT15M"}}
	planD := func(t *testing.T, g Group, body []string) WritePlan {
		t.Helper()
		p, err := Plan(g, parse(t, cal(body)), d)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		return p
	}
	email := func(trigger string) []string {
		return []string{"BEGIN:VALARM", "ACTION:EMAIL", "TRIGGER:" + trigger, "DESCRIPTION:Reminder", "SUMMARY:Reminder", "ATTENDEE:mailto:" + meEmail, "END:VALARM"}
	}
	title := map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}
	noAlarm := with(t, masterBody(), map[string]string{
		"BEGIN:VALARM": "", "ACTION:DISPLAY": "", "TRIGGER:-PT15M": "", "DESCRIPTION:Reminder": "", "END:VALARM": "",
	})
	stored := seed{id: "row-m", parts: masterParts()}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	t.Run("a null row", func(t *testing.T) {
		for _, c := range []struct {
			name string
			body []string
			want string
		}{
			{"a title edit with the display default", with(t, masterBody(), title), "null"},
			{"the display default spelt otherwise", with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning", "TRIGGER:-PT15M": "TRIGGER:-P0DT0H15M0S"}), "null"},
			{"the display default and an EMAIL alarm", with(t, masterBody(), title, email("-PT2H")...), "null"},
			{"another display alarm", with(t, masterBody(), map[string]string{"TRIGGER:-PT15M": "TRIGGER:-PT30M"}), `[{"Type":1,"Trigger":"-PT30M"},{"Type":0,"Trigger":"-PT15M"}]`},
			{"no alarm", with(t, noAlarm, title), `[{"Type":0,"Trigger":"-PT15M"}]`},
		} {
			got := open(t, onlyUpdate(t, planD(t, g, c.body), "row-m"), &stored)
			if n := got.field("Notifications"); n != c.want {
				t.Errorf("%s: Notifications = %s, want %s", c.name, n, c.want)
			}
		}
		if p := planD(t, g, masterBody()); len(p.Ops) != 0 {
			t.Errorf("the object re-sent as served (display alarm only): ops = %s, want none", describe(p))
		}
	})
	t.Run("an explicit list with an email entry", func(t *testing.T) {
		const list = `[{"Type":1,"Trigger":"-PT15M"},{"Type":0,"Trigger":"-PT1H"}]`
		custom := seed{id: "row-c", parts: masterParts(), notifications: list}.row(t)
		gc := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{custom}}, groupKeys(t))
		if p := planD(t, gc, masterBody()); len(p.Ops) != 0 {
			t.Errorf("the object re-sent as served: ops = %s, want none", describe(p))
		}
		if p := planD(t, gc, with(t, masterBody(), nil, email("-PT3H")...)); len(p.Ops) != 0 {
			t.Errorf("an EMAIL alarm added: ops = %s, want none (ignored)", describe(p))
		}
		for _, c := range []struct {
			name string
			body []string
			want string
		}{
			{"a title edit", with(t, masterBody(), title), list},
			{"another display alarm", with(t, masterBody(), map[string]string{"TRIGGER:-PT15M": "TRIGGER:-PT45M"}), `[{"Type":1,"Trigger":"-PT45M"},{"Type":0,"Trigger":"-PT1H"}]`},
			{"no alarm", with(t, noAlarm, title), `[{"Type":0,"Trigger":"-PT1H"}]`},
		} {
			got := open(t, onlyUpdate(t, planD(t, gc, c.body), "row-c"), &custom)
			if n := got.field("Notifications"); n != c.want {
				t.Errorf("%s: Notifications = %s, want %s", c.name, n, c.want)
			}
		}
	})
	t.Run("the kept email entries count in Proton's limit of 10", func(t *testing.T) {
		custom := seed{id: "row-c", parts: masterParts(), notifications: `[{"Type":0,"Trigger":"-PT1H"}]`}.row(t)
		gc := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{custom}}, groupKeys(t))
		var ten []string
		want := make([]string, 0, 10)
		for i := 1; i <= 10; i++ {
			ten = append(ten, "BEGIN:VALARM", "ACTION:DISPLAY", fmt.Sprintf("TRIGGER:-PT%dM", i), "END:VALARM")
			if i < 10 {
				want = append(want, fmt.Sprintf(`{"Type":1,"Trigger":"-PT%dM"}`, i))
			}
		}
		p := planD(t, gc, append(slices.Clone(noAlarm), ten...))
		got := open(t, onlyUpdate(t, p, "row-c"), &custom)
		if n, w := got.field("Notifications"), "["+strings.Join(want, ",")+`,{"Type":0,"Trigger":"-PT1H"}]`; n != w {
			t.Errorf("ten display alarms: Notifications = %s, want the first nine and the email entry", n)
		}
		if !slices.Contains(p.Notes, noteAlarms) {
			t.Errorf("notes %v lack %q", p.Notes, noteAlarms)
		}
	})
	t.Run("a create", func(t *testing.T) {
		for _, c := range []struct {
			name string
			body []string
			want string
		}{
			{"the display default", masterBody(), `[{"Type":1,"Trigger":"-PT15M"}]`},
			{"the display default and an EMAIL alarm", with(t, masterBody(), nil, email("-PT15M")...), `[{"Type":1,"Trigger":"-PT15M"}]`},
			{"an EMAIL alarm only", with(t, noAlarm, nil, email("-PT15M")...), "[]"},
		} {
			if n := open(t, onlyCreate(t, planD(t, Group{UID: planUID}, c.body)), nil).field("Notifications"); n != c.want {
				t.Errorf("%s: Notifications = %s, want %s", c.name, n, c.want)
			}
		}
	})
}

// Ruling M4-R4: SEQUENCE is max(incoming, stored), stored+1 when the times
// change and the client did not bump it; never lowered, never dropped, not
// bumped by a text edit.
func TestTimeEditBumpsSequence(t *testing.T) {
	stored := seed{id: "row-m", parts: masterParts()}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	moved := map[string]string{
		"DTSTART;TZID=Europe/Paris:20261115T100000": "DTSTART;TZID=Europe/Paris:20261115T140000",
		"DTEND;TZID=Europe/Paris:20261115T110000":   "DTEND;TZID=Europe/Paris:20261115T150000",
	}
	withSeq := func(seq string, swaps map[string]string) []string {
		m := map[string]string{"SEQUENCE:2": seq}
		for k, v := range swaps {
			m[k] = v
		}
		return with(t, masterBody(), m)
	}
	for _, c := range []struct {
		name string
		body []string
		want string
	}{
		{"moved, not bumped", withSeq("SEQUENCE:2", moved), "SEQUENCE:3"},
		{"moved, sent lower", withSeq("SEQUENCE:0", moved), "SEQUENCE:3"},
		{"moved, bumped by the client", withSeq("SEQUENCE:7", moved), "SEQUENCE:7"},
		{"moved, no SEQUENCE sent", withSeq("", moved), "SEQUENCE:3"},
		{"same instants in another zone", withSeq("SEQUENCE:2", map[string]string{
			"DTSTART;TZID=Europe/Paris:20261115T100000": "DTSTART;TZID=Europe/Berlin:20261115T100000",
			"DTEND;TZID=Europe/Paris:20261115T110000":   "DTEND;TZID=Europe/Berlin:20261115T110000",
		}), "SEQUENCE:3"},
		{"title only", withSeq("SEQUENCE:2", map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}), "SEQUENCE:2"},
		{"title only, sent lower", withSeq("SEQUENCE:1", map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}), "SEQUENCE:2"},
		{"title only, no SEQUENCE sent", withSeq("", map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}), "SEQUENCE:2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := open(t, onlyUpdate(t, plan(t, g, cal(c.body)), "row-m"), &stored)
			if l := lineOf(got.parts[ical.SharedSigned], "SEQUENCE"); l != c.want {
				t.Errorf("shared signed SEQUENCE line = %q, want %q", l, c.want)
			}
		})
	}
	t.Run("a stored SEQUENCE:0 the body leaves out stays", func(t *testing.T) {
		parts := masterParts()
		parts[ical.SharedSigned] = strings.Replace(parts[ical.SharedSigned], "SEQUENCE:2", "SEQUENCE:0", 1)
		zero := seed{id: "row-z", parts: parts}.row(t)
		gz := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{zero}}, groupKeys(t))
		got := open(t, onlyUpdate(t, plan(t, gz, cal(withSeq("", map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}))), "row-z"), &zero)
		if got.parts[ical.SharedSigned] != parts[ical.SharedSigned] {
			t.Errorf("shared signed part =\n%q\nwant it as stored", got.parts[ical.SharedSigned])
		}
	})
}

var paris = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic(err)
	}
	return loc
}()

// rid is the recurrence ID of the weekly series' occurrence on day d of
// November 2026, 10:00 Paris time (ParseWrite's key, the row's column).
func rid(d int) *int64 {
	v := time.Date(2026, 11, d, 10, 0, 0, 0, paris).Unix()
	return &v
}

func seriesParts(seq string) map[ical.PartKind]string {
	p := masterParts()
	p[ical.SharedSigned] = frag("UID:"+planUID, stamp, "DTSTART;TZID=Europe/Paris:20261115T100000",
		"DTEND;TZID=Europe/Paris:20261115T110000", "RRULE:FREQ=WEEKLY;COUNT=10", seq)
	return p
}

func seriesBody(seq string) []string {
	out := masterBody()
	for i, l := range out {
		if l == "SEQUENCE:2" {
			out[i] = seq
		}
	}
	return append(out, "RRULE:FREQ=WEEKLY;COUNT=10")
}

// exceptionParts is the stored exception of day d, moved to 12:00.
func exceptionParts(d int, seq string) map[ical.PartKind]string {
	day := fmt.Sprintf("202611%02d", d)
	return map[ical.PartKind]string{
		ical.SharedSigned: frag("UID:"+planUID, stamp, "RECURRENCE-ID;TZID=Europe/Paris:"+day+"T100000",
			"DTSTART;TZID=Europe/Paris:"+day+"T120000", "DTEND;TZID=Europe/Paris:"+day+"T130000", seq),
		ical.SharedEncrypted: frag("UID:"+planUID, stamp, "SUMMARY:Standup moved"),
		ical.CalendarSigned:  frag("UID:"+planUID, stamp, "STATUS:CONFIRMED"),
	}
}

func exceptionBody(d int, seq string) []string {
	day := fmt.Sprintf("202611%02d", d)
	return append([]string{
		"UID:" + planUID, stamp, "RECURRENCE-ID;TZID=Europe/Paris:" + day + "T100000",
		"DTSTART;TZID=Europe/Paris:" + day + "T120000", "DTEND;TZID=Europe/Paris:" + day + "T130000", seq,
		"SUMMARY:Standup moved", "STATUS:CONFIRMED",
	}, alarm15...)
}

// Research E.2: an exception row is written with a SEQUENCE at least its
// master's, or Proton refuses it (2001); a create mints its keys.
func TestExceptionCreateHasSequenceAtLeastMaster(t *testing.T) {
	g := group(t, seed{id: "row-m", parts: seriesParts("SEQUENCE:4")})
	for _, seq := range []string{"SEQUENCE:0", "SEQUENCE:3"} {
		t.Run(seq, func(t *testing.T) {
			p := plan(t, g, cal(seriesBody("SEQUENCE:4"), exceptionBody(22, seq)))
			if len(p.Ops) != 1 || p.Ops[0].Create == nil {
				t.Fatalf("ops = %s, want one create (the master is unchanged)", describe(p))
			}
			if p.Created {
				t.Error("Created for a new exception of an existing event")
			}
			body := p.Ops[0].Create
			if body.SharedKeyPacket == "" {
				t.Error("the create carries no shared key packet")
			}
			got := open(t, p.Ops[0], nil)
			ss := got.parts[ical.SharedSigned]
			if l := lineOf(ss, "SEQUENCE"); l != "SEQUENCE:4" {
				t.Errorf("SEQUENCE line = %q, want SEQUENCE:4", l)
			}
			if l := lineOf(ss, "RECURRENCE-ID"); l != "RECURRENCE-ID;TZID=Europe/Paris:20261122T100000" {
				t.Errorf("RECURRENCE-ID line = %q", l)
			}
		})
	}
	t.Run("a higher one is kept", func(t *testing.T) {
		p := plan(t, g, cal(seriesBody("SEQUENCE:4"), exceptionBody(22, "SEQUENCE:9")))
		if l := lineOf(open(t, onlyCreate(t, p), nil).parts[ical.SharedSigned], "SEQUENCE"); l != "SEQUENCE:9" {
			t.Errorf("SEQUENCE line = %q, want SEQUENCE:9", l)
		}
	})
}

// Constraints: deleting one occurrence is the master's new EXDATE, in both
// signed parts, plus the deletion of that occurrence's exception row, in
// one call, master first.
func TestDeleteOneOccurrenceOfASeries(t *testing.T) {
	t.Run("an occurrence without an exception row", func(t *testing.T) {
		stored := seed{id: "row-m", parts: seriesParts("SEQUENCE:1")}.row(t)
		g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
		p := plan(t, g, cal(append(seriesBody("SEQUENCE:1"), "EXDATE;TZID=Europe/Paris:20261129T100000")))
		got := open(t, onlyUpdate(t, p, "row-m"), &stored)
		for _, k := range []ical.PartKind{ical.SharedSigned, ical.CalendarSigned} {
			if l := lineOf(got.parts[k], "EXDATE"); l != "EXDATE;TZID=Europe/Paris:20261129T100000" {
				t.Errorf("%s EXDATE line = %q", k, l)
			}
		}
		if l := lineOf(got.parts[ical.SharedSigned], "SEQUENCE"); l != "SEQUENCE:2" {
			t.Errorf("SEQUENCE line = %q, want SEQUENCE:2 (an EXDATE is a shared signed change)", l)
		}
	})
	t.Run("an occurrence with an exception row", func(t *testing.T) {
		stored := seed{id: "row-m", parts: seriesParts("SEQUENCE:1")}.row(t)
		exc := seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1")}.row(t)
		g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored, exc}}, groupKeys(t))
		p := plan(t, g, cal(append(seriesBody("SEQUENCE:1"), "EXDATE;TZID=Europe/Paris:20261122T100000")))
		if got := describe(p); got != "[update:row-m delete:row-x22]" {
			t.Fatalf("ops = %s", got)
		}
		got := open(t, p.Ops[0], &stored)
		for _, k := range []ical.PartKind{ical.SharedSigned, ical.CalendarSigned} {
			if l := lineOf(got.parts[k], "EXDATE"); l != "EXDATE;TZID=Europe/Paris:20261122T100000" {
				t.Errorf("%s EXDATE line = %q", k, l)
			}
		}
	})
}

// Research E.2 (mirror the body): a master whose times change keeps the
// exception rows the body still holds (updated, SEQUENCE raised to the
// master's), creates the new ones and deletes the absent ones, in one call:
// master, exceptions by recurrence ID, deletes.
func TestMasterEditMirrorsExceptions(t *testing.T) {
	stored := seed{id: "row-m", parts: seriesParts("SEQUENCE:1")}.row(t)
	x22 := seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1")}.row(t)
	x29 := seed{id: "row-x29", rid: rid(29), parts: exceptionParts(29, "SEQUENCE:1")}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored, x22, x29}}, groupKeys(t))
	master := with(t, seriesBody("SEQUENCE:1"), map[string]string{
		"DTSTART;TZID=Europe/Paris:20261115T100000": "DTSTART;TZID=Europe/Paris:20261115T090000",
		"DTEND;TZID=Europe/Paris:20261115T110000":   "DTEND;TZID=Europe/Paris:20261115T100000",
	})
	p := plan(t, g, cal(master, exceptionBody(22, "SEQUENCE:1"), exceptionBody(26, "SEQUENCE:1")))
	if got := describe(p); got != "[update:row-m update:row-x22 create delete:row-x29]" {
		t.Fatalf("ops = %s", got)
	}
	m := open(t, p.Ops[0], &stored)
	if l := lineOf(m.parts[ical.SharedSigned], "DTSTART"); l != "DTSTART;TZID=Europe/Paris:20261115T090000" {
		t.Errorf("master DTSTART = %q", l)
	}
	if l := lineOf(m.parts[ical.SharedSigned], "SEQUENCE"); l != "SEQUENCE:2" {
		t.Errorf("master SEQUENCE = %q, want SEQUENCE:2", l)
	}
	kept := open(t, p.Ops[1], &x22)
	if l := lineOf(kept.parts[ical.SharedSigned], "SEQUENCE"); l != "SEQUENCE:2" {
		t.Errorf("kept exception SEQUENCE = %q, want SEQUENCE:2 (the master's)", l)
	}
	if l := lineOf(kept.parts[ical.SharedSigned], "RECURRENCE-ID"); l != "RECURRENCE-ID;TZID=Europe/Paris:20261122T100000" {
		t.Errorf("kept exception RECURRENCE-ID = %q", l)
	}
	added := open(t, p.Ops[2], nil)
	if l := lineOf(added.parts[ical.SharedSigned], "RECURRENCE-ID"); l != "RECURRENCE-ID;TZID=Europe/Paris:20261126T100000" {
		t.Errorf("new exception RECURRENCE-ID = %q", l)
	}
	if l := lineOf(added.parts[ical.SharedSigned], "SEQUENCE"); l != "SEQUENCE:2" {
		t.Errorf("new exception SEQUENCE = %q, want SEQUENCE:2", l)
	}
}

// attendeeParts is a master with an organizer and one attendee (token tok1).
func attendeeParts() map[ical.PartKind]string {
	p := masterParts()
	p[ical.SharedSigned] = frag("UID:"+planUID, stamp, "DTSTART;TZID=Europe/Paris:20261115T100000",
		"DTEND;TZID=Europe/Paris:20261115T110000", "SEQUENCE:2", "ORGANIZER;CN=Me:mailto:"+meEmail)
	p[ical.Attendees] = frag("UID:"+planUID, stamp, "ATTENDEE;CN=Ann;ROLE=REQ-PARTICIPANT;X-PM-TOKEN=tok1:mailto:ann@x.test")
	return p
}

const annStatus = `[{"ID":"att1","Token":"tok1","Status":3,"Comment":{"Message":"see you","Type":0}}]`

func attendeeBody() []string {
	return append(masterBody(), "ORGANIZER;CN=Me:mailto:"+meEmail, "ATTENDEE;CN=Ann;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:ann@x.test")
}

// Constraints, SPEC §8.4: adding or removing an attendee, creating an
// event with attendees, or an occurrence with another attendee set than its
// master's (ruling M4-R28), is refused (invitations are out of scope).
func TestAttendeeChangeIsRefused(t *testing.T) {
	g := group(t, seed{id: "row-m", parts: attendeeParts(), attendees: annStatus})
	for _, c := range []struct {
		name string
		g    Group
		body string
	}{
		{"an attendee added", g, cal(append(attendeeBody(), "ATTENDEE;CN=Bob:mailto:bob@x.test"))},
		{"an attendee removed", g, cal(with(t, attendeeBody(), map[string]string{"ATTENDEE;CN=Ann;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:ann@x.test": ""}))},
		{"a create with organizer and attendee", Group{UID: planUID}, cal(attendeeBody())},
		{"an occurrence with another attendee", g, cal(attendeeBody(), append(exceptionBody(22, "SEQUENCE:2"),
			"ORGANIZER;CN=Me:mailto:"+meEmail, "ATTENDEE;CN=Ann:mailto:ann@x.test", "ATTENDEE;CN=Bob:mailto:bob@x.test"))},
		{"an occurrence without the series' attendee", g, cal(attendeeBody(), append(exceptionBody(22, "SEQUENCE:2"),
			"ORGANIZER;CN=Me:mailto:"+meEmail, "ATTENDEE;CN=Bob:mailto:bob@x.test"))},
		{"an occurrence without attendees", g, cal(attendeeBody(), exceptionBody(22, "SEQUENCE:2"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Plan(c.g, parse(t, c.body), planDefaults(t))
			refusedWith(t, err, RefuseAttendeeChange)
		})
	}
	t.Run("the same attendees pass", func(t *testing.T) {
		if p := plan(t, g, cal(with(t, attendeeBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}))); len(p.Ops) != 1 {
			t.Errorf("ops = %s, want the master's update", describe(p))
		}
	})
}

// Constraints, research B.3: a row with an AddressKeyPacket is someone
// else's invitation: neither rewritten nor deleted.
func TestInvitationRowIsRefused(t *testing.T) {
	g := group(t, seed{id: "row-m", parts: masterParts(), invitation: true})
	if g.Rows[0].Placeholder {
		t.Fatal("precondition: the invitation row does not open")
	}
	_, err := Plan(g, parse(t, cal(with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}))), planDefaults(t))
	refusedWith(t, err, RefuseInvitation)
	_, err = PlanDelete(g)
	refusedWith(t, err, RefuseInvitation)
	t.Run("an invitation that does not open", func(t *testing.T) {
		// Ruling M4-R27: a delete skips the trust checks, never this one.
		g := group(t, seed{id: "row-m", parts: masterParts()}, seed{id: "row-b", parts: masterParts(), strangerInvitation: true})
		if !g.Rows[1].Placeholder {
			t.Fatal("precondition: the invitation row opens")
		}
		_, err := PlanDelete(g)
		refusedWith(t, err, RefuseInvitation)
	})
}

// Ruling M3-R4, OPEN.md: a group the service serves as a placeholder (keys
// that failed, a row that does not decrypt, notifications that do not read)
// is refused; the X-PCB-PLACEHOLDER property decides nothing. Deleting it is
// planned (ruling M4-R27).
func TestPlaceholderGroupIsRefused(t *testing.T) {
	body := cal(with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}))
	noKeys := groupKeys(t)
	noKeys.Calendar = nil
	for _, c := range []struct {
		name string
		g    Group
	}{
		{"the calendar's keys failed", OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{seed{id: "row-m", parts: masterParts()}.row(t)}}, noKeys)},
		{"a row that does not decrypt", group(t, seed{id: "row-m", parts: masterParts(), otherCal: true})},
		{"notifications that do not read", group(t, seed{id: "row-m", parts: masterParts(), notifications: `[{"Trigger":"-PT15M"}]`})},
		{"one exception that does not decrypt", group(t, seed{id: "row-m", parts: seriesParts("SEQUENCE:1")},
			seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1"), otherCal: true})},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := body
			if len(c.g.Rows) > 1 {
				b = cal(seriesBody("SEQUENCE:1"), exceptionBody(22, "SEQUENCE:1"))
			}
			_, err := Plan(c.g, parse(t, b), planDefaults(t))
			refusedWith(t, err, RefusePlaceholder)
			deletesEveryRow(t, c.g)
		})
	}
	t.Run("a readable event that carries the marker", func(t *testing.T) {
		parts := masterParts()
		parts[ical.SharedEncrypted] = frag("UID:"+planUID, stamp, "SUMMARY:Standup", "DESCRIPTION:Daily sync", "X-MOZ-GENERATION:3", "X-PCB-PLACEHOLDER:1")
		stored := seed{id: "row-m", parts: parts}.row(t)
		g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
		p := plan(t, g, cal(with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}, "X-PCB-PLACEHOLDER:1")))
		got := open(t, onlyUpdate(t, p, "row-m"), &stored)
		if strings.Contains(got.parts[ical.SharedEncrypted], "X-PCB-PLACEHOLDER") {
			t.Error("the marker was written back")
		}
	})
}

// Every refusal of the codec and of sealing becomes a *WriteRefusal with a
// stable pcb name and the codec's fixed reason; any other error stays a
// fault (constraints, rulings M4-R13, M4-R15, M4-R16, M4-R19).
func TestRefusalMapping(t *testing.T) {
	for _, code := range []ical.RefusalCode{
		ical.NotOneUID, ical.TooMany, ical.Floating, ical.UnknownTZID, ical.UnsupportedRecurrence,
		ical.Range, ical.RDate, ical.ExRule, ical.BadData, ical.MisroutedStoredLine,
	} {
		t.Run(string(code), func(t *testing.T) {
			err := asRefusal(fmt.Errorf("wrapped: %w", &ical.Refusal{Code: code, Reason: "fixed reason"}))
			refusedWith(t, err, "pcb:"+string(code))
			var r *WriteRefusal
			if errors.As(err, &r); r.Reason != "fixed reason" {
				t.Errorf("reason = %q, want the codec's", r.Reason)
			}
		})
	}
	for _, c := range []struct {
		err  error
		code string
	}{
		{cards.ErrMisroutedProperty, RefuseMisrouted},
		{cards.ErrUntrustedSessionKey, RefuseUntrustedKey},
		{cards.ErrNeedsNewSessionKey, RefuseUnsupportedChange},
	} {
		t.Run(c.err.Error(), func(t *testing.T) {
			refusedWith(t, asRefusal(fmt.Errorf("sealing the shared encrypted part: %w", c.err)), c.code)
		})
	}
	t.Run("a fault stays a fault", func(t *testing.T) {
		fault := errors.New("signing failed")
		if err := asRefusal(fault); err != fault { //nolint:errorlint // the very value, not a wrapper
			t.Errorf("asRefusal(fault) = %v, want it unchanged", err)
		}
		if asRefusal(nil) != nil {
			t.Error("asRefusal(nil) is not nil")
		}
	})
	if RefuseMisrouted != "pcb:misrouted-stored-line" {
		t.Errorf("RefuseMisrouted = %s, SPEC §5.3 names pcb:misrouted-stored-line", RefuseMisrouted)
	}
}

func titleEdit(t *testing.T) string {
	return cal(with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}))
}

// deletesEveryRow asserts that PlanDelete plans the deletion of g's rows,
// in order (ruling M4-R27: a delete skips the trust checks).
func deletesEveryRow(t *testing.T, g Group) {
	t.Helper()
	p, err := PlanDelete(g)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	var want []string
	for _, r := range g.Rows {
		want = append(want, "delete:"+r.ID)
	}
	if got := describe(p); got != "["+strings.Join(want, " ")+"]" {
		t.Errorf("delete ops = %s, want %v", got, want)
	}
}

// The refusals of Patch, Seal and SessionKeys, reached through Plan.
func TestPlanRefusalsFromPatchSealAndKeys(t *testing.T) {
	t.Run("a stored signed part holding an encrypted-only line", func(t *testing.T) {
		clear := map[ical.PartKind]string{ical.CalendarSigned: frag("UID:"+planUID, stamp, "STATUS:CONFIRMED", "TRANSP:OPAQUE", "LOCATION:Room 4")}
		_, err := Plan(group(t, seed{id: "row-m", parts: masterParts(), clear: clear}), parse(t, titleEdit(t)), planDefaults(t))
		refusedWith(t, err, RefuseMisrouted)
	})
	t.Run("an update needing a session key the row lacks", func(t *testing.T) {
		// No calendar encrypted part, so no calendar session key: a first
		// COMMENT would need one (rulings M4-R13, M4-R15).
		_, err := Plan(group(t, seed{id: "row-m", parts: masterParts()}), parse(t, cal(append(masterBody(), "COMMENT:bring slides"))), planDefaults(t))
		refusedWith(t, err, RefuseUnsupportedChange)
	})
	t.Run("an unproven calendar session key, although the edit leaves it unused", func(t *testing.T) {
		// The calendar encrypted part holds nothing but UID and DTSTAMP:
		// nothing proves its key (ical.ProvesKey). SessionKeys errs, and
		// any error refuses.
		parts := masterParts()
		parts[ical.CalendarEncrypted] = frag("UID:"+planUID, stamp)
		g := group(t, seed{id: "row-m", parts: parts})
		if _, _, err := g.Rows[0].Event.SessionKeys(); !errors.Is(err, cards.ErrUntrustedSessionKey) {
			t.Fatalf("precondition: SessionKeys err = %v", err)
		}
		_, err := Plan(g, parse(t, titleEdit(t)), planDefaults(t))
		refusedWith(t, err, RefuseUntrustedKey)
	})
	t.Run("parts proving another UID", func(t *testing.T) {
		// The row's UID column is the event's, but its parts were made for
		// another UID: the proof is bound to the row's UID and fails.
		parts := masterParts()
		for k, text := range parts {
			parts[k] = strings.ReplaceAll(text, "UID:"+planUID, "UID:other@pcb.test")
		}
		_, err := Plan(group(t, seed{id: "row-m", parts: parts}), parse(t, titleEdit(t)), planDefaults(t))
		refusedWith(t, err, RefuseUntrustedKey)
	})
}

// fullParts is a master with every kind of part we write.
func fullParts() map[ical.PartKind]string {
	p := attendeeParts()
	p[ical.CalendarEncrypted] = frag("UID:"+planUID, stamp, "COMMENT:bring slides")
	return p
}

// Rulings M4-R18, M4-R19: the group is checked before anything is planned:
// a part not authored by us, a part of ours unsigned (any kind, the
// attendees and personal parts included, unlike the render's marker) or
// failing its signature, a part that does not decrypt, a row of another
// UID. The body adds an attendee, which planning would refuse otherwise
// (the control case): the group's refusal comes first. A delete re-signs
// nothing and skips these checks (ruling M4-R27): it is planned.
func TestGroupChecksComeBeforePlanning(t *testing.T) {
	body := parse(t, cal(append(with(t, masterBody(), map[string]string{"SEQUENCE:2": ""}), "SEQUENCE:2", "COMMENT:bring slides",
		"ORGANIZER;CN=Me:mailto:"+meEmail, "ATTENDEE;CN=Ann:mailto:ann@x.test", "ATTENDEE;CN=Bob:mailto:bob@x.test")))
	base := func() seed { return seed{id: "row-m", parts: fullParts(), attendees: annStatus} }
	t.Run("control", func(t *testing.T) {
		_, err := Plan(group(t, base()), body, planDefaults(t))
		refusedWith(t, err, RefuseAttendeeChange)
	})
	all := []ical.PartKind{ical.SharedSigned, ical.SharedEncrypted, ical.CalendarSigned, ical.CalendarEncrypted, ical.Attendees}
	type fault struct {
		name string
		seed func() seed
		code string
	}
	var faults []fault
	for _, k := range all {
		faults = append(faults,
			fault{"foreign " + k.String(), func() seed { s := base(); s.foreign = []ical.PartKind{k}; return s }, RefuseNotOurs},
			fault{"unsigned " + k.String(), func() seed { s := base(); s.unsigned = []ical.PartKind{k}; return s }, RefuseUnverified},
			fault{"bad signature on the " + k.String(), func() seed { s := base(); s.badSig = []ical.PartKind{k}; return s }, RefuseUnverified},
		)
	}
	faults = append(faults,
		fault{"an unsigned personal part of ours", func() seed { s := base(); s.personal = "own-unsigned"; return s }, RefuseUnverified},
		fault{"a personal part that does not decrypt", func() seed { s := base(); s.personal = "undecryptable"; return s }, RefuseUndecryptable},
		// Write-I3: never re-sent, so never left behind by an update.
		fault{"a verified personal part of ours", func() seed { s := base(); s.personal = "own"; return s }, RefusePersonalPart},
		fault{"a row of another UID", func() seed { s := base(); s.uidColumn = "other@pcb.test"; return s }, RefuseUntrustedKey},
		fault{"two shared encrypted parts", func() seed { s := base(); s.dup = []ical.PartKind{ical.SharedEncrypted}; return s }, RefuseUnsupportedChange},
	)
	for _, f := range faults {
		t.Run(f.name, func(t *testing.T) {
			g := group(t, f.seed())
			if g.Rows[0].Placeholder {
				t.Fatal("precondition: the row renders as a placeholder")
			}
			p, err := Plan(g, body, planDefaults(t))
			refusedWith(t, err, f.code)
			if len(p.Ops) != 0 {
				t.Errorf("a refused plan has ops %s", describe(p))
			}
			deletesEveryRow(t, g)
		})
	}
	t.Run("two exception rows for one occurrence", func(t *testing.T) {
		g := group(t, seed{id: "row-m", parts: seriesParts("SEQUENCE:1")},
			seed{id: "row-x1", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1")},
			seed{id: "row-x2", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1")})
		_, err := Plan(g, parse(t, cal(seriesBody("SEQUENCE:1"), exceptionBody(22, "SEQUENCE:1"))), planDefaults(t))
		refusedWith(t, err, RefuseUnsupportedChange)
	})
}

// Ruling M4-R8: a body without DTSTAMP takes the write time (UTC) in every
// part; without DTEND (and DURATION), a timed event ends at its start and
// an all-day one the next day.
func TestMissingStampAndEndAreDefaulted(t *testing.T) {
	for _, c := range []struct {
		name, start, end string
	}{
		{"timed", "DTSTART;TZID=Europe/Paris:20261115T100000", "DTEND;TZID=Europe/Paris:20261115T100000"},
		{"all day", "DTSTART;VALUE=DATE:20261231", "DTEND;VALUE=DATE:20270101"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := plan(t, Group{UID: planUID}, cal([]string{"UID:" + planUID, c.start, "SUMMARY:Party"}))
			if !p.Created || len(p.Ops) != 1 || p.Ops[0].Create == nil {
				t.Fatalf("ops = %s (created %v), want one create", describe(p), p.Created)
			}
			got := open(t, p.Ops[0], nil)
			for k, text := range got.parts {
				if l := lineOf(text, "DTSTAMP"); l != "DTSTAMP:20261112T083000Z" {
					t.Errorf("%s DTSTAMP = %q, want the write time", k, l)
				}
			}
			if l := lineOf(got.parts[ical.SharedSigned], "DTEND"); l != c.end {
				t.Errorf("DTEND = %q, want %q", l, c.end)
			}
		})
	}
	t.Run("a DTSTAMP sent is kept", func(t *testing.T) {
		p := plan(t, Group{UID: planUID}, cal(masterBody()))
		if l := lineOf(open(t, onlyCreate(t, p), nil).parts[ical.SharedSigned], "DTSTAMP"); l != stamp {
			t.Errorf("DTSTAMP = %q, want the body's", l)
		}
	})
}

// Ruling M4-R9: editing an existing attendee's parameters (PARTSTAT, ROLE,
// CN) is not written: the stored line stays byte for byte, and the update
// re-sends the row's attendee statuses (research A.2: a [] would wipe the
// RSVPs).
func TestAttendeeParameterEditsAreIgnored(t *testing.T) {
	stored := seed{id: "row-m", parts: attendeeParts(), attendees: annStatus}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	body := with(t, attendeeBody(), map[string]string{
		"SUMMARY:Standup": "SUMMARY:Planning",
		"ATTENDEE;CN=Ann;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:ann@x.test": "ATTENDEE;CN=Annie;ROLE=OPT-PARTICIPANT;PARTSTAT=DECLINED:mailto:ann@x.test",
	})
	p := plan(t, g, cal(body))
	got := open(t, onlyUpdate(t, p, "row-m"), &stored)
	if got.parts[ical.Attendees] != attendeeParts()[ical.Attendees] {
		t.Errorf("attendees part =\n%q\nwant it as stored", got.parts[ical.Attendees])
	}
	if a := got.field("Attendees"); a != `[{"Token":"tok1","Status":3,"Comment":{"Message":"see you","Type":0}}]` {
		t.Errorf("Attendees = %s, want the row's", a)
	}
}

// Research A.2 (Task 5 review): an update re-sends the stored row's
// Attendees, Color and IsOrganizer, never a zero value; a create sends Color
// null and no attendee; a new exception takes its master's Color.
func TestRowFieldsOnCreateAndUpdate(t *testing.T) {
	one := 1
	stored := seed{id: "row-m", parts: seriesParts("SEQUENCE:1"), attendees: annStatus, color: "#3CBB3A", isOrganizer: &one}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	p := plan(t, g, cal(with(t, seriesBody("SEQUENCE:1"), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}), exceptionBody(22, "SEQUENCE:1")))
	if got := describe(p); got != "[update:row-m create]" {
		t.Fatalf("ops = %s", got)
	}
	up := open(t, p.Ops[0], &stored)
	for name, want := range map[string]string{
		"Color": `"#3CBB3A"`, "IsOrganizer": "1",
		"Attendees": `[{"Token":"tok1","Status":3,"Comment":{"Message":"see you","Type":0}}]`,
	} {
		if got := up.field(name); got != want {
			t.Errorf("update %s = %s, want %s", name, got, want)
		}
	}
	exc := open(t, p.Ops[1], nil)
	if got := exc.field("Color"); got != `"#3CBB3A"` {
		t.Errorf("new exception Color = %s, want the master's", got)
	}
	if got := exc.field("Attendees"); got != "[]" {
		t.Errorf("new exception Attendees = %s, want [] (its master has no attendee line)", got)
	}
	if got := exc.field("IsOrganizer"); got != "1" {
		t.Errorf("new exception IsOrganizer = %s, want the master's", got)
	}
	t.Run("a row without colour", func(t *testing.T) {
		plain := seed{id: "row-p", parts: masterParts()}.row(t)
		gp := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{plain}}, groupKeys(t))
		got := open(t, onlyUpdate(t, plan(t, gp, titleEdit(t)), "row-p"), &plain)
		for name, want := range map[string]string{"Color": "null", "Attendees": "[]", "IsOrganizer": "<absent>"} {
			if v := got.field(name); v != want {
				t.Errorf("update %s = %s, want %s", name, v, want)
			}
		}
	})
	t.Run("a create", func(t *testing.T) {
		got := open(t, onlyCreate(t, plan(t, Group{UID: planUID}, cal(masterBody()))), nil)
		for name, want := range map[string]string{"Color": "null", "Attendees": "[]", "IsOrganizer": "<absent>"} {
			if v := got.field(name); v != want {
				t.Errorf("create %s = %s, want %s", name, v, want)
			}
		}
	})
}

// Research B.5, ruling M4-R15: an update reuses the row's session keys and
// carries no key packet (its parts open with the stored key packets); a
// create mints keys and wraps them to the primary calendar key.
func TestUpdateCarriesNoKeyPacketsAndCreateMints(t *testing.T) {
	stored := seed{id: "row-m", parts: fullParts(), attendees: annStatus}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	body := with(t, append(attendeeBody(), "COMMENT:bring slides"), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"})
	op := onlyUpdate(t, plan(t, g, cal(body)), "row-m")
	if b := op.Update.Body; b.SharedKeyPacket != "" || b.CalendarKeyPacket != "" {
		t.Fatal("the update carries a key packet")
	}
	got := open(t, op, &stored) // the stored key packets open every part
	for _, k := range []ical.PartKind{ical.SharedEncrypted, ical.CalendarEncrypted, ical.Attendees} {
		if got.parts[k] == "" {
			t.Errorf("no %s", k)
		}
	}
	if !strings.Contains(got.parts[ical.SharedEncrypted], "SUMMARY:Planning") {
		t.Error("the edit is not in the shared encrypted part")
	}
	p := plan(t, Group{UID: planUID}, cal(append(masterBody(), "COMMENT:bring slides")))
	c := onlyCreate(t, p).Create
	if c.SharedKeyPacket == "" || c.CalendarKeyPacket == "" {
		t.Fatal("the create carries no key packet")
	}
	other := ring(t, keySet(t).otherCal)
	for _, kp := range []string{c.SharedKeyPacket, c.CalendarKeyPacket} {
		b, err := base64.StdEncoding.DecodeString(kp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.DecryptSessionKey(b); err == nil {
			t.Error("a key packet opens with another calendar key")
		}
	}
	if got := open(t, p.Ops[0], nil); !strings.Contains(got.parts[ical.CalendarEncrypted], "COMMENT:bring slides") {
		t.Error("the created calendar encrypted part does not open with the calendar key")
	}
}

// Ruling M4-R11, research E.3: a series whose master is gone is sent back
// with Thunderbird's faked master, which ParseWrite drops: its exception
// rows are updated (or deleted when left out), and no master and no new
// exception is ever created.
func TestExceptionsOnlyBodyUpdatesExceptionRows(t *testing.T) {
	x22 := seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1")}.row(t)
	x29 := seed{id: "row-x29", rid: rid(29), parts: exceptionParts(29, "SEQUENCE:1")}.row(t)
	orphans := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{x22, x29}}, groupKeys(t))
	faked := append(with(t, seriesBody("SEQUENCE:1"), nil), "X-MOZ-FAKED-MASTER:1")
	edited := with(t, exceptionBody(22, "SEQUENCE:1"), map[string]string{"SUMMARY:Standup moved": "SUMMARY:Standup moved again"})
	t.Run("an edited exception", func(t *testing.T) {
		in := parse(t, cal(faked, edited, exceptionBody(29, "SEQUENCE:1")))
		if in.Master != nil {
			t.Fatal("precondition: ParseWrite kept the faked master")
		}
		p, err := Plan(orphans, in, planDefaults(t))
		if err != nil {
			t.Fatal(err)
		}
		got := open(t, onlyUpdate(t, p, "row-x22"), &x22)
		if !strings.Contains(got.parts[ical.SharedEncrypted], "SUMMARY:Standup moved again") {
			t.Error("the edit is not written")
		}
	})
	t.Run("an exception left out", func(t *testing.T) {
		p := plan(t, orphans, cal(faked, exceptionBody(22, "SEQUENCE:1")))
		if got := describe(p); got != "[delete:row-x29]" {
			t.Errorf("ops = %s", got)
		}
	})
	t.Run("a new exception", func(t *testing.T) {
		_, err := Plan(orphans, parse(t, cal(faked, exceptionBody(22, "SEQUENCE:1"), exceptionBody(26, "SEQUENCE:1"))), planDefaults(t))
		refusedWith(t, err, RefuseUnsupportedChange)
	})
	t.Run("a real master", func(t *testing.T) {
		_, err := Plan(orphans, parse(t, cal(seriesBody("SEQUENCE:1"), exceptionBody(22, "SEQUENCE:1"))), planDefaults(t))
		refusedWith(t, err, RefuseUnsupportedChange)
	})
	t.Run("an event created without its master", func(t *testing.T) {
		_, err := Plan(Group{UID: planUID}, parse(t, cal(faked, exceptionBody(22, "SEQUENCE:1"))), planDefaults(t))
		refusedWith(t, err, RefuseUnsupportedChange)
	})
	t.Run("a body leaving out an existing master", func(t *testing.T) {
		g := group(t, seed{id: "row-m", parts: seriesParts("SEQUENCE:1")}, seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1")})
		_, err := Plan(g, parse(t, cal(faked, exceptionBody(22, "SEQUENCE:1"))), planDefaults(t))
		refusedWith(t, err, RefuseUnsupportedChange)
	})
}

// A create plans every row of the body in one call, master first; a DELETE
// plans every row of the group, duplicate masters included; a PUT edits the
// master the render serves (ruling M3-R3: the latest edit) and leaves a
// hidden duplicate alone.
func TestCreateDeleteAndDuplicateMasters(t *testing.T) {
	p := plan(t, Group{UID: planUID}, cal(seriesBody("SEQUENCE:1"), exceptionBody(29, "SEQUENCE:0"), exceptionBody(22, "SEQUENCE:0")))
	if got := describe(p); !p.Created || got != "[create create create]" {
		t.Fatalf("ops = %s (created %v)", got, p.Created)
	}
	for i, want := range []string{"", "RECURRENCE-ID;TZID=Europe/Paris:20261122T100000", "RECURRENCE-ID;TZID=Europe/Paris:20261129T100000"} {
		if l := lineOf(open(t, p.Ops[i], nil).parts[ical.SharedSigned], "RECURRENCE-ID"); l != want {
			t.Errorf("op %d RECURRENCE-ID = %q, want %q", i, l, want)
		}
	}
	g := group(t, seed{id: "row-a", parts: masterParts(), lastEdit: 200}, seed{id: "row-b", parts: masterParts(), lastEdit: 100},
		seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:2")})
	d, err := PlanDelete(g)
	if err != nil {
		t.Fatal(err)
	}
	if got := describe(d); got != "[delete:row-a delete:row-b delete:row-x22]" {
		t.Errorf("delete ops = %s", got)
	}
	if _, err := PlanDelete(Group{UID: planUID}); err == nil {
		t.Error("PlanDelete of no rows succeeded")
	}
	u := plan(t, g, cal(with(t, masterBody(), map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}), exceptionBody(22, "SEQUENCE:2")))
	onlyUpdate(t, u, "row-a")
}

// Plan works on the body's UID only.
func TestPlanNeedsTheGroupsUID(t *testing.T) {
	if _, err := Plan(Group{UID: "another@pcb.test"}, parse(t, cal(masterBody())), planDefaults(t)); err == nil {
		t.Error("Plan accepted a body of another UID")
	}
}

// Ruling M4-R28: Thunderbird copies the master's attendees into a new
// exception. The same set, by address (scheme and case folded), is allowed:
// the new row takes the master's attendee lines as stored, X-PM-TOKEN
// included, and its statuses, so the RSVPs stay as they are.
func TestNewExceptionTakesTheMastersAttendees(t *testing.T) {
	parts := attendeeParts()
	parts[ical.SharedSigned] = frag("UID:"+planUID, stamp, "DTSTART;TZID=Europe/Paris:20261115T100000",
		"DTEND;TZID=Europe/Paris:20261115T110000", "RRULE:FREQ=WEEKLY;COUNT=10", "SEQUENCE:2", "ORGANIZER;CN=Me:mailto:"+meEmail)
	one := 1
	g := group(t, seed{id: "row-m", parts: parts, attendees: annStatus, isOrganizer: &one})
	master := append(seriesBody("SEQUENCE:2"), "ORGANIZER;CN=Me:mailto:"+meEmail,
		"ATTENDEE;CN=Ann;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:ann@x.test")
	exception := append(exceptionBody(22, "SEQUENCE:2"), "ORGANIZER;CN=Me:mailto:"+meEmail,
		"ATTENDEE;CN=Ann;PARTSTAT=ACCEPTED:MAILTO:Ann@X.test")
	p := plan(t, g, cal(master, exception))
	got := open(t, onlyCreate(t, p), nil)
	want := frag("UID:"+planUID, stamp, "ATTENDEE;CN=Ann;ROLE=REQ-PARTICIPANT;X-PM-TOKEN=tok1:mailto:ann@x.test")
	if got.parts[ical.Attendees] != want {
		t.Errorf("attendees part =\n%q\nwant\n%q", got.parts[ical.Attendees], want)
	}
	if a := got.field("Attendees"); a != `[{"Token":"tok1","Status":3,"Comment":{"Message":"see you","Type":0}}]` {
		t.Errorf("Attendees = %s, want the master's", a)
	}
	if l := lineOf(got.parts[ical.SharedSigned], "RECURRENCE-ID"); l != "RECURRENCE-ID;TZID=Europe/Paris:20261122T100000" {
		t.Errorf("RECURRENCE-ID = %q", l)
	}
	if v := got.field("IsOrganizer"); v != "1" {
		t.Errorf("IsOrganizer = %s, want the master's", v)
	}
	// The organizer goes with the attendees: the master's line, whether the
	// client's names it otherwise or leaves it out; another address is
	// refused (review M5).
	ann := "ATTENDEE;CN=Ann;PARTSTAT=ACCEPTED:mailto:ann@x.test"
	for name, organizer := range map[string][]string{
		"the same organizer spelt otherwise": {"ORGANIZER:MAILTO:ME@PCB.TEST"},
		"no organizer":                       nil,
	} {
		t.Run(name, func(t *testing.T) {
			got := open(t, onlyCreate(t, plan(t, g, cal(master, append(append(exceptionBody(22, "SEQUENCE:2"), organizer...), ann)))), nil)
			if l := lineOf(got.parts[ical.SharedSigned], "ORGANIZER"); l != "ORGANIZER;CN=Me:mailto:"+meEmail {
				t.Errorf("ORGANIZER = %q, want the master's line", l)
			}
		})
	}
	t.Run("another organizer", func(t *testing.T) {
		_, err := Plan(g, parse(t, cal(master, append(exceptionBody(22, "SEQUENCE:2"), "ORGANIZER:mailto:bob@x.test", ann))), planDefaults(t))
		refusedWith(t, err, RefuseAttendeeChange)
	})
}

// Review M1: the row's fields the render never reads are read only when
// re-sent. A shape we cannot re-send is a fault, never a placeholder, and
// never blocks a delete; a row the render cannot open is a placeholder for
// a PUT and deleted all the same.
func TestOddRowShapes(t *testing.T) {
	t.Run("IsOrganizer of another type", func(t *testing.T) {
		g := group(t, seed{id: "row-m", parts: masterParts(), fields: map[string]any{"IsOrganizer": true}})
		if g.Rows[0].Placeholder {
			t.Fatal("precondition: the render cannot open the row")
		}
		deletesEveryRow(t, g)
		if p := plan(t, g, cal(masterBody())); len(p.Ops) != 0 { // nothing re-sent: no fault
			t.Errorf("ops = %s, want none", describe(p))
		}
		_, err := Plan(g, parse(t, titleEdit(t)), planDefaults(t))
		var r *WriteRefusal
		if err == nil || errors.As(err, &r) {
			t.Errorf("err = %v, want a fault", err)
		}
	})
	t.Run("an attendee status that is not a number", func(t *testing.T) {
		g := group(t, seed{id: "row-m", parts: masterParts(), attendees: `[{"ID":"a1","Token":"tok1","Status":"3"}]`})
		if !g.Rows[0].Placeholder {
			t.Fatal("precondition: the render opens the row")
		}
		_, err := Plan(g, parse(t, titleEdit(t)), planDefaults(t))
		refusedWith(t, err, RefusePlaceholder)
		deletesEveryRow(t, g)
	})
	t.Run("an address key packet of another type", func(t *testing.T) {
		g := group(t, seed{id: "row-m", parts: masterParts(), fields: map[string]any{"AddressKeyPacket": map[string]int{"x": 1}}})
		_, err := PlanDelete(g)
		refusedWith(t, err, RefuseInvitation)
	})
	t.Run("a row that is not JSON", func(t *testing.T) {
		g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{{ID: "row-m", UID: planUID, Raw: []byte("not json")}}}, groupKeys(t))
		_, err := PlanDelete(g)
		var r *WriteRefusal
		if err == nil || errors.As(err, &r) {
			t.Errorf("err = %v, want a fault", err)
		}
	})
}

// 7b must pass the write time: a zero one would stamp year 1.
func TestPlanNeedsTheWriteTime(t *testing.T) {
	d := planDefaults(t)
	d.Now = time.Time{}
	_, err := Plan(Group{UID: planUID}, parse(t, cal(masterBody())), d)
	var r *WriteRefusal
	if err == nil || errors.As(err, &r) {
		t.Errorf("err = %v, want a fault", err)
	}
}

// Review M4: edge cases of matching and SEQUENCE.
func TestSeriesEdgeCases(t *testing.T) {
	t.Run("an all-day series exception is updated in place", func(t *testing.T) {
		// ParseWrite keys a DATE RECURRENCE-ID at midnight UTC, as Proton's
		// RecurrenceID column: the row is matched, not deleted and re-created.
		day := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC).Unix()
		mp := map[ical.PartKind]string{
			ical.SharedSigned:    frag("UID:"+planUID, stamp, "DTSTART;VALUE=DATE:20261115", "DTEND;VALUE=DATE:20261116", "RRULE:FREQ=WEEKLY;COUNT=10", "SEQUENCE:1"),
			ical.SharedEncrypted: frag("UID:"+planUID, stamp, "SUMMARY:Holiday"),
			ical.CalendarSigned:  frag("UID:"+planUID, stamp, "STATUS:CONFIRMED"),
		}
		xp := map[ical.PartKind]string{
			ical.SharedSigned:    frag("UID:"+planUID, stamp, "RECURRENCE-ID;VALUE=DATE:20261122", "DTSTART;VALUE=DATE:20261123", "DTEND;VALUE=DATE:20261124", "SEQUENCE:1"),
			ical.SharedEncrypted: frag("UID:"+planUID, stamp, "SUMMARY:Holiday moved"),
			ical.CalendarSigned:  frag("UID:"+planUID, stamp, "STATUS:CONFIRMED"),
		}
		x := seed{id: "row-x", rid: &day, fullDay: true, parts: xp}.row(t)
		g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{seed{id: "row-m", fullDay: true, parts: mp}.row(t), x}}, groupKeys(t))
		alarm := []string{"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15H", "END:VALARM"}
		master := append([]string{"UID:" + planUID, stamp, "DTSTART;VALUE=DATE:20261115", "DTEND;VALUE=DATE:20261116",
			"RRULE:FREQ=WEEKLY;COUNT=10", "SEQUENCE:1", "SUMMARY:Holiday", "STATUS:CONFIRMED"}, alarm...)
		exc := append([]string{"UID:" + planUID, stamp, "RECURRENCE-ID;VALUE=DATE:20261122", "DTSTART;VALUE=DATE:20261123",
			"DTEND;VALUE=DATE:20261124", "SEQUENCE:1", "SUMMARY:Holiday moved again", "STATUS:CONFIRMED"}, alarm...)
		got := open(t, onlyUpdate(t, plan(t, g, cal(master, exc)), "row-x"), &x)
		if !strings.Contains(got.parts[ical.SharedEncrypted], "SUMMARY:Holiday moved again") {
			t.Error("the edit is not written")
		}
	})
	t.Run("an RRULE change raises a kept exception with its master", func(t *testing.T) {
		stored := seed{id: "row-m", parts: seriesParts("SEQUENCE:1")}.row(t)
		x22 := seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:1")}.row(t)
		g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored, x22}}, groupKeys(t))
		master := with(t, seriesBody("SEQUENCE:1"), map[string]string{"RRULE:FREQ=WEEKLY;COUNT=10": "RRULE:FREQ=WEEKLY;COUNT=12"})
		p := plan(t, g, cal(master, exceptionBody(22, "SEQUENCE:1")))
		if got := describe(p); got != "[update:row-m update:row-x22]" {
			t.Fatalf("ops = %s", got)
		}
		for i, r := range []*cache.StoredRow{&stored, &x22} {
			if l := lineOf(open(t, p.Ops[i], r).parts[ical.SharedSigned], "SEQUENCE"); l != "SEQUENCE:2" {
				t.Errorf("%s SEQUENCE = %q, want SEQUENCE:2", r.ID, l)
			}
		}
	})
	t.Run("an exception without attendees under a master with some stays editable", func(t *testing.T) {
		// Ruling M4-R28 applies to new exceptions; an existing row is
		// compared with its own attendees (none).
		parts := attendeeParts()
		parts[ical.SharedSigned] = frag("UID:"+planUID, stamp, "DTSTART;TZID=Europe/Paris:20261115T100000",
			"DTEND;TZID=Europe/Paris:20261115T110000", "RRULE:FREQ=WEEKLY;COUNT=10", "SEQUENCE:2", "ORGANIZER;CN=Me:mailto:"+meEmail)
		x22 := seed{id: "row-x22", rid: rid(22), parts: exceptionParts(22, "SEQUENCE:2")}.row(t)
		g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{seed{id: "row-m", parts: parts, attendees: annStatus}.row(t), x22}}, groupKeys(t))
		master := append(seriesBody("SEQUENCE:2"), "ORGANIZER;CN=Me:mailto:"+meEmail,
			"ATTENDEE;CN=Ann;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:ann@x.test")
		exc := with(t, exceptionBody(22, "SEQUENCE:2"), map[string]string{"SUMMARY:Standup moved": "SUMMARY:Standup moved again"})
		got := open(t, onlyUpdate(t, plan(t, g, cal(master, exc)), "row-x22"), &x22)
		if !strings.Contains(got.parts[ical.SharedEncrypted], "SUMMARY:Standup moved again") {
			t.Error("the edit is not written")
		}
	})
}

// Fix 3: only a master's DTSTART must be an occurrence of its RRULE. An
// exception moved to a day off the rule (here a Wednesday of a Mondays
// series) carries no rule and is planned, with the series and against a
// stored master alike; the same day as the master's start is refused.
func TestAnExceptionMovedOffTheRuleIsPlanned(t *testing.T) {
	master := []string{"UID:" + planUID, stamp, "DTSTART;TZID=Europe/Paris:20261116T100000", "DTEND;TZID=Europe/Paris:20261116T110000",
		"RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=3", "SUMMARY:Standup"}
	exception := []string{"UID:" + planUID, stamp, "RECURRENCE-ID;TZID=Europe/Paris:20261123T100000",
		"DTSTART;TZID=Europe/Paris:20261125T100000", "DTEND;TZID=Europe/Paris:20261125T110000", "SUMMARY:Standup moved"}
	p := plan(t, Group{UID: planUID}, cal(master, exception))
	if len(p.Ops) != 2 || p.Ops[0].Create == nil || p.Ops[1].Create == nil {
		t.Errorf("the series with its exception: ops = %s, want two creates", describe(p))
	}

	stored := seed{id: "row-m", parts: map[ical.PartKind]string{
		ical.SharedSigned:    frag("UID:"+planUID, stamp, "DTSTART;TZID=Europe/Paris:20261116T100000", "DTEND;TZID=Europe/Paris:20261116T110000", "RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=3"),
		ical.SharedEncrypted: frag("UID:"+planUID, stamp, "SUMMARY:Standup"),
	}}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	if p := plan(t, g, cal(master, exception)); !slices.ContainsFunc(p.Ops, func(op events.Op) bool { return op.Create != nil }) {
		t.Errorf("an exception added to the stored series: ops = %s, want its create", describe(p))
	}

	offMaster := with(t, master, map[string]string{
		"DTSTART;TZID=Europe/Paris:20261116T100000": "DTSTART;TZID=Europe/Paris:20261125T100000",
		"DTEND;TZID=Europe/Paris:20261116T110000":   "DTEND;TZID=Europe/Paris:20261125T110000",
	})
	_, err := Plan(Group{UID: planUID}, parse(t, cal(offMaster)), planDefaults(t))
	refusedWith(t, err, "pcb:"+string(ical.UnsupportedRecurrence))
}

// Fix 5 (live 2026-11-13, HUMAN-CHECKS 6.3 D7): Proton refuses an
// exception of a master whose signed part has no SEQUENCE, so every master
// pcb writes carries one: the client's, or 0. A master stored without one
// gets SEQUENCE:0 when it is written anyway, and when an exception is
// created beside it; an unchanged body still plans nothing.
func TestEveryWrittenMasterCarriesASequence(t *testing.T) {
	seqOf := func(t *testing.T, o opened) string {
		t.Helper()
		var lines []string
		for _, text := range o.parts {
			for l := range strings.SplitSeq(text, "\r\n") {
				if strings.HasPrefix(l, "SEQUENCE") {
					lines = append(lines, l)
				}
			}
		}
		return strings.Join(lines, " ")
	}
	noSeq := with(t, masterBody(), map[string]string{"SEQUENCE:2": ""})
	t.Run("a create", func(t *testing.T) {
		for _, c := range []struct {
			name string
			body string
			want string
		}{
			{"a single event without SEQUENCE", cal(noSeq), "SEQUENCE:0"},
			{"a series without SEQUENCE", cal(with(t, noSeq, nil, "RRULE:FREQ=DAILY;COUNT=5")), "SEQUENCE:0"},
			{"the client's SEQUENCE", cal(masterBody()), "SEQUENCE:2"},
		} {
			if got := seqOf(t, open(t, onlyCreate(t, plan(t, Group{UID: planUID}, c.body)), nil)); got != c.want {
				t.Errorf("%s: %q, want %q", c.name, got, c.want)
			}
		}
	})
	parts := masterParts()
	parts[ical.SharedSigned] = strings.Replace(parts[ical.SharedSigned], "SEQUENCE:2\r\n", "", 1)
	parts[ical.SharedSigned] = strings.Replace(parts[ical.SharedSigned], "DTEND;TZID=Europe/Paris:20261115T110000", "DTEND;TZID=Europe/Paris:20261115T110000\r\nRRULE:FREQ=DAILY;COUNT=5", 1)
	stored := seed{id: "row-m", parts: parts}.row(t)
	g := OpenGroup(cache.Group{UID: planUID, Rows: []cache.StoredRow{stored}}, groupKeys(t))
	series := with(t, noSeq, map[string]string{"DTEND;TZID=Europe/Paris:20261115T110000": "DTEND;TZID=Europe/Paris:20261115T110000\r\nRRULE:FREQ=DAILY;COUNT=5"})
	series = strings.Split(strings.Join(series, "\r\n"), "\r\n")
	if p := plan(t, g, cal(series)); len(p.Ops) != 0 {
		t.Errorf("the stored series re-sent unchanged, no SEQUENCE: ops = %s, want none", describe(p))
	}
	if got := seqOf(t, open(t, onlyUpdate(t, plan(t, g, cal(with(t, series, map[string]string{"SUMMARY:Standup": "SUMMARY:Planning"}))), "row-m"), &stored)); got != "SEQUENCE:0" {
		t.Errorf("a title edit of the series stored without SEQUENCE: %q, want SEQUENCE:0", got)
	}
	exception := []string{"UID:" + planUID, stamp, "RECURRENCE-ID;TZID=Europe/Paris:20261116T100000",
		"DTSTART;TZID=Europe/Paris:20261116T120000", "DTEND;TZID=Europe/Paris:20261116T130000", "SEQUENCE:1", "SUMMARY:Standup moved"}
	p := plan(t, g, cal(series, exception))
	if len(p.Ops) != 2 || p.Ops[0].Update == nil || p.Ops[0].Update.ID != "row-m" || p.Ops[1].Create == nil {
		t.Fatalf("an exception beside the unchanged series: ops = %s, want the master's update, then the create", describe(p))
	}
	if got := seqOf(t, open(t, p.Ops[0], &stored)); got != "SEQUENCE:0" {
		t.Errorf("the master beside the new exception: %q, want SEQUENCE:0", got)
	}
	if got := seqOf(t, open(t, p.Ops[1], nil)); got != "SEQUENCE:1" {
		t.Errorf("the new exception: %q, want its SEQUENCE:1", got)
	}
}
