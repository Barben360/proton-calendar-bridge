package protontest

// The event write endpoint, PUT /calendar/v1/{cal}/events/sync, with
// Proton's rules as the M4 research reads them (sections A to H, cited
// "research X"). The fake is stricter than Proton where that catches what a
// bridge must not send: it holds the calendar key, so it opens and checks
// the encrypted parts too, which Proton cannot. Where the research marks a
// behaviour [conflict] or [unknown], the fake takes the strict reading and
// a WriteOptions switch selects the other one. Every code or rule the
// research does not name is marked "assumption" where it is made (ruling
// M4-R3).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// WriteOptions select the other reading of each write behaviour the
// research could not settle (research L.4), so a test can cover both.
type WriteOptions struct {
	// DropUnbumpedSequence: an update whose SEQUENCE is not above the row's
	// answers success but keeps the old row and records no change (research
	// A.4, "silent no-op" [conflict]: carbonate and caloxide saw it live,
	// proton-cal's live suite did not). Default: the update is stored.
	DropUnbumpedSequence bool
	// RefuseNewKeyPacketsOnUpdate: an update carrying a key packet for a
	// session key the row already has answers 2001 (research B.5
	// [unknown]). Default: that key packet is ignored and the row keeps its
	// own. A key packet for a session key the row lacks (no calendar
	// encrypted part yet) is taken either way, as carbonate and caloxide
	// send it.
	RefuseNewKeyPacketsOnUpdate bool
	// RejectDuplicateUID: a create whose UID and RECURRENCE-ID (none for a
	// master) match a stored row answers 2500 (research A.4: no source saw
	// a "UID already exists" error; 2500 is the fake's choice). Default: the
	// duplicate is stored, as real data shows (M3-R3).
	RejectDuplicateUID bool
	// AllOrNothing: a call with a failing entry applies none of its entries
	// and answers HTTP 422 with the first failing entry's code and error
	// (research A.5 [unknown]). Default: each entry stands alone, the 1001
	// envelope carrying a code per entry.
	AllOrNothing bool
	// Lab, when set, chooses the answer to each create and update entry, by
	// the UID of its shared signed part and whether it is an update, before
	// the fake's own checks: it stands in for each answer Proton may give
	// where the research cannot predict it (the pcb-lab write-matrix probe's
	// tests, ruling L5). Nil: the fake answers alone.
	Lab func(uid string, update bool) LabAnswer
}

// LabAnswer is WriteOptions.Lab's choice for one entry.
type LabAnswer struct {
	// Code, when not zero, refuses the entry with it.
	Code int
	// Lenient stores what the fake would refuse on its own reading of the
	// research: properties outside the routing table (DURATION), floating
	// times and TZIDs outside the tz database (stored as UTC, the TZID as
	// sent), STATUS:TENTATIVE, a PersonalEventContent part (served as the
	// row's PersonalEvents), and an update's key packet for a session key
	// the row already has (taken, the parts read under it). Signatures,
	// decryption and the fragment form are still checked.
	Lenient bool
	// Edit, when set, changes the row as stored and served once the entry
	// succeeds: its JSON fields, which override the columns (a Proton that
	// rewrites what it stores).
	Edit func(row map[string]any)
}

// Proton codes of the sync endpoint (research A.4).
const (
	codeOK         = 1000
	codeBatch      = 1001 // the envelope of per-entry answers
	codeBadValue   = 2000 // STATUS:TENTATIVE
	codeNotAllowed = 2011 // RRULE with RECURRENCE-ID, a colour off the palette
	codeGone       = 2501 // a deleted row
	// codeInvalid is research's code for a bad signature and a low exception
	// SEQUENCE. Assumption: the fake answers it too for every other refusal
	// the research gives no code for (key packets, fragment form, routing,
	// null lists, attendees without ORGANIZER, a missing Permissions,
	// SEQUENCE with parameters: Proton answers HTTP 500 there).
	codeInvalid = 2001
	// codeBadID is research's "not a valid ID" (a GET with an unknown ID).
	// Assumption: the fake answers it for an ID never issued and for another
	// member's MemberID.
	codeBadID = 2061
	// codeExists: assumption, for RejectDuplicateUID (no source saw one).
	codeExists = 2500
)

// palette is Proton's colour palette, the only Color the endpoint takes
// (research A.2: 2011 otherwise), as proton-cal (pkg/calcolor) and carbonate
// (internal/calendar/create.go) list it. Assumption: the match is exact,
// upper case as listed.
var palette = []string{
	"#8080FF", "#DB60D6", "#EC3E7C", "#F78400", "#936D58",
	"#5252CC", "#A839A4", "#BA1E55", "#C44800", "#54473F",
	"#415DF0", "#179FD9", "#1DA583", "#3CBB3A", "#B4A40E",
	"#273EB2", "#0A77A6", "#0F735A", "#258723", "#807304",
}

