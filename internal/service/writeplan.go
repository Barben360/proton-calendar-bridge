package service

// The write planner (Task 7a): given a UID's rows, opened and checked, and a
// client's parsed PUT body, it decides the exact operations of the one Sync
// call that writes it, sealed, or refuses with a *WriteRefusal (CalDAV 403
// with a pcb precondition). It does no I/O and reads no clock: the write time
// and the keys come in through Defaults. The executor (write.go) refreshes the
// rows, evaluates the precondition, calls Plan or PlanDelete, sends the ops in
// one Sync call and reads the result back.

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// The pcb precondition names of a *WriteRefusal (CalDAV: 403 with
// <pcb:name xmlns:pcb="urn:pcb">). A refusal of the iCalendar codec keeps its
// own code under the same prefix: "pcb:" + ical.RefusalCode (for instance
// pcb:unsupported-recurrence, pcb:misrouted-stored-line).
const (
	// RefusePlaceholder: a row of the event renders as a placeholder (it
	// does not decrypt or parse, or the calendar's keys failed; ruling
	// M3-R4). The service's state decides, never an X-PCB-PLACEHOLDER line.
	RefusePlaceholder = "pcb:placeholder"
	// RefuseUndecryptable: a part of a row does not decrypt (a personal part
	// Open skipped), although the row renders.
	RefuseUndecryptable = "pcb:undecryptable"
	// RefuseNotOurs: a part is authored by an address that is not ours
	// (ruling M4-R18): we never re-sign or re-encrypt another author's
	// content.
	RefuseNotOurs = "pcb:not-ours"
	// RefuseUnverified: a part of ours is unsigned or its signature does not
	// verify, whatever its kind (rulings M4-R18, M4-R19).
	RefuseUnverified = "pcb:unverified"
	// RefuseInvitation: a row carries an AddressKeyPacket, someone else's
	// invitation (constraints, research B.3).
	RefuseInvitation = "pcb:invitation"
	// RefuseUntrustedKey: a row's session key is not proven ours
	// (cards.ErrUntrustedSessionKey, rulings M4-R14, M4-R19), or a row's
	// UID column is not the event's (the proof binds to the UID Open read).
	// The executor's key-packet pinning (M4-R17) uses it too.
	RefuseUntrustedKey = "pcb:untrusted-key"
	// RefuseAttendeeChange: the body adds or removes an attendee, a new
	// event carries attendees, or a new exception has another attendee set
	// than its master (invitations are out of scope; ruling M4-R28).
	RefuseAttendeeChange = "pcb:attendee-change"
	// RefuseUnsupportedChange: a change v1 cannot write: one needing a
	// session key the row lacks (cards.ErrNeedsNewSessionKey, rulings
	// M4-R13, M4-R15), an event without its master, rows Proton should not
	// hold (two exception rows for one occurrence, two parts of one kind).
	RefuseUnsupportedChange = "pcb:unsupported-change"
	// RefuseMisrouted is ical.MisroutedStoredLine's name, and
	// cards.ErrMisroutedProperty's (ruling M4-R16, M4-R19): a signed part
	// would carry a property that belongs in an encrypted part.
	RefuseMisrouted = "pcb:" + string(ical.MisroutedStoredLine)
	// RefusePersonalPart: a row of the event holds a personal part, which an
	// update never re-sends (M4-R29); what Proton does to a part left out is
	// live check L.4 #10, so the update is refused until it passes
	// (write-I3). A delete is not.
	RefusePersonalPart = "pcb:personal-part"
)

// WriteRefusal is a write the bridge refuses (CalDAV 403). Code is a pcb
// precondition name (the Refuse* constants, or "pcb:" + an ical refusal
// code). Reason is short and fixed: it names rules and properties, never
// event content, Proton's text or key material.
type WriteRefusal struct {
	Code   string
	Reason string
}

func (r *WriteRefusal) Error() string { return "write refused (" + r.Code + "): " + r.Reason }

func refused(code, reason string) *WriteRefusal { return &WriteRefusal{Code: code, Reason: reason} }

