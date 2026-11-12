package service

// The write executor (Task 7b): PutObject and DeleteObject, during Serve
// only, on Serve's cache handle. One write of a calendar runs at a time, and
// never beside the syncer's pass of that calendar (calLocks). A write
// refreshes the object's rows from Proton, checks that their key packets are
// the ones it cached (ruling M4-R17), evaluates the precondition on the fresh
// state, plans (writeplan.go), sends the plan in one Sync call that is never
// sent again, reads the rows back and stores them (ApplyWrite, Proton's
// listed rows only). It answers success only when the rows read back show
// what it sent (constraints, write flow M4-R5).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// Precondition is a write's HTTP precondition. IfMatch is the If-Match
// value: an ETag as the read API returns it (quoted), compared as an exact
// string, or "*" for "the object exists"; "" for none. IfNoneMatchAny is
// If-None-Match: * (the object must not exist).
type Precondition struct {
	IfMatch        string `json:"if_match,omitempty"`
	IfNoneMatchAny bool   `json:"if_none_match_any,omitempty"`
}

// PutResult is a successful PUT: Created when the event is new (CalDAV
// 201), else it was updated or left as it was (204). No ETag: the object is
// what Proton stored, which the client reads again.
type PutResult struct {
	Created bool `json:"created"`
}

// The causes of the errors the writes return, each in an *exitcode.Error
// (test with errors.Is or errors.As).
var (
	// ErrNotServing: writes run only during Serve, on its cache handle
	// (CalDAV: 503).
	ErrNotServing = errors.New("writes need pcb serve running for the logged account")
	// ErrPreconditionFailed: If-Match or If-None-Match does not hold on the
	// object as Proton has it now (CalDAV: 412).
	ErrPreconditionFailed = errors.New("the object does not match the request's precondition")
	// ErrNotWritable: the write gate refuses the calendar (ruling M4-R1;
	// CalDAV: 403 need-privileges).
	ErrNotWritable = errors.New("the calendar is read only")
	// ErrNoSuchObject: no object has that name in the calendar (CalDAV:
	// 404).
	ErrNoSuchObject = errors.New("no such object in the calendar")
	// ErrWriteNotConfirmed: Proton may or may not hold the write, or holds
	// part of it, or answered success but stored something else (a dropped
	// edit): the rows read back do not show it. The cache holds what Proton
	// has, if it could be read (CalDAV: 503, the client reads again).
	ErrWriteNotConfirmed = errors.New("the write was not confirmed by Proton")
)

// UIDConflictError: the PUT's UID lives at another name of the calendar,
// or the name holds another UID (CalDAV: 409 no-uid-conflict). Href is the
// name, within the calendar, of the object holding the UID: the adapter
// prefixes the calendar's path.
type UIDConflictError struct {
	Href string
}

func (e *UIDConflictError) Error() string {
	return "the event's UID or name is already used by another object of the calendar"
}

// The pcb preconditions of the executor's refusals, beside the planner's.
const (
	// RefuseProtonRefused: Proton refused the write, whole or one entry of
	// it (constraints, write flow); the reason gives its code only.
	RefuseProtonRefused = "pcb:proton-refused"
	// RefuseBadName: the resource name cannot name an object.
	RefuseBadName = "pcb:bad-resource-name"
)

// maxNameBytes bounds a client-chosen resource name.
const maxNameBytes = 255

// readBackTimeout bounds the read-back of a write once its Sync call left:
// it runs even when the caller hung up, so the cache holds what Proton has.
const readBackTimeout = 30 * time.Second

func writeFail(code exitcode.Code, cause error, hint string) *exitcode.Error {
	return &exitcode.Error{Code: code, Err: cause, Hint: hint}
}

func notServing() error {
	return writeFail(exitcode.Blocked, ErrNotServing, "run pcb serve")
}

func preconditionFailed() error {
	return writeFail(exitcode.Usage, ErrPreconditionFailed, "read the object again, then retry the change")
}

func notWritable() error {
	return writeFail(exitcode.Usage, ErrNotWritable, "the calendar is not yours to write to, or its keys could not be checked; pcb calendar list shows it read only")
}

func noSuchObject() error {
	return writeFail(exitcode.Usage, ErrNoSuchObject, "list the calendar again")
}

func uidConflict(href string) error {
	return writeFail(exitcode.Usage, &UIDConflictError{Href: href}, "change the object at its own name")
}

func refusal(r *WriteRefusal) error {
	return writeFail(exitcode.Usage, r, "pcb does not write this change to Proton; make it in the Proton web app")
}

func notConfirmed() error {
	return writeFail(exitcode.Blocked, ErrWriteNotConfirmed, "read the calendar again: it shows what Proton holds; retry the change if it is missing")
}