// SyncCall is one request the sync endpoint received.
type SyncCall struct {
	CalendarID string
	Body       SyncRequest // zero when the body was not JSON
}

// SyncRequest is a sync body (research A.1).
type SyncRequest struct {
	MemberID string
	IsImport *int
	Events   []SyncEntry
}

// SyncEntry is one operation: a create (Event, no ID), an update (ID and
// Event) or a delete (ID alone).
type SyncEntry struct {
	ID             string
	Overwrite      *int // 0 on a create; 1 has no known meaning (refused)
	DeletionReason *int
	Event          *SyncEvent
}

// SyncEvent is an entry's Event (research A.2). Notifications and Color are
// raw so that absent (nil) and null stay apart.
type SyncEvent struct {
	Permissions, IsOrganizer           *int
	SharedKeyPacket, CalendarKeyPacket *string
	SharedEventContent                 []SyncPart
	CalendarEventContent               []SyncPart
	AttendeesEventContent              []SyncPart
	Attendees                          []SyncAttendee
	Notifications, Color               json.RawMessage
	// Invitations are out of M4's scope: anything but null or [] is refused.
	AddedProtonAttendees, RemovedAttendeeAddresses json.RawMessage
	// PersonalEventContent is a personal part (the field name is not
	// verified, research A.2): refused (2001) unless a lenient LabAnswer
	// takes it.
	PersonalEventContent json.RawMessage
}

// SyncPart is a card part as sent: Type 2 clear and signed, 3 encrypted and
// signed.
type SyncPart struct {
	Type            int
	Data, Signature string
}

// SyncAttendee is an attendee's status, re-sent on update (research A.2).
type SyncAttendee struct {
	Token   string
	Status  int
	Comment json.RawMessage
}

// syncRecord is a sync request as received, for SyncCalls.
type syncRecord struct {
	calID string
	body  []byte
}

// SyncCalls returns every request the sync endpoint received, in order,
// decoded afresh: a copy the caller may change.
func (f *Fake) SyncCalls() []SyncCall {
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	out := make([]SyncCall, 0, len(f.cal.syncs))
	for _, rec := range f.cal.syncs {
		c := SyncCall{CalendarID: rec.calID}
		_ = json.Unmarshal(rec.body, &c.Body) // not JSON: the zero body
		out = append(out, c)
	}
	return out
}

// serveSync answers PUT /calendar/v1/{cal}/events/sync. Caller holds
// f.cal.mu.
func (f *Fake) serveSync(w http.ResponseWriter, r *http.Request, cal *calendarRec) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		reply(w, http.StatusBadRequest, codeInvalid, "fake: cannot read the body")
		return
	}
	f.cal.syncs = append(f.cal.syncs, syncRecord{cal.id, body})
	var req SyncRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	// Assumption (HTTP 400/2001): a field Proton does not define is
	// refused, so a misspelt one cannot vanish.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		reply(w, http.StatusBadRequest, codeInvalid, "fake: the sync body does not decode: "+err.Error())
		return
	}
	// A call of deletes may leave MemberID out (research A.1, caloxide live).
	deletesWithoutMember := req.MemberID == "" && onlyDeletes(req.Events)
	if req.MemberID != cal.memberID && !deletesWithoutMember {
		reply(w, http.StatusUnprocessableEntity, codeBadID, "Invalid member ID")
		return
	}
	if !cal.writable() {
		// Assumption: Proton's answer is unknown; HTTP and code 403 are the fake's.
		reply(w, http.StatusForbidden, http.StatusForbidden, "fake: our member cannot write to this calendar")
		return
	}
	tx := f.begin(cal)
	nulls := nullLists(body)
	results := make([]entryResult, len(req.Events))
	for i, e := range req.Events {
		if e.Event != nil && nulls[i] != "" {
			results[i] = failed(codeInvalid, "fake: %s must be a list, [] when empty", nulls[i])
			continue
		}
		results[i] = tx.apply(e)
	}
	if f.o.Write.AllOrNothing {
		for _, res := range results {
			if res.code != codeOK {
				reply(w, http.StatusUnprocessableEntity, res.code, res.msg)
				return
			}
		}
	}
	tx.commit()
	responses := []any{}
	for i, res := range results {
		if res.delete && res.code == codeOK {
			continue // a delete answers nothing (research A.3)
		}
		answer := map[string]any{"Code": res.code}
		if res.code != codeOK {
			answer["Error"] = res.msg
		}
		if res.row != nil {
			row, ok := f.rowJSON(cal, res.row)
			if !ok {
				reply(w, http.StatusInternalServerError, 0, "fake: cannot serve the row")
				return
			}
			answer["Event"] = row
		}
		responses = append(responses, map[string]any{"Index": i, "Response": answer})
	}
	writeJSON(w, http.StatusOK, map[string]any{"Code": codeBatch, "Responses": responses})
}