// asRefusal maps a refusal of the codec or of sealing to a *WriteRefusal and
// returns any other error as it is (a fault, not a refusal).
func asRefusal(err error) error {
	var r *ical.Refusal
	switch {
	case err == nil:
		return nil
	case errors.As(err, &r):
		return refused("pcb:"+string(r.Code), r.Reason)
	case errors.Is(err, cards.ErrMisroutedProperty):
		return refused(RefuseMisrouted, "a signed part would hold a property that belongs in an encrypted part")
	case errors.Is(err, cards.ErrUntrustedSessionKey):
		return refused(RefuseUntrustedKey, "the event's session key is not proven ours")
	case errors.Is(err, cards.ErrNeedsNewSessionKey):
		return refused(RefuseUnsupportedChange, "the change needs a session key the event does not have")
	}
	return err
}

// GroupKeys are the keys a group opens with, as the render uses them.
// Calendar is the calendar's keyring, nil when its keys did not unlock
// (every row is then a placeholder). Addresses are our address keyrings by
// address ID; Own returns our keyring for an email that is one of our
// addresses, nil otherwise.
type GroupKeys struct {
	Calendar  *crypto.KeyRing
	Addresses map[string]*crypto.KeyRing
	Own       func(email string) *crypto.KeyRing
}

// GroupRow is one row of a group: the cached row, its opened event and
// whether the service renders it as a placeholder.
type GroupRow struct {
	cache.StoredRow
	// Event is the row opened (cards.Open); zero for a placeholder.
	Event cards.Event
	// Placeholder: the row renders as a placeholder (ruling M3-R4): the
	// calendar's keys failed, a part does not decrypt, the parts do not
	// merge, or the row's Notifications do not read.
	Placeholder bool
}

// Group is the rows of one UID in one calendar, in the cache's order
// (master first, then by recurrence ID, then by row ID), opened. A create
// plans against a Group with the body's UID and no rows.
type Group struct {
	UID  string
	Rows []GroupRow
}

// OpenGroup opens every row of g as the render does (decryptRow): a row the
// render would serve as a placeholder is marked so.
func OpenGroup(g cache.Group, k GroupKeys) Group {
	out := Group{UID: g.UID, Rows: make([]GroupRow, len(g.Rows))}
	for i, row := range g.Rows {
		ev, ok := openRow(row, k)
		out.Rows[i] = GroupRow{StoredRow: row, Event: ev, Placeholder: !ok}
	}
	return out
}

func openRow(row cache.StoredRow, k GroupKeys) (cards.Event, bool) {
	if k.Calendar == nil {
		return cards.Event{}, false
	}
	ev, err := cards.Open(row.Raw, k.Calendar, k.Addresses, k.Own)
	if err != nil {
		return cards.Event{}, false
	}
	if _, err := ical.MergeEvent(texts(ev.Parts)); err != nil {
		return cards.Event{}, false
	}
	if _, err := ical.Alarms(ev.Notifications, nil, row.FullDay); err != nil {
		return cards.Event{}, false
	}
	return ev, true
}

func texts(parts []cards.Part) [][]byte {
	out := make([][]byte, len(parts))
	for i, p := range parts {
		out[i] = p.Text
	}
	return out
}

// Defaults are the calendar's inputs to a plan: its default notifications
// (part-day and full-day, the rendered set of a row whose Notifications are
// null), the keys that seal (Signer, the primary key of our member's address
// alone in its ring; CalendarPrimary, the primary calendar key alone in its
// ring, the recipient of a create's key packets) and the write time (ruling
// M4-R8's DTSTAMP).
type Defaults struct {
	PartDay, FullDay        []ical.Notification
	Signer, CalendarPrimary *crypto.KeyRing
	Now                     time.Time
}

func (d Defaults) notifications(allDay bool) []ical.Notification {
	if allDay {
		return d.FullDay
	}
	return d.PartDay
}

// WritePlan is what one write sends: the operations of one Sync call, master
// first, then the exceptions by recurrence ID (updates and creates), then the
// deletes. Ops is empty when the body changes nothing: nothing is sent.
// Created says the event is new (CalDAV 201). Notes are what the plan
// accepted and normalised, for a debug log: fixed strings, no content.
type WritePlan struct {
	Ops     []events.Op
	Created bool
	Notes   []string
}

// Notes of a plan besides ParseWrite's.
const (
	noteStamp  = "DTSTAMP set to the write time"
	noteEnd    = "DTEND defaulted"
	noteAlarms = "unsupported VALARMs dropped"
)

// row is a group row as the planner reads it.
type row struct {
	*GroupRow
	parts  map[ical.PartKind][]byte // the parts Patch reads (no personal part)
	stored ical.Component           // the parts merged, as rendered before alarms and row data
	meta   rowMeta
}

