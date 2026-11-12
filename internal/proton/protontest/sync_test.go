package protontest_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// writer builds sync bodies the way the bridge must (constraints: fragments,
// signer, key packets), so each test changes one thing from a valid write.
type writer struct {
	f      *protontest.Fake
	tk     api.Tokens
	cal    string
	signer *crypto.KeyRing // the member address's key
	calKey *crypto.KeyRing // the primary calendar key
}

func newWriter(t *testing.T, o protontest.Options) *writer {
	t.Helper()
	f := protontest.New(t, o)
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"})
	return &writer{f: f, tk: f.Session(t, f.Username), cal: cal, signer: f.AddressKeyRing(), calKey: f.CalendarKeyRing(cal)}
}

// card is an event's lines per part, UID and DTSTAMP left out (fragment adds
// them). A nil calEnc or attendees leaves that part out.
type card struct {
	uid                                                   string
	sharedSigned, sharedEnc, calSigned, calEnc, attendees []string
}

// fragment is a part's text in Proton's form: UID and DTSTAMP first, CRLF,
// no VERSION/PRODID, no CRLF after the last line.
func fragment(uid string, lines ...string) string {
	all := append([]string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20261111T090000Z"}, lines...)
	return strings.Join(append(all, "END:VEVENT", "END:VCALENDAR"), "\r\n")
}

// sessionKeys are an event's two session keys.
type sessionKeys struct{ shared, cal *crypto.SessionKey }

func newKeys(t *testing.T) sessionKeys {
	t.Helper()
	shared, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	cal, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	return sessionKeys{shared, cal}
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

// signText is a text-mode (type 0x01) detached signature of text.
func signText(t *testing.T, kr *crypto.KeyRing, text string) string {
	t.Helper()
	sig, err := kr.SignDetached(crypto.NewPlainMessageFromString(text))
	if err != nil {
		t.Fatal(err)
	}
	armored, err := sig.GetArmored()
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

func signedPart(t *testing.T, kr *crypto.KeyRing, text string) map[string]any {
	t.Helper()
	return map[string]any{"Type": 2, "Data": text, "Signature": sign(t, kr, []byte(text))}
}

func encryptedPart(t *testing.T, kr *crypto.KeyRing, text string, sk *crypto.SessionKey) map[string]any {
	t.Helper()
	data, err := sk.Encrypt(crypto.NewPlainMessage([]byte(text)))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"Type": 3, "Data": base64.StdEncoding.EncodeToString(data), "Signature": sign(t, kr, []byte(text))}
}

func wrap(t *testing.T, kr *crypto.KeyRing, sk *crypto.SessionKey) string {
	t.Helper()
	kp, err := kr.EncryptSessionKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(kp)
}

// update is c sealed under k and signed by the address key, as an update's
// Event: no key packets, Notifications and Color null.
func (w *writer) update(t *testing.T, c card, k sessionKeys) map[string]any {
	t.Helper()
	calendar := []any{signedPart(t, w.signer, fragment(c.uid, c.calSigned...))}
	if c.calEnc != nil {
		calendar = append(calendar, encryptedPart(t, w.signer, fragment(c.uid, c.calEnc...), k.cal))
	}
	attendees := []any{}
	if c.attendees != nil {
		attendees = append(attendees, encryptedPart(t, w.signer, fragment(c.uid, c.attendees...), k.shared))
	}
	return map[string]any{
		"Permissions": 1,
		"SharedEventContent": []any{
			signedPart(t, w.signer, fragment(c.uid, c.sharedSigned...)),
			encryptedPart(t, w.signer, fragment(c.uid, c.sharedEnc...), k.shared),
		},
		"CalendarEventContent":  calendar,
		"AttendeesEventContent": attendees,
		"Attendees":             []any{},
		"Notifications":         nil,
		"Color":                 nil,
	}
}

// create is update plus the key packets, wrapped to the primary calendar key.
func (w *writer) create(t *testing.T, c card, k sessionKeys) map[string]any {
	t.Helper()
	ev := w.update(t, c, k)
	ev["SharedKeyPacket"] = wrap(t, w.calKey, k.shared)
	if c.calEnc != nil {
		ev["CalendarKeyPacket"] = wrap(t, w.calKey, k.cal)
	}
	return ev
}

type entryAnswer struct {
	Index    int
	Response struct {
		Code  int
		Error string
		Event struct{ ID string }
	}
}

type syncAnswer struct {
	Code      int
	Error     string
	Responses []entryAnswer
}

// do sends one request with tk and a JSON body; it returns the status and
// the raw answer.
func do(t *testing.T, f *protontest.Fake, tk api.Tokens, method, path string, body any) (int, json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("x-pm-uid", tk.UID)
	req.Header.Set("Authorization", "Bearer "+tk.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// syncAs sends entries to the calendar's sync endpoint as member.
func (w *writer) syncAs(t *testing.T, member string, entries ...map[string]any) (int, syncAnswer) {
	t.Helper()
	status, raw := do(t, w.f, w.tk, http.MethodPut, "/calendar/v1/"+url.PathEscape(w.cal)+"/events/sync",
		map[string]any{"MemberID": member, "Events": entries})
	var ans syncAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		t.Fatal(err)
	}
	return status, ans
}

func (w *writer) sync(t *testing.T, entries ...map[string]any) (int, syncAnswer) {
	t.Helper()
	return w.syncAs(t, w.f.MemberID(w.cal), entries...)
}

// codes are the per-entry codes of ans, by Index.
func codes(ans syncAnswer) map[int]int {
	out := map[int]int{}
	for _, r := range ans.Responses {
		out[r.Index] = r.Response.Code
	}
	return out
}

// mustCreate sends one create and returns the new row ID.
func (w *writer) mustCreate(t *testing.T, ev map[string]any) string {
	t.Helper()
	st, ans := w.sync(t, map[string]any{"Overwrite": 0, "Event": ev})
	if st != 200 || ans.Code != 1001 || len(ans.Responses) != 1 || ans.Responses[0].Response.Code != 1000 {
		t.Fatalf("create: HTTP %d, answer %+v; want 200, 1001 and one 1000 entry", st, ans)
	}
	id := ans.Responses[0].Response.Event.ID
	if id == "" {
		t.Fatal("create: the answer echoes no row")
	}
	return id
}

// entryCode sends one entry and returns its code, failing on a request-level
// error.
func (w *writer) entryCode(t *testing.T, entry map[string]any) int {
	t.Helper()
	st, ans := w.sync(t, entry)
	if st != 200 || ans.Code != 1001 || len(ans.Responses) != 1 {
		t.Fatalf("sync: HTTP %d, answer %+v; want 200, 1001 and one entry", st, ans)
	}
	return ans.Responses[0].Response.Code
}

// row is the row as GET serves it.
func (w *writer) row(t *testing.T, id string) map[string]any {
	t.Helper()
	var out struct{ Event map[string]any }
	if st, _ := rawGet(t, w.f, w.tk, "/calendar/v1/"+url.PathEscape(w.cal)+"/events/"+url.PathEscape(id), &out); st != 200 {
		t.Fatalf("get %s: HTTP %d", id, st)
	}
	return out.Event
}

// byUID lists the rows of uid.
func (w *writer) byUID(t *testing.T, uid string) []map[string]any {
	t.Helper()
	var out struct{ Events []map[string]any }
	if st, _ := rawGet(t, w.f, w.tk, "/calendar/v1/"+url.PathEscape(w.cal)+"/events?UID="+url.QueryEscape(uid), &out); st != 200 {
		t.Fatalf("list UID %s: HTTP %d", uid, st)
	}
	return out.Events
}

// partData is the Data of part i of group in a served row.
func partData(row map[string]any, group string, i int) string {
	parts, _ := row[group].([]any)
	if i >= len(parts) {
		return ""
	}
	return parts[i].(map[string]any)["Data"].(string)
}

// open decrypts an encrypted part's Data with sk.
func open(t *testing.T, sk *crypto.SessionKey, data string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sk.Decrypt(raw)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return string(plain.GetBinary())
}

func stranger(t *testing.T) *crypto.KeyRing {
	t.Helper()
	k, err := crypto.GenerateKey("Stranger", "stranger@pcb.test", "x25519", 0)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := crypto.NewKeyRing(k)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// at is an instant of 2026 in UTC, as a decoded JSON number.
func at(m time.Month, d, h int) float64 {
	return float64(time.Date(2026, m, d, h, 0, 0, 0, time.UTC).Unix())
}

// simple is a one-hour event with SEQUENCE seq and a summary.
func simple(uid, seq, summary string) card {
	return card{
		uid:          uid,
		sharedSigned: []string{"DTSTART:20261112T090000Z", "DTEND:20261112T100000Z", "SEQUENCE:" + seq},
		sharedEnc:    []string{"SUMMARY:" + summary},
		calSigned:    []string{"STATUS:CONFIRMED"},
	}
}

func TestSyncCreateParsesTheSignedPart(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	k := newKeys(t)
	master := card{
		uid: "weekly@pcb.test",
		sharedSigned: []string{"DTSTART;TZID=Europe/Paris:20261112T090000", "DTEND;TZID=Europe/Paris:20261112T100000",
			"RRULE:FREQ=WEEKLY;COUNT=3", "EXDATE;TZID=Europe/Paris:20261119T090000", "SEQUENCE:0"},
		sharedEnc: []string{"SUMMARY:Standup"},
		calSigned: []string{"EXDATE;TZID=Europe/Paris:20261119T090000", "STATUS:CONFIRMED", "TRANSP:OPAQUE"},
	}
	sent := w.create(t, master, k)
	id := w.mustCreate(t, sent)

	row := w.row(t, id)
	for field, want := range map[string]any{
		"UID": "weekly@pcb.test", "StartTime": at(11, 12, 8), "EndTime": at(11, 12, 9),
		"StartTimezone": "Europe/Paris", "EndTimezone": "Europe/Paris", "FullDay": 0.0,
		"RRule": "FREQ=WEEKLY;COUNT=3", "RecurrenceID": nil,
	} {
		if row[field] != want {
			t.Errorf("%s = %v, want %v", field, row[field], want)
		}
	}
	if ex, _ := row["Exdates"].([]any); len(ex) != 1 || ex[0] != at(11, 19, 8) {
		t.Errorf("Exdates = %v, want [%v]", row["Exdates"], at(11, 19, 8))
	}
	if got, want := partData(row, "SharedEvents", 0), sent["SharedEventContent"].([]any)[0].(map[string]any)["Data"]; got != want {
		t.Errorf("the shared signed part is not served as sent:\n%q\n%q", got, want)
	}
	if !strings.Contains(open(t, k.shared, partData(row, "SharedEvents", 1)), "\r\nSUMMARY:Standup\r\n") {
		t.Error("the shared encrypted part does not open with the session key sent")
	}

	exception := card{
		uid:          "weekly@pcb.test",
		sharedSigned: []string{"DTSTART;TZID=Europe/Paris:20261126T100000", "DTEND;TZID=Europe/Paris:20261126T110000", "RECURRENCE-ID;TZID=Europe/Paris:20261126T090000", "SEQUENCE:0"},
		sharedEnc:    []string{"SUMMARY:Standup, later"},
		calSigned:    []string{"STATUS:CONFIRMED"},
	}
	exRow := w.row(t, w.mustCreate(t, w.create(t, exception, newKeys(t))))
	if exRow["RecurrenceID"] != at(11, 26, 8) || exRow["RRule"] != nil {
		t.Errorf("exception: RecurrenceID %v RRule %v; want %v and null", exRow["RecurrenceID"], exRow["RRule"], at(11, 26, 8))
	}

	allDay := card{
		uid:          "holiday@pcb.test",
		sharedSigned: []string{"DTSTART;VALUE=DATE:20261224", "DTEND;VALUE=DATE:20261225", "SEQUENCE:0"},
		sharedEnc:    []string{"SUMMARY:Off"},
		calSigned:    []string{"TRANSP:TRANSPARENT"},
	}
	dayRow := w.row(t, w.mustCreate(t, w.create(t, allDay, newKeys(t))))
	if dayRow["FullDay"] != 1.0 || dayRow["StartTime"] != at(12, 24, 0) || dayRow["EndTime"] != at(12, 25, 0) {
		t.Errorf("all-day: FullDay %v StartTime %v EndTime %v; want 1 and midnight UTC of 24 and 25 Dec", dayRow["FullDay"], dayRow["StartTime"], dayRow["EndTime"])
	}
}

func TestSyncRefusesABadSignature(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	other := stranger(t)
	c := card{
		uid:          "sig@pcb.test",
		sharedSigned: []string{"DTSTART:20261112T090000Z", "DTEND:20261112T100000Z", "SEQUENCE:0", "ORGANIZER:mailto:me@pcb.test"},
		sharedEnc:    []string{"SUMMARY:Signed"},
		calSigned:    []string{"STATUS:CONFIRMED"},
		calEnc:       []string{"COMMENT:note"},
		attendees:    []string{"ATTENDEE;PARTSTAT=ACCEPTED;X-PM-TOKEN=tok:mailto:ann@pcb.test"},
	}
	k := newKeys(t)
	resign := func(group string, i int, kr *crypto.KeyRing, sk *crypto.SessionKey) func(ev map[string]any) {
		return func(ev map[string]any) {
			p := ev[group].([]any)[i].(map[string]any)
			text := []byte(p["Data"].(string))
			if sk != nil {
				text = []byte(open(t, sk, p["Data"].(string)))
			}
			p["Signature"] = sign(t, kr, text)
		}
	}
	for name, spoil := range map[string]func(ev map[string]any){
		"shared signed part by another key":      resign("SharedEventContent", 0, other, nil),
		"shared encrypted part by another key":   resign("SharedEventContent", 1, other, k.shared),
		"calendar signed part by another key":    resign("CalendarEventContent", 0, other, nil),
		"calendar encrypted part by another key": resign("CalendarEventContent", 1, other, k.cal),
		"attendees part by another key":          resign("AttendeesEventContent", 0, other, k.shared),
		// Research A.4: another address's key is 2001 (carbonate, caloxide live).
		"shared signed part by our other address":    resign("SharedEventContent", 0, w.f.OtherAddressKeyRing(), nil),
		"shared encrypted part by our other address": resign("SharedEventContent", 1, w.f.OtherAddressKeyRing(), k.shared),
		// Stricter than Proton (carbonate's text mode works live): binary only.
		"shared signed part in text mode": func(ev map[string]any) {
			p := ev["SharedEventContent"].([]any)[0].(map[string]any)
			p["Signature"] = signText(t, w.signer, p["Data"].(string))
		},
		"shared encrypted part in text mode": func(ev map[string]any) {
			p := ev["SharedEventContent"].([]any)[1].(map[string]any)
			p["Signature"] = signText(t, w.signer, open(t, k.shared, p["Data"].(string)))
		},
		"shared signed part changed after signing": func(ev map[string]any) {
			p := ev["SharedEventContent"].([]any)[0].(map[string]any)
			p["Data"] = strings.Replace(p["Data"].(string), "SEQUENCE:0", "SEQUENCE:1", 1)
		},
		"calendar signed part without signature": func(ev map[string]any) {
			ev["CalendarEventContent"].([]any)[0].(map[string]any)["Signature"] = ""
		},
	} {
		ev := w.create(t, c, k)
		spoil(ev)
		if code := w.entryCode(t, map[string]any{"Event": ev}); code != 2001 {
			t.Errorf("%s: code %d, want 2001", name, code)
		}
	}
	if rows := w.byUID(t, c.uid); len(rows) != 0 {
		t.Fatalf("%d rows stored from refused writes", len(rows))
	}
	w.mustCreate(t, w.create(t, c, k)) // the same event, signed by the address key
}

func TestSyncRefusesKeyPacketsToARetiredKey(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	retired := w.f.RetiredCalendarKeyRing(w.cal)
	both, err := w.calKey.Copy()
	if err != nil {
		t.Fatal(err)
	}
	if err := both.AddKey(retired.GetKeys()[0]); err != nil {
		t.Fatal(err)
	}
	c := simple("kp@pcb.test", "0", "Keys")
	c.calEnc = []string{"COMMENT:note"}
	k := newKeys(t)
	for name, spoil := range map[string]func(ev map[string]any){
		"shared key packet to the retired key":    func(ev map[string]any) { ev["SharedKeyPacket"] = wrap(t, retired, k.shared) },
		"calendar key packet to the retired key":  func(ev map[string]any) { ev["CalendarKeyPacket"] = wrap(t, retired, k.cal) },
		"shared key packet to every calendar key": func(ev map[string]any) { ev["SharedKeyPacket"] = wrap(t, both, k.shared) },
		"shared key packet to another key":        func(ev map[string]any) { ev["SharedKeyPacket"] = wrap(t, stranger(t), k.shared) },
		"no shared key packet":                    func(ev map[string]any) { delete(ev, "SharedKeyPacket") },
		"no calendar key packet":                  func(ev map[string]any) { delete(ev, "CalendarKeyPacket") },
		"a key packet that is not base64":         func(ev map[string]any) { ev["SharedKeyPacket"] = "not base64!" },
	} {
		ev := w.create(t, c, k)
		spoil(ev)
		if code := w.entryCode(t, map[string]any{"Event": ev}); code != 2001 {
			t.Errorf("%s: code %d, want 2001", name, code)
		}
	}
	if rows := w.byUID(t, c.uid); len(rows) != 0 {
		t.Fatalf("%d rows stored from refused writes", len(rows))
	}
	w.mustCreate(t, w.create(t, c, k)) // to the primary key only
}

func TestSyncUpdateReplacesAndKeepsAbsentNotifications(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	k := newKeys(t)
	c := simple("upd@pcb.test", "0", "Before")
	c.calEnc = []string{"COMMENT:private"}
	ev := w.create(t, c, k)
	ev["Notifications"] = []map[string]any{{"Type": 1, "Trigger": "-PT15M"}}
	ev["Color"] = "#3CBB3A"
	ev["IsOrganizer"] = 0
	id := w.mustCreate(t, ev)
	before := w.row(t, id)

	// Absent Notifications and null Color keep the row's; the omitted
	// calendar encrypted part goes (full replace); no key packets are sent.
	c.calEnc, c.sharedEnc = nil, []string{"SUMMARY:After"}
	upd := w.update(t, c, k)
	delete(upd, "Notifications")
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": upd}); code != 1000 {
		t.Fatalf("update: code %d, want 1000", code)
	}
	after := w.row(t, id)
	if n, _ := json.Marshal(after["Notifications"]); string(n) != `[{"Trigger":"-PT15M","Type":1}]` {
		t.Errorf("absent Notifications: %s, want the row's [-PT15M]", n)
	}
	if after["Color"] != "#3CBB3A" {
		t.Errorf("null Color on update: %v, want the row's #3CBB3A", after["Color"])
	}
	if after["IsOrganizer"] != 0.0 {
		t.Errorf("absent IsOrganizer on update: %v, want the row's 0", after["IsOrganizer"])
	}
	if got := open(t, k.shared, partData(after, "SharedEvents", 1)); !strings.Contains(got, "SUMMARY:After") || strings.Contains(got, "Before") {
		t.Errorf("shared encrypted part not replaced: %q", got)
	}
	if parts := after["CalendarEvents"].([]any); len(parts) != 1 {
		t.Errorf("%d calendar parts after an update without the encrypted one, want 1", len(parts))
	}
	if after["SharedKeyPacket"] != before["SharedKeyPacket"] || after["SharedKeyPacket"] == nil {
		t.Error("an update must keep the row's key packets")
	}
	if after["LastEditTime"].(float64) <= before["LastEditTime"].(float64) {
		t.Error("an update must move LastEditTime")
	}

	for _, tc := range []struct {
		sent any
		want string
	}{{[]any{}, "[]"}, {nil, "null"}, {[]map[string]any{{"Type": 0, "Trigger": "-P1D"}}, `[{"Trigger":"-P1D","Type":0}]`}} {
		upd := w.update(t, c, k)
		upd["Notifications"] = tc.sent
		if code := w.entryCode(t, map[string]any{"ID": id, "Event": upd}); code != 1000 {
			t.Fatalf("update with Notifications %v: code %d", tc.sent, code)
		}
		if n, _ := json.Marshal(w.row(t, id)["Notifications"]); string(n) != tc.want {
			t.Errorf("Notifications sent %v: row has %s, want %s", tc.sent, n, tc.want)
		}
	}
}

// TestSyncUpdatesASeededRow: a row the fake made (AddEvent) takes a sync
// update sealed with its own session key; its personal part stays (research
// L.4.10: unknown, the fake keeps it).
func TestSyncUpdatesASeededRow(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	id := w.f.AddEvent(w.cal, protontest.Event{UID: "seed@pcb.test", Start: 1, End: 2, PersonalAlarms: true,
		Card: "SUMMARY:Seeded\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT5M\r\nEND:VALARM"})
	seeded := w.row(t, id)
	kp, err := base64.StdEncoding.DecodeString(seeded["SharedKeyPacket"].(string))
	if err != nil {
		t.Fatal(err)
	}
	sk, err := w.calKey.DecryptSessionKey(kp)
	if err != nil {
		t.Fatal(err)
	}
	upd := w.update(t, simple("seed@pcb.test", "1", "Edited"), sessionKeys{shared: sk})
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": upd}); code != 1000 {
		t.Fatalf("update of a seeded row: code %d, want 1000", code)
	}
	row := w.row(t, id)
	if !strings.Contains(open(t, sk, partData(row, "SharedEvents", 1)), "SUMMARY:Edited") {
		t.Error("the seeded row was not replaced")
	}
	if p, _ := row["PersonalEvents"].([]any); len(p) != 1 || partData(row, "PersonalEvents", 0) != partData(seeded, "PersonalEvents", 0) {
		t.Errorf("personal parts after the update: %v, want the stored one kept", row["PersonalEvents"])
	}
}

func TestSyncExceptionNeedsSequenceAtLeastMaster(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	master := simple("ex@pcb.test", "2", "Daily")
	master.sharedSigned = append(master.sharedSigned, "RRULE:FREQ=DAILY;COUNT=5")
	mk := newKeys(t)
	masterID := w.mustCreate(t, w.create(t, master, mk))
	exception := func(seq string) card {
		return card{
			uid:          "ex@pcb.test",
			sharedSigned: []string{"DTSTART:20261113T100000Z", "DTEND:20261113T110000Z", "RECURRENCE-ID:20261113T090000Z", "SEQUENCE:" + seq},
			sharedEnc:    []string{"SUMMARY:Moved"},
			calSigned:    []string{"STATUS:CONFIRMED"},
		}
	}
	ek := newKeys(t)
	if code := w.entryCode(t, map[string]any{"Event": w.create(t, exception("1"), ek)}); code != 2001 {
		t.Errorf("exception SEQUENCE 1 < master 2: code %d, want 2001", code)
	}
	exID := w.mustCreate(t, w.create(t, exception("2"), ek))
	if code := w.entryCode(t, map[string]any{"ID": exID, "Event": w.update(t, exception("1"), ek)}); code != 2001 {
		t.Errorf("exception updated down to SEQUENCE 1: code %d, want 2001", code)
	}
	// Master first in one call: the exception is checked against the master
	// as this call left it.
	bumped := master
	bumped.sharedSigned = []string{"DTSTART:20261112T090000Z", "DTEND:20261112T100000Z", "SEQUENCE:3", "RRULE:FREQ=DAILY;COUNT=5"}
	_, ans := w.sync(t,
		map[string]any{"ID": masterID, "Event": w.update(t, bumped, mk)},
		map[string]any{"ID": exID, "Event": w.update(t, exception("2"), ek)},
		map[string]any{"ID": exID, "Event": w.update(t, exception("3"), ek)})
	if got := codes(ans); got[0] != 1000 || got[1] != 2001 || got[2] != 1000 {
		t.Errorf("master to 3, exception at 2 then 3: codes %v, want 1000, 2001, 1000", got)
	}
}

// TestSyncRefusesWhatProtonRefuses covers the rest of research A.4 and the
// fragment and routing rules of the constraints.
func TestSyncRefusesWhatProtonRefuses(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	with := func(edit func(c *card)) card {
		c := simple("bad@pcb.test", "0", "Bad")
		c.calEnc = []string{"COMMENT:note"}
		edit(&c)
		return c
	}
	cases := []struct {
		name string
		c    card
		ev   func(ev map[string]any)
		want int
	}{
		{"STATUS:TENTATIVE", with(func(c *card) { c.calSigned = []string{"STATUS:TENTATIVE"} }), nil, 2000},
		{"RRULE with RECURRENCE-ID", with(func(c *card) {
			c.sharedSigned = append(c.sharedSigned, "RRULE:FREQ=DAILY", "RECURRENCE-ID:20261112T090000Z")
		}), nil, 2011},
		{"a colour off the palette", with(func(*card) {}), func(ev map[string]any) { ev["Color"] = "#FF0000" }, 2011},
		{"DURATION instead of DTEND", with(func(c *card) {
			c.sharedSigned = []string{"DTSTART:20261112T090000Z", "DURATION:PT1H", "SEQUENCE:0"}
		}), nil, 2001},
		{"SUMMARY in the shared signed part", with(func(c *card) { c.sharedSigned = append(c.sharedSigned, "SUMMARY:x") }), nil, 2001},
		{"COMMENT in the shared encrypted part", with(func(c *card) { c.sharedEnc = append(c.sharedEnc, "COMMENT:x") }), nil, 2001},
		{"STATUS in the shared encrypted part", with(func(c *card) { c.sharedEnc = append(c.sharedEnc, "STATUS:CONFIRMED") }), nil, 2001},
		{"SUMMARY in the calendar encrypted part", with(func(c *card) { c.calEnc = append(c.calEnc, "SUMMARY:x") }), nil, 2001},
		{"COLOR in a part", with(func(c *card) { c.sharedEnc = append(c.sharedEnc, "COLOR:red") }), nil, 2001},
		{"X-PCB-INTEGRITY in a part", with(func(c *card) { c.sharedEnc = append(c.sharedEnc, "X-PCB-INTEGRITY:1") }), nil, 2001},
		{"a VALARM in a part", with(func(c *card) {
			c.sharedEnc = append(c.sharedEnc, "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "END:VALARM")
		}), nil, 2001},
		{"EXDATE in the shared signed part only", with(func(c *card) {
			c.sharedSigned = append(c.sharedSigned, "RRULE:FREQ=DAILY", "EXDATE:20261113T090000Z")
		}), nil, 2001},
		{"EXDATE in the calendar signed part only", with(func(c *card) {
			c.sharedSigned = append(c.sharedSigned, "RRULE:FREQ=DAILY")
			c.calSigned = append(c.calSigned, "EXDATE:20261113T090000Z")
		}), nil, 2001},
		{"a floating DTSTART", with(func(c *card) {
			c.sharedSigned = []string{"DTSTART:20261112T090000", "DTEND:20261112T100000", "SEQUENCE:0"}
		}), nil, 2001},
		{"a TZID the tz database lacks", with(func(c *card) {
			c.sharedSigned = []string{"DTSTART;TZID=Mars/Olympus:20261112T090000", "DTEND;TZID=Mars/Olympus:20261112T100000", "SEQUENCE:0"}
		}), nil, 2001},
		{"another UID in a part", with(func(*card) {}), func(ev map[string]any) {
			cal := ev["CalendarEventContent"].([]any)
			cal[0] = signedPart(t, w.signer, fragment("other@pcb.test", "STATUS:CONFIRMED"))
		}, 2001},
		{"no Permissions", with(func(*card) {}), func(ev map[string]any) { delete(ev, "Permissions") }, 2001},
		// Research A.4 (carbonate live): "Shared event has attendees but does not have an organizer".
		{"attendees without ORGANIZER", with(func(c *card) {
			c.attendees = []string{"ATTENDEE;X-PM-TOKEN=t1:mailto:ann@pcb.test"}
		}), nil, 2001},
		// Research A.4: SEQUENCE;VALUE=TEXT:0 (Proton answers HTTP 500; the fake 2001).
		{"SEQUENCE with a parameter", with(func(c *card) {
			c.sharedSigned = []string{"DTSTART:20261112T090000Z", "DTEND:20261112T100000Z", "SEQUENCE;VALUE=TEXT:0"}
		}), nil, 2001},
		{"invitations (AddedProtonAttendees)", with(func(*card) {}), func(ev map[string]any) {
			ev["AddedProtonAttendees"] = []any{map[string]any{"Email": "ann@pcb.test"}}
		}, 2001},
		{"invitations (RemovedAttendeeAddresses)", with(func(*card) {}), func(ev map[string]any) {
			ev["RemovedAttendeeAddresses"] = []any{"ann@pcb.test"}
		}, 2001},
	}
	// Fragment form: each spoils the shared signed part's text, then re-signs it.
	for name, spoil := range map[string]func(string) string{
		"a VERSION line":             func(s string) string { return strings.Replace(s, "BEGIN:VEVENT", "VERSION:2.0\r\nBEGIN:VEVENT", 1) },
		"a CRLF after the last line": func(s string) string { return s + "\r\n" },
		"LF line ends":               func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") },
		"a line over 75 octets": func(s string) string {
			return strings.Replace(s, "SEQUENCE:0", "SEQUENCE:0"+strings.Repeat("0", 70), 1)
		},
		"no DTSTAMP": func(s string) string {
			return strings.Replace(s, "DTSTAMP:20261111T090000Z\r\n", "", 1)
		},
	} {
		cases = append(cases, struct {
			name string
			c    card
			ev   func(ev map[string]any)
			want int
		}{name, with(func(*card) {}), func(ev map[string]any) {
			parts := ev["SharedEventContent"].([]any)
			parts[0] = signedPart(t, w.signer, spoil(parts[0].(map[string]any)["Data"].(string)))
		}, 2001})
	}
	k := newKeys(t)
	for _, tc := range cases {
		ev := w.create(t, tc.c, k)
		if tc.ev != nil {
			tc.ev(ev)
		}
		if code := w.entryCode(t, map[string]any{"Event": ev}); code != tc.want {
			t.Errorf("%s: code %d, want %d", tc.name, code, tc.want)
		}
	}
	if rows := w.byUID(t, "bad@pcb.test"); len(rows) != 0 {
		t.Fatalf("%d rows stored from refused writes", len(rows))
	}
	good := w.create(t, with(func(*card) {}), k)
	good["Color"] = "#3CBB3A"
	w.mustCreate(t, good)

	if code := w.entryCode(t, map[string]any{"ID": "evnosuchrow==", "Event": w.update(t, simple("bad@pcb.test", "0", "x"), k)}); code != 2061 {
		t.Errorf("update of an unknown ID: code %d, want 2061", code)
	}
	if st, ans := w.syncAs(t, "memnotours==", map[string]any{"Event": w.create(t, simple("m@pcb.test", "0", "x"), k)}); st != 422 || ans.Code != 2061 {
		t.Errorf("another MemberID: HTTP %d code %d, want 422/2061", st, ans.Code)
	}
}

// TestSyncRefusesMalformedEntries covers the entry-level rules: Overwrite 1
// (no known meaning), a field Proton does not define, a UID change on update.
func TestSyncRefusesMalformedEntries(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	k := newKeys(t)
	if code := w.entryCode(t, map[string]any{"Overwrite": 1, "Event": w.create(t, simple("ow@pcb.test", "0", "x"), k)}); code != 2001 {
		t.Errorf("Overwrite 1: code %d, want 2001", code)
	}
	misspelt := w.create(t, simple("ms@pcb.test", "0", "x"), k)
	misspelt["SharedEventsContent"] = misspelt["SharedEventContent"]
	if st, ans := w.sync(t, map[string]any{"Event": misspelt}); st != 400 || ans.Code != 2001 {
		t.Errorf("an unknown Event field: HTTP %d code %d, want 400/2001", st, ans.Code)
	}
	if rows := len(w.byUID(t, "ow@pcb.test")) + len(w.byUID(t, "ms@pcb.test")); rows != 0 {
		t.Errorf("%d rows stored from refused writes", rows)
	}
	id := w.mustCreate(t, w.create(t, simple("uid@pcb.test", "0", "x"), k))
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": w.update(t, simple("other@pcb.test", "1", "x"), k)}); code != 2001 {
		t.Errorf("update changing the UID: code %d, want 2001", code)
	}
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": w.update(t, simple("uid@pcb.test", "1", "x"), k)}); code != 1000 {
		t.Errorf("the same update keeping the UID: code %d, want 1000", code)
	}
}

// TestSyncRefusesNullArrays: research A.2 (proton-cal live), the part lists
// and Attendees are [] when empty, never null; the fake refuses them absent
// too (stricter than Proton: an omitted Attendees wipes the RSVPs).
func TestSyncRefusesNullArrays(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	k := newKeys(t)
	for _, field := range []string{"SharedEventContent", "CalendarEventContent", "AttendeesEventContent", "Attendees"} {
		for name, spoil := range map[string]func(ev map[string]any){
			"null":   func(ev map[string]any) { ev[field] = nil },
			"absent": func(ev map[string]any) { delete(ev, field) },
		} {
			ev := w.create(t, simple("null@pcb.test", "0", "x"), k)
			spoil(ev)
			if code := w.entryCode(t, map[string]any{"Event": ev}); code != 2001 {
				t.Errorf("%s %s: code %d, want 2001", field, name, code)
			}
		}
	}
	if rows := w.byUID(t, "null@pcb.test"); len(rows) != 0 {
		t.Fatalf("%d rows stored from refused writes", len(rows))
	}
	w.mustCreate(t, w.create(t, simple("null@pcb.test", "0", "x"), k)) // every list []
}

// TestSyncSignerIsTheMemberAddressKey (research B.3): a calendar of the
// account's second address takes parts signed by that address's key only,
// and its seeded rows are signed by it.
func TestSyncSignerIsTheMemberAddressKey(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	w.cal = w.f.AddCalendar(w.f.Username, protontest.Calendar{ToSecondAddress: true})
	w.calKey = w.f.CalendarKeyRing(w.cal)
	k := newKeys(t)
	if code := w.entryCode(t, map[string]any{"Event": w.create(t, simple("2nd@pcb.test", "0", "x"), k)}); code != 2001 {
		t.Errorf("signed by the first address's key: code %d, want 2001", code)
	}
	w.signer = w.f.OtherAddressKeyRing()
	w.mustCreate(t, w.create(t, simple("2nd@pcb.test", "0", "x"), k))

	seeded := w.row(t, w.f.AddEvent(w.cal, protontest.Event{UID: "s@pcb.test", Start: 1, End: 2}))
	part := seeded["SharedEvents"].([]any)[0].(map[string]any)
	sig, err := crypto.NewPGPSignatureFromArmored(part["Signature"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.signer.VerifyDetached(crypto.NewPlainMessage([]byte(part["Data"].(string))), sig, 0); err != nil {
		t.Errorf("a seeded row of the second address's calendar is not signed by its key: %v", err)
	}
}

// TestSyncUpdateReplacesAttendees: research A.2 (proton-cal live), an update
// replaces the Attendees statuses; an attendee keeps its ID across updates.
func TestSyncUpdateReplacesAttendees(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	k := newKeys(t)
	c := simple("att@pcb.test", "0", "Meeting")
	c.sharedSigned = append(c.sharedSigned, "ORGANIZER:mailto:me@pcb.test")
	c.attendees = []string{"ATTENDEE;X-PM-TOKEN=tokA:mailto:ann@pcb.test", "ATTENDEE;X-PM-TOKEN=tokB:mailto:bob@pcb.test"}
	withStatuses := func(ev map[string]any, statuses ...any) map[string]any {
		ev["Attendees"] = statuses
		return ev
	}
	status := func(token string, s int) map[string]any { return map[string]any{"Token": token, "Status": s} }
	type att struct {
		ID, Token string
		Status    int
	}
	attendees := func(id string) map[string]att {
		var out struct{ Event struct{ Attendees []att } }
		rawGet(t, w.f, w.tk, "/calendar/v1/"+url.PathEscape(w.cal)+"/events/"+url.PathEscape(id), &out)
		m := map[string]att{}
		for _, a := range out.Event.Attendees {
			m[a.Token] = a
		}
		return m
	}
	id := w.mustCreate(t, withStatuses(w.create(t, c, k), status("tokA", 0), status("tokB", 0)))
	first := attendees(id)
	if len(first) != 2 || first["tokA"].ID == "" || first["tokA"].ID == first["tokB"].ID {
		t.Fatalf("after create: %+v, want two attendees with distinct IDs", first)
	}
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": withStatuses(w.update(t, c, k), status("tokA", 3), status("tokB", 0))}); code != 1000 {
		t.Fatalf("update: code %d", code)
	}
	if got := attendees(id); got["tokA"] != (att{first["tokA"].ID, "tokA", 3}) || got["tokB"] != first["tokB"] {
		t.Errorf("after re-sending both: %+v, want the same IDs and tokA accepted", got)
	}
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": withStatuses(w.update(t, c, k), status("tokB", 2))}); code != 1000 {
		t.Fatalf("update: code %d", code)
	}
	if got := attendees(id); len(got) != 1 || got["tokB"] != (att{first["tokB"].ID, "tokB", 2}) {
		t.Errorf("after re-sending tokB only: %+v, want tokB alone, same ID, declined", got)
	}
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": w.update(t, c, k)}); code != 1000 {
		t.Fatalf("update: code %d", code)
	}
	if got := attendees(id); len(got) != 0 {
		t.Errorf("after an update with Attendees []: %+v, want none", got)
	}
}