func onlyDeletes(entries []SyncEntry) bool {
	return !slices.ContainsFunc(entries, func(e SyncEntry) bool { return e.Event != nil })
}

// lists are the Event fields that must be lists, [] when empty, never null
// (research A.2, proton-cal live). Assumption, stricter than Proton: absent
// is refused too (an omitted Attendees wipes the RSVPs, research A.2).
var lists = []string{"SharedEventContent", "CalendarEventContent", "AttendeesEventContent", "Attendees"}

// nullLists returns, for each entry of a sync body, the first of lists its
// Event has null or absent, "" when none: the typed decoding cannot tell a
// null list from an empty one.
func nullLists(body []byte) []string {
	var raw struct {
		Events []struct{ Event map[string]json.RawMessage }
	}
	_ = json.Unmarshal(body, &raw) // the typed decoding already succeeded
	out := make([]string, len(raw.Events))
	for i, e := range raw.Events {
		for _, name := range lists {
			if v, ok := e.Event[name]; !ok || string(bytes.TrimSpace(v)) == "null" {
				out[i] = name
				break
			}
		}
	}
	return out
}

// entryResult is one entry's outcome: its code, the row to echo (none for a
// delete or a failure) and whether it was a delete.
type entryResult struct {
	code   int
	msg    string
	row    *eventRec
	delete bool
}

func failed(code int, format string, args ...any) entryResult {
	return entryResult{code: code, msg: fmt.Sprintf(format, args...)}
}

// syncTx applies a call's entries in order to a copy of the calendar's rows,
// so an entry sees what the earlier ones did (master first); commit makes
// the copy the calendar's.
type syncTx struct {
	f       *Fake
	cal     *calendarRec
	rows    []*eventRec
	changes []feedEntry // id and action, recorded on commit
	gone    []string
}

func (f *Fake) begin(cal *calendarRec) *syncTx {
	return &syncTx{f: f, cal: cal, rows: slices.Clone(cal.events().rows)}
}

func (tx *syncTx) commit() {
	s := tx.cal.events()
	s.rows = tx.rows
	for _, c := range tx.changes {
		s.record(c.id, c.action)
	}
	for _, id := range tx.gone {
		s.gone[id] = true
	}
}

func (tx *syncTx) apply(e SyncEntry) entryResult {
	var lab LabAnswer
	if e.Event != nil && tx.f.o.Write.Lab != nil {
		lab = tx.f.o.Write.Lab(sharedSignedUID(*e.Event), e.ID != "")
		if lab.Code != 0 {
			return failed(lab.Code, "fake: the lab's answer")
		}
	}
	var res entryResult
	switch {
	case e.ID == "" && e.Event != nil:
		if e.Overwrite != nil && *e.Overwrite != 0 {
			return failed(codeInvalid, "fake: Overwrite %d has no known meaning (research A.1)", *e.Overwrite)
		}
		res = tx.create(*e.Event, lab.Lenient)
	case e.ID != "" && e.Event != nil:
		res = tx.update(e.ID, *e.Event, lab.Lenient)
	case e.ID != "":
		return tx.remove(e.ID)
	default:
		return failed(codeInvalid, "fake: an entry needs an ID, an Event or both")
	}
	if res.code == codeOK && res.row != nil && lab.Edit != nil {
		lab.Edit(res.row.parts)
	}
	return res
}

// sharedSignedUID is the UID line's value in ev's shared signed part, "" if
// none.
func sharedSignedUID(ev SyncEvent) string {
	for _, p := range ev.SharedEventContent {
		if p.Type != 2 {
			continue
		}
		for _, l := range strings.Split(p.Data, "\r\n") {
			if v, ok := strings.CutPrefix(l, "UID:"); ok {
				return v
			}
		}
	}
	return ""
}

// find returns the index of row id, or -1 and the failure to answer.
func (tx *syncTx) find(id string) (int, entryResult) {
	if i := slices.IndexFunc(tx.rows, func(r *eventRec) bool { return r.id == id }); i >= 0 {
		return i, entryResult{}
	}
	if tx.cal.events().gone[id] || slices.Contains(tx.gone, id) {
		return -1, failed(codeGone, "Event does not exist")
	}
	return -1, failed(codeBadID, "Invalid event ID")
}

func (tx *syncTx) remove(id string) entryResult {
	i, res := tx.find(id)
	if i < 0 {
		res.delete = true
		return res
	}
	tx.rows = slices.Delete(tx.rows, i, i+1)
	tx.gone = append(tx.gone, id)
	tx.changes = append(tx.changes, feedEntry{id: id, action: actionDelete})
	return entryResult{code: codeOK, delete: true}
}

// sessionKeys are a row's two session keys; cal is nil when the row has no
// calendar encrypted part.
type sessionKeys struct{ shared, cal *crypto.SessionKey }