func protonRefused(status, code int) error {
	reason := fmt.Sprintf("Proton refused the write (code %d)", code)
	if code == 0 {
		reason = fmt.Sprintf("Proton refused the write (HTTP %d)", status)
	}
	return refusal(refused(RefuseProtonRefused, reason))
}

// planFault is a plan that failed without refusing: a bug or broken keys,
// never the client's body.
func planFault() error {
	return blocked("the write could not be prepared", "run pcb doctor; if it persists, report it without any event content")
}

// planError maps what Plan or PlanDelete returned.
func planError(err error) error {
	var r *WriteRefusal
	if errors.As(err, &r) {
		return refusal(r)
	}
	return planFault()
}

// ProtonRetryAfter is how long Proton asked to wait before the next
// request, as err carries it (a 429's Retry-After), 0 when it says nothing:
// for the adapter's Retry-After on a 503.
func ProtonRetryAfter(err error) time.Duration { return retryAfter(err) }

// ObjectName is uid's canonical resource name: the UID when it is 1 to 200
// bytes of [A-Za-z0-9._@-] (Thunderbird's own names, E08 §4.4), else "~"
// and its unpadded base64url; then ".ics".
func ObjectName(uid string) string {
	if plainUID(uid) {
		return uid + ".ics"
	}
	return "~" + base64.RawURLEncoding.EncodeToString([]byte(uid)) + ".ics"
}

// UIDOfName is ObjectName's inverse, exact: false for a name that is not
// the canonical name of a UID.
func UIDOfName(name string) (string, bool) {
	stem, ok := strings.CutSuffix(name, ".ics")
	if !ok {
		return "", false
	}
	uid := stem
	if enc, encoded := strings.CutPrefix(stem, "~"); encoded {
		b, err := base64.RawURLEncoding.Strict().DecodeString(enc)
		if err != nil {
			return "", false
		}
		uid = string(b)
	}
	if uid == "" || ObjectName(uid) != name {
		return "", false
	}
	return uid, true
}