func TestSyncRefusesAMemberWithoutWrite(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	for name, c := range map[string]protontest.Calendar{
		"read-only member":    {Permissions: 2 | 4},
		"subscribed calendar": {Type: 1},
		"holidays calendar":   {Type: 2},
	} {
		w.cal = w.f.AddCalendar(w.f.Username, c)
		w.calKey = w.f.CalendarKeyRing(w.cal)
		if st, ans := w.sync(t, map[string]any{"Event": w.create(t, simple("ro@pcb.test", "0", "x"), newKeys(t))}); st != 403 || len(ans.Responses) != 0 {
			t.Errorf("%s: HTTP %d, answer %+v; want 403", name, st, ans)
		}
		if rows := w.byUID(t, "ro@pcb.test"); len(rows) != 0 {
			t.Errorf("%s: %d rows stored", name, len(rows))
		}
	}
}

func TestSyncDeleteAnswersNoResponses(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	k := newKeys(t)
	a := w.mustCreate(t, w.create(t, simple("del@pcb.test", "0", "A"), k))
	b := w.f.AddEvent(w.cal, protontest.Event{UID: "del2@pcb.test", Start: 1, End: 2})
	st, raw := do(t, w.f, w.tk, http.MethodPut, "/calendar/v1/"+url.PathEscape(w.cal)+"/events/sync",
		map[string]any{"MemberID": w.f.MemberID(w.cal), "Events": []any{map[string]any{"ID": a}, map[string]any{"ID": b, "DeletionReason": 0}}})
	if st != 200 || string(raw) != `{"Code":1001,"Responses":[]}` {
		t.Errorf("delete: HTTP %d %s, want 200 {\"Code\":1001,\"Responses\":[]}", st, raw)
	}
	for _, id := range []string{a, b} {
		if st, code := rawGet(t, w.f, w.tk, "/calendar/v1/"+url.PathEscape(w.cal)+"/events/"+url.PathEscape(id), nil); st != 422 || code != 2501 {
			t.Errorf("deleted row %s: HTTP %d code %d, want 422/2501", id, st, code)
		}
	}
	if code := w.entryCode(t, map[string]any{"ID": a}); code != 2501 {
		t.Errorf("delete of a deleted row: code %d, want 2501", code)
	}
	// Research A.1 (caloxide live): a delete may leave MemberID out; a write
	// may not.
	c := w.mustCreate(t, w.create(t, simple("del3@pcb.test", "0", "C"), k))
	if st, raw := do(t, w.f, w.tk, http.MethodPut, "/calendar/v1/"+url.PathEscape(w.cal)+"/events/sync",
		map[string]any{"Events": []any{map[string]any{"ID": c}}}); st != 200 || string(raw) != `{"Code":1001,"Responses":[]}` {
		t.Errorf("delete without MemberID: HTTP %d %s, want 200 and no response", st, raw)
	}
	if st, ans := w.syncAs(t, "", map[string]any{"Event": w.create(t, simple("del4@pcb.test", "0", "D"), k)}); st != 422 || ans.Code != 2061 {
		t.Errorf("create without MemberID: HTTP %d code %d, want 422/2061", st, ans.Code)
	}
	if code := w.entryCode(t, map[string]any{"ID": a, "Event": w.update(t, simple("del@pcb.test", "1", "B"), k)}); code != 2501 {
		t.Errorf("update of a deleted row: code %d, want 2501", code)
	}
}

