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
// Version 2: X-PCB-INTEGRITY (ruling M3-R33).
const renderVersion = 2

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
	load sync.Mutex   // serialises key loads and refreshes; held across Proton calls
	kmu  sync.RWMutex // guards gen, ks, refreshedAt and ks.users; held for reading while keys are used
	gen  uint64       // bumped by drop, so a load that raced it is discarded
	ks   *keyState
	// refreshedAt is when the calendars' settings were last read from
	// their bootstraps (Serve refreshes them every settingsRefresh).
	refreshedAt time.Time

	lru      *lru
	decrypts atomic.Int64 // rows decrypted (tests)
	// beforeLoad, when set (tests), runs as a key load starts.
	beforeLoad func()

	smu           sync.Mutex
	undecryptable map[string]bool // row IDs served as placeholders
	unverified    map[string]bool // row IDs rendered with X-PCB-INTEGRITY:unverified (M3-R33)
	duplicates    map[string]int  // masters left out (M3-R3) by calendar ID and UID, as last rendered
}

// keyState is the unlocked keys of one account: its address keys and each
// calendar's keys, unlocked on the first read of the calendar.
type keyState struct {
	account  string
	u        *keys.Unlocked
	own      map[string]*crypto.KeyRing // address keyring by lower-case email
	addrTags []string                   // addressKeyTags of u
	// cals is written under both load and kmu, so a holder of load may read
	// it without kmu.
	cals map[string]*calKeys
	// users counts the loads using u outside kmu (acquire); a dropped state
	// is closed by its last user.
	users   int
	dropped bool
}

// calKeys is one calendar's keys and the inputs its rendering uses besides
// the rows. It is never modified once built (a refresh replaces it).
type calKeys struct {
	access *calendar.Access // nil when the keys did not unlock
	err    string           // the secret-free reason they did not
	// settings is every non-row input of the render: the calendar's default
	// notifications and the IDs of the keys that open it (sorted calendar
	// keys, sorted address keys), so a settings change or a key rotation
	// changes the object ETags, the CTag (hash) and the token generation
	// (gen, ruling M3-R11).
	settings         []byte
	hash, gen        string
	partDay, fullDay json.RawMessage
}

// newCalKeys builds calendar calID's keys from its unlocked access and the
// address-key tags of the account (addressKeyTags).
func newCalKeys(calID string, a *calendar.Access, addrTags []string) *calKeys {
	partDay, _ := json.Marshal(a.Settings.DefaultPartDayNotifications)
	fullDay, _ := json.Marshal(a.Settings.DefaultFullDayNotifications)
	calTags := make([]string, 0, len(a.Keys))
	for _, k := range a.Keys {
		calTags = append(calTags, keyTag(k.ID, k.Unlocked))
	}
	slices.Sort(calTags)
	settings, _ := json.Marshal(struct {
		PartDay, FullDay          json.RawMessage
		CalendarKeys, AddressKeys []string
	}{partDay, fullDay, calTags, addrTags})
	return &calKeys{access: a, settings: settings, hash: settingsHash(settings), gen: tokenGen(calID, settings),
		partDay: partDay, fullDay: fullDay}
}

// keyTag names a key ID with whether it unlocked (ruling M3-R16): a key
// that opens again under the same ID (reactivation) changes the render.
func keyTag(id string, unlocked bool) string {
	if unlocked {
		return id + ":unlocked"
	}
	return id + ":locked"
}

// addressKeyTags tags every address key ID with whether that key is in its
// address's unlocked keyring (matched by fingerprint), sorted.
func addressKeyTags(u *keys.Unlocked) []string {
	var tags []string
	for _, addr := range u.Addresses {
		open := map[string]bool{}
		if kr, ok := u.AddrKRs[addr.ID]; ok {
			for _, k := range kr.GetKeys() {
				open[k.GetFingerprint()] = true
			}
		}
		for _, k := range addr.Keys {
			unlocked := false
			if key, err := crypto.NewKeyFromArmored(string(k.PrivateKey)); err == nil {
				unlocked = open[key.GetFingerprint()]
			}
			tags = append(tags, keyTag(k.ID, unlocked))
		}
	}
	slices.Sort(tags)
	return tags
}