func plainUID(uid string) bool {
	if uid == "" || len(uid) > 200 {
		return false
	}
	for i := range len(uid) {
		switch c := uid[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// validName reports whether a client-chosen resource name can name an
// object: 1 to maxNameBytes bytes of UTF-8, no "/", no control character,
// not "." or "..".
func validName(name string) bool {
	if name == "" || len(name) > maxNameBytes || name == "." || name == ".." || !utf8.ValidString(name) {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r == '/' || r < 0x20 || r == 0x7f })
}

// calLocks serialises, per calendar, the writes and the syncer's pass of
// that calendar (syncer.Options.Lock). Each lock is a one-slot semaphore, so
// waiting for it ends with the waiter's ctx.
//
// A holder keeps the lock across its Proton requests: a write across its
// refresh, its Sync call and its read-back, the syncer across its pass of
// the calendar. A GET among them may sleep through the client's backoff (a
// 429's Retry-After, up to 60 s and 3 tries, or a 5xx retry) with the lock
// held: releasing it there would let the other side store rows between the
// write's refresh and its plan, or between its call and its read-back. A
// waiter whose ctx ends meanwhile gives up with the unavailable class.
type calLocks struct {
	mu sync.Mutex
	m  map[string]chan struct{}
	// waiting, when set (tests), is called when a write finds its
	// calendar's lock taken, before it waits for it.
	waiting func(calID string)
}

// lock takes calendar calID's lock and returns its release; waiting is
// called first when the lock is taken. It returns ctx's error when ctx ends
// before the lock is free.
func (c *calLocks) lock(ctx context.Context, calID string, waiting func(string)) (func(), error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]chan struct{}{}
	}
	slot := c.m[calID]
	if slot == nil {
		slot = make(chan struct{}, 1)
		c.m[calID] = slot
	}
	c.mu.Unlock()
	release := func() { <-slot }
	select {
	case slot <- struct{}{}:
		return release, nil
	default:
	}
	if waiting != nil {
		waiting(calID)
	}
	select {
	case slot <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// write is one write in progress: the calendar, Serve's account and logger,
// and the calendar's lock, held until done.
type write struct {
	calID, account string
	log            *slog.Logger
	unlock         func()
}

// onWriter runs fn on Serve's cache handle, held so that Serve does not
// close it meanwhile; ErrNotServing outside Serve or when another account
// is logged in. fn must not call Proton.
func (l *Local) onWriter(fn func(db *cache.Cache, s *served) error) error {
	l.smu.RLock()
	defer l.smu.RUnlock()
	s := l.served
	if s == nil {
		return notServing()
	}
	st, err := store.LoadState(l.o.Dirs.State)
	if err != nil {
		return stateError(err)
	}
	if st.AccountID != s.account {
		return notServing()
	}
	return fn(s.db, s)
}

// beginWrite checks the calendar (synced, write gate) and takes its lock.
func (l *Local) beginWrite(ctx context.Context, calID string) (*write, error) {
	w := &write{calID: calID}
	var typ calendar.Type
	err := l.onWriter(func(db *cache.Cache, s *served) error {
		w.account, w.log = s.account, s.log
		c, err := syncedCalendar(ctx, db, calID)
		typ = calendar.ParseType(c.Type)
		return l.fail(err)
	})
	if err != nil {
		return nil, err
	}
	if err := l.ensureKeys(ctx, map[string]calendar.Type{calID: typ}); err != nil {
		return nil, err
	}
	ck := l.r.calKeys(calID)
	if ck == nil {
		return nil, errKeysDropped()
	}
	if !ck.access.Writable() {
		return nil, notWritable()
	}
	unlock, err := l.cals.lock(ctx, calID, l.cals.waiting)
	if err != nil {
		return nil, l.fail(err)
	}
	w.unlock = unlock
	return w, nil
}

// objectState is an object as the cache holds it after the refresh: its
// rows, the UID a name stands for and whether that UID has rows, and the
// calendar's served names (names.go).
type objectState struct {
	group    cache.Group
	live     bool
	nameUID  string // the UID the requested name stands for, "" if none
	nameLive bool   // nameUID has rows
	names    nameIndex
}

// hrefOf is the name a UID is served at (names.served).
func (s objectState) hrefOf(uid string) string { return s.names.served(uid) }

// read returns the cached state of uid, and the UID name stands for
// (names.standsFor: its alias, else the UID it is the canonical or the
// alternative name of).
func (l *Local) read(ctx context.Context, w *write, uid, name string) (objectState, error) {
	var st objectState
	err := l.onWriter(func(db *cache.Cache, _ *served) error {
		return l.cacheFail(db.Snapshot(ctx, w.calID, func(v cache.View) error {
			var err error
			if uid != "" {
				if st.group, st.live, err = v.Group(uid); err != nil {
					return err
				}
			}
			aliases, err := v.AliasesFor()
			if err != nil {
				return err
			}
			st.names = newNames(aliases)
			st.nameUID = st.names.standsFor(name)
			if st.nameUID != "" {
				_, st.nameLive, err = v.Group(st.nameUID)
			}
			return err
		}))
	})
	if !st.live {
		st.group = cache.Group{UID: uid}
	}
	return st, err
}

// listRows reads uid's rows from Proton (ListByUID), on the session of
// Serve's account.
func (l *Local) listRows(ctx context.Context, w *write, uid string) ([]events.Row, error) {
	var rows []events.Row
	err := l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		if acc.ID != w.account {
			return otherAccount()
		}
		var err error
		rows, err = events.ListByUID(ctx, c, w.calID, uid)
		return err
	})
	return rows, err
}

func otherAccount() error {
	return final("pcb is now logged in to another Proton account than pcb serve started with", "restart pcb serve")
}

// storeRows makes rows, as ListByUID returned them, the cached content of uid
// (ApplyWrite: the feed cursor is left alone).
func (l *Local) storeRows(ctx context.Context, w *write, uid string, rows []events.Row) error {
	return l.storeRowsPinned(ctx, w, uid, rows, nil)
}

// storeRowsPinned is storeRows that first pins the rows of pins that have
// no pin yet (ruling M4-R36: a create's rows, with the packets sent).
func (l *Local) storeRowsPinned(ctx context.Context, w *write, uid string, rows []events.Row, pins map[string]cache.Pin) error {
	out := make([]cache.Row, len(rows))
	for i, r := range rows {
		out[i] = cache.Row{ID: r.ID, UID: r.UID, Start: r.Start, End: r.End, FullDay: r.FullDay,
			RecurrenceID: r.RecurrenceID, RRule: r.RRule, LastEditTime: r.LastEditTime, Raw: r.Raw}
	}
	return l.onWriter(func(db *cache.Cache, _ *served) error {
		_, err := db.ApplyWritePinned(ctx, w.calID, uid, out, pins, l.o.Now())
		return l.cacheFail(err)
	})
}

// pinned returns the pins of the rows ids (ruling M4-R30).
func (l *Local) pinned(ctx context.Context, w *write, ids []string) (map[string]cache.Pin, error) {
	var out map[string]cache.Pin
	err := l.onWriter(func(db *cache.Cache, _ *served) error {
		return l.cacheFail(db.Snapshot(ctx, w.calID, func(v cache.View) error {
			var err error
			out, err = v.Pins(ids)
			return err
		}))
	})
	return out, err
}