// rowMeta is what the planner reads from the row as received: the columns
// cards.Open does not return. Only UID is typed (Open read it as a string);
// the others stay raw, so that a shape the render never reads (an
// IsOrganizer of another type, say) neither makes the row unreadable nor
// blocks a delete. They are interpreted only when re-sent, and a value that
// cannot be is a fault (rowFields), not a refusal.
type rowMeta struct {
	UID                                      string
	AddressKeyPacket, IsOrganizer, Attendees json.RawMessage
}

// invitation reports whether a row's raw AddressKeyPacket wraps its key to
// one of our addresses: anything but absent, null or "" counts.
func invitation(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", `""`:
		return false
	}
	return true
}

// checkGroup applies the group rules before any planning (rulings M4-R18,
// M4-R19, constraints): a row that is a placeholder, someone else's
// invitation, of another UID, with a part that did not decrypt, a part not
// authored by us or a part of ours that is not verified (unsigned parts of
// every kind included, unlike the render's unverifiedParts), a personal
// part (write-I3: never re-sent, live check L.4 #10), two parts of one
// kind, or two exception rows for one occurrence is refused.
func checkGroup(g Group) ([]*row, error) {
	out := make([]*row, len(g.Rows))
	seen := map[int64]bool{}
	for i := range g.Rows {
		gr := &g.Rows[i]
		if gr.Placeholder {
			return nil, refused(RefusePlaceholder, "the event does not decrypt or cannot be read")
		}
		var meta rowMeta
		if err := json.Unmarshal(gr.Raw, &meta); err != nil { // Open read it: a Group built otherwise
			return nil, errors.New("planning a write: a row is not an event row")
		}
		switch {
		case invitation(meta.AddressKeyPacket):
			return nil, refused(RefuseInvitation, "the event is an invitation from someone else")
		case meta.UID != g.UID:
			// The key proof binds to the row's UID (ical.ProvesKey): it
			// must be the event's (Task 4 re-review 3).
			return nil, refused(RefuseUntrustedKey, "a row of the event has another UID")
		case gr.Event.SkippedParts > 0:
			return nil, refused(RefuseUndecryptable, "a part of the event does not decrypt")
		}
		r := &row{GroupRow: gr, parts: map[ical.PartKind][]byte{}, meta: meta}
		for _, p := range gr.Event.Parts {
			switch {
			case !p.Own:
				return nil, refused(RefuseNotOurs, "a part of the event is authored by an address that is not ours")
			case !p.Signed || !p.Verified:
				return nil, refused(RefuseUnverified, "a part of the event is unsigned or its signature does not verify")
			case p.Kind == cards.Personal:
				return nil, refused(RefusePersonalPart, "this event has per-user data pcb cannot rewrite yet")
			}
			k := ical.PartKind(p.Kind)
			if _, dup := r.parts[k]; dup {
				return nil, refused(RefuseUnsupportedChange, "a row of the event holds two parts of one kind")
			}
			r.parts[k] = p.Text
		}
		stored, err := ical.MergeEvent(texts(gr.Event.Parts))
		if err != nil { // OpenGroup merged them: a Group built otherwise
			return nil, refused(RefusePlaceholder, "the event does not decrypt or cannot be read")
		}
		r.stored = stored
		if rid := gr.RecurrenceID; rid != nil {
			if seen[*rid] {
				return nil, refused(RefuseUnsupportedChange, "two rows of the event are exceptions for one occurrence")
			}
			seen[*rid] = true
		}
		out[i] = r
	}
	return out, nil
}

// PlanDelete plans the DELETE of the whole object: every row of the group,
// in one call (deleting a master alone would orphan its exceptions,
// research E.2). A delete re-signs and re-encrypts nothing, so the trust
// checks of a PUT (placeholder, undecryptable, unverified, not ours) do not
// apply: one can delete an event one cannot read (ruling M4-R27). Someone
// else's invitation (a row with an AddressKeyPacket) is still refused, read
// from the row as received (that field alone, whatever the others hold),
// whether it opens or not. A row that is not a JSON object, which the cache
// never holds, and an empty group (the executor answers 404 first) are
// faults.
func PlanDelete(current Group) (WritePlan, error) {
	if len(current.Rows) == 0 {
		return WritePlan{}, errors.New("planning a delete: the event has no rows")
	}
	p := WritePlan{Ops: make([]events.Op, len(current.Rows))}
	for i, r := range current.Rows {
		var meta struct{ AddressKeyPacket json.RawMessage }
		switch {
		case json.Unmarshal(r.Raw, &meta) != nil:
			return WritePlan{}, errors.New("planning a delete: a row is not an event row")
		case invitation(meta.AddressKeyPacket):
			return WritePlan{}, refused(RefuseInvitation, "the event is an invitation from someone else")
		}
		p.Ops[i] = events.Op{Delete: &events.Delete{ID: r.ID}}
	}
	return p, nil
}