func (tx *syncTx) create(ev SyncEvent, lenient bool) entryResult {
	if ev.SharedKeyPacket == nil {
		return failed(codeInvalid, "fake: a create needs a SharedKeyPacket")
	}
	var keys sessionKeys
	var err error
	if keys.shared, err = tx.newKeyPacket(*ev.SharedKeyPacket); err != nil {
		return failed(codeInvalid, "fake: SharedKeyPacket: %v", err)
	}
	if ev.CalendarKeyPacket != nil {
		if keys.cal, err = tx.newKeyPacket(*ev.CalendarKeyPacket); err != nil {
			return failed(codeInvalid, "fake: CalendarKeyPacket: %v", err)
		}
	}
	row, res := tx.build(ev, nil, keys, lenient)
	if row == nil {
		return res
	}
	if tx.f.o.Write.RejectDuplicateUID && slices.ContainsFunc(tx.rows, func(r *eventRec) bool {
		return r.cols.UID == row.cols.UID && sameInstant(r.cols.RecurrenceID, row.cols.RecurrenceID)
	}) {
		return failed(codeExists, "fake: an event with this UID already exists")
	}
	row.parts["SharedKeyPacket"] = *ev.SharedKeyPacket
	row.parts["CalendarKeyPacket"] = nullable(ev.CalendarKeyPacket)
	row.parts["Attendees"] = servedAttendees(ev.Attendees, row.id)
	tx.rows = append(tx.rows, row)
	tx.changes = append(tx.changes, feedEntry{id: row.id, action: actionCreate})
	return entryResult{code: codeOK, row: row}
}

func (tx *syncTx) update(id string, ev SyncEvent, lenient bool) entryResult {
	i, res := tx.find(id)
	if i < 0 {
		return res
	}
	old := tx.rows[i]
	if err := tx.f.ensureParts(tx.cal, old); err != nil {
		return failed(codeInvalid, "fake: the stored row: %v", err)
	}
	keys, err := tx.rowKeys(old)
	if err != nil {
		return failed(codeInvalid, "fake: the stored row's key packets: %v", err)
	}
	sharedPacket, calPacket := old.parts["SharedKeyPacket"], old.parts["CalendarKeyPacket"]
	if ev.SharedKeyPacket != nil && tx.f.o.Write.RefuseNewKeyPacketsOnUpdate {
		return failed(codeInvalid, "fake: an update cannot replace the row's shared key packet")
	}
	if ev.SharedKeyPacket != nil && lenient {
		if keys.shared, err = tx.newKeyPacket(*ev.SharedKeyPacket); err != nil {
			return failed(codeInvalid, "fake: SharedKeyPacket: %v", err)
		}
		sharedPacket = *ev.SharedKeyPacket
	}
	if ev.CalendarKeyPacket != nil {
		switch {
		case keys.cal == nil || lenient:
			if keys.cal, err = tx.newKeyPacket(*ev.CalendarKeyPacket); err != nil {
				return failed(codeInvalid, "fake: CalendarKeyPacket: %v", err)
			}
			calPacket = *ev.CalendarKeyPacket
		case tx.f.o.Write.RefuseNewKeyPacketsOnUpdate:
			return failed(codeInvalid, "fake: an update cannot replace the row's calendar key packet")
		}
	}
	row, res := tx.build(ev, old, keys, lenient)
	if row == nil {
		return res
	}
	if row.cols.UID != old.cols.UID {
		return failed(codeInvalid, "fake: an update cannot change the UID")
	}
	if tx.f.o.Write.DropUnbumpedSequence && row.cols.Sequence <= old.cols.Sequence {
		return entryResult{code: codeOK, row: old}
	}
	row.id, row.sharedID, row.createTime = old.id, old.sharedID, old.createTime
	for _, k := range []string{"AddressKeyPacket", "AddressID", "PersonalEvents"} {
		if k == "PersonalEvents" && len(bytes.TrimSpace(ev.PersonalEventContent)) > 0 {
			continue // a lenient update's own, from build
		}
		row.parts[k] = old.parts[k] // kept; for personal parts, research L.4.10 [unknown]
	}
	row.parts["SharedKeyPacket"], row.parts["CalendarKeyPacket"] = sharedPacket, calPacket
	row.parts["Attendees"] = servedAttendees(ev.Attendees, row.id) // replaced, IDs kept per token
	tx.rows[i] = row
	tx.changes = append(tx.changes, feedEntry{id: id, action: actionUpdate})
	return entryResult{code: codeOK, row: row}
}