func TestSyncWritesReachTheFeed(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	start := latest(t, w.f, w.tk, w.cal)
	k := newKeys(t)
	id := w.mustCreate(t, w.create(t, simple("feed@pcb.test", "0", "A"), k))
	if code := w.entryCode(t, map[string]any{"ID": id, "Event": w.update(t, simple("feed@pcb.test", "1", "B"), k)}); code != 1000 {
		t.Fatalf("update: code %d", code)
	}
	bad := w.create(t, simple("feed2@pcb.test", "0", "x"), k)
	bad["Color"] = "#FF0000"
	if code := w.entryCode(t, map[string]any{"Event": bad}); code != 2011 {
		t.Fatalf("refused create: code %d", code)
	}
	if st, ans := w.sync(t, map[string]any{"ID": id}); st != 200 || len(ans.Responses) != 0 {
		t.Fatalf("delete: HTTP %d, answer %+v; want 200 and no response", st, ans)
	}
	got, _, _ := feed(t, w.f, w.tk, w.cal, start)
	want := []struct {
		id     string
		action int
	}{{id, 1}, {id, 2}, {id, 0}}
	if len(got.CalendarEvents) != len(want) {
		t.Fatalf("feed = %+v, want create, update, delete of %s and nothing for the refused write", got.CalendarEvents, id)
	}
	for i, e := range want {
		if g := got.CalendarEvents[i]; g.ID != e.id || g.Action != e.action {
			t.Errorf("entry %d = %+v, want %+v", i, g, e)
		}
	}
}

