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
// that calendar (syncer.Options.Lock).
type calLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
	// waiting, when set (tests), is called when a write finds its
	// calendar's lock taken, before it waits for it.
	waiting func(calID string)
}

// lock takes calendar calID's lock and returns its release; waiting is
// called first when the lock is taken.
func (c *calLocks) lock(calID string, waiting func(string)) func() {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*sync.Mutex{}
	}
	m := c.m[calID]
	if m == nil {
		m = &sync.Mutex{}
		c.m[calID] = m
	}
	c.mu.Unlock()
	if !m.TryLock() {
		if waiting != nil {
			waiting(calID)
		}
		m.Lock()
	}
	return m.Unlock
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
	w.unlock = l.cals.lock(calID, l.cals.waiting)
	return w, nil
}

// objectState is an object as the cache holds it after the refresh: its
// rows, the UID a name stands for and whether that UID has rows, and the
// calendar's aliases (UID to name).
type objectState struct {
	group    cache.Group
	live     bool
	nameUID  string // the UID the requested name stands for, "" if none
	nameLive bool   // nameUID has rows
	aliases  map[string]string
}

// hrefOf is the name a UID is served at: its alias, else its canonical
// name.
func (s objectState) hrefOf(uid string) string {
	if a, ok := s.aliases[uid]; ok {
		return a
	}
	return ObjectName(uid)
}

// read returns the cached state of uid, and the UID name stands for: its
// alias, else the UID it is the canonical name of.
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
			if st.aliases, err = v.AliasesFor(); err != nil {
				return err
			}
			aliased, ok, err := v.AliasOf(name)
			switch {
			case err != nil:
				return err
			case ok:
				st.nameUID = aliased
			default:
				st.nameUID, _ = UIDOfName(name)
			}
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
	out := make([]cache.Row, len(rows))
	for i, r := range rows {
		out[i] = cache.Row{ID: r.ID, UID: r.UID, Start: r.Start, End: r.End, FullDay: r.FullDay,
			RecurrenceID: r.RecurrenceID, RRule: r.RRule, LastEditTime: r.LastEditTime, Raw: r.Raw}
	}
	return l.onWriter(func(db *cache.Cache, _ *served) error {
		_, err := db.ApplyWrite(ctx, w.calID, uid, out, l.o.Now())
		return l.cacheFail(err)
	})
}