func rowIDs(rows []events.Row) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func storedIDs(g cache.Group) []string {
	ids := make([]string, len(g.Rows))
	for i, r := range g.Rows {
		ids[i] = r.ID
	}
	return ids
}

// packetDiff names the first key packet of got that is not want's
// (cache.SamePacket), "" when none.
func packetDiff(want, got cache.Pin) string {
	switch {
	case !cache.SamePacket(want.Shared, got.Shared):
		return "SharedKeyPacket"
	case !cache.SamePacket(want.Calendar, got.Calendar):
		return "CalendarKeyPacket"
	case !cache.SamePacket(want.Address, got.Address):
		return "AddressKeyPacket"
	}
	return ""
}

// changedPackets returns the ID of a refreshed row whose key packets differ
// from its pin, the packets the cache first stored for it (ruling M4-R30:
// Proton never changes a row's key packets, research B.5, so a later sync
// pass storing other ones does not move the pin), and which packet; "" when
// none does. A row never stored has no pin: it is pinned when stored.
func changedPackets(pins map[string]cache.Pin, fresh []events.Row) (id, field string) {
	for _, r := range fresh {
		old, ok := pins[r.ID]
		if !ok {
			continue
		}
		if f := packetDiff(old, cache.PinOf(r.Raw)); f != "" {
			return r.ID, f
		}
	}
	return "", ""
}

// writeDefaults are a plan's calendar inputs: the default notifications,
// the sealing keys and the write time.
func writeDefaults(a *calendar.Access, now time.Time) Defaults {
	conv := func(ns []calendar.Notification) []ical.Notification {
		if ns == nil {
			return nil
		}
		out := make([]ical.Notification, len(ns))
		for i, n := range ns {
			out[i] = ical.Notification{Type: n.Type, Trigger: n.Trigger}
		}
		return out
	}
	return Defaults{PartDay: conv(a.Settings.DefaultPartDayNotifications), FullDay: conv(a.Settings.DefaultFullDayNotifications),
		Signer: a.Signer, CalendarPrimary: a.Primary, Now: now}
}

// planned is a plan and what sending it needs.
type planned struct {
	WritePlan
	memberID string
}

// withKeys runs fn on the calendar's keys as held now (Access and the
// group keys of the render), under kmu.
func (l *Local) withKeys(w *write, fn func(*calendar.Access, GroupKeys) error) error {
	l.r.kmu.RLock()
	defer l.r.kmu.RUnlock()
	ks := l.r.ks
	if ks == nil || ks.account != w.account || ks.cals[w.calID] == nil || ks.cals[w.calID].access == nil {
		return errKeysDropped()
	}
	a := ks.cals[w.calID].access
	return fn(a, GroupKeys{Calendar: a.KR, Addresses: ks.u.AddrKRs, Own: func(email string) *crypto.KeyRing {
		return ks.own[strings.ToLower(email)]
	}})
}

// plan runs fn on the calendar's keys, as held now, and the group opened
// with them; the write gate is checked again on those keys.
func (l *Local) plan(w *write, g cache.Group, fn func(Group, *calendar.Access) (WritePlan, error)) (planned, error) {
	var out planned
	err := l.withKeys(w, func(a *calendar.Access, k GroupKeys) error {
		if !a.Writable() {
			return notWritable()
		}
		p, err := fn(OpenGroup(g, k), a)
		if err != nil {
			return planError(err)
		}
		out = planned{WritePlan: p, memberID: a.MemberID}
		return nil
	})
	return out, err
}

// etagNow is the object's ETag under the calendar's keys as held now.
func (l *Local) etagNow(w *write, g cache.Group) (string, error) {
	ck := l.r.calKeys(w.calID)
	if ck == nil {
		return "", errKeysDropped()
	}
	return objectETag(g.Listed(), ck), nil
}

// checkPrecondition evaluates cond on the object at the requested name:
// exists, with ETag etag.
func checkPrecondition(cond Precondition, exists bool, etag string) error {
	switch {
	case cond.IfNoneMatchAny && exists:
		return preconditionFailed()
	case cond.IfMatch == "":
		return nil
	case !exists, cond.IfMatch != "*" && cond.IfMatch != etag:
		return preconditionFailed()
	}
	return nil
}