func TestListByUID(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	rid := int64(1762938000)
	master := w.f.AddEvent(w.cal, protontest.Event{UID: "series@pcb.test", Start: 1, End: 2, RRule: "FREQ=DAILY"})
	w.f.AddEvent(w.cal, protontest.Event{UID: "other@pcb.test", Start: 1, End: 2})
	exception := w.f.AddEvent(w.cal, protontest.Event{UID: "series@pcb.test", Start: 3, End: 4, RecurrenceID: &rid})

	rows := w.byUID(t, "series@pcb.test")
	if len(rows) != 2 || rows[0]["ID"] != master || rows[1]["ID"] != exception {
		t.Fatalf("UID series: %d rows, want the master then the exception", len(rows))
	}
	if rows := w.byUID(t, "nobody@pcb.test"); len(rows) != 0 {
		t.Errorf("unknown UID: %d rows, want none", len(rows))
	}
	var page struct {
		Events []struct{ ID string }
		More   int
	}
	base := "/calendar/v1/" + url.PathEscape(w.cal) + "/events?UID=series%40pcb.test&PageSize=1"
	rawGet(t, w.f, w.tk, base+"&Page=0", &page)
	if len(page.Events) != 1 || page.Events[0].ID != master || page.More != 1 {
		t.Errorf("page 0 of 1 row: %+v, want the master and More 1", page)
	}
	page.More = -1
	rawGet(t, w.f, w.tk, base+"&Page=1", &page)
	if len(page.Events) != 1 || page.Events[0].ID != exception || page.More != 0 {
		t.Errorf("page 1 of 1 row: %+v, want the exception and More 0", page)
	}
}

