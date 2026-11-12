package service

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// settingsRefresh is how often Serve reads the calendars' settings again
// from their bootstraps (ruling M3-R10), so a change of the default
// notifications or a calendar key rotation reaches the rendering without a
// restart.
const settingsRefresh = 10 * time.Minute

// ServeOptions tune Serve: the interval between sync passes (0:
// DefaultWatchInterval) and a callback after each pass, as WatchSync, with
// the render counters (WatchPass.Render).
// Ready, when set, is called once Serve holds the sync lock, a ready login
// and its cache handle, before the first pass: pcb serve binds its listener
// then, so a refused start (logged out, a sync running) never binds it.
// Logger gets what the writes served meanwhile (PutObject, DeleteObject)
// have to say: refusals worth a look at warning level, normalisations at
// debug; IDs and fixed text only, never event content (nil discards it).
type ServeOptions struct {
	Interval time.Duration
	OnPass   func(WatchPass)
	Ready    func()
	Logger   *slog.Logger
}

// Serve runs the sync loop of serve mode until ctx ends (nil) or a person
// is needed (that error), as WatchSync does: it holds the sync lock, opens
// the cache once as its writer and syncs on that handle; meanwhile the read
// API reads the same handle (ruling M3-R6). After a pass that got through,
// it refreshes the calendars' settings when settingsRefresh has passed. It
// needs a ready login.
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
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	l.smu.Lock()
	l.served = &served{db: db, account: acc.ID, log: log}
	l.smu.Unlock()
	defer func() {
		l.smu.Lock()
		l.served = nil
		l.smu.Unlock()
		_ = db.Close()
	}()
	if o.Ready != nil {
		o.Ready()
	}
	var onPass func(WatchPass)
	if o.OnPass != nil {
		onPass = func(p WatchPass) {
			counts := l.RenderCounts()
			p.Render = &counts
			o.OnPass(p)
		}
	}
	return l.watch(ctx, SyncRequest{}, interval, onPass, func(ctx context.Context, req SyncRequest) (SyncResult, error) {
		res, err := l.syncPassOn(ctx, req, db, acc.ID)
		if err == nil || exitcode.From(err) == exitcode.Partial {
			if cals, cerr := db.Calendars(ctx); cerr == nil {
				live := make(map[string]calendar.Type, len(cals))
				for _, c := range cals {
					live[c.ID] = calendar.ParseType(c.Type)
				}
				l.refreshSettings(ctx, live)
			}
		}
		return res, err
	})
}

// refreshSettings reads again the bootstrap of every calendar whose keys
// are held and that the cache still has (live), once settingsRefresh has
// passed since they were last read (one Proton call per calendar); the keys
// of a calendar the cache no longer has are dropped. A calendar whose settings or key IDs changed,
// or whose keys failed and now unlock, gets its new keys: its ETags, CTag
// and token generation change, so clients fetch its objects again. A
// failure keeps what is held (a transient one is tried at the next pass).
func (l *Local) refreshSettings(ctx context.Context, live map[string]calendar.Type) {
	l.r.kmu.RLock()
	due := l.r.ks != nil && !l.o.Now().Before(l.r.refreshedAt.Add(settingsRefresh))
	l.r.kmu.RUnlock()
	if !due {
		return
	}
	l.r.load.Lock()
	defer l.r.load.Unlock()
	_ = l.withSession(ctx, func(ctx context.Context, c *api.Client, acc store.Account) error {
		gen, ks := l.r.acquire()
		defer l.r.release(ks)
		if ks == nil || ks.account != acc.ID {
			return nil
		}
		// ks.cals changes only under load, which is held.
		fresh := map[string]*calKeys{}
		var gone []string
		discard := func() {
			for _, ck := range fresh {
				ck.access.Close()
			}
		}
		for _, id := range slices.Sorted(maps.Keys(ks.cals)) {
			typ, ok := live[id]
			if !ok {
				gone = append(gone, id)
				continue
			}
			a, err := calendar.Unlock(ctx, c, ks.u, calendar.Calendar{ID: id, Type: typ})
			switch {
			case err == nil:
				ck := newCalKeys(id, a, ks.addrTags)
				if old := ks.cals[id]; old.access != nil && bytes.Equal(old.settings, ck.settings) {
					a.Close()
					continue
				}
				fresh[id] = ck
			case api.SessionWide(ctx, err):
				discard()
				return err
			}
		}
		l.r.kmu.Lock()
		defer l.r.kmu.Unlock()
		if l.closed.Load() || l.r.gen != gen {
			discard()
			return nil
		}
		for id, ck := range fresh {
			old := ks.cals[id]
			ks.cals[id] = ck
			old.access.Close() // no render holds it: they hold kmu for reading
		}
		l.r.noteGates(ks.account, fresh)
		for _, id := range gone {
			ks.cals[id].access.Close()
			delete(ks.cals, id)
		}
		l.r.refreshedAt = l.o.Now()
		return nil
	})
}
