package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// renderVersion is folded into every object ETag: bump it whenever the
// same rows render differently, so that clients fetch every object again.
const renderVersion = 1

// placeholderSummary is the SUMMARY of an event pcb cannot decrypt (ruling
// M3-R4).
const placeholderSummary = "(pcb could not decrypt this event)"

// keysUnavailable stands for the settings of a calendar whose keys failed:
// no settings JSON reads like it, so its objects' ETags and its CTag change
// when the keys unlock again.
var keysUnavailable = []byte("keys-unavailable")

// renderer is what rendering keeps for the process, in memory only: the
// unlocked keys, the rendered objects and the counters Status reports. All
// of it is dropped with the session client (login, relogin, logout, Close).
type renderer struct {
	load sync.Mutex   // serialises key loading; held across Proton calls
	kmu  sync.RWMutex // guards gen and ks; held for reading while keys are used
	gen  uint64       // bumped by drop, so a load that raced it is discarded
	ks   *keyState

	lru      *lru
	decrypts atomic.Int64 // rows decrypted (tests)

	smu           sync.Mutex
	undecryptable map[string]bool // row IDs served as placeholders
	duplicates    map[string]bool // duplicate masters left out (M3-R3)
}

// keyState is the unlocked keys of one account: its address keys and each
// calendar's keys, unlocked on the first read of the calendar.
type keyState struct {
	account string
	u       *keys.Unlocked
	own     map[string]*crypto.KeyRing // address keyring by lower-case email
	cals    map[string]*calKeys
}

// calKeys is one calendar's keys and the settings its rendering uses. It is
// never modified once built.
type calKeys struct {
	access *calendar.Access // nil when the keys did not unlock
	err    string           // the secret-free reason they did not
	// settings is what the render reads from the calendar (its default
	// notifications), as folded into the object ETags; hash names it in the
	// CTag.
	settings         []byte
	hash             string
	partDay, fullDay json.RawMessage
}

func newCalKeys(a *calendar.Access) *calKeys {
	partDay, _ := json.Marshal(a.Settings.DefaultPartDayNotifications)
	fullDay, _ := json.Marshal(a.Settings.DefaultFullDayNotifications)
	settings, _ := json.Marshal(struct{ PartDay, FullDay json.RawMessage }{partDay, fullDay})
	return &calKeys{access: a, settings: settings, hash: settingsHash(settings), partDay: partDay, fullDay: fullDay}
}

func failedCalKeys(reason string) *calKeys {
	return &calKeys{err: reason, settings: keysUnavailable, hash: settingsHash(keysUnavailable)}
}

func settingsHash(settings []byte) string {
	sum := sha256.Sum256(settings)
	return hex.EncodeToString(sum[:])[:16]
}

func newKeyState(account string, u *keys.Unlocked) *keyState {
	ks := &keyState{account: account, u: u, own: map[string]*crypto.KeyRing{}, cals: map[string]*calKeys{}}
	for _, a := range u.Addresses {
		if kr, ok := u.AddrKRs[a.ID]; ok {
			ks.own[strings.ToLower(a.Email)] = kr
		}
	}
	return ks
}

func (ks *keyState) close() {
	for _, ck := range ks.cals {
		ck.access.Close()
	}
	ks.u.Close()
}

// has reports whether the keys of every calendar of ids were tried.
func (r *renderer) has(ids []string) bool {
	r.kmu.RLock()
	defer r.kmu.RUnlock()
	if r.ks == nil {
		return false
	}
	for _, id := range ids {
		if r.ks.cals[id] == nil {
			return false
		}
	}
	return true
}

// calKeys returns calendar id's keys, nil when they were not loaded.
func (r *renderer) calKeys(id string) *calKeys {
	r.kmu.RLock()
	defer r.kmu.RUnlock()
	if r.ks == nil {
		return nil
	}
	return r.ks.cals[id]
}

// drop forgets the keys (clearing their private parameters), the rendered
// objects and the counters. It waits for the renders using the keys.
func (r *renderer) drop() {
	r.kmu.Lock()
	r.gen++
	if r.ks != nil {
		r.ks.close()
		r.ks = nil
	}
	r.kmu.Unlock()
	r.lru.clear()
	r.smu.Lock()
	r.undecryptable, r.duplicates = nil, nil
	r.smu.Unlock()
}

// note adds row id to the counted set (on) or removes it from it.
func (r *renderer) note(set *map[string]bool, id string, on bool) {
	r.smu.Lock()
	defer r.smu.Unlock()
	if on {
		if *set == nil {
			*set = map[string]bool{}
		}
		(*set)[id] = true
	} else {
		delete(*set, id)
	}
}

// counts returns the placeholders served and the duplicate masters left
// out since the keys were loaded.
func (r *renderer) counts() (undecryptable, duplicates int) {
	r.smu.Lock()
	defer r.smu.Unlock()
	return len(r.undecryptable), len(r.duplicates)
}