func TestSyncCallsRecordsEveryRequest(t *testing.T) {
	t.Parallel()
	w := newWriter(t, protontest.Options{})
	if n := len(w.f.SyncCalls()); n != 0 {
		t.Fatalf("%d sync calls before any write", n)
	}
	k := newKeys(t)
	id := w.mustCreate(t, w.create(t, simple("calls@pcb.test", "0", "A"), k))
	w.sync(t, map[string]any{"ID": id})
	calls := w.f.SyncCalls()
	if len(calls) != 2 || calls[0].CalendarID != w.cal || calls[1].CalendarID != w.cal {
		t.Fatalf("SyncCalls = %+v, want two calls to %s", calls, w.cal)
	}
	first, second := calls[0].Body, calls[1].Body
	if first.MemberID != w.f.MemberID(w.cal) || len(first.Events) != 1 || first.Events[0].Event == nil ||
		first.Events[0].Event.SharedKeyPacket == nil || len(first.Events[0].Event.SharedEventContent) != 2 {
		t.Errorf("first call body = %+v, want the create as sent", first)
	}
	if len(second.Events) != 1 || second.Events[0].ID != id || second.Events[0].Event != nil {
		t.Errorf("second call body = %+v, want the delete of %s", second, id)
	}
	calls[0].Body.Events[0].ID = "changed"
	if w.f.SyncCalls()[0].Body.Events[0].ID != "" {
		t.Error("SyncCalls must return a copy")
	}
}