// PutObject implements API.
func (l *Local) PutObject(ctx context.Context, calID, name string, body []byte, cond Precondition) (PutResult, error) {
	if l.closed.Load() {
		return PutResult{}, errClosed()
	}
	if err := l.onWriter(func(*cache.Cache, *served) error { return nil }); err != nil {
		return PutResult{}, err
	}
	if !validName(name) {
		return PutResult{}, refusal(refused(RefuseBadName, "the resource name cannot name an event"))
	}
	in, err := ical.ParseWrite(body)
	if err != nil {
		return PutResult{}, planError(asRefusal(err))
	}
	w, err := l.beginWrite(ctx, calID)
	if err != nil {
		return PutResult{}, err
	}
	defer w.unlock()

	fresh, err := l.listRows(ctx, w, in.UID)
	if err != nil {
		return PutResult{}, err
	}
	pins, err := l.pinned(ctx, w, rowIDs(fresh))
	if err != nil {
		return PutResult{}, err
	}
	if id, field := changedPackets(pins, fresh); id != "" {
		// Not stored either: the sync passes store what Proton serves, the
		// pin stays what was first seen.
		w.log.Warn("refusing a write: a row's key packet changed since pcb first stored it", "calendar", calID, "row", id, "field", field)
		return PutResult{}, refusal(refused(RefuseUntrustedKey, "a key packet of the event changed since pcb read it"))
	}
	if err := l.storeRows(ctx, w, in.UID, fresh); err != nil {
		return PutResult{}, err
	}
	st, err := l.read(ctx, w, in.UID, name)
	if err != nil {
		return PutResult{}, err
	}
	// One object per UID, one UID per name: the UID served at another
	// name, or the name serving another UID, is a conflict naming the
	// object that holds it. A name that only decodes as the canonical name
	// of a UID with no rows, or the alias of a deleted UID (ruling M4-R22),
	// is free.
	if href := st.hrefOf(in.UID); st.live && href != name {
		return PutResult{}, uidConflict(href)
	}
	if st.nameUID != "" && st.nameUID != in.UID && st.nameLive && st.hrefOf(st.nameUID) == name {
		return PutResult{}, uidConflict(name)
	}
	// Another UID's alternative name is reserved for it (ruling M4-R38): an
	// alias there could later stand where that UID must be served.
	if alt, ok := uidOfAlternativeName(name); ok && alt != in.UID {
		return PutResult{}, refusal(refused(RefuseBadName, "the resource name is reserved for another event"))
	}
	etag, err := l.etagNow(w, st.group)
	if err != nil {
		return PutResult{}, err
	}
	if err := checkPrecondition(cond, st.live, etag); err != nil {
		return PutResult{}, err
	}
	p, err := l.plan(w, st.group, func(g Group, a *calendar.Access) (WritePlan, error) {
		return Plan(g, in, writeDefaults(a, l.o.Now()))
	})
	if err != nil {
		return PutResult{}, err
	}
	if len(p.Notes) > 0 {
		w.log.Debug("write normalised", "calendar", calID, "notes", strings.Join(p.Notes, "; "))
	}
	if len(p.Ops) == 0 {
		return PutResult{}, nil
	}
	if p.Created && st.hrefOf(in.UID) != name {
		// Before the write: a create that reaches Proton is then served at
		// the client's name, whatever happens to the answer.
		err := l.onWriter(func(db *cache.Cache, _ *served) error {
			return l.cacheFail(db.PutAlias(ctx, calID, name, in.UID))
		})
		if err != nil {
			return PutResult{}, err
		}
	}
	if err := l.execute(ctx, w, in.UID, st.group, p, false); err != nil {
		return PutResult{}, err
	}
	return PutResult{Created: p.Created}, nil
}

// DeleteObject implements API.
func (l *Local) DeleteObject(ctx context.Context, calID, name string, cond Precondition) error {
	if l.closed.Load() {
		return errClosed()
	}
	if err := l.onWriter(func(*cache.Cache, *served) error { return nil }); err != nil {
		return err
	}
	if !validName(name) {
		return noSuchObject()
	}
	w, err := l.beginWrite(ctx, calID)
	if err != nil {
		return err
	}
	defer w.unlock()

	named, err := l.read(ctx, w, "", name)
	if err != nil {
		return err
	}
	uid := named.nameUID
	if uid == "" || named.hrefOf(uid) != name {
		return noSuchObject()
	}
	fresh, err := l.listRows(ctx, w, uid)
	if err != nil {
		return err
	}
	// No key-packet check: a delete encrypts nothing (ruling M4-R27).
	if err := l.storeRows(ctx, w, uid, fresh); err != nil {
		return err
	}
	st, err := l.read(ctx, w, uid, name)
	if err != nil {
		return err
	}
	if !st.live || st.hrefOf(uid) != name {
		return noSuchObject()
	}
	etag, err := l.etagNow(w, st.group)
	if err != nil {
		return err
	}
	if err := checkPrecondition(cond, true, etag); err != nil {
		return err
	}
	p, err := l.plan(w, st.group, func(g Group, _ *calendar.Access) (WritePlan, error) { return PlanDelete(g) })
	if err != nil {
		return err
	}
	return l.execute(ctx, w, uid, st.group, p, true)
}