// Plan plans a PUT of in over current, the rows of in's UID (none for a
// create), with the calendar's defaults d. The group checks come first
// (checkGroup). Then the body is mirrored (constraints, research E.2):
//   - no row: the master and its exceptions are created (a body without a
//     master cannot be created);
//   - a master row (the one the render serves when duplicates exist; the
//     others are left alone): the master is updated, an exception row the
//     body still has is updated, one it no longer has is deleted (deleting
//     one occurrence is the master's new EXDATE plus that delete), a new
//     exception is created;
//   - exception rows only (a series whose master is gone, which Thunderbird
//     sends with a faked master that ParseWrite drops, ruling M4-R11): the
//     exception rows are updated or deleted; no master and no new exception
//     is ever created.
//
// Each row is patched in place (ical.Patch) after ruling M4-R8's defaults
// (DTSTAMP the write time, DTEND from DTSTART); an attendee added or removed
// is refused, edits of an existing attendee's parameters are ignored (ruling
// M4-R9), a new exception takes its master's attendees when it has the same
// set and is refused otherwise (ruling M4-R28). SEQUENCE is max(incoming, stored), stored+1 when the shared signed
// content changes and the client did not bump it, and an exception's is at
// least its master's (ruling M4-R4). Every master written carries a SEQUENCE,
// 0 when neither the client nor the row has one: Proton refuses an exception
// of a master whose signed part has none (live 2026-11-13, HUMAN-CHECKS 6.3
// D7). A master stored without one gets SEQUENCE:0 with any write to its
// series (an exception created, updated or deleted), first in the call, its
// body otherwise unchanged; a body that changes nothing still plans nothing. Notifications follow the tri-state
// (research D). An update re-sends the row's Color (null when it has none),
// Attendees and IsOrganizer, reuses the row's session keys only when
// SessionKeys trusts both, and carries no key packet; a create mints its
// keys and sends Color null (an exception the master's Color). A row whose
// parts and notifications do not change gets no operation.
func Plan(current Group, in ical.Incoming, d Defaults) (WritePlan, error) {
	if in.UID == "" || in.UID != current.UID {
		return WritePlan{}, errors.New("planning a write: the body's UID is not the event's")
	}
	if d.Now.IsZero() {
		return WritePlan{}, errors.New("planning a write: no write time")
	}
	rows, err := checkGroup(current)
	if err != nil {
		return WritePlan{}, err
	}
	pl := planner{d: d, notes: slices.Clone(in.Notes)}
	var master *row
	exceptions := map[int64]*row{}
	for _, r := range rows {
		if r.RecurrenceID != nil {
			exceptions[*r.RecurrenceID] = r
			continue
		}
		// The render's master (render): rows come in order, a later one
		// wins a tie.
		if master == nil || r.LastEdit >= master.LastEdit {
			master = r
		}
	}
	var out WritePlan
	minSeq := 0
	switch {
	case len(rows) == 0:
		if in.Master == nil {
			return WritePlan{}, refused(RefuseUnsupportedChange, "an event cannot be created without its master")
		}
		// A DTSTART off its RRULE is refused when the write introduces it:
		// here a create; below an update changing DTSTART or RRULE.
		if r := ical.StartOffRule(in.Master.Comp); r != nil {
			return WritePlan{}, asRefusal(r)
		}
		seq, op, err := pl.create(*in.Master, 0, nil)
		if err != nil {
			return WritePlan{}, err
		}
		out.Ops, out.Created, minSeq = append(out.Ops, op), true, seq
	case master != nil:
		if in.Master == nil {
			return WritePlan{}, refused(RefuseUnsupportedChange, "the body leaves out the event's master")
		}
		// An update keeping the stored DTSTART and RRULE passes even when
		// the stored series is off its rule (a re-PUT, a title edit).
		if r := ical.StartOffRule(in.Master.Comp); r != nil && !ical.SameRecurrence(master.stored, in.Master.Comp) {
			return WritePlan{}, asRefusal(r)
		}
		seq, op, err := pl.update(master, *in.Master, 0, false)
		if err != nil {
			return WritePlan{}, err
		}
		out.Ops, minSeq = appendOp(out.Ops, op), seq
	case in.Master != nil:
		return WritePlan{}, refused(RefuseUnsupportedChange, "adding a master to exceptions without one is not supported")
	}
	for _, rid := range sortedKeys(in.Exceptions) {
		we := in.Exceptions[rid]
		r := exceptions[rid]
		var op *events.Op
		switch {
		case r != nil:
			_, op, err = pl.update(r, we, minSeq, false)
		case master == nil && len(rows) > 0:
			return WritePlan{}, refused(RefuseUnsupportedChange, "a new exception needs its series' master")
		default:
			var created events.Op
			_, created, err = pl.create(we, minSeq, master)
			op = &created
		}
		if err != nil {
			return WritePlan{}, err
		}
		out.Ops = appendOp(out.Ops, op)
	}
	for _, rid := range sortedKeys(exceptions) {
		if _, kept := in.Exceptions[rid]; !kept {
			out.Ops = append(out.Ops, events.Op{Delete: &events.Delete{ID: exceptions[rid].ID}})
		}
	}
	// A stored master without SEQUENCE gets SEQUENCE:0 with any write to its
	// series, first in the call (Fix 5): Proton refuses an exception of
	// such a master (live D7), and the master's body may be unchanged.
	if master != nil && len(out.Ops) > 0 && !hasSequence(master.stored) && !updates(out.Ops[0], master.ID) {
		_, op, err := pl.update(master, *in.Master, 0, true)
		if err != nil {
			return WritePlan{}, err
		}
		if op != nil {
			out.Ops = append([]events.Op{*op}, out.Ops...)
		}
	}
	out.Notes = pl.notes
	return out, nil
}

