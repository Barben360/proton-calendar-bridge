package service

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// The read API for adapters (Collections, Objects, ObjectsByUID,
// ObjectsByName, Changes)
// reads the event cache: during Serve, Serve's own writer handle (WAL read
// transactions beside its writes, ruling M3-R6); otherwise a read-only
// handle opened for each call (CLI use). Its only network use is unlocking
// the keys, once per process and calendar (ensureKeys), plus Serve's
// settings refresh (serve.go).

const syncTokenPrefix = "urn:pcb:sync:1:"

// syncToken is "urn:pcb:sync:1:<epoch>:<seq>:<gen>": position seq of the
// cache's life epoch, under the render inputs of generation gen (ruling
// M3-R11).
func syncToken(epoch string, seq int64, gen string) string {
	return syncTokenPrefix + epoch + ":" + strconv.FormatInt(seq, 10) + ":" + gen
}

// parseToken reads a sync token; ok is false when it is not one of ours
// (an earlier token without generation included).
func parseToken(token string) (epoch string, seq int64, gen string, ok bool) {
	rest, found := strings.CutPrefix(token, syncTokenPrefix)
	if !found {
		return "", 0, "", false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 || parts[0] == "" || !isDigits(parts[1]) || len(parts[2]) != 8 || !isHex(parts[2]) {
		return "", 0, "", false
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	return parts[0], seq, parts[2], err == nil
}

func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }
func isHex(s string) bool    { return strings.Trim(s, "0123456789abcdef") == "" }

// position is where the calendar stands in the change counter: its newest
// row or tombstone, or its floor when that is higher (a calendar created
// since, with no row past it yet).
func position(v cache.View) int64 { return max(v.MaxSeq(), v.Floor()) }

// served is the cache handle of a running Serve, the account it is for and
// the logger of the writes it serves.
type served struct {
	db      *cache.Cache
	account string
	log     *slog.Logger
}

func notSynced() error { return mustAct("no events are cached yet", "run pcb sync") }

// readCache returns the cache to read and the function releasing it: Serve's
// handle while Serve runs (for the account it serves only), else a read-only
// handle opened for this call. db is nil when there is no cache for the
// logged account. Release it before any Proton call: Serve waits for the
// readers to close its handle.
func (l *Local) readCache() (db *cache.Cache, release func(), err error) {
	l.smu.RLock()
	if s := l.served; s != nil {
		st, err := store.LoadState(l.o.Dirs.State)
		if err != nil {
			l.smu.RUnlock()
			return nil, nil, stateError(err)
		}
		if st.AccountID != s.account {
			l.smu.RUnlock()
			return nil, func() {}, nil
		}
		return s.db, l.smu.RUnlock, nil
	}
	l.smu.RUnlock()
	db, err = l.openCache()
	if err != nil || db == nil {
		return nil, func() {}, err
	}
	return db, func() { _ = db.Close() }, nil
}

// withCache runs fn on the cache to read (readCache), released when fn
// returns; no cache is a MustAct. fn must not call Proton.
func (l *Local) withCache(fn func(*cache.Cache) error) error {
	db, release, err := l.readCache()
	if err != nil {
		return err
	}
	defer release()
	if db == nil {
		return notSynced()
	}
	return fn(db)
}

// syncedCalendar returns calendar id of the cache; ErrNoSuchCalendar when
// the cache has no such calendar, ErrCalendarPending when it has not synced
// it yet (ruling M3-R25).
func syncedCalendar(ctx context.Context, db *cache.Cache, id string) (cache.Calendar, error) {
	cals, err := db.Calendars(ctx)
	if err != nil {
		return cache.Calendar{}, cacheError(err)
	}
	for _, c := range cals {
		if c.ID == id {
			if c.SyncedAt.IsZero() {
				return cache.Calendar{}, calendarPending()
			}
			return c, nil
		}
	}
	return cache.Calendar{}, noSuchCalendar()
}

// cacheFail maps a failure to read the cache; nil stays nil.
func (l *Local) cacheFail(err error) error {
	if err == nil {
		return nil
	}
	return l.fail(cacheError(err))
}

// errKeysDropped: a login, logout or Close dropped the keys during a read.
func errKeysDropped() error { return blocked("the login changed during the read", "retry") }

// ensureKeys unlocks what the calendars ids need and is not unlocked yet:
// the address keys, then each calendar's keys from its bootstrap (one Proton
// call per calendar). A calendar whose keys do not unlock is kept as
// failed: its objects are placeholders (ruling M3-R4) and Status names the
// reason. A failure that is not the calendar's own (network, Proton
// unavailable, the session) is returned and nothing is kept, so the next
// read tries again.
//
// cals gives each calendar's type, from the cache (the bootstrap has none): it
// feeds the write gate.
func (l *Local) ensureKeys(ctx context.Context, cals map[string]calendar.Type) error {
	ids := slices.Sorted(maps.Keys(cals))
	if l.r.has(ids) {
		return nil
	}
	l.r.load.Lock()
	defer l.r.load.Unlock()
	if l.r.has(ids) {
		return nil
	}
	return l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		return l.loadKeys(ctx, c, acc, cals)
	})
}