// execute sends p in one Sync call, never sent again, reads uid's rows back
// and stores them, and says how it went; whole is a DELETE of the object.
//
//   - nil when the rows read back show every operation done (confirm),
//     whatever Proton answered (rulings M4-R31, M4-R34);
//   - after a refusal (an entry's code, or a whole call refused with a 4xx
//     that is not session-wide), and after a DELETE answered success: 412
//     when uid's rows changed since the plan (a concurrent edit), else a 403
//     RefuseProtonRefused for the refusal (M4-R34);
//   - else ErrWriteNotConfirmed (503) for a call that may have reached
//     Proton, and the unavailable error of its cause for one that did not
//     (503, Retry-After for a 429).
//
// A row the write created is pinned with the packets it sent, and a row
// read back under other packets than its pin or the sent ones is never
// confirmed (ruling M4-R36).
func (l *Local) execute(ctx context.Context, w *write, uid string, before cache.Group, p planned, whole bool) error {
	res, syncErr, err := l.sendOnce(ctx, w, p)
	if err != nil {
		return err
	}
	var (
		entry   *events.EntryError
		ae      *api.Error
		applied []bool // per operation, whether it may have been applied; nil: unknown
	)
	for _, r := range res {
		if errors.As(r.Err, &entry) {
			break
		}
	}
	switch {
	case syncErr == nil:
		applied = make([]bool, len(p.Ops))
		for i, r := range res {
			applied[i] = r.Err == nil || errors.Is(r.Err, events.ErrAmbiguous)
		}
	case errors.Is(syncErr, events.ErrAmbiguous):
	case errors.As(syncErr, &ae) && !api.SessionWide(ctx, syncErr):
		// Refused as a whole, before anything was applied.
		applied = make([]bool, len(p.Ops))
	case api.SessionWide(ctx, syncErr) || errors.Is(syncErr, api.ErrNotSent):
		return l.fail(syncErr)
	default:
		return planFault()
	}
	refusedErr := func() error {
		if entry != nil {
			w.log.Warn("Proton refused a write", "calendar", w.calID, "entry", entry.Index, "code", entry.Code)
			return protonRefused(0, entry.Code)
		}
		w.log.Warn("Proton refused a write", "calendar", w.calID, "status", ae.Status, "code", ae.Code)
		return protonRefused(ae.Status, ae.Code)
	}
	refused := entry != nil || ae != nil
	if err := l.pinAnswered(ctx, w, p.Ops, res); err != nil {
		return err
	}

	// The read-back runs even when the caller hung up meanwhile.
	backCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
	defer cancel()
	pins, err := l.pinned(backCtx, w, storedIDs(before))
	if err != nil {
		return err
	}
	rows, err := l.listRows(backCtx, w, uid)
	if err != nil {
		if ae != nil {
			return refusedErr() // nothing was applied
		}
		w.log.Warn("a write could not be read back", "calendar", w.calID)
		return notConfirmed()
	}
	var v verdict
	if err := l.withKeys(w, func(_ *calendar.Access, k GroupKeys) error {
		v = confirm(p.Ops, before, rows, pins, whole, k)
		return nil
	}); err != nil {
		// Nothing stored: without the keys the rows created cannot be
		// told apart, so they cannot be pinned with what was sent.
		return err
	}
	if err := l.storeRowsPinned(backCtx, w, uid, rows, v.sent); err != nil {
		return err
	}
	switch {
	case v.packetRow != "":
		w.log.Warn("a write is not confirmed: a row came back under other key packets than pcb sent or pinned", "calendar", w.calID, "row", v.packetRow, "field", v.packetField)
		return notConfirmed()
	case v.ok:
		return nil
	case (refused || whole) && applied != nil && changedSince(before, p.Ops, applied, rows):
		w.log.Warn("a write met a concurrent change of its event", "calendar", w.calID, "refused", refused)
		return preconditionFailed()
	case refused:
		return refusedErr()
	}
	w.log.Warn("a write is not confirmed by the rows read back", "calendar", w.calID, "ambiguous", syncErr != nil)
	return notConfirmed()
}

