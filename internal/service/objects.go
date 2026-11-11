package service

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// The read API for adapters (Collections, Objects, ObjectsByUID, Changes)
// reads the event cache; in serve mode the cache is Serve's own writer
// handle (WAL read transactions beside its writes, ruling M3-R6), otherwise
// a read-only handle opened for each call (CLI use). Its only network use is
// unlocking the keys, once per process (see ensureKeys).

const syncTokenPrefix = "urn:pcb:sync:1:"

// syncToken names the calendar's state seq of the cache's life epoch.
func syncToken(epoch string, seq int64) string {
	return syncTokenPrefix + epoch + ":" + strconv.FormatInt(seq, 10)
}

// parseToken reads a sync token; ok is false when it is not one of ours.
func parseToken(token string) (epoch string, seq int64, ok bool) {
	rest, found := strings.CutPrefix(token, syncTokenPrefix)
	if !found {
		return "", 0, false
	}
	epoch, digits, found := strings.Cut(rest, ":")
	if !found || epoch == "" || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", 0, false
	}
	seq, err := strconv.ParseInt(digits, 10, 64)
	return epoch, seq, err == nil
}

// served is the cache handle of a running Serve, and the account it is for.
type served struct {
	db      *cache.Cache
	account string
}

func notSynced() error { return mustAct("no events are cached yet", "run pcb sync") }

// readCache returns the cache to read and the function releasing it: Serve's
// handle while Serve runs (for the account it serves only), else a read-only
// handle opened for this call. db is nil when there is no cache for the
// logged account.
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

// syncedCalendar returns calendar id of the cache; ErrNoSuchCalendar when
// the cache has no such calendar or never synced it.
func syncedCalendar(ctx context.Context, db *cache.Cache, id string) (cache.Calendar, error) {
	cals, err := db.Calendars(ctx)
	if err != nil {
		return cache.Calendar{}, cacheError(err)
	}
	for _, c := range cals {
		if c.ID == id && !c.SyncedAt.IsZero() {
			return c, nil
		}
	}
	return cache.Calendar{}, noSuchCalendar()
}

// ensureKeys unlocks what the calendars ids need and is not unlocked yet:
// the address keys, then each calendar's keys from its bootstrap (one Proton
// call per calendar, the only network use of a read). A calendar whose keys
// do not unlock is kept as failed: its objects are placeholders (ruling
// M3-R4) and Status names the reason. A failure that is not the calendar's
// own (network, Proton unavailable, the session) is returned and nothing is
// kept, so the next read tries again.
func (l *Local) ensureKeys(ctx context.Context, ids []string) error {
	if l.r.has(ids) {
		return nil
	}
	l.r.load.Lock()
	defer l.r.load.Unlock()
	if l.r.has(ids) {
		return nil
	}
	return l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		return l.loadKeys(ctx, c, acc, ids)
	})
}

func (l *Local) loadKeys(ctx context.Context, c *api.Client, acc store.Account, ids []string) error {
	l.r.kmu.RLock()
	gen, cur := l.r.gen, l.r.ks
	l.r.kmu.RUnlock()
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
		a, err := calendar.Unlock(ctx, c, u, calendar.Calendar{ID: id})
		switch {
		case err == nil:
			fresh[id] = newCalKeys(a)
		case api.SessionWide(ctx, err):
			discard()
			return err
		default:
			fresh[id] = failedCalKeys(keysError(err))
		}
	}
	l.r.kmu.Lock()
	defer l.r.kmu.Unlock()
	if l.r.gen != gen { // a login or logout dropped the keys meanwhile
		discard()
		return blocked("the login changed during the read", "retry")
	}
	if cur == nil {
		if l.r.ks != nil {
			l.r.ks.close()
		}
		l.r.ks = newKeyState(acc.ID, u)
	}
	maps.Copy(l.r.ks.cals, fresh)
	return nil
}