// build checks ev (old: the row it updates, nil on a create) and returns
// the row it makes, with new IDs, or nil and the failure.
func (tx *syncTx) build(ev SyncEvent, old *eventRec, keys sessionKeys, lenient bool) (*eventRec, entryResult) {
	if ev.Permissions == nil { // assumption: every bridge sends it (research A.2)
		return nil, failed(codeInvalid, "fake: Permissions is missing")
	}
	for _, raw := range []json.RawMessage{ev.AddedProtonAttendees, ev.RemovedAttendeeAddresses} {
		if s := string(bytes.TrimSpace(raw)); s != "" && s != "null" && s != "[]" {
			return nil, failed(codeInvalid, "fake: invitations are out of scope")
		}
	}
	var personal []SyncPart
	if s := string(bytes.TrimSpace(ev.PersonalEventContent)); s != "" && s != "null" {
		var p SyncPart
		if !lenient || json.Unmarshal(ev.PersonalEventContent, &p) != nil {
			return nil, failed(codeInvalid, "fake: PersonalEventContent is not a field the research knows")
		}
		personal = []SyncPart{p}
	}
	card, err := tx.openCard(ev, keys, lenient)
	if err != nil {
		return nil, failed(codeInvalid, "fake: %v", err)
	}
	cols, err := parseColumns(card[slotSharedSigned], lenient)
	if err != nil {
		return nil, failed(codeInvalid, "fake: shared signed part: %v", err)
	}
	if card[slotAttendees] != nil && propValues(card[slotSharedSigned], "ORGANIZER") == nil {
		// Research A.4 (carbonate live); the code is an assumption.
		return nil, failed(codeInvalid, "Shared event has attendees but does not have an organizer")
	}
	if cols.RRule != "" && cols.RecurrenceID != nil {
		return nil, failed(codeNotAllowed, "These properties are not supported: RECURRENCE-ID with RRULE")
	}
	for _, p := range card[slotCalendarSigned] {
		if p.name == "STATUS" && strings.EqualFold(p.value, "TENTATIVE") && !lenient {
			return nil, failed(codeBadValue, "fake: STATUS:TENTATIVE is refused")
		}
	}
	if cols.RecurrenceID != nil {
		m := tx.master(cols.UID)
		switch {
		case m != nil && !m.cols.HasSequence:
			// Live 2026-11-13 (HUMAN-CHECKS 6.3 D7): the create of an
			// exception of a master whose signed part has no SEQUENCE;
			// the message is the fake's (pcb logged the code alone). The
			// same refusal of an exception update is [assumed, not
			// measured live].
			return nil, failed(codeInvalid, "fake: the main event has no SEQUENCE")
		case m != nil && cols.Sequence < m.cols.Sequence:
			return nil, failed(codeInvalid, "Single edits should have a Sequence greater or equal to main event")
		}
	}
	if cols.Color, err = colorOf(ev.Color, old); err != nil {
		return nil, failed(codeNotAllowed, "Not a valid Proton color")
	}
	if cols.Notifications, err = notificationsOf(ev.Notifications); err != nil {
		return nil, failed(codeInvalid, "fake: Notifications: %v", err)
	}
	cols.Permissions, cols.IsOrganizer = *ev.Permissions, 1
	switch {
	case ev.IsOrganizer != nil:
		cols.IsOrganizer = *ev.IsOrganizer
	case old != nil:
		cols.IsOrganizer = old.cols.IsOrganizer
	}
	row := tx.f.newRow(cols)
	author := tx.f.author(tx.cal)
	row.parts = map[string]any{ // Attendees: by the caller, once the row ID is final
		"Author": author, "AddressKeyPacket": nil, "AddressID": nil, "PersonalEvents": []any{},
		"SharedEvents":    servedParts(ev.SharedEventContent, author),
		"CalendarEvents":  servedParts(ev.CalendarEventContent, author),
		"AttendeesEvents": servedParts(ev.AttendeesEventContent, author),
	}
	if personal != nil {
		row.parts["PersonalEvents"] = servedParts(personal, author)
	}
	return row, entryResult{}
}

// master is the first master row of uid, nil when none.
func (tx *syncTx) master(uid string) *eventRec {
	for _, r := range tx.rows {
		if r.cols.UID == uid && r.cols.RecurrenceID == nil {
			return r
		}
	}
	return nil
}

// newKeyPacket opens a key packet sent with a write: it must open with the
// primary calendar key and with no other listed key (constraints: to the
// primary calendar key only).
func (tx *syncTx) newKeyPacket(b64 string) (*crypto.SessionKey, error) {
	kp, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("not base64")
	}
	primary := tx.cal.primaryKey()
	for _, k := range tx.cal.keys {
		if k.key == primary {
			continue
		}
		if kr, err := crypto.NewKeyRing(k.key); err == nil {
			if _, err := kr.DecryptSessionKey(kp); err == nil {
				return nil, errors.New("encrypted to a calendar key that is not the primary one")
			}
		}
	}
	kr, err := tx.cal.primaryRing()
	if err != nil {
		return nil, err
	}
	sk, err := kr.DecryptSessionKey(kp)
	if err != nil {
		return nil, errors.New("does not open with the primary calendar key")
	}
	return sk, nil
}