// pinAnswered pins each row Proton answered as created with the packets
// sent (ruling M4-R37), before the read-back: a read-back that fails then
// leaves the next sync pass a pin to check, not a first sight to take.
func (l *Local) pinAnswered(ctx context.Context, w *write, ops []events.Op, res []events.Result) error {
	pins := map[string]cache.Pin{}
	for i, r := range res {
		if r.Err == nil && r.Row != nil && r.Row.ID != "" && i < len(ops) && ops[i].Create != nil {
			pins[r.Row.ID] = cache.Pin{Shared: ops[i].Create.SharedKeyPacket, Calendar: ops[i].Create.CalendarKeyPacket}
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
	defer cancel()
	return l.onWriter(func(db *cache.Cache, _ *served) error {
		return l.cacheFail(db.PinRows(ctx, w.calID, pins))
	})
}

// changedSince reports whether uid's rows read back show a change the
// write does not explain (ruling M4-R34): a row of before that no applied
// operation touched is gone or has another ETag, or more rows appeared than
// the applied creates made.
func changedSince(before cache.Group, ops []events.Op, applied []bool, rows []events.Row) bool {
	touched := map[string]bool{}
	creates := 0
	for i, op := range ops {
		switch {
		case !applied[i]:
		case op.Update != nil:
			touched[op.Update.ID] = true
		case op.Delete != nil:
			touched[op.Delete.ID] = true
		case op.Create != nil:
			creates++
		}
	}
	now := make(map[string]events.Row, len(rows))
	for _, r := range rows {
		now[r.ID] = r
	}
	old := make(map[string]bool, len(before.Rows))
	for _, b := range before.Rows {
		old[b.ID] = true
		if touched[b.ID] {
			continue
		}
		r, ok := now[b.ID]
		if !ok {
			return true
		}
		// By meaning, as the precondition compares: the cache keeps the
		// first spelling of a row whose ETag did not change.
		if tag, err := cache.ETag(r.Raw); err != nil || tag != b.ETag {
			return true
		}
	}
	added := 0
	for _, r := range rows {
		if !old[r.ID] {
			added++
		}
	}
	return added > creates
}

// sendOnce sends p's operations in one Sync call. syncErr is Sync's error;
// err is the service's when the call could not be made, or when the
// session had to be renewed after the call (a 401 is answered before any
// processing, ruling M4-R23): the call is never sent again.
func (l *Local) sendOnce(ctx context.Context, w *write, p planned) (res []events.Result, syncErr, err error) {
	sent := false
	err = l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		switch {
		case acc.ID != w.account:
			return otherAccount()
		case sent:
			return blocked("the Proton session was renewed and the write was not sent again", "retry the change")
		}
		sent = true
		res, syncErr = events.Sync(ctx, c, w.calID, p.memberID, p.Ops)
		if errors.Is(syncErr, api.ErrSessionEnded) {
			return syncErr // withSession renews the session
		}
		return nil
	})
	return res, syncErr, err
}

// rowFields are the row fields confirm compares.
type rowFields struct {
	Notifications json.RawMessage
	Color         *string
	Attendees     []struct {
		Token  string
		Status int
	}
}

// normalNotifications is a Notifications value, null or a list, in a form
// that compares by meaning (order aside); ok false when it does not decode.
func normalNotifications(raw []byte) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "null", true
	}
	var list []ical.Notification
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", false
	}
	keys := make([]string, len(list))
	for i, n := range list {
		keys[i] = fmt.Sprint(n.Type) + "\x00" + n.Trigger
	}
	slices.Sort(keys)
	return "[" + strings.Join(keys, "\x01") + "]", true
}

// fieldsHold reports whether a listed row's fields hold what body sent:
// the attendees' tokens and statuses, the notifications (when sent) and
// the colour (when one is).
func fieldsHold(raw json.RawMessage, body events.Body) bool {
	var s rowFields
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	got := make([]string, len(s.Attendees))
	for i, a := range s.Attendees {
		got[i] = fmt.Sprint(a.Status) + "\x00" + a.Token
	}
	want := make([]string, len(body.Attendees))
	for i, a := range body.Attendees {
		want[i] = fmt.Sprint(a.Status) + "\x00" + a.Token
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return false
	}
	if !body.Notifications.IsZero() {
		sent, _ := body.Notifications.MarshalJSON()
		want, ok1 := normalNotifications(sent)
		got, ok2 := normalNotifications(s.Notifications)
		if !ok1 || !ok2 || want != got {
			return false
		}
	}
	// Null keeps the row's colour on an update and is the calendar's on a
	// create: only a colour sent is checked.
	if sent, _ := body.Color.MarshalJSON(); !body.Color.IsZero() && string(sent) != "null" {
		var c string
		if json.Unmarshal(sent, &c) != nil || s.Color == nil || !strings.EqualFold(*s.Color, c) {
			return false
		}
	}
	return true
}