func TestBootstrapServesFlagsAndASignedPassphrase(t *testing.T) {
	t.Parallel()
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "good", true: "BadPassphraseSignature"}[bad], func(t *testing.T) {
			t.Parallel()
			f := protontest.New(t, protontest.Options{BadPassphraseSignature: bad})
			cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"})
			readOnly := f.AddCalendar(f.Username, protontest.Calendar{Name: "Shared", Permissions: 2})
			a := login(t, f, f.Username, f.Password)
			tk := api.Tokens{UID: a.UID, AccessToken: a.AccessToken}
			var b struct {
				Keys []struct {
					PrivateKey string
					Flags      int
				}
				Passphrase struct {
					MemberPassphrases []struct{ MemberID, Passphrase, Signature string }
				}
				Members []struct {
					ID          string
					Permissions int
				}
			}
			if st, _ := rawGet(t, f, tk, "/calendar/v2/"+url.PathEscape(cal)+"/bootstrap", &b); st != 200 {
				t.Fatalf("bootstrap: HTTP %d", st)
			}
			addr := f.AddressKeyRing()
			if len(b.Passphrase.MemberPassphrases) != 1 || b.Passphrase.MemberPassphrases[0].MemberID != f.MemberID(cal) {
				t.Fatalf("member passphrases = %+v, want ours", b.Passphrase.MemberPassphrases)
			}
			mp := b.Passphrase.MemberPassphrases[0]
			msg, err := crypto.NewPGPMessageFromArmored(mp.Passphrase)
			if err != nil {
				t.Fatal(err)
			}
			pass, err := addr.Decrypt(msg, nil, 0)
			if err != nil {
				t.Fatalf("the passphrase does not decrypt with the address key: %v", err)
			}
			sig, err := crypto.NewPGPSignatureFromArmored(mp.Signature)
			if err != nil {
				t.Fatalf("the passphrase signature is not an armored signature: %v", err)
			}
			if verified := addr.VerifyDetached(pass, sig, 0) == nil; verified == bad {
				t.Errorf("the passphrase signature verifies with the address key: %v, want %v", verified, !bad)
			}

			fingerprints := map[int][]string{}
			for _, k := range b.Keys {
				key, err := crypto.NewKeyFromArmored(k.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := key.Unlock(pass.GetBinary()); err != nil {
					t.Errorf("a key with Flags %d does not unlock with the passphrase: %v", k.Flags, err)
				}
				fingerprints[k.Flags] = append(fingerprints[k.Flags], key.GetFingerprint())
			}
			primary := f.CalendarKeyRing(cal).GetKeys()[0].GetFingerprint()
			retired := f.RetiredCalendarKeyRing(cal).GetKeys()[0].GetFingerprint()
			if !slices.Equal(fingerprints[3], []string{primary}) || !slices.Equal(fingerprints[1], []string{retired}) || len(b.Keys) != 2 {
				t.Errorf("keys by Flags = %v, want the primary (%s) alone with 3 and the retired (%s) with 1", fingerprints, primary, retired)
			}
			if len(b.Members) != 1 || b.Members[0].Permissions != 127 {
				t.Errorf("members = %+v, want ours with Permissions 127", b.Members)
			}
			if st, _ := rawGet(t, f, tk, "/calendar/v2/"+url.PathEscape(readOnly)+"/bootstrap", &b); st != 200 || b.Members[0].Permissions != 2 {
				t.Errorf("Calendar.Permissions 2: HTTP %d, members %+v; want Permissions 2", st, b.Members)
			}

			// The account's two addresses hold different keys: the first
			// AddressKeyRing's, the second OtherAddressKeyRing's.
			var addrs struct {
				Addresses []struct {
					Order int
					Keys  []struct{ PrivateKey string }
				}
			}
			if st, _ := rawGet(t, f, tk, "/core/v4/addresses", &addrs); st != 200 {
				t.Fatalf("addresses: HTTP %d", st)
			}
			byOrder := map[int]string{}
			for _, a := range addrs.Addresses {
				key, err := crypto.NewKeyFromArmored(a.Keys[0].PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				byOrder[a.Order] = key.GetFingerprint()
			}
			first, second := f.AddressKeyRing().GetKeys()[0].GetFingerprint(), f.OtherAddressKeyRing().GetKeys()[0].GetFingerprint()
			if len(byOrder) != 2 || byOrder[1] != first || byOrder[2] != second || first == second {
				t.Errorf("address keys by Order = %v, want 1: %s and 2: %s", byOrder, first, second)
			}
		})
	}
}