func appendOp(ops []events.Op, op *events.Op) []events.Op {
	if op == nil {
		return ops
	}
	return append(ops, *op)
}

func sortedKeys[V any](m map[int64]V) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// masterColor is the Color of a new row: null for a new event (master nil),
// its master's for a new exception, so that it looks like the rest of the
// series in Proton's apps (ruling M4-R29).
func masterColor(master *row) events.Optional[string] {
	if master == nil {
		return events.Null[string]()
	}
	return rowColor(master)
}

// rowColor re-sends the row's Color: null when it has none (null on an
// update keeps the row's, research A.2).
func rowColor(r *row) events.Optional[string] {
	if r.Event.Color == "" {
		return events.Null[string]()
	}
	return events.Value(r.Event.Color)
}

type planner struct {
	d     Defaults
	notes []string
}

func (pl *planner) note(s string) {
	if !slices.Contains(pl.notes, s) {
		pl.notes = append(pl.notes, s)
	}
}

// prepared is we's VEVENT with ruling M4-R8's defaults: DTSTAMP the write
// time (UTC) when absent; DTEND, when absent (ParseWrite already made a
// DURATION a DTEND), the next day for an all-day event and DTSTART itself
// for a timed one.
func (pl *planner) prepared(we ical.WriteEvent) (ical.Component, error) {
	c := we.Comp.Clone()
	if _, ok := c.Get("DTSTAMP"); !ok {
		c.Items = append(c.Items, ical.Item{Prop: ical.Property{Name: "DTSTAMP", Value: pl.d.Now.UTC().Format("20060102T150405Z")}})
		pl.note(noteStamp)
	}
	if _, ok := c.Get("DTEND"); !ok {
		start, ok := c.Get("DTSTART")
		if !ok { // ParseWrite refuses a VEVENT without DTSTART
			return ical.Component{}, errors.New("planning a write: an event without DTSTART")
		}
		end := ical.Property{Name: "DTEND", Params: start.Params, Value: start.Value}
		if we.AllDay {
			day, err := time.Parse("20060102", start.Value)
			if err != nil {
				return ical.Component{}, errors.New("planning a write: a malformed all-day DTSTART")
			}
			end.Value = day.AddDate(0, 0, 1).Format("20060102")
		}
		c.Items = append(c.Items, ical.Item{Prop: end})
		pl.note(noteEnd)
	}
	return c, nil
}

// sequence reads a VEVENT's SEQUENCE: its value (0 when absent or not a
// number) and whether it has one.
func sequence(c ical.Component) (int, bool) {
	p, ok := c.Get("SEQUENCE")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(p.Value))
	if err != nil || n < 0 {
		return 0, true
	}
	return n, true
}