// Collections implements API: every synced calendar of the cache with its
// CTag and sync token (no network once the keys are unlocked). It needs a
// cache (MustAct otherwise).
func (l *Local) Collections(ctx context.Context) ([]Collection, error) {
	if l.closed.Load() {
		return nil, errClosed()
	}
	db, release, err := l.readCache()
	if err != nil {
		return nil, err
	}
	defer release()
	if db == nil {
		return nil, notSynced()
	}
	cals, err := db.Calendars(ctx)
	if err != nil {
		return nil, l.fail(cacheError(err))
	}
	var synced []cache.Calendar
	var ids []string
	for _, c := range cals {
		if !c.SyncedAt.IsZero() {
			synced = append(synced, c)
			ids = append(ids, c.ID)
		}
	}
	if len(synced) == 0 {
		return nil, notSynced()
	}
	if err := l.ensureKeys(ctx, ids); err != nil {
		return nil, err
	}
	out := make([]Collection, 0, len(synced))
	for _, c := range synced {
		ck := l.r.calKeys(c.ID)
		if ck == nil {
			return nil, blocked("the login changed during the read", "retry")
		}
		var epoch string
		var seq int64
		if err := db.Snapshot(ctx, c.ID, func(v cache.View) error {
			epoch, seq = v.Epoch(), v.MaxSeq()
			return nil
		}); err != nil {
			return nil, l.fail(cacheError(err))
		}
		out = append(out, Collection{
			ID: c.ID, Name: c.Name, Color: c.Color, Description: c.Description,
			ReadOnly:  c.Type != calendar.Normal.String(),
			CTag:      epoch + ":" + strconv.FormatInt(seq, 10) + ":" + ck.hash,
			SyncToken: syncToken(epoch, seq),
		})
	}
	return out, nil
}

// calendarRead opens the cache for a read of calendar calID and unlocks its
// keys; release must be called when done.
func (l *Local) calendarRead(ctx context.Context, calID string) (*cache.Cache, func(), error) {
	if l.closed.Load() {
		return nil, nil, errClosed()
	}
	db, release, err := l.readCache()
	if err != nil {
		return nil, nil, err
	}
	if db == nil {
		release()
		return nil, nil, notSynced()
	}
	if _, err := syncedCalendar(ctx, db, calID); err != nil {
		release()
		return nil, nil, l.fail(err)
	}
	if err := l.ensureKeys(ctx, []string{calID}); err != nil {
		release()
		return nil, nil, err
	}
	return db, release, nil
}

// infos lists the groups' objects, skipping a group without UID (it cannot
// be named).
func infos(groups []cache.Group, ck *calKeys) []ObjectInfo {
	out := make([]ObjectInfo, 0, len(groups))
	for _, g := range groups {
		if g.UID != "" {
			out = append(out, ObjectInfo{UID: g.UID, ETag: objectETag(g, ck)})
		}
	}
	return out
}

// Objects implements API: every object of the calendar with its ETag, and
// the sync token of that state, from one read transaction.
func (l *Local) Objects(ctx context.Context, calID string) ([]ObjectInfo, string, error) {
	db, release, err := l.calendarRead(ctx, calID)
	if err != nil {
		return nil, "", err
	}
	defer release()
	ck := l.r.calKeys(calID)
	if ck == nil {
		return nil, "", blocked("the login changed during the read", "retry")
	}
	var out []ObjectInfo
	var token string
	err = db.Snapshot(ctx, calID, func(v cache.View) error {
		groups, err := v.Groups()
		if err != nil {
			return err
		}
		out, token = infos(groups, ck), syncToken(v.Epoch(), v.MaxSeq())
		return nil
	})
	if err != nil {
		return nil, "", l.fail(cacheError(err))
	}
	return out, token, nil
}