// The switches (research L.4): each [conflict] or [unknown] the other way.

func TestSyncUnbumpedSequence(t *testing.T) {
	t.Parallel()
	for _, drop := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "DropUnbumpedSequence"}[drop], func(t *testing.T) {
			t.Parallel()
			w := newWriter(t, protontest.Options{Write: protontest.WriteOptions{DropUnbumpedSequence: drop}})
			k := newKeys(t)
			id := w.mustCreate(t, w.create(t, simple("seq@pcb.test", "1", "First"), k))
			head := latest(t, w.f, w.tk, w.cal)
			if code := w.entryCode(t, map[string]any{"ID": id, "Event": w.update(t, simple("seq@pcb.test", "1", "Second"), k)}); code != 1000 {
				t.Fatalf("unbumped update: code %d, want 1000 either way", code)
			}
			kept := strings.Contains(open(t, k.shared, partData(w.row(t, id), "SharedEvents", 1)), "SUMMARY:First")
			entries, _, _ := feed(t, w.f, w.tk, w.cal, head)
			if kept != drop || (len(entries.CalendarEvents) == 0) != drop {
				t.Errorf("unbumped update: old row kept %v, feed %+v; want kept and no feed entry only with DropUnbumpedSequence", kept, entries.CalendarEvents)
			}
			if code := w.entryCode(t, map[string]any{"ID": id, "Event": w.update(t, simple("seq@pcb.test", "2", "Third"), k)}); code != 1000 {
				t.Fatalf("bumped update: code %d", code)
			}
			if !strings.Contains(open(t, k.shared, partData(w.row(t, id), "SharedEvents", 1)), "SUMMARY:Third") {
				t.Error("a bumped update must be stored either way")
			}
		})
	}
}