// hasSequence reports whether c holds a SEQUENCE.
func hasSequence(c ical.Component) bool {
	_, has := sequence(c)
	return has
}

// updates reports whether op is an update of row id.
func updates(op events.Op, id string) bool {
	return op.Update != nil && op.Update.ID == id
}

// isMaster reports whether c is a master: it has no RECURRENCE-ID.
func isMaster(c ical.Component) bool {
	_, ok := c.Get("RECURRENCE-ID")
	return !ok
}

// setSequence makes c's SEQUENCE the bare value n (research A.4: a
// parameter on it is a 500), in place, or appended.
func setSequence(c *ical.Component, n int) {
	p := ical.Property{Name: "SEQUENCE", Value: strconv.Itoa(n)}
	for i, it := range c.Items {
		if it.Comp == nil && strings.EqualFold(it.Prop.Name, "SEQUENCE") {
			c.Items[i].Prop = p
			return
		}
	}
	c.Items = append(c.Items, ical.Item{Prop: p})
}

// create plans a new row from we: a new event (master nil), or a new
// exception of master. SEQUENCE is at least minSeq; Notifications are null
// when they equal the defaults, else the alarms ([] for none); Color is null
// for a new event, the master's for an exception. A new event with attendees
// is refused (invitations are out of scope); a new exception must carry its
// master's attendee set, by address as AttendeeChanges compares them (ruling
// M4-R28), and then takes the master's attendee lines (their X-PM-TOKEN
// included) and statuses, so the RSVPs stay as stored. It returns the row's
// SEQUENCE.
func (pl *planner) create(we ical.WriteEvent, minSeq int, master *row) (int, events.Op, error) {
	c, err := pl.prepared(we)
	if err != nil {
		return 0, events.Op{}, err
	}
	var attendees events.AttendeeList
	var isOrganizer *int
	if master == nil {
		added, _, err := ical.AttendeeChanges(nil, c)
		if err != nil {
			return 0, events.Op{}, asRefusal(err)
		}
		if len(added) > 0 {
			return 0, events.Op{}, refused(RefuseAttendeeChange, "a new event with attendees is not supported")
		}
	} else {
		if attendees, err = pl.masterAttendees(master, &c); err != nil {
			return 0, events.Op{}, err
		}
		if _, isOrganizer, err = master.rowFields(); err != nil {
			return 0, events.Op{}, err
		}
	}
	seq, has := sequence(c)
	if seq = max(seq, minSeq); has || seq > 0 || isMaster(c) {
		setSequence(&c, seq)
	}
	parts, _, err := ical.Patch(nil, c)
	if err != nil {
		return 0, events.Op{}, asRefusal(err)
	}
	// A new event stores the client's device alarms: null only when they
	// are the calendar's whole default set, so never when it holds an email.
	// A new exception follows its master as an update would (Fix 4 review
	// I1): the master's value when the alarms mean its device part (null
	// stays null), else them plus the master's email entries, so a moved
	// occurrence keeps the series' email reminder.
	list := pl.alarms(we)
	notifications := events.Value(wireNotifications(list))
	switch {
	case master != nil && strings.TrimSpace(string(master.Event.Notifications)) != "":
		if notifications, _, err = pl.notificationsAfter(master, list); err != nil {
			return 0, events.Op{}, err
		}
	case ical.SameNotifications(list, pl.d.notifications(we.AllDay)):
		notifications = events.Null[events.NotificationList]()
	}
	sealed, err := cards.Seal(parts, cards.SealKeys{Signer: pl.d.Signer, CalendarPrimary: pl.d.CalendarPrimary})
	if err != nil {
		return 0, events.Op{}, asRefusal(err)
	}
	body := events.Body{WireEvent: sealed, Notifications: notifications, Color: masterColor(master), Attendees: attendees, IsOrganizer: isOrganizer}
	return seq, events.Op{Create: &body}, nil
}