// rowKeys opens a stored row's session keys: the shared one from its
// SharedKeyPacket (any listed calendar key: the row may predate a rotation)
// or its AddressKeyPacket (the address key), the calendar one from its
// CalendarKeyPacket, nil when it has none.
func (tx *syncTx) rowKeys(r *eventRec) (sessionKeys, error) {
	var calKR *crypto.KeyRing
	for _, k := range tx.cal.keys {
		var err error
		if calKR == nil {
			calKR, err = crypto.NewKeyRing(k.key)
		} else {
			err = calKR.AddKey(k.key)
		}
		if err != nil {
			return sessionKeys{}, err
		}
	}
	addrKR, err := memberKey(tx.cal)
	if err != nil {
		return sessionKeys{}, err
	}
	open := func(field string, kr *crypto.KeyRing) (*crypto.SessionKey, error) {
		b64, _ := r.parts[field].(string)
		if b64 == "" {
			return nil, nil
		}
		kp, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, err
		}
		return kr.DecryptSessionKey(kp)
	}
	var keys sessionKeys
	if keys.shared, err = open("SharedKeyPacket", calKR); err != nil {
		return sessionKeys{}, err
	}
	if keys.shared == nil {
		if keys.shared, err = open("AddressKeyPacket", addrKR); err != nil {
			return sessionKeys{}, err
		}
	}
	if keys.cal, err = open("CalendarKeyPacket", calKR); err != nil {
		return sessionKeys{}, err
	}
	return keys, nil
}

// slot is a card part's place in the row.
type slot int

const (
	slotSharedSigned slot = iota
	slotSharedEncrypted
	slotCalendarSigned
	slotCalendarEncrypted
	slotAttendees
)

func (s slot) String() string {
	return [...]string{"shared signed part", "shared encrypted part", "calendar signed part", "calendar encrypted part", "attendees part"}[s]
}

// routing is the constraints' routing table: what each part holds besides
// UID and DTSTAMP. The shared encrypted part holds every property no other
// part owns, except the ones never routed. Assumption: Proton reads its
// signed parts only; the fake checks the encrypted ones too, as the web
// app would misread a misrouted property.
var routing = map[slot][]string{
	slotSharedSigned:      {"DTSTART", "DTEND", "RECURRENCE-ID", "RRULE", "EXDATE", "SEQUENCE", "ORGANIZER", "X-PM-CONFERENCE-ID"},
	slotCalendarSigned:    {"EXDATE", "STATUS", "TRANSP"},
	slotCalendarEncrypted: {"COMMENT"},
	slotAttendees:         {"ATTENDEE"},
}

// neverRouted are the properties no part may hold: DURATION is sent as
// DTEND, COLOR is the row's Color, the rest are the bridge's own or
// VCALENDAR-level.
var neverRouted = []string{"DURATION", "COLOR", "X-PCB-INTEGRITY", "X-PCB-PLACEHOLDER", "X-PM-SESSION-KEY",
	"VERSION", "PRODID", "CALSCALE", "METHOD"}

// routedTo says whether the routing table puts property name in slot s.
func routedTo(s slot, name string) bool {
	switch {
	case name == "UID" || name == "DTSTAMP":
		return true
	case slices.Contains(neverRouted, name):
		return false
	case s != slotSharedEncrypted:
		return slices.Contains(routing[s], name)
	}
	for _, owned := range routing {
		if slices.Contains(owned, name) {
			return false
		}
	}
	return true
}

// partGroup is one of the Event's three part lists.
type partGroup struct {
	name   string
	parts  []SyncPart
	slots  map[int]slot // part Type → slot
	key    *crypto.SessionKey
	needed []int // the Types the list must hold
}