func (l *Local) loadKeys(ctx context.Context, c *api.Client, acc store.Account, cals map[string]calendar.Type) error {
	ids := slices.Sorted(maps.Keys(cals))
	if l.r.beforeLoad != nil {
		l.r.beforeLoad()
	}
	gen, held := l.r.acquire()
	defer l.r.release(held)
	cur := held
	if cur != nil && cur.account != acc.ID {
		cur = nil
	}
	var u *keys.Unlocked
	if cur != nil {
		u = cur.u
	} else {
		user, addrs, err := keys.Fetch(ctx, c)
		if err != nil {
			return err
		}
		u, err = keys.Unlock(user, addrs, acc.KeyPass)
		if errors.Is(err, keys.ErrKeysLocked) {
			return errKeysChanged
		}
		if err != nil {
			return err
		}
	}
	var addrTags []string
	if cur != nil {
		addrTags = cur.addrTags
	} else {
		addrTags = addressKeyTags(u)
	}
	fresh := map[string]*calKeys{}
	discard := func() {
		for _, ck := range fresh {
			ck.access.Close()
		}
		if cur == nil {
			u.Close()
		}
	}
	for _, id := range ids {
		if fresh[id] != nil || (cur != nil && cur.cals[id] != nil) {
			continue
		}
		a, err := calendar.Unlock(ctx, c, u, calendar.Calendar{ID: id, Type: cals[id]})
		switch {
		case err == nil:
			fresh[id] = newCalKeys(id, a, addrTags)
		case api.SessionWide(ctx, err):
			discard()
			return err
		default:
			fresh[id] = failedCalKeys(id, keysError(err))
		}
	}
	l.r.kmu.Lock()
	defer l.r.kmu.Unlock()
	// Close marks the service closed, then drops the keys: a load that read
	// the generation after that drop must install nothing either.
	if l.closed.Load() {
		discard()
		return errClosed()
	}
	if l.r.gen != gen { // a login or logout dropped the keys meanwhile
		discard()
		return errKeysDropped()
	}
	if cur == nil {
		l.r.retire()
		l.r.ks = newKeyState(acc.ID, u, addrTags)
		l.r.refreshedAt = l.o.Now()
	}
	maps.Copy(l.r.ks.cals, fresh)
	return nil
}