// masterAttendees checks that c, a new exception of master, has its
// master's attendee set (ruling M4-R28) and makes c's ATTENDEE lines the
// master's, as stored: the client's carry no X-PM-TOKEN (ParseWrite drops
// it) and may differ in parameters, which v1 does not write (ruling M4-R9).
// With attendees comes the master's ORGANIZER line; a client ORGANIZER of
// another address is refused.
// It returns the master's attendee statuses, for the new row (none when the
// master has no attendee).
func (pl *planner) masterAttendees(master *row, c *ical.Component) (events.AttendeeList, error) {
	added, removed, err := ical.AttendeeChanges(master.parts, *c)
	if err != nil {
		return nil, asRefusal(err)
	}
	if len(added) > 0 || len(removed) > 0 {
		return nil, refused(RefuseAttendeeChange, "a new occurrence must have its series' attendees")
	}
	var lines []ical.Item
	for _, p := range master.stored.Props() {
		if strings.EqualFold(p.Name, "ATTENDEE") {
			lines = append(lines, ical.Item{Prop: p})
		}
	}
	if len(lines) == 0 { // no attendee on either side: no statuses either
		return nil, nil
	}
	// The organizer goes with the attendees (Proton refuses attendees
	// without one, research A.4): the master's line, when the client's
	// names the same address or is absent; another one is refused.
	organizer, hasOrganizer := master.stored.Get("ORGANIZER")
	if p, ok := c.Get("ORGANIZER"); ok && (!hasOrganizer || !sameAddress(p.Value, organizer.Value)) {
		return nil, refused(RefuseAttendeeChange, "a new occurrence must have its series' organizer")
	}
	kept := c.Items[:0:0]
	for _, it := range c.Items {
		if it.Comp == nil && (strings.EqualFold(it.Prop.Name, "ATTENDEE") || strings.EqualFold(it.Prop.Name, "ORGANIZER")) {
			continue
		}
		kept = append(kept, it)
	}
	if hasOrganizer {
		kept = append(kept, ical.Item{Prop: organizer})
	}
	kept = append(kept, lines...)
	c.Items = kept
	statuses, _, err := master.rowFields()
	return statuses, err
}