// pinned returns the pins of rows' IDs (ruling M4-R30).
func (l *Local) pinned(ctx context.Context, w *write, rows []events.Row) (map[string]cache.Pin, error) {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
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
		switch now := cache.PinOf(r.Raw); {
		case now.Shared != old.Shared:
			return r.ID, "SharedKeyPacket"
		case now.Calendar != old.Calendar:
			return r.ID, "CalendarKeyPacket"
		case now.Address != old.Address:
			return r.ID, "AddressKeyPacket"
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
	pins, err := l.pinned(ctx, w, fresh)
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
	if err := l.execute(ctx, w, in.UID, st.group, p); err != nil {
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
	return l.execute(ctx, w, uid, st.group, p)
}

// execute sends p in one Sync call, never sent again, reads uid's rows back
// and stores them, and says how it went: nil only when the rows read back
// show every operation done (confirmed). A refused entry, or a whole call
// Proton refused (a 4xx that is not session-wide), is a 403
// (RefuseProtonRefused) once read back; a call that may have reached Proton
// and is not confirmed is ErrWriteNotConfirmed (503); a call that did not
// reach it is the unavailable error of its cause (503, Retry-After for a
// 429).
func (l *Local) execute(ctx context.Context, w *write, uid string, before cache.Group, p planned) error {
	res, syncErr, err := l.sendOnce(ctx, w, p)
	if err != nil {
		return err
	}
	var (
		entry *events.EntryError
		ae    *api.Error
	)
	for _, r := range res {
		if errors.As(r.Err, &entry) {
			break
		}
	}
	// The read-back runs even when the caller hung up meanwhile.
	backCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
	defer cancel()
	switch {
	case syncErr == nil, errors.Is(syncErr, events.ErrAmbiguous):
	case errors.As(syncErr, &ae) && !api.SessionWide(ctx, syncErr):
		// Refused as a whole, before anything was applied: the cache is
		// refreshed when Proton can be read, the answer is the refusal.
		if rows, err := l.listRows(backCtx, w, uid); err == nil {
			_ = l.storeRows(backCtx, w, uid, rows)
		}
		w.log.Warn("Proton refused a write", "calendar", w.calID, "status", ae.Status, "code", ae.Code)
		return protonRefused(ae.Status, ae.Code)
	case api.SessionWide(ctx, syncErr) || errors.Is(syncErr, api.ErrNotSent):
		return l.fail(syncErr)
	default:
		return planFault()
	}
	rows, err := l.listRows(backCtx, w, uid)
	if err != nil {
		w.log.Warn("a write could not be read back", "calendar", w.calID)
		return notConfirmed()
	}
	if err := l.storeRows(backCtx, w, uid, rows); err != nil {
		return err
	}
	if entry != nil {
		w.log.Warn("Proton refused a write", "calendar", w.calID, "entry", entry.Index, "code", entry.Code)
		return protonRefused(0, entry.Code)
	}
	ok := false
	if err := l.withKeys(w, func(_ *calendar.Access, k GroupKeys) error {
		ok = confirmed(p.Ops, before, rows, k)
		return nil
	}); err != nil || !ok {
		w.log.Warn("a write is not confirmed by the rows read back", "calendar", w.calID, "ambiguous", syncErr != nil)
		return notConfirmed()
	}
	return nil
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

// rowFields are the row fields confirmed compares.
type rowFields struct {
	Notifications json.RawMessage
	Color         *string
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
// the notifications (when sent) and the colour (when one is).
func fieldsHold(raw json.RawMessage, body events.Body) bool {
	var s rowFields
	if json.Unmarshal(raw, &s) != nil {
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
// parts open with the key packets body carries (a create) or else the
// row's (an update reuses them).
func partsHold(row events.Row, body events.Body, k GroupKeys) bool {
	got, err := cards.Open(row.Raw, k.Calendar, k.Addresses, k.Own)
	if err != nil {
		return false
	}
	for _, p := range got.Parts {
		if p.Kind != cards.Personal && (!p.Own || !p.Verified) {
			return false
		}
	}
	pin := cache.PinOf(row.Raw)
	shared, calendarPacket := body.SharedKeyPacket, body.CalendarKeyPacket
	if shared == "" {
		shared = pin.Shared
	}
	if calendarPacket == "" {
		calendarPacket = pin.Calendar
	}
	sentRaw, err := json.Marshal(map[string]any{
		"ID": "sent", "UID": row.UID, "SharedKeyPacket": shared, "CalendarKeyPacket": calendarPacket,
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

// holds reports whether row, as listed, holds body: its parts (partsHold)
// and its fields (fieldsHold).
func holds(row events.Row, body events.Body, k GroupKeys) bool {
	return fieldsHold(row.Raw, body) && partsHold(row, body, k)
}

// confirmed reports whether rows, uid's rows read back after the write,
// show every operation of ops done: a deleted row gone, an updated row
// holding what was sent, a created row (one that was not in before)
// holding what was sent, each row standing for one operation.
func confirmed(ops []events.Op, before cache.Group, rows []events.Row, k GroupKeys) bool {
	old := make(map[string]bool, len(before.Rows))
	for _, r := range before.Rows {
		old[r.ID] = true
	}
	byID := make(map[string]events.Row, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	used := map[string]bool{}
	for _, op := range ops {
		switch {
		case op.Delete != nil:
			if _, ok := byID[op.Delete.ID]; ok {
				return false
			}
		case op.Update != nil:
			r, ok := byID[op.Update.ID]
			if !ok || used[r.ID] || !holds(r, op.Update.Body, k) {
				return false
			}
			used[r.ID] = true
		case op.Create != nil:
			i := slices.IndexFunc(rows, func(r events.Row) bool { return !old[r.ID] && !used[r.ID] && holds(r, *op.Create, k) })
			if i < 0 {
				return false
			}
			used[rows[i].ID] = true
		}
	}
	return true
}