// Collections implements API: every calendar of the cache, a synced one
// with its CTag and sync token, one not synced yet as Pending (ruling
// M3-R25). It may call Proton (ruling M3-R14): the CTag holds the calendar
// settings, read from each synced calendar's bootstrap the first time
// (ensureKeys). Outside Serve it opens the cache read-only for the call;
// during Serve it reads Serve's handle. No cache, or one without any
// calendar, is a MustAct.
func (l *Local) Collections(ctx context.Context) ([]Collection, error) {
	if l.closed.Load() {
		return nil, errClosed()
	}
	var known []cache.Calendar
	ids := map[string]calendar.Type{}
	err := l.withCache(func(db *cache.Cache) error {
		cals, err := db.Calendars(ctx)
		if err != nil {
			return l.fail(cacheError(err))
		}
		known = cals
		for _, c := range cals {
			if !c.SyncedAt.IsZero() {
				ids[c.ID] = calendar.ParseType(c.Type)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(known) == 0 {
		return nil, notSynced()
	}
	if len(ids) > 0 {
		if err := l.ensureKeys(ctx, ids); err != nil {
			return nil, err
		}
	}
	out := make([]Collection, 0, len(known))
	err = l.withCache(func(db *cache.Cache) error {
		for _, c := range known {
			// A calendar not synced yet has no keys: read only until it is.
			coll := Collection{ID: c.ID, Name: c.Name, Color: c.Color, Description: c.Description, ReadOnly: true,
				Pending: c.SyncedAt.IsZero()}
			if coll.Pending {
				out = append(out, coll)
				continue
			}
			ck := l.r.calKeys(c.ID)
			if ck == nil {
				return errKeysDropped()
			}
			// The write gate (ruling M4-R1, replacing M3-R1): calendars whose
			// keys did not unlock are read only too.
			coll.ReadOnly = !ck.access.Writable()
			var epoch string
			var seq int64
			if err := db.Snapshot(ctx, c.ID, func(v cache.View) error {
				epoch, seq = v.Epoch(), position(v)
				return nil
			}); err != nil {
				return l.fail(cacheError(err))
			}
			coll.CTag = epoch + ":" + strconv.FormatInt(seq, 10) + ":" + ck.hash
			coll.SyncToken = syncToken(epoch, seq, ck.gen)
			out = append(out, coll)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// calendarRead checks that the cache holds calendar calID, unlocks its keys
// (with the cache released), then runs fn on the cache with the calendar's
// keys.
func (l *Local) calendarRead(ctx context.Context, calID string, fn func(*cache.Cache, *calKeys) error) error {
	if l.closed.Load() {
		return errClosed()
	}
	var typ calendar.Type
	err := l.withCache(func(db *cache.Cache) error {
		c, err := syncedCalendar(ctx, db, calID)
		typ = calendar.ParseType(c.Type)
		return l.fail(err)
	})
	if err != nil {
		return err
	}
	if err := l.ensureKeys(ctx, map[string]calendar.Type{calID: typ}); err != nil {
		return err
	}
	return l.withCache(func(db *cache.Cache) error {
		ck := l.r.calKeys(calID)
		if ck == nil {
			return errKeysDropped()
		}
		return fn(db, ck)
	})
}

// infos lists the groups' objects at their served names, skipping a group
// without UID (it cannot be named).
func infos(groups []cache.Listed, ck *calKeys, n nameIndex) []ObjectInfo {
	out := make([]ObjectInfo, 0, len(groups))
	for _, g := range groups {
		if g.UID != "" {
			out = append(out, ObjectInfo{UID: g.UID, Name: n.served(g.UID), ETag: objectETag(g, ck)})
		}
	}
	return out
}

// servedNames reads the calendar's aliases from v (names.go).
func servedNames(v cache.View) (nameIndex, error) {
	aliases, err := v.AliasesFor()
	if err != nil {
		return nameIndex{}, err
	}
	return newNames(aliases), nil
}

// liveIn reports whether a UID is one of groups'.
func liveIn(groups []cache.Listed) func(string) bool {
	live := make(map[string]bool, len(groups))
	for _, g := range groups {
		live[g.UID] = true
	}
	return func(uid string) bool { return live[uid] }
}

// Objects implements API: every object of the calendar with its ETag, and
// the sync token of that state, from one read transaction. Outside Serve
// it opens the cache read-only for the call; during Serve it reads Serve's
// handle. It calls Proton only to unlock the calendar's keys the first time.
func (l *Local) Objects(ctx context.Context, calID string) ([]ObjectInfo, string, error) {
	var out []ObjectInfo
	var token string
	err := l.calendarRead(ctx, calID, func(db *cache.Cache, ck *calKeys) error {
		err := db.Snapshot(ctx, calID, func(v cache.View) error {
			groups, err := v.Listing()
			if err != nil {
				return err
			}
			n, err := servedNames(v)
			if err != nil {
				return err
			}
			out, token = infos(groups, ck, n), syncToken(v.Epoch(), position(v), ck.gen)
			l.r.keepDuplicates(calID, liveIn(groups))
			return nil
		})
		return l.cacheFail(err)
	})
	if err != nil {
		return nil, "", err
	}
	return out, token, nil
}

// ObjectsByUID implements API: the rendered objects of uids, in their
// order, at their served names, Missing for a UID with no live row.
// Rendered objects are kept in memory by ETag (LRU), so an unchanged object
// is decrypted once. Outside Serve it opens the cache read-only for the
// call; during Serve it reads Serve's handle. It calls Proton only to
// unlock the calendar's keys the first time.
func (l *Local) ObjectsByUID(ctx context.Context, calID string, uids []string) ([]Object, error) {
	return l.objects(ctx, calID, func(n nameIndex) ([]string, []string) {
		served := make([]string, len(uids))
		for i, uid := range uids {
			served[i] = n.served(uid)
		}
		return uids, served
	})
}

// ObjectsByName implements API: the rendered objects served at names, in
// their order (names.resolve), as ObjectsByUID renders them.
func (l *Local) ObjectsByName(ctx context.Context, calID string, names []string) ([]Object, error) {
	return l.objects(ctx, calID, func(n nameIndex) ([]string, []string) {
		uids := make([]string, len(names))
		for i, name := range names {
			uids[i] = n.resolve(name)
		}
		return uids, names
	})
}

// objects renders the objects pick chooses, from one read transaction:
// pick gets the calendar's served names and returns, per object, its UID
// ("" for none: Missing) and its name.
func (l *Local) objects(ctx context.Context, calID string, pick func(nameIndex) (uids, names []string)) ([]Object, error) {
	var uids, served []string
	var groups []*cache.Group
	err := l.calendarRead(ctx, calID, func(db *cache.Cache, _ *calKeys) error {
		err := db.Snapshot(ctx, calID, func(v cache.View) error {
			n, err := servedNames(v)
			if err != nil {
				return err
			}
			uids, served = pick(n)
			groups = make([]*cache.Group, len(uids))
			for i, uid := range uids {
				if uid == "" {
					continue
				}
				g, ok, err := v.Group(uid)
				if err != nil {
					return err
				}
				if ok {
					groups[i] = &g
				}
			}
			return nil
		})
		return l.cacheFail(err)
	})
	if err != nil {
		return nil, err
	}
	l.r.kmu.RLock()
	defer l.r.kmu.RUnlock()
	ks := l.r.ks
	if ks == nil || ks.cals[calID] == nil {
		return nil, errKeysDropped()
	}
	ck := ks.cals[calID]
	out := make([]Object, len(uids))
	for i, uid := range uids {
		g := groups[i]
		if g == nil {
			out[i] = Object{UID: uid, Name: served[i], Missing: true}
			continue
		}
		etag := objectETag(g.Listed(), ck)
		data, ok := l.r.lru.get(etag)
		if !ok {
			data = l.render(calID, *g, ks, ck)
			l.r.lru.put(etag, data)
		}
		out[i] = Object{UID: uid, Name: served[i], ETag: etag, Data: data}
	}
	return out, nil
}

// Changes implements API: the objects changed and the UIDs deleted since
// token, and the new token, from one read transaction; an empty token lists
// every object. A token is answered when it names this cache's life (epoch)
// and a position of this calendar's life: at or above its floor (ruling
// M3-R12) and its newest deletion recorded without a UID, at most where it
// stands now. Tombstones are never pruned, so no older bound applies
// (ruling M3-R9). A token of another generation (other render inputs, or
// another calendar) is answered with its deletions and every live object as
// changed, so the client fetches everything once (ruling M3-R11). Anything
// else is ErrInvalidSyncToken (a Usage error): the client must list in
// full. Outside Serve it opens the cache read-only for the call; during
// Serve it reads Serve's handle.
func (l *Local) Changes(ctx context.Context, calID, token string) (ChangeSet, error) {
	if l.closed.Load() {
		return ChangeSet{}, errClosed()
	}
	full := token == ""
	epoch, since, gen, ok := parseToken(token)
	if !full && !ok {
		return ChangeSet{}, invalidSyncToken()
	}
	out := ChangeSet{Changed: []ObjectInfo{}, Deleted: []ObjectInfo{}}
	err := l.calendarRead(ctx, calID, func(db *cache.Cache, ck *calKeys) error {
		err := db.Snapshot(ctx, calID, func(v cache.View) error {
			pos := position(v)
			if !full && (epoch != v.Epoch() || since < v.Floor() || since < v.UnnamedTombstoneSeq() || since > pos) {
				return invalidSyncToken()
			}
			var changed []cache.Listed
			var deleted []string
			var err error
			switch {
			case full:
				changed, err = v.Listing()
			case gen != ck.gen:
				if _, deleted, err = v.ChangedSince(since); err == nil {
					changed, err = v.Listing()
				}
			default:
				changed, deleted, err = v.ChangedSince(since)
			}
			if err != nil {
				return err
			}
			n, err := servedNames(v)
			if err != nil {
				return err
			}
			out.Changed = infos(changed, ck, n)
			for _, uid := range deleted {
				if uid != "" {
					// At the name it was served at: an alias outlives its
					// UID (ruling M4-R22).
					out.Deleted = append(out.Deleted, ObjectInfo{UID: uid, Name: n.served(uid)})
				}
			}
			if full {
				l.r.keepDuplicates(calID, liveIn(changed))
			}
			for _, uid := range deleted {
				l.r.noteDuplicates(calID, uid, 0)
			}
			out.Token = syncToken(v.Epoch(), pos, ck.gen)
			return nil
		})
		if errors.Is(err, ErrInvalidSyncToken) {
			return err
		}
		return l.cacheFail(err)
	})
	if err != nil {
		return ChangeSet{}, err
	}
	return out, nil
}