// ObjectsByUID implements API: the rendered objects of uids, in their
// order, Missing for a UID with no live row. Rendered objects are kept in
// memory by ETag (LRU), so an unchanged object is decrypted once.
func (l *Local) ObjectsByUID(ctx context.Context, calID string, uids []string) ([]Object, error) {
	db, release, err := l.calendarRead(ctx, calID)
	if err != nil {
		return nil, err
	}
	defer release()
	groups := make([]*cache.Group, len(uids))
	err = db.Snapshot(ctx, calID, func(v cache.View) error {
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
	if err != nil {
		return nil, l.fail(cacheError(err))
	}
	l.r.kmu.RLock()
	defer l.r.kmu.RUnlock()
	ks := l.r.ks
	if ks == nil || ks.cals[calID] == nil {
		return nil, blocked("the login changed during the read", "retry")
	}
	ck := ks.cals[calID]
	out := make([]Object, len(uids))
	for i, uid := range uids {
		g := groups[i]
		if g == nil {
			out[i] = Object{UID: uid, Missing: true}
			continue
		}
		etag := objectETag(*g, ck)
		data, ok := l.r.lru.get(etag)
		if !ok {
			data = l.render(*g, ks, ck)
			l.r.lru.put(etag, data)
		}
		out[i] = Object{UID: uid, ETag: etag, Data: data}
	}
	return out, nil
}

// Changes implements API: the objects changed and the UIDs deleted since
// token, and the new token, from one read transaction; an empty token lists
// every object. A token is valid when it names this cache's life (epoch) and
// a position the calendar has reached that every deletion since can be named
// from: at or above its newest deletion recorded without a UID. Tombstones
// are never pruned, so no older bound applies (MinTombstoneSeq is the oldest
// deletion, not a pruning mark). Anything else is ErrInvalidSyncToken (a
// Usage error): the client must list in full.
func (l *Local) Changes(ctx context.Context, calID, token string) (ChangeSet, error) {
	if l.closed.Load() {
		return ChangeSet{}, errClosed()
	}
	full := token == ""
	epoch, since, ok := parseToken(token)
	if !full && !ok {
		return ChangeSet{}, invalidSyncToken()
	}
	db, release, err := l.calendarRead(ctx, calID)
	if err != nil {
		return ChangeSet{}, err
	}
	defer release()
	ck := l.r.calKeys(calID)
	if ck == nil {
		return ChangeSet{}, blocked("the login changed during the read", "retry")
	}
	out := ChangeSet{Changed: []ObjectInfo{}, Deleted: []string{}}
	err = db.Snapshot(ctx, calID, func(v cache.View) error {
		if !full && (epoch != v.Epoch() || since < v.UnnamedTombstoneSeq() || since > v.MaxSeq()) {
			return invalidSyncToken()
		}
		var changed []cache.Group
		var deleted []string
		var err error
		if full {
			changed, err = v.Groups()
		} else {
			changed, deleted, err = v.ChangedSince(since)
		}
		if err != nil {
			return err
		}
		out.Changed = infos(changed, ck)
		for _, uid := range deleted {
			if uid != "" {
				out.Deleted = append(out.Deleted, uid)
			}
		}
		out.Token = syncToken(v.Epoch(), v.MaxSeq())
		return nil
	})
	if errors.Is(err, ErrInvalidSyncToken) {
		return ChangeSet{}, err
	}
	if err != nil {
		return ChangeSet{}, l.fail(cacheError(err))
	}
	return out, nil
}

// ServeOptions tune Serve: the interval between sync passes (0:
// DefaultWatchInterval) and a callback after each pass, as WatchSync.
type ServeOptions struct {
	Interval time.Duration
	OnPass   func(WatchPass)
}

// Serve runs the sync loop of serve mode until ctx ends (nil) or a person
// is needed (that error), as WatchSync does: it holds the sync lock, opens
// the cache once as its writer and syncs on that handle; meanwhile the read
// API reads the same handle (ruling M3-R6). It needs a ready login.
func (l *Local) Serve(ctx context.Context, o ServeOptions) error {
	if l.closed.Load() {
		return errClosed()
	}
	interval, err := watchInterval(o.Interval)
	if err != nil {
		return err
	}
	unlock, err := l.lockSync()
	if err != nil {
		return err
	}
	defer unlock()
	_, acc, err := l.stored()
	if err != nil {
		return err
	}
	db, err := cache.Open(cache.Path(l.o.Dirs.State), acc.ID)
	if err != nil {
		return l.fail(cacheError(err))
	}
	l.smu.Lock()
	l.served = &served{db: db, account: acc.ID}
	l.smu.Unlock()
	defer func() {
		l.smu.Lock()
		l.served = nil
		l.smu.Unlock()
		_ = db.Close()
	}()
	return l.watch(ctx, SyncRequest{}, interval, o.OnPass, func(ctx context.Context, req SyncRequest) (SyncResult, error) {
		return l.syncPassOn(ctx, req, db, acc.ID)
	})
}
