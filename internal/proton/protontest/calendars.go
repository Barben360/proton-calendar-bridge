package protontest

import (
	"cmp"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// Calendar describes a calendar for AddCalendar.
type Calendar struct {
	Name, Description, Color string
	Type                     int  // 0 normal, 1 subscribed, 2 holidays, anything else is served as is
	LegacyTopLevel           bool // name, description and colour at the top level, not on the member
	ToSecondAddress          bool // our member is the user's second address: the passphrase is encrypted to its key, its key signs
	BrokenPassphrase         bool // ... to an unrelated key (no address key decrypts it)
	ExtraLockedKey           bool // another calendar key, no passphrase opens it, listed first
	ForeignMemberFirst       bool // another member, listed first with its passphrase entry encrypted to an unrelated key
	// Permissions is our member's bitmask (research H: 8 is Write); 0
	// serves 127, every bit, as for an owned calendar.
	Permissions int
	// NoPrimaryKey flags no calendar key primary; TwoPrimaryKeys flags the
	// retired key primary too (anomalies a client must fail closed on).
	NoPrimaryKey, TwoPrimaryKeys bool
	// The calendar's default reminders, as its bootstrap serves them; nil:
	// one DISPLAY reminder, -PT1H for part-day events and -PT15H for full-day
	// ones (see SetDefaultNotifications).
	PartDayNotifications, FullDayNotifications []Notification
}

// calendars holds the fake's calendar state, guarded by mu.
type calendars struct {
	mu       sync.Mutex
	byUser   map[string]*userCalendars    // username → state
	byID     map[string]*calendarRec      // calendar ID → record
	clock    int64                        // fake timestamps issued (see tick)
	feedPage int                          // SetFeedPageSize, 0: default
	onList   func(calID string, page int) // OnListPage, nil: none
	syncs    []syncRecord                 // every sync request, in order (SyncCalls)
}

// user returns username's state, creating it. Caller holds mu.
func (c *calendars) user(username string) *userCalendars {
	u := c.byUser[username]
	if u == nil {
		u = &userCalendars{}
		c.byUser[username] = u
	}
	return u
}

type userCalendars struct {
	list      []*calendarRec
	defaultID string
}

type calendarRec struct {
	id         string
	owner      string // username
	def        Calendar
	passphrase string
	memberID   string
	// keys are the calendar keys in the bootstrap's order, unlocked here:
	// with ExtraLockedKey the key no passphrase opens, then the retired
	// key, then the primary one, then any key a rotation added.
	keys    []calendarKey
	retired *crypto.Key // a former key: listed, opens, takes no new key packet
	extra   *crypto.Key // the key no passphrase opens (ExtraLockedKey)
	// Filled on the first request of the user, from the address list.
	ready   bool
	address gpa.Address
	encPass string
	passSig string        // detached signature of the passphrase
	foreign foreignMember // set with ForeignMemberFirst
	ev      *eventState   // rows and feed, see events.go
}

// calendarKey is one calendar key: unlocked, and as the bootstrap serves it.
type calendarKey struct {
	key     *crypto.Key
	armored string // locked
	primary bool   // Flags 3 (active and primary), else 1 (active)
}

// foreignMember is another member of a shared calendar.
type foreignMember struct{ id, encPass, passSig string }

// primaryKey is the key new key packets must go to. Caller holds mu.
func (c *calendarRec) primaryKey() *crypto.Key {
	for _, k := range c.keys {
		if k.primary {
			return k.key
		}
	}
	panic("protontest: a calendar without a primary key")
}

// primaryRing is the primary key alone in a keyring. Caller holds mu.
func (c *calendarRec) primaryRing() (*crypto.KeyRing, error) {
	return crypto.NewKeyRing(c.primaryKey())
}

// permissions is our member's bitmask.
func (c *calendarRec) permissions() int {
	if c.def.Permissions == 0 {
		return 127
	}
	return c.def.Permissions
}

// writable says whether our member may write events: a normal calendar and
// the Write bit (research H).
func (c *calendarRec) writable() bool { return c.def.Type == 0 && c.permissions()&8 != 0 }

// newCalendarKey generates a calendar key (x25519) locked with pass.
func (f *Fake) newCalendarKey(name string, pass []byte) calendarKey {
	f.t.Helper()
	key, err := crypto.GenerateKey(name, "", "x25519", 0)
	if err != nil {
		f.t.Fatalf("protontest: calendar key: %v", err)
	}
	return calendarKey{key: key, armored: f.lockedArmor(key, pass)}
}

// lockedArmor is key locked with pass and armored, as the bootstrap serves it.
func (f *Fake) lockedArmor(key *crypto.Key, pass []byte) string {
	f.t.Helper()
	locked, err := key.Lock(pass)
	if err != nil {
		f.t.Fatalf("protontest: lock calendar key: %v", err)
	}
	armored, err := locked.Armor()
	if err != nil {
		f.t.Fatal(err)
	}
	return armored
}

// AddCalendar adds a calendar for username and returns its ID. It has two
// calendar keys (x25519), a retired one and the primary one, locked with a
// random base64 passphrase; that passphrase is encrypted, on the user's
// first calendar request, to the public key of the first address (the
// second with ToSecondAddress, an unrelated key with BrokenPassphrase) and
// signed by the address key.
func (f *Fake) AddCalendar(username string, c Calendar) (id string) {
	f.t.Helper()
	id = "cal" + hex.EncodeToString(random(f.t, 8)) + "=="
	pass := base64.StdEncoding.EncodeToString(random(f.t, 32))
	retired := f.newCalendarKey("Retired calendar key", []byte(pass))
	primary := f.newCalendarKey("Calendar key", []byte(pass))
	primary.primary = true
	rec := &calendarRec{
		id: id, owner: username, def: c, passphrase: pass, memberID: "mem" + hex.EncodeToString(random(f.t, 6)) + "==",
		keys: []calendarKey{retired, primary}, retired: retired.key,
	}
	if c.ExtraLockedKey {
		extra := f.newCalendarKey("Old calendar key", random(f.t, 16))
		rec.keys = append([]calendarKey{extra}, rec.keys...)
		rec.extra = extra.key
	}
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	u := f.cal.user(username)
	u.list = append(u.list, rec)
	f.cal.byID[id] = rec
	return id
}

// MemberID returns our member's ID in the calendar, as the bootstrap lists
// it and a sync body names it.
func (f *Fake) MemberID(calID string) string {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	rec := f.cal.byID[calID]
	if rec == nil {
		f.t.Fatalf("protontest: unknown calendar %q", calID)
	}
	return rec.memberID
}

// SetDefaultCalendar makes id the default calendar of username.
func (f *Fake) SetDefaultCalendar(username, id string) {
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	f.cal.user(username).defaultID = id
}

// RotateCalendarKey adds a new calendar key, locked with the calendar's
// passphrase, listed last and primary, as a key rotation does; the rows
// keep their key packets to the former key, which stays listed.
func (f *Fake) RotateCalendarKey(id string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	rec := f.cal.byID[id]
	if rec == nil {
		f.t.Fatalf("protontest: unknown calendar %q", id)
	}
	key := f.newCalendarKey("Calendar key", []byte(rec.passphrase))
	key.primary = true
	for i := range rec.keys {
		rec.keys[i].primary = false
	}
	rec.keys = append(rec.keys, key)
}

// ReactivateCalendarKey locks the calendar's extra key (ExtraLockedKey) with
// the calendar passphrase, listed under the same ID: the key now opens, as
// after a key reactivation.
func (f *Fake) ReactivateCalendarKey(id string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	rec := f.cal.byID[id]
	if rec == nil || rec.extra == nil {
		f.t.Fatalf("protontest: calendar %q has no extra key", id)
	}
	rec.keys[0].armored = f.lockedArmor(rec.extra, []byte(rec.passphrase))
}

// RemoveCalendar takes the calendar out of username's list, as if deleted
// in Proton: the list no longer shows it and its routes answer as unknown.
func (f *Fake) RemoveCalendar(username, id string) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	u := f.cal.user(username)
	i := slices.IndexFunc(u.list, func(r *calendarRec) bool { return r.id == id })
	if i < 0 {
		f.t.Fatalf("protontest: user %q has no calendar %q", username, id)
	}
	u.list = slices.Delete(u.list, i, i+1)
	delete(f.cal.byID, id)
}