// openCard checks every part of ev and returns their properties by slot: the
// part lists' shape, each signature (binary mode, by the key of the
// calendar's address, over the exact bytes of a signed part and over the
// plaintext of an encrypted one), each
// decryption under its session key, the fragment form, the routing, one UID
// across parts and the same EXDATEs in both signed parts.
func (tx *syncTx) openCard(ev SyncEvent, keys sessionKeys, lenient bool) (map[slot][]property, error) {
	addrKR, err := memberKey(tx.cal)
	if err != nil {
		return nil, err
	}
	card := map[slot][]property{}
	for _, g := range []partGroup{
		{"SharedEventContent", ev.SharedEventContent, map[int]slot{2: slotSharedSigned, 3: slotSharedEncrypted}, keys.shared, []int{2, 3}},
		{"CalendarEventContent", ev.CalendarEventContent, map[int]slot{2: slotCalendarSigned, 3: slotCalendarEncrypted}, keys.cal, nil},
		{"AttendeesEventContent", ev.AttendeesEventContent, map[int]slot{3: slotAttendees}, keys.shared, nil},
	} {
		seen := map[int]bool{}
		for _, p := range g.parts {
			s, ok := g.slots[p.Type]
			if !ok || seen[p.Type] {
				return nil, fmt.Errorf("%s: a part of Type %d is not expected", g.name, p.Type)
			}
			seen[p.Type] = true
			text := []byte(p.Data)
			if p.Type == 3 {
				if g.key == nil {
					return nil, fmt.Errorf("%s: no session key for it", s)
				}
				if text, err = decryptPart(p.Data, g.key); err != nil {
					return nil, fmt.Errorf("%s: %w", s, err)
				}
			}
			if !verifies(text, p.Signature, addrKR) {
				return nil, fmt.Errorf("%s: Provide data signed using the address key", s)
			}
			if card[s], err = partProperties(s, string(text), lenient); err != nil {
				return nil, fmt.Errorf("%s: %w", s, err)
			}
		}
		for _, t := range g.needed {
			if !seen[t] {
				return nil, fmt.Errorf("%s: no part of Type %d", g.name, t)
			}
		}
	}
	uid := propValues(card[slotSharedSigned], "UID")
	for s, props := range card {
		if !slices.Equal(propValues(props, "UID"), uid) {
			return nil, fmt.Errorf("%s: another UID than the shared signed part's", s)
		}
	}
	// Assumption: the same EXDATE lines in the same order (every writer
	// copies them, research B.1).
	if !slices.Equal(propValues(card[slotSharedSigned], "EXDATE"), propValues(card[slotCalendarSigned], "EXDATE")) {
		return nil, errors.New("EXDATE must be the same in both signed parts")
	}
	return card, nil
}

// decryptPart opens an encrypted part's Data: base64 of a SEIPD packet under
// sk.
func decryptPart(data string, sk *crypto.SessionKey) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, errors.New("data is not base64")
	}
	plain, err := sk.Decrypt(raw)
	if err != nil {
		return nil, errors.New("does not decrypt with its session key")
	}
	return plain.GetBinary(), nil
}

// verifies reports whether armoredSig is a binary-mode detached signature
// of text by kr, over the exact bytes, as internal/proton/cards checks it.
// Binary mode only is stricter than Proton: carbonate's text-mode
// signatures (type 0x01) work live (research B.2), but the constraints
// require binary, which covers the bytes sent.
func verifies(text []byte, armoredSig string, kr *crypto.KeyRing) bool {
	sig, err := crypto.NewPGPSignatureFromArmored(armoredSig)
	if err != nil {
		return false
	}
	p, err := packet.Read(bytes.NewReader(sig.GetBinary()))
	if s, ok := p.(*packet.Signature); err != nil || !ok || s.SigType != packet.SigTypeBinary {
		return false
	}
	return kr.VerifyDetached(crypto.NewPlainMessage(text), sig, 0) == nil
}

// partProperties checks a part's text is a fragment in Proton's form
// (constraints: one VEVENT alone in a VCALENDAR, CRLF, lines of at most 75
// octets, no CRLF after the last line) holding one UID, one DTSTAMP and only
// what the routing table gives slot s, and returns its properties.
func partProperties(s slot, text string, lenient bool) ([]property, error) {
	if strings.HasSuffix(text, "\r\n") {
		return nil, errors.New("a CRLF after the last line")
	}
	lines := strings.Split(text, "\r\n")
	for _, l := range lines {
		switch {
		case strings.ContainsAny(l, "\r\n"):
			return nil, errors.New("a line end that is not CRLF")
		case len(l) > 75:
			return nil, errors.New("a line over 75 octets")
		case l == "":
			return nil, errors.New("an empty line")
		}
	}
	n := len(lines)
	if n < 4 || lines[0] != "BEGIN:VCALENDAR" || lines[1] != "BEGIN:VEVENT" || lines[n-2] != "END:VEVENT" || lines[n-1] != "END:VCALENDAR" {
		return nil, errors.New("not one VEVENT alone in a VCALENDAR")
	}
	if c := lines[2][0]; c == ' ' || c == '\t' {
		return nil, errors.New("a continuation line first")
	}
	var props []property
	for _, u := range cardUnits(strings.Join(lines[2:n-2], "\r\n")) {
		p, err := parseProperty(u)
		if err != nil {
			return nil, err
		}
		if p.name == "BEGIN" || p.name == "END" {
			return nil, errors.New("a nested component")
		}
		if !routedTo(s, p.name) && !lenient {
			return nil, fmt.Errorf("%s does not belong here", p.name)
		}
		props = append(props, p)
	}
	if len(propValues(props, "UID")) != 1 || len(propValues(props, "DTSTAMP")) != 1 {
		return nil, errors.New("not one UID and one DTSTAMP")
	}
	return props, nil
}