// plaintexts are an opened event's parts, kind and text, sorted; the
// personal part is left out (never written).
func plaintexts(ev cards.Event) []string {
	var out []string
	for _, p := range ev.Parts {
		if p.Kind != cards.Personal {
			out = append(out, fmt.Sprint(int(p.Kind))+"\x00"+string(p.Text))
		}
	}
	slices.Sort(out)
	return out
}

// partsHold reports whether a listed row holds the parts body sent, by
// meaning (ruling M4-R32): the row opens with the calendar's keys, every
// part of it is ours and verifies, and its plaintext per part is the
// plaintext sent, whatever armour or base64 spelling Proton serves. The sent
// parts open with packets: the ones a create sent, an updated row's pin
// (ruling M4-R36).
func partsHold(row events.Row, body events.Body, packets cache.Pin, k GroupKeys) bool {
	got, err := cards.Open(row.Raw, k.Calendar, k.Addresses, k.Own)
	if err != nil {
		return false
	}
	for _, p := range got.Parts {
		if p.Kind != cards.Personal && (!p.Own || !p.Verified) {
			return false
		}
	}
	sentRaw, err := json.Marshal(map[string]any{
		"ID": "sent", "UID": row.UID, "SharedKeyPacket": packets.Shared, "CalendarKeyPacket": packets.Calendar,
		"SharedEvents": body.SharedEventContent, "CalendarEvents": body.CalendarEventContent, "AttendeesEvents": body.AttendeesEventContent,
	})
	if err != nil {
		return false
	}
	sent, err := cards.Open(sentRaw, k.Calendar, nil, nil)
	if err != nil {
		return false
	}
	return slices.Equal(plaintexts(got), plaintexts(sent))
}

// holds reports whether row, as listed, holds body, its sent parts opened
// with packets: its parts (partsHold) and its fields (fieldsHold).
func holds(row events.Row, body events.Body, packets cache.Pin, k GroupKeys) bool {
	return fieldsHold(row.Raw, body) && partsHold(row, body, packets, k)
}

// verdict is what the rows read back say of a write.
type verdict struct {
	// ok: every operation is done.
	ok bool
	// sent: the rows the write created, by ID, with the packets sent: their
	// pins (ruling M4-R36).
	sent map[string]cache.Pin
	// packetRow, packetField: a row the write created or updated that came
	// back under another key packet than the one sent or pinned.
	packetRow, packetField string
}

// confirm reads rows, uid's rows read back after the write, against ops:
// a deleted row gone; an updated row under its pin's packets (pins: the
// pins of before's rows) holding what was sent; a created row (one that was
// not in before) holding what was sent, under the packets sent; each row
// standing for one operation; and for a DELETE of the object (whole), no
// row left.
func confirm(ops []events.Op, before cache.Group, rows []events.Row, pins map[string]cache.Pin, whole bool, k GroupKeys) verdict {
	v := verdict{ok: true, sent: map[string]cache.Pin{}}
	old := make(map[string]bool, len(before.Rows))
	for _, r := range before.Rows {
		old[r.ID] = true
	}
	byID := make(map[string]events.Row, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	used := map[string]bool{}
	packets := func(id string, want cache.Pin, r events.Row) {
		if f := packetDiff(want, cache.PinOf(r.Raw)); f != "" {
			v.ok = false
			if v.packetRow == "" {
				v.packetRow, v.packetField = id, f
			}
		}
	}
	for _, op := range ops {
		switch {
		case op.Delete != nil:
			if _, ok := byID[op.Delete.ID]; ok {
				v.ok = false
			}
		case op.Update != nil:
			r, ok := byID[op.Update.ID]
			pin, pinned := pins[op.Update.ID]
			if !ok || used[r.ID] || !pinned {
				v.ok = false
				continue
			}
			used[r.ID] = true
			packets(r.ID, pin, r)
			if !holds(r, op.Update.Body, pin, k) {
				v.ok = false
			}
		case op.Create != nil:
			// The row is the create's by its signed content alone (ruling
			// M4-R37): its unsigned fields decide only whether the write
			// is confirmed, so a server changing one cannot make the row
			// a stranger, pinned from its echo.
			sent := cache.Pin{Shared: op.Create.SharedKeyPacket, Calendar: op.Create.CalendarKeyPacket}
			i := slices.IndexFunc(rows, func(r events.Row) bool { return !old[r.ID] && !used[r.ID] && partsHold(r, *op.Create, sent, k) })
			if i < 0 {
				v.ok = false
				continue
			}
			r := rows[i]
			used[r.ID] = true
			v.sent[r.ID] = sent
			packets(r.ID, sent, r)
			if !fieldsHold(r.Raw, *op.Create) {
				v.ok = false
			}
		}
	}
	if whole && len(rows) > 0 {
		v.ok = false
	}
	return v
}