// objectETag is the ETag of g's object: "v1-" and 32 hex of the SHA-256 of
// the render version, every row's (ID, ETag) sorted by ID, and the calendar
// settings the render uses. Any row change, a settings change or a new
// render version changes it.
func objectETag(g cache.Group, ck *calKeys) string {
	rows := slices.Clone(g.Rows)
	slices.SortFunc(rows, func(a, b cache.StoredRow) int { return strings.Compare(a.ID, b.ID) })
	h := sha256.New()
	field(h, []byte("pcb-object "+strconv.Itoa(renderVersion)))
	for _, r := range rows {
		field(h, []byte(r.ID))
		field(h, []byte(r.ETag))
	}
	field(h, ck.settings)
	return `"v1-` + hex.EncodeToString(h.Sum(nil))[:32] + `"`
}

// field writes b length-prefixed, so no two inputs share an encoding.
func field(h hash.Hash, b []byte) {
	h.Write(binary.AppendUvarint(nil, uint64(len(b))))
	h.Write(b)
}

// render renders g's object (one per UID): the most recently edited master
// (duplicates are left out and counted, ruling M3-R3), then the exceptions.
// A row that does not decrypt or parse, or every row of a calendar whose
// keys failed, renders as a placeholder (ruling M3-R4). The caller holds
// kmu for reading; ks and ck are the keys under it.
func (l *Local) render(g cache.Group, ks *keyState, ck *calKeys) []byte {
	var master *cache.StoredRow
	var exceptions []ical.Exception
	for i := range g.Rows {
		row := &g.Rows[i]
		if row.RecurrenceID != nil {
			exceptions = append(exceptions, ical.Exception{Comp: l.renderRow(row, ks, ck), RecurrenceID: *row.RecurrenceID})
			continue
		}
		// Rows come in ID order: on a tie, the later ID wins.
		if master == nil || row.LastEdit >= master.LastEdit {
			if master != nil {
				l.r.note(&l.r.duplicates, master.ID, true)
			}
			master = row
		} else {
			l.r.note(&l.r.duplicates, row.ID, true)
		}
	}
	var m *ical.Component
	if master != nil {
		l.r.note(&l.r.duplicates, master.ID, false)
		c := l.renderRow(master, ks, ck)
		m = &c
	}
	return ical.Object(m, exceptions)
}

// renderRow decrypts and merges one row into its VEVENT, or returns its
// placeholder.
func (l *Local) renderRow(row *cache.StoredRow, ks *keyState, ck *calKeys) ical.Component {
	ev, ok := l.decryptRow(row, ks, ck)
	l.r.note(&l.r.undecryptable, row.ID, !ok)
	if !ok {
		return placeholder(row)
	}
	return ev
}

func (l *Local) decryptRow(row *cache.StoredRow, ks *keyState, ck *calKeys) (ical.Component, bool) {
	if ck.access == nil {
		return ical.Component{}, false
	}
	l.r.decrypts.Add(1)
	opened, err := cards.Open(row.Raw, ck.access.KR, ks.u.AddrKRs, func(email string) *crypto.KeyRing {
		return ks.own[strings.ToLower(email)]
	})
	if err != nil {
		return ical.Component{}, false
	}
	parts := make([][]byte, len(opened.Parts))
	for i, p := range opened.Parts {
		parts[i] = p.Text
	}
	ev, err := ical.MergeEvent(parts)
	if err != nil {
		return ical.Component{}, false
	}
	defaults := ck.partDay
	if row.FullDay {
		defaults = ck.fullDay
	}
	if err := ical.ApplyNotifications(&ev, opened.Notifications, defaults, row.FullDay); err != nil {
		return ical.Component{}, false
	}
	statuses := make(map[string]int, len(opened.Attendees))
	for _, a := range opened.Attendees {
		statuses[a.Token] = a.Status
	}
	ical.ApplyRow(&ev, opened.Color, statuses)
	return ev, true
}

// placeholder is the minimal VEVENT of a row pcb cannot render (ruling
// M3-R4), from the row's plaintext columns only: UID, DTSTAMP (its last
// edit), DTSTART/DTEND (DATE values for an all-day row), RECURRENCE-ID for an
// exception, the placeholder SUMMARY and X-PCB-PLACEHOLDER:1 (M4 refuses
// writes to such an object).
func placeholder(row *cache.StoredRow) ical.Component {
	at := func(name string, unix int64) ical.Property {
		t := time.Unix(unix, 0).UTC()
		if row.FullDay {
			return ical.Property{Name: name, Params: ";VALUE=DATE", Value: t.Format("20060102")}
		}
		return ical.Property{Name: name, Value: t.Format("20060102T150405Z")}
	}
	props := []ical.Property{
		{Name: "UID", Value: textOnly(row.UID)},
		{Name: "DTSTAMP", Value: time.Unix(row.LastEdit, 0).UTC().Format("20060102T150405Z")},
		at("DTSTART", row.Start),
	}
	switch end := row.End; {
	case row.FullDay && end <= row.Start:
		props = append(props, at("DTEND", row.Start+24*3600))
	case end > row.Start:
		props = append(props, at("DTEND", end))
	}
	if row.RecurrenceID != nil {
		props = append(props, at("RECURRENCE-ID", *row.RecurrenceID))
	}
	props = append(props, ical.Property{Name: "SUMMARY", Value: placeholderSummary}, ical.Property{Name: "X-PCB-PLACEHOLDER", Value: "1"})
	c := ical.Component{Name: "VEVENT"}
	for _, p := range props {
		c.Items = append(c.Items, ical.Item{Prop: p})
	}
	return c
}

// textOnly drops the control characters of s, so a plaintext column cannot
// end its content line.
func textOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