// unescapeText is a TEXT value unescaped (RFC 5545 §3.3.11): "\\", "\;",
// "\," and "\n" or "\N" stand for what they escape; any other backslash
// is kept. Proton stores a UID so: the row of `UID:a\,b` is `a,b` (measured
// live 2026-11-13, HUMAN-CHECKS W10).
func unescapeText(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			switch c := v[i+1]; c {
			case '\\', ';', ',':
				b.WriteByte(c)
				i++
				continue
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// propValues are the values of the properties named name, in order.
func propValues(props []property, name string) []string {
	var out []string
	for _, p := range props {
		if p.name == name {
			out = append(out, p.value)
		}
	}
	return out
}

// parseColumns reads the row's columns from its shared signed part, as
// Proton does (research C): times from DTSTART and DTEND (a DATE is a full
// day at midnight UTC), the zones from their TZID, RRULE verbatim,
// RECURRENCE-ID and EXDATE as unix seconds. SEQUENCE is a bare integer
// (research A.4).
func parseColumns(props []property, lenient bool) (columns, error) {
	var c columns
	var hasStart, hasEnd, endDate bool
	for _, p := range props {
		var err error
		switch p.name {
		case "UID":
			c.UID = unescapeText(p.value)
		case "DTSTART":
			var t time.Time
			t, c.FullDay, c.StartTZ, err = instantOf(p, lenient)
			c.Start, hasStart = t.Unix(), true
		case "DTEND":
			var t time.Time
			t, endDate, c.EndTZ, err = instantOf(p, lenient)
			c.End, hasEnd = t.Unix(), true
		case "RRULE":
			c.RRule = p.value
		case "RECURRENCE-ID":
			var t time.Time
			t, _, _, err = instant(p, p.value)
			c.RecurrenceID = new(t.Unix())
		case "EXDATE":
			for _, v := range strings.Split(p.value, ",") {
				t, _, _, e := instant(p, v)
				c.Exdates, err = append(c.Exdates, t.Unix()), errors.Join(err, e)
			}
		case "SEQUENCE":
			if len(p.params) > 0 {
				return columns{}, errors.New("SEQUENCE with parameters")
			}
			c.Sequence, err = strconv.Atoi(p.value)
			c.HasSequence = true
		}
		if err != nil {
			return columns{}, fmt.Errorf("%s: %w", p.name, err)
		}
	}
	switch {
	case !hasStart:
		return columns{}, errors.New("no DTSTART")
	case hasEnd && endDate != c.FullDay:
		return columns{}, errors.New("DTSTART and DTEND of different types")
	case !hasEnd && c.FullDay: // RFC 5545: one day
		c.End, c.EndTZ = c.Start+86400, c.StartTZ
	case !hasEnd:
		c.End, c.EndTZ = c.Start, c.StartTZ
	}
	return c, nil
}

// instantOf is instant on p's value; lenient, a floating time or a TZID
// outside the tz database is read as UTC and keeps the TZID as sent.
func instantOf(p property, lenient bool) (time.Time, bool, string, error) {
	t, date, zone, err := instant(p, p.value)
	if err != nil && lenient {
		if lt, lerr := time.Parse(localLayout, p.value); lerr == nil {
			return lt, false, p.params["TZID"], nil
		}
	}
	return t, date, zone, err
}

// colorOf reads an entry's Color: absent or null keeps the row's on update
// (research A.2) and is none on create; anything else must be a palette
// colour.
func colorOf(raw json.RawMessage, old *eventRec) (string, error) {
	if s := string(bytes.TrimSpace(raw)); s == "" || s == "null" {
		if old != nil {
			return old.cols.Color, nil
		}
		return "", nil
	}
	var c string
	if err := json.Unmarshal(raw, &c); err != nil || !slices.Contains(palette, c) {
		return "", errors.New("not a palette colour")
	}
	return c, nil
}

// notificationsOf reads an entry's Notifications (research D): absent is
// null (the calendar's defaults) on an update as on create, never the
// row's own [verified: live 2026-11-13, L.4 #10]; null is null, [] none, a
// list is checked (Type 0 email or 1 device, Trigger a duration).
func notificationsOf(raw json.RawMessage) ([]Notification, error) {
	if s := string(bytes.TrimSpace(raw)); s == "" || s == "null" {
		return nil, nil
	}
	var list []Notification
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil {
		return nil, errors.New("not a list of {Type, Trigger}")
	}
	for _, n := range list {
		if _, _, err := parseDuration(n.Trigger); err != nil || (n.Type != 0 && n.Type != 1) {
			return nil, fmt.Errorf("reminder {%d %q}", n.Type, n.Trigger)
		}
	}
	return list, nil
}

// servedParts are parts as a row serves them: as sent, with their author.
func servedParts(parts []SyncPart, author string) []any {
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, map[string]any{"Type": p.Type, "Data": p.Data, "Signature": p.Signature, "Author": author})
	}
	return out
}

// servedAttendees is the row's Attendees column from the statuses sent.
func servedAttendees(list []SyncAttendee, rowID string) []any {
	out := make([]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{"ID": attendeeID(rowID, a.Token), "Token": a.Token, "Status": a.Status, "Permissions": 3})
	}
	return out
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func sameInstant(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