// sameAddress compares two calendar addresses as AttendeeChanges does:
// trimmed and case-folded, scheme included.
func sameAddress(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// update plans row r's update to we (nil when nothing changes) and returns
// its SEQUENCE, at least minSeq. A master gets SEQUENCE:0 when neither we
// nor r has one; when that is its only change, it is written only with
// needSequence (another row of its series is written), else nothing is
// planned.
func (pl *planner) update(r *row, we ical.WriteEvent, minSeq int, needSequence bool) (int, *events.Op, error) {
	c, err := pl.prepared(we)
	if err != nil {
		return 0, nil, err
	}
	added, removed, err := ical.AttendeeChanges(r.parts, c)
	if err != nil {
		return 0, nil, asRefusal(err)
	}
	if len(added) > 0 || len(removed) > 0 {
		return 0, nil, refused(RefuseAttendeeChange, "adding or removing attendees is not supported")
	}
	stored, storedHas := sequence(r.stored)
	seq, has := sequence(c)
	bump := ical.IsSharedSignedChange(r.stored, c) && seq <= stored
	seq = max(seq, stored)
	if bump {
		seq = stored + 1
	}
	bare := c.Clone() // as sent, for a master stored without SEQUENCE
	seq = max(seq, minSeq)
	zero := !has && !storedHas && seq == 0 && isMaster(c)
	if has || storedHas || seq > 0 || zero {
		setSequence(&c, seq)
	}
	parts, changed, err := ical.Patch(r.parts, c)
	if err != nil {
		return 0, nil, asRefusal(err)
	}
	notifications, kept, err := pl.updatedNotifications(r, we)
	if err != nil {
		return 0, nil, err
	}
	if kept && !slices.Contains(mapValues(changed), true) {
		return seq, nil, nil
	}
	if zero && kept && !needSequence {
		if _, changedBare, err := ical.Patch(r.parts, bare); err == nil && !slices.Contains(mapValues(changedBare), true) {
			return seq, nil, nil // SEQUENCE:0 alone: not worth a write
		}
	}
	shared, calendar, err := r.Event.SessionKeys()
	if err != nil {
		return 0, nil, refused(RefuseUntrustedKey, "the event's session key is not proven ours")
	}
	sealed, err := cards.Seal(parts, cards.SealKeys{Signer: pl.d.Signer, Shared: shared, Calendar: calendar, Update: true})
	if err != nil {
		return 0, nil, asRefusal(err)
	}
	if sealed.SharedKeyPacket != "" || sealed.CalendarKeyPacket != "" { // Seal never makes one on an update
		return 0, nil, errors.New("planning a write: an update would carry a key packet")
	}
	attendees, isOrganizer, err := r.rowFields()
	if err != nil {
		return 0, nil, err
	}
	body := events.Body{
		WireEvent: sealed, Attendees: attendees, Notifications: notifications, Color: rowColor(r),
		IsOrganizer: isOrganizer,
	}
	return seq, &events.Op{Update: &events.Update{ID: r.ID, Body: body}}, nil
}

// rowFields are the row's fields an update re-sends, and a new exception
// takes from its master: the attendee statuses as received (research A.2:
// a [] would wipe the RSVPs) and IsOrganizer (nil when absent or null). A
// value of another shape than Proton's (Attendees: Token string, Status
// number, Comment anything, as cards.Open reads them; IsOrganizer: a
// number) is a fault: we do not guess what to send back.
func (r *row) rowFields() (events.AttendeeList, *int, error) {
	var list []struct {
		Token   string
		Status  int
		Comment json.RawMessage
	}
	if raw := strings.TrimSpace(string(r.meta.Attendees)); raw != "" && raw != "null" {
		if err := json.Unmarshal(r.meta.Attendees, &list); err != nil {
			return nil, nil, errors.New("planning a write: a row's Attendees cannot be re-sent")
		}
	}
	out := make(events.AttendeeList, len(list))
	for i, a := range list {
		out[i] = events.Attendee{Token: a.Token, Status: a.Status, Comment: a.Comment}
	}
	var isOrganizer *int
	if raw := strings.TrimSpace(string(r.meta.IsOrganizer)); raw != "" && raw != "null" {
		if err := json.Unmarshal(r.meta.IsOrganizer, &isOrganizer); err != nil {
			return nil, nil, errors.New("planning a write: a row's IsOrganizer cannot be re-sent")
		}
	}
	return out, isOrganizer, nil
}

func mapValues(m map[ical.PartKind]bool) []bool {
	out := make([]bool, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// alarms maps we's VALARMs to notifications, noting the dropped ones.
func (pl *planner) alarms(we ical.WriteEvent) []ical.Notification {
	list, dropped := ical.NotificationsFromAlarms(we.Alarms, we.AllDay)
	if dropped > 0 {
		pl.note(noteAlarms)
	}
	return list
}

// updatedNotifications is the tri-state on update (research D). Email
// notifications are Proton-side (Fix 4, choice A): the render does not
// serve them and the body's alarms map to device notifications alone. When
// these mean the device part of r's set (the calendar's defaults when r's
// Notifications are null, the row's list, the parts' VALARMs when the row
// has none), r's value is kept as it is (kept: null stays null, absent
// stays absent). Otherwise the new list is the body's notifications
// followed by the email ones of r's set, unchanged (none for the parts'
// VALARMs), within Proton's limit: a null row's email default is not lost.
func (pl *planner) updatedNotifications(r *row, we ical.WriteEvent) (events.Optional[events.NotificationList], bool, error) {
	return pl.notificationsAfter(r, pl.alarms(we))
}

// notificationsAfter is updatedNotifications for the device notifications
// list, against r's set: of the row itself on an update, of the master for
// a new exception.
func (pl *planner) notificationsAfter(r *row, list []ical.Notification) (events.Optional[events.NotificationList], bool, error) {
	raw := strings.TrimSpace(string(r.Event.Notifications))
	var effective []ical.Notification
	keep := events.Optional[events.NotificationList]{} // absent: the row keeps its own
	switch raw {
	case "":
		effective, _ = ical.NotificationsFromAlarms(r.stored.Subs("VALARM"), r.FullDay)
	case "null":
		effective, keep = pl.d.notifications(r.FullDay), events.Null[events.NotificationList]()
	default:
		// OpenGroup read it with ical.Alarms: a list of {Type, Trigger}.
		if err := json.Unmarshal(r.Event.Notifications, &effective); err != nil {
			return events.Optional[events.NotificationList]{}, false, refused(RefusePlaceholder, "the event does not decrypt or cannot be read")
		}
		keep = events.Value(wireNotifications(effective))
	}
	device, email := ical.SplitEmail(effective)
	if ical.SameNotifications(list, device) {
		return keep, true, nil
	}
	if room := ical.MaxNotifications - len(email); len(list) > room {
		list = list[:max(room, 0)]
		pl.note(noteAlarms)
	}
	return events.Value(wireNotifications(append(list, email...))), false, nil
}

func wireNotifications(list []ical.Notification) events.NotificationList {
	out := make(events.NotificationList, len(list))
	for i, n := range list {
		out[i] = events.Notification{Type: n.Type, Trigger: n.Trigger}
	}
	return out
}