func failedCalKeys(calID, reason string) *calKeys {
	return &calKeys{err: reason, settings: keysUnavailable, hash: settingsHash(keysUnavailable), gen: tokenGen(calID, keysUnavailable)}
}

// tokenGen is the generation part of calendar calID's sync tokens: 8 hex of
// the SHA-256 of the render version, the calendar ID and the settings (key
// IDs included). A token of another generation was minted under other
// render inputs, or for another calendar (ruling M3-R11).
func tokenGen(calID string, settings []byte) string {
	h := sha256.New()
	field(h, []byte("pcb-token "+strconv.Itoa(renderVersion)))
	field(h, []byte(calID))
	field(h, settings)
	return hex.EncodeToString(h.Sum(nil))[:8]
}

func settingsHash(settings []byte) string {
	sum := sha256.Sum256(settings)
	return hex.EncodeToString(sum[:])[:16]
}

func newKeyState(account string, u *keys.Unlocked, addrTags []string) *keyState {
	ks := &keyState{account: account, u: u, addrTags: addrTags, own: map[string]*crypto.KeyRing{}, cals: map[string]*calKeys{}}
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

// acquire returns the generation and the keys, counting the caller as a
// user of the keys until release: a load or refresh uses them across
// Proton calls without holding kmu.
func (r *renderer) acquire() (uint64, *keyState) {
	r.kmu.Lock()
	defer r.kmu.Unlock()
	if r.ks != nil {
		r.ks.users++
	}
	return r.gen, r.ks
}

// release ends a use started by acquire; the last user of dropped keys
// closes them.
func (r *renderer) release(ks *keyState) {
	if ks == nil {
		return
	}
	r.kmu.Lock()
	defer r.kmu.Unlock()
	ks.users--
	if ks.dropped && ks.users == 0 {
		ks.close()
	}
}

// retire forgets the current keys, closing them unless a load still uses
// them (its release does). The caller holds kmu.
func (r *renderer) retire() {
	if r.ks == nil {
		return
	}
	r.ks.dropped = true
	if r.ks.users == 0 {
		r.ks.close()
	}
	r.ks = nil
}

// drop forgets the keys (clearing their private parameters), the rendered
// objects and the counters. It waits for the renders using the keys.
func (r *renderer) drop() {
	r.kmu.Lock()
	r.gen++
	r.retire()
	r.kmu.Unlock()
	r.lru.clear()
	r.smu.Lock()
	r.undecryptable, r.unverified, r.duplicates = nil, nil, nil
	r.smu.Unlock()
}

// noteRow records whether row id rendered as a placeholder, and whether it
// rendered as unverified.
func (r *renderer) noteRow(id string, placeholder, unverified bool) {
	r.smu.Lock()
	defer r.smu.Unlock()
	mark(&r.undecryptable, id, placeholder)
	mark(&r.unverified, id, unverified)
}

// mark adds id to the set *m when on, else removes it.
func mark(m *map[string]bool, id string, on bool) {
	if !on {
		delete(*m, id)
		return
	}
	if *m == nil {
		*m = map[string]bool{}
	}
	(*m)[id] = true
}

// noteDuplicates records how many masters the last render of a UID left
// out, so a duplicate that is gone stops being counted.
func (r *renderer) noteDuplicates(calID, uid string, n int) {
	r.smu.Lock()
	defer r.smu.Unlock()
	key := calID + "\x00" + uid
	if n == 0 {
		delete(r.duplicates, key)
		return
	}
	if r.duplicates == nil {
		r.duplicates = map[string]int{}
	}
	r.duplicates[key] = n
}

// keepDuplicates forgets the counts of calendar calID's UIDs that live does
// not hold: a UID deleted whole no longer renders, so only a read that sees
// it gone can uncount it.
func (r *renderer) keepDuplicates(calID string, live func(uid string) bool) {
	r.smu.Lock()
	defer r.smu.Unlock()
	prefix := calID + "\x00"
	for key := range r.duplicates {
		if uid, ok := strings.CutPrefix(key, prefix); ok && !live(uid) {
			delete(r.duplicates, key)
		}
	}
}

// RenderCounts returns the render counters of this process (ruling
// M3-R34). It is not part of API: another process sees none of them. Serve
// hands them to each pass (WatchPass.Render).
func (l *Local) RenderCounts() RenderCounts {
	var out RenderCounts
	l.r.kmu.RLock()
	if ks := l.r.ks; ks != nil {
		for id, ck := range ks.cals {
			if ck.err != "" {
				out.KeysErrors = append(out.KeysErrors, id)
			}
		}
	}
	l.r.kmu.RUnlock()
	slices.Sort(out.KeysErrors)
	l.r.smu.Lock()
	defer l.r.smu.Unlock()
	for _, n := range l.r.duplicates {
		out.DuplicateMasters += n
	}
	out.Undecryptable, out.Unverified = len(l.r.undecryptable), len(l.r.unverified)
	return out
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
func (l *Local) render(calID string, g cache.Group, ks *keyState, ck *calKeys) []byte {
	var master *cache.StoredRow
	var exceptions []ical.Exception
	masters := 0
	for i := range g.Rows {
		row := &g.Rows[i]
		if row.RecurrenceID != nil {
			exceptions = append(exceptions, ical.Exception{Comp: l.renderRow(row, ks, ck), RecurrenceID: *row.RecurrenceID})
			continue
		}
		masters++
		// Rows come in ID order: on a tie, the later ID wins.
		if master == nil || row.LastEdit >= master.LastEdit {
			master = row
		}
	}
	l.r.noteDuplicates(calID, g.UID, max(masters-1, 0))
	var m *ical.Component
	if master != nil {
		c := l.renderRow(master, ks, ck)
		m = &c
	}
	return ical.Object(m, exceptions)
}

// renderRow decrypts and merges one row into its VEVENT, or returns its
// placeholder.
func (l *Local) renderRow(row *cache.StoredRow, ks *keyState, ck *calKeys) ical.Component {
	ev, unverified, ok := l.decryptRow(row, ks, ck)
	l.r.noteRow(row.ID, !ok, ok && unverified)
	if !ok {
		return placeholder(row)
	}
	return ev
}

// decryptRow renders row's VEVENT; unverified when a part of it has a bad or
// missing signature (unverifiedParts). Such an event is served marked
// X-PCB-INTEGRITY:unverified, never dropped (ruling M3-R33).
func (l *Local) decryptRow(row *cache.StoredRow, ks *keyState, ck *calKeys) (ev ical.Component, unverified, ok bool) {
	if ck.access == nil {
		return ical.Component{}, false, false
	}
	l.r.decrypts.Add(1)
	opened, err := cards.Open(row.Raw, ck.access.KR, ks.u.AddrKRs, func(email string) *crypto.KeyRing {
		return ks.own[strings.ToLower(email)]
	})
	if err != nil {
		return ical.Component{}, false, false
	}
	parts := make([][]byte, len(opened.Parts))
	for i, p := range opened.Parts {
		parts[i] = p.Text
	}
	unverified = unverifiedParts(opened.Parts)
	ev, err = ical.MergeEvent(parts)
	if err != nil {
		return ical.Component{}, false, false
	}
	defaults := ck.partDay
	if row.FullDay {
		defaults = ck.fullDay
	}
	if err := ical.ApplyNotifications(&ev, opened.Notifications, defaults, row.FullDay); err != nil {
		return ical.Component{}, false, false
	}
	statuses := make(map[string]int, len(opened.Attendees))
	for _, a := range opened.Attendees {
		statuses[a.Token] = a.Status
	}
	ical.ApplyRow(&ev, opened.Color, statuses)
	ical.MarkIntegrity(&ev, unverified)
	return ev, unverified, true
}

// unverifiedParts reports whether a part of an event has a bad or missing
// signature (SPEC §5.3): one that does not verify with the key of its
// author, one of our addresses, including a part sent unsigned (a server
// could otherwise drop a signature to pass an edit).
func unverifiedParts(parts []cards.Part) bool {
	for _, p := range parts {
		if !p.Verified {
			return true
		}
	}
	return false
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