func TestSyncNewKeyPacketsOnUpdate(t *testing.T) {
	t.Parallel()
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "ignored", true: "RefuseNewKeyPacketsOnUpdate"}[refuse], func(t *testing.T) {
			t.Parallel()
			w := newWriter(t, protontest.Options{Write: protontest.WriteOptions{RefuseNewKeyPacketsOnUpdate: refuse}})
			k := newKeys(t)
			c := simple("kpu@pcb.test", "0", "First")
			id := w.mustCreate(t, w.create(t, c, k))
			stored := w.row(t, id)["SharedKeyPacket"]

			c.sharedEnc = []string{"SUMMARY:Second"}
			upd := w.update(t, c, k)
			upd["SharedKeyPacket"] = wrap(t, w.calKey, k.shared) // the same key, wrapped again
			code := w.entryCode(t, map[string]any{"ID": id, "Event": upd})
			row := w.row(t, id)
			if want := map[bool]int{false: 1000, true: 2001}[refuse]; code != want {
				t.Errorf("update with a new shared key packet: code %d, want %d", code, want)
			}
			if row["SharedKeyPacket"] != stored {
				t.Error("the row's shared key packet must stay either way")
			}
			if stored := strings.Contains(open(t, k.shared, partData(row, "SharedEvents", 1)), "SUMMARY:Second"); stored == refuse {
				t.Errorf("update stored: %v, want %v", stored, !refuse)
			}

			// A row without a calendar session key takes one on update either
			// way (carbonate and caloxide send one then; research B.5).
			c.calEnc = []string{"COMMENT:now private"}
			upd = w.update(t, c, k)
			upd["CalendarKeyPacket"] = wrap(t, w.calKey, k.cal)
			if code := w.entryCode(t, map[string]any{"ID": id, "Event": upd}); code != 1000 {
				t.Errorf("first calendar key packet on update: code %d, want 1000", code)
			}
			if got := open(t, k.cal, partData(w.row(t, id), "CalendarEvents", 1)); !strings.Contains(got, "COMMENT:now private") {
				t.Errorf("calendar encrypted part = %q", got)
			}
			// Now the row has a calendar session key: a new packet for it is
			// like the shared one.
			calPacket := w.row(t, id)["CalendarKeyPacket"]
			upd = w.update(t, c, k)
			upd["CalendarKeyPacket"] = wrap(t, w.calKey, k.cal)
			if code, want := w.entryCode(t, map[string]any{"ID": id, "Event": upd}), map[bool]int{false: 1000, true: 2001}[refuse]; code != want {
				t.Errorf("update with a second calendar key packet: code %d, want %d", code, want)
			}
			if w.row(t, id)["CalendarKeyPacket"] != calPacket {
				t.Error("the row's calendar key packet must stay either way")
			}
		})
	}
}

func TestSyncDuplicateUID(t *testing.T) {
	t.Parallel()
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "RejectDuplicateUID"}[reject], func(t *testing.T) {
			t.Parallel()
			w := newWriter(t, protontest.Options{Write: protontest.WriteOptions{RejectDuplicateUID: reject}})
			k := newKeys(t)
			w.mustCreate(t, w.create(t, simple("dup@pcb.test", "0", "One"), k))
			code := w.entryCode(t, map[string]any{"Event": w.create(t, simple("dup@pcb.test", "0", "Two"), newKeys(t))})
			rows := len(w.byUID(t, "dup@pcb.test"))
			if want := map[bool]int{false: 1000, true: 2500}[reject]; code != want || rows != map[bool]int{false: 2, true: 1}[reject] {
				t.Errorf("second master of a UID: code %d, %d rows; want %d and %d", code, rows, want, map[bool]int{false: 2, true: 1}[reject])
			}
		})
	}
}

func TestSyncBatchAtomicity(t *testing.T) {
	t.Parallel()
	for _, atomic := range []bool{false, true} {
		t.Run(map[bool]string{false: "per entry", true: "AllOrNothing"}[atomic], func(t *testing.T) {
			t.Parallel()
			w := newWriter(t, protontest.Options{Write: protontest.WriteOptions{AllOrNothing: atomic}})
			k := newKeys(t)
			doomed := w.mustCreate(t, w.create(t, simple("gone@pcb.test", "0", "x"), k))
			head := latest(t, w.f, w.tk, w.cal)
			bad := simple("b@pcb.test", "0", "Bad")
			bad.calSigned = []string{"STATUS:TENTATIVE"}
			st, ans := w.sync(t,
				map[string]any{"Event": w.create(t, simple("a@pcb.test", "0", "Good"), k)},
				map[string]any{"Event": w.create(t, bad, k)},
				map[string]any{"ID": doomed})
			stored := len(w.byUID(t, "a@pcb.test")) + len(w.byUID(t, "gone@pcb.test"))
			entries, _, _ := feed(t, w.f, w.tk, w.cal, head)
			if !atomic {
				if got := codes(ans); st != 200 || len(ans.Responses) != 2 || got[0] != 1000 || got[1] != 2000 {
					t.Errorf("per entry: HTTP %d, codes %v; want 200 with 1000 and 2000 (the delete answers nothing)", st, got)
				}
				if stored != 1 || len(entries.CalendarEvents) != 2 {
					t.Errorf("per entry: %d of a@ and gone@ stored, feed %+v; want a@ only and two entries", stored, entries.CalendarEvents)
				}
				return
			}
			if st != 422 || ans.Code != 2000 {
				t.Errorf("AllOrNothing: HTTP %d code %d, want 422 with the failing entry's 2000", st, ans.Code)
			}
			if stored != 1 || len(entries.CalendarEvents) != 0 {
				t.Errorf("AllOrNothing: %d of a@ and gone@ stored, feed %+v; want gone@ only and no entry", stored, entries.CalendarEvents)
			}
		})
	}
}