// SetDefaultNotifications replaces the calendar's default reminders (nil
// restores the fake's defaults), as a change in Proton's calendar settings.
func (f *Fake) SetDefaultNotifications(id string, partDay, fullDay []Notification) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	rec := f.cal.byID[id]
	if rec == nil {
		f.t.Fatalf("protontest: unknown calendar %q", id)
	}
	rec.def.PartDayNotifications, rec.def.FullDayNotifications = partDay, fullDay
}

// SetPermissions changes our member's bitmask in the calendar, as when its
// owner changes the share (research H); 0 serves 127 again.
func (f *Fake) SetPermissions(id string, perms int) {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	rec := f.cal.byID[id]
	if rec == nil {
		f.t.Fatalf("protontest: unknown calendar %q", id)
	}
	rec.def.Permissions = perms
}

// EncryptToCalendar encrypts plaintext to the calendar's primary key and
// returns the armored message.
func (f *Fake) EncryptToCalendar(id string, plaintext []byte) string {
	f.t.Helper()
	msg, err := f.CalendarKeyRing(id).Encrypt(crypto.NewPlainMessage(plaintext), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	armored, err := msg.GetArmored()
	if err != nil {
		f.t.Fatal(err)
	}
	return armored
}

// AddAddress gives username another address (with a key) and returns its ID.
// The account is changed, so the backend is not reused after this test.
func (f *Fake) AddAddress(username, email string) (addrID string) {
	f.t.Helper()
	f.pool.spoil()
	f.mu.Lock()
	id, pw := f.users[username], f.passwords[username]
	f.mu.Unlock()
	addr, err := f.backend.CreateAddress(id, email, []byte(pw), true)
	if err != nil {
		f.t.Fatalf("protontest: add address: %v", err)
	}
	return addr
}

func (f *Fake) serveCalendar(w http.ResponseWriter, r *http.Request) {
	name, front := f.sessionUser(r)
	if !front {
		var ok bool
		if name, ok = f.whoami(r); !ok {
			reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
			return
		}
	}
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	u := f.cal.user(name)
	if segs, ok := eventSegments(r); ok {
		// Rows, writes and the feed need no address keys, so a Session works
		// here.
		f.serveEvents(w, r, u, segs)
		return
	}
	if front { // the backend, which the other routes need, does not know the token
		reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
		return
	}
	if !f.materialize(u, r) {
		reply(w, http.StatusInternalServerError, 0, "fake: cannot prepare calendars")
		return
	}
	path := r.URL.Path
	if r.Method != http.MethodGet {
		reply(w, http.StatusNotFound, 2000, "fake: route not implemented")
		return
	}
	switch {
	case path == "/settings/calendar":
		writeJSON(w, http.StatusOK, map[string]any{"Code": 1000, "CalendarUserSettings": map[string]any{"DefaultCalendarID": u.defaultID}})
	case path == "/calendar/v1":
		cals := make([]any, 0, len(u.list))
		for _, rec := range u.list {
			cals = append(cals, rec.listEntry())
		}
		writeJSON(w, http.StatusOK, map[string]any{"Code": 1000, "Calendars": cals})
	case strings.HasPrefix(path, "/calendar/v2/") && strings.HasSuffix(path, "/bootstrap") &&
		!strings.Contains(strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/calendar/v2/"), "/bootstrap"), "/"):
		// The ID is one escaped path segment, like Proton's router sees it.
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/calendar/v2/"), "/bootstrap")
		i := slices.IndexFunc(u.list, func(c *calendarRec) bool { return c.id == id })
		if i < 0 {
			reply(w, http.StatusUnprocessableEntity, 2501, "Calendar does not exist")
			return
		}
		writeJSON(w, http.StatusOK, u.list[i].bootstrap())
	default:
		reply(w, http.StatusNotFound, 2000, "fake: route not implemented")
	}
}

// eventSegments returns the unescaped path segments after /calendar/v1/ when
// r targets a calendar's events or model events. An ID holding "/" arrives
// escaped and stays one segment, like in Proton's router.
func eventSegments(r *http.Request) ([]string, bool) {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/calendar/v1/")
	if !ok {
		return nil, false
	}
	segs := strings.Split(rest, "/")
	if len(segs) < 2 || (segs[1] != "events" && segs[1] != "modelevents") {
		return nil, false
	}
	for i, s := range segs {
		v, err := url.PathUnescape(s)
		if err != nil {
			return nil, false
		}
		segs[i] = v
	}
	return segs, true
}

// materialize encrypts the passphrases of the user's new calendars to the
// address keys fetched from the backend with r's session. Caller holds mu.
func (f *Fake) materialize(u *userCalendars, r *http.Request) bool {
	var pending []*calendarRec
	for _, rec := range u.list {
		if !rec.ready {
			pending = append(pending, rec)
		}
	}
	if len(pending) == 0 {
		return true
	}
	hdr := http.Header{"x-pm-uid": {r.Header.Get("x-pm-uid")}, "Authorization": {r.Header.Get("Authorization")}}
	var out struct{ Addresses []gpa.Address }
	if status, err := f.backendCall(http.MethodGet, "/core/v4/addresses", hdr, nil, &out); err != nil || status != http.StatusOK {
		return false
	}
	slices.SortStableFunc(out.Addresses, func(a, b gpa.Address) int { return cmp.Compare(a.Order, b.Order) })
	for _, rec := range pending {
		idx := 0
		if rec.def.ToSecondAddress {
			idx = 1
		}
		if idx >= len(out.Addresses) {
			f.t.Errorf("protontest: calendar %s needs address #%d, the user has %d", rec.id, idx+1, len(out.Addresses))
			return false
		}
		rec.address = out.Addresses[idx]
		pub, err := recipientRing(rec)
		if err != nil {
			f.t.Errorf("protontest: recipient keys: %v", err)
			return false
		}
		msg, err := pub.Encrypt(crypto.NewPlainMessage([]byte(rec.passphrase)), nil)
		if err != nil {
			f.t.Errorf("protontest: encrypt passphrase: %v", err)
			return false
		}
		if rec.encPass, err = msg.GetArmored(); err != nil {
			return false
		}
		signed := []byte(rec.passphrase)
		if f.o.ReplayedPassphraseSignature {
			signed = []byte("the passphrase of another calendar")
		}
		if rec.passSig, err = signPassphrase(rec, signed, f.o.BadPassphraseSignature); err != nil {
			f.t.Errorf("protontest: sign passphrase: %v", err)
			return false
		}
		if f.o.EmptyPassphraseSignature {
			rec.passSig = ""
		}
		if rec.def.ForeignMemberFirst {
			other, err := recipientRing(&calendarRec{def: Calendar{BrokenPassphrase: true}})
			if err != nil {
				return false
			}
			m, err := other.Encrypt(crypto.NewPlainMessage([]byte("not-the-passphrase")), nil)
			if err != nil {
				return false
			}
			rec.foreign.id = "mem" + hex.EncodeToString(random(f.t, 6)) + "=="
			if rec.foreign.encPass, err = m.GetArmored(); err != nil {
				return false
			}
			// The inviter's key signed it: not one of ours.
			if rec.foreign.passSig, err = signPassphrase(rec, []byte("not-the-passphrase"), true); err != nil {
				return false
			}
		}
		rec.ready = true
	}
	return true
}

// signPassphrase is the detached signature of a member passphrase (its
// plaintext, binary mode) by the key of the calendar's address (research G),
// or by an unrelated key when foreign.
func signPassphrase(rec *calendarRec, pass []byte, foreign bool) (string, error) {
	kr, err := memberKey(rec)
	if foreign {
		kr, err = stranger()
	}
	if err != nil {
		return "", err
	}
	sig, err := kr.SignDetached(crypto.NewPlainMessage(pass))
	if err != nil {
		return "", err
	}
	return sig.GetArmored()
}

// recipientRing is the public keyring the passphrase is encrypted to.
func recipientRing(rec *calendarRec) (*crypto.KeyRing, error) {
	if rec.def.BrokenPassphrase {
		k, err := crypto.GenerateKey("Unrelated", "", "x25519", 0)
		if err != nil {
			return nil, err
		}
		return crypto.NewKeyRing(k)
	}
	var kr *crypto.KeyRing
	for _, k := range rec.address.Keys {
		key, err := crypto.NewKey(k.PrivateKey)
		if err != nil {
			return nil, err
		}
		pub, err := key.ToPublic()
		if err != nil {
			return nil, err
		}
		if kr == nil {
			kr, err = crypto.NewKeyRing(pub)
		} else {
			err = kr.AddKey(pub)
		}
		if err != nil {
			return nil, err
		}
	}
	if kr == nil {
		return nil, errors.New("the address has no key")
	}
	return kr, nil
}

func (c *calendarRec) member() map[string]any {
	m := map[string]any{
		"ID": c.memberID, "CalendarID": c.id, "AddressID": c.address.ID, "Email": c.address.Email,
		"Display": 1, "Permissions": c.permissions(), "Flags": 1,
		"Name": c.def.Name, "Description": c.def.Description, "Color": c.def.Color,
	}
	if c.def.LegacyTopLevel {
		m["Name"], m["Description"], m["Color"] = "", "", ""
	}
	return m
}

// members is ours, preceded by a foreign one with ForeignMemberFirst.
func (c *calendarRec) members() []any {
	ours := c.member()
	if !c.def.ForeignMemberFirst {
		return []any{ours}
	}
	other := map[string]any{"ID": c.foreign.id, "CalendarID": c.id, "AddressID": "foreign-addr", "Email": "other@example.org", "Permissions": 1, "Display": 1}
	return []any{other, ours}
}

func (c *calendarRec) memberPassphrases() []any {
	ours := map[string]any{"MemberID": c.memberID, "Passphrase": c.encPass, "Signature": c.passSig}
	if !c.def.ForeignMemberFirst {
		return []any{ours}
	}
	return []any{map[string]any{"MemberID": c.foreign.id, "Passphrase": c.foreign.encPass, "Signature": c.foreign.passSig}, ours}
}

func (c *calendarRec) listEntry() map[string]any {
	e := map[string]any{"ID": c.id, "Type": c.def.Type, "CreateTime": 1700000000, "Members": []any{c.member()}}
	if c.def.LegacyTopLevel {
		e["Name"], e["Description"], e["Color"] = c.def.Name, c.def.Description, c.def.Color
	}
	return e
}

func (c *calendarRec) bootstrap() map[string]any {
	keys := make([]any, 0, len(c.keys))
	for i, k := range c.keys {
		flags := 1 // active
		if (k.primary && !c.def.NoPrimaryKey) || (c.def.TwoPrimaryKeys && k.key == c.retired) {
			flags = 3 // active and primary
		}
		keys = append(keys, map[string]any{"ID": "key" + strconv.Itoa(i), "CalendarID": c.id, "PassphraseID": "pass1", "PrivateKey": k.armored, "Flags": flags})
	}
	return map[string]any{
		"Code": 1000,
		"Keys": keys,
		"Passphrase": map[string]any{
			"ID": "pass1", "Flags": 1,
			"MemberPassphrases": c.memberPassphrases(),
		},
		"Members": c.members(),
		"CalendarSettings": map[string]any{
			"ID": "set1", "CalendarID": c.id, "DefaultEventDuration": 30, "MakesUserBusy": 1,
			"DefaultPartDayNotifications": notifications(c.def.PartDayNotifications, "-PT1H"),
			"DefaultFullDayNotifications": notifications(c.def.FullDayNotifications, "-PT15H"),
		},
	}
}

// notifications serves a default reminder list; nil is one DISPLAY reminder
// at trigger.
func notifications(list []Notification, trigger string) []any {
	if list == nil {
		return []any{map[string]any{"Type": 1, "Trigger": trigger}}
	}
	out := make([]any, 0, len(list))
	for _, n := range list {
		out = append(out, map[string]any{"Type": n.Type, "Trigger": n.Trigger})
	}
	return out
}
