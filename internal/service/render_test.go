package service_test

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

func (e *env) changes(t *testing.T, cal, token string) service.ChangeSet {
	t.Helper()
	cs, err := e.svc.Changes(t.Context(), cal, token)
	if err != nil {
		t.Fatalf("Changes(%q): %v", token, err)
	}
	return cs
}

func changedUIDs(cs service.ChangeSet) []string {
	var out []string
	for _, c := range cs.Changed {
		out = append(out, c.UID)
	}
	return out
}

func (e *env) collection(t *testing.T, id string) service.Collection {
	t.Helper()
	list, err := e.svc.Collections(t.Context())
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	for _, c := range list {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no collection %s in %+v", id, list)
	return service.Collection{}
}

// TestObjectChanges edits one rig step by step: each subtest builds on the
// state the previous one left.
func TestObjectChanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(work, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Notifications: []protontest.Notification{}})
	rid := base + day
	e.f.AddEvent(work, protontest.Event{UID: "s@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY",
		Card: card("s@x", "Standup", base, "RRULE:FREQ=DAILY")})
	exc := e.f.AddEvent(work, protontest.Event{UID: "s@x", Start: rid, End: rid + 3600, RecurrenceID: &rid,
		Card: card("s@x", "Standup moved", rid, "RECURRENCE-ID:"+stamp(rid))})
	oldDup := e.f.AddEvent(work, protontest.Event{UID: "d@x", Start: base, End: base + 60, Card: card("d@x", "Old", base)})
	e.f.AddEvent(work, protontest.Event{UID: "d@x", Start: base, End: base + 60, Card: card("d@x", "New", base)})
	e.f.AddEvent(work, protontest.Event{UID: "e@x", Start: base, End: base + 60, Card: card("e@x", "E1", base)})
	e.f.AddEvent(work, protontest.Event{UID: "e@x", Start: base, End: base + 60, Card: card("e@x", "E2", base)})
	e.f.AddEvent(work, protontest.Event{UID: "f@x", Start: base, End: base + 60, Card: card("f@x", "F1", base)})
	e.f.AddEvent(work, protontest.Event{UID: "f@x", Start: base, End: base + 60, Card: card("f@x", "F2", base)})
	// Other's first listed key opens with no passphrase (until reactivated).
	other := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Other", ExtraLockedKey: true})
	e.f.AddEvent(other, protontest.Event{UID: "o1@x", Start: base, End: base + 60})
	e.login(t, e.req())
	e.sync(t, false)
	etags0, tok0 := e.listing(t, work)
	top := t // a service opened in a subtest lives for the whole test

	var tok1 string
	t.Run("TestObjectETagChangesWithAnyRow", func(t *testing.T) {
		e.f.UpdateEvent(work, exc, protontest.Event{UID: "s@x", Start: rid, End: rid + 3600, RecurrenceID: &rid,
			Card: card("s@x", "Standup cancelled", rid, "RECURRENCE-ID:"+stamp(rid))})
		e.sync(t, false)
		etags, tok := e.listing(t, work)
		if etags["s@x"] == etags0["s@x"] || etags["a@x"] != etags0["a@x"] || tok == tok0 {
			t.Errorf("after an exception edit: ETags %v (were %v), token %q (was %q); want only s@x's ETag and the token changed", etags, etags0, tok, tok0)
		}
		o := e.objects(t, work, "s@x")["s@x"]
		contains(t, "s@x", o.Data, "SUMMARY:Standup\r\n", "SUMMARY:Standup cancelled\r\n")
		if o.ETag != etags["s@x"] {
			t.Errorf("object ETag %q, listing %q", o.ETag, etags["s@x"])
		}
		cs := e.changes(t, work, tok0)
		if !slices.Equal(changedUIDs(cs), []string{"s@x"}) || len(cs.Deleted) != 0 || cs.Token != tok || cs.Changed[0].ETag != etags["s@x"] {
			t.Errorf("Changes(before the edit) = %+v, want s@x changed and token %q", cs, tok)
		}
		tok1 = tok
	})

	// Ruling M3-R11: the calendar ID is in the token's generation, so a
	// token of another calendar never reads as a delta.
	t.Run("TokenOfAnotherCalendarListsInFull", func(t *testing.T) {
		e.f.AddEvent(other, protontest.Event{UID: "o2@x", Start: base, End: base + 60})
		e.sync(t, false) // other now stands past work's token, o1@x below it
		cs := e.changes(t, other, tok1)
		if !slices.Equal(changedUIDs(cs), []string{"o1@x", "o2@x"}) {
			t.Errorf("Changes(other, work's token) = %+v, want every object of other", cs)
		}
	})

	// Ruling M3-R12: a calendar dropped and created again under its ID
	// refuses the tokens of its former life (whose deletions it lost).
	t.Run("RecreatedCalendarRefusesOldTokens", func(t *testing.T) {
		_, tokO := e.listing(t, other)
		db, err := cache.Open(cache.Path(e.dirs.State), e.f.UserID(e.f.Username))
		if err != nil {
			t.Fatal(err)
		}
		err = db.DropCalendar(t.Context(), other)
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		e.sync(t, false) // other is listed again and created anew
		_, err = e.svc.Changes(t.Context(), other, tokO)
		if !errors.Is(err, service.ErrInvalidSyncToken) {
			t.Errorf("Changes(token of the former calendar) = %v, want ErrInvalidSyncToken", err)
		}
		_, tok := e.listing(t, other)
		if cs := e.changes(t, other, tok); len(cs.Changed)+len(cs.Deleted) != 0 {
			t.Errorf("Changes(token of the new calendar) = %+v, want nothing", cs)
		}
	})

	t.Run("TestChangesSinceToken", func(t *testing.T) {
		// Deleting an exception changes its UID's object; it is no deletion.
		e.f.DeleteEvent(work, exc)
		e.sync(t, false)
		cs := e.changes(t, work, tok1)
		if !slices.Equal(changedUIDs(cs), []string{"s@x"}) || len(cs.Deleted) != 0 {
			t.Errorf("after deleting the exception: %+v, want s@x changed, nothing deleted", cs)
		}
		lacks(t, "s@x", e.objects(t, work, "s@x")["s@x"].Data, "Standup cancelled")
		tok2 := cs.Token
		// Deleting the last row of a UID is a deletion, reported once.
		var a string
		for _, ev := range e.events(t, service.EventsRequest{Calendar: work}) {
			if ev.UID == "a@x" {
				a = ev.ID
			}
		}
		e.f.DeleteEvent(work, a)
		e.f.AddEvent(work, protontest.Event{UID: "n@x", Start: base, End: base + 60})
		e.sync(t, false)
		cs = e.changes(t, work, tok2)
		if !slices.Equal(changedUIDs(cs), []string{"n@x"}) || !slices.Equal(cs.Deleted, []string{"a@x"}) {
			t.Errorf("after deleting a@x and adding n@x: %+v", cs)
		}
		if again := e.changes(t, work, cs.Token); len(again.Changed)+len(again.Deleted) != 0 || again.Token != cs.Token {
			t.Errorf("Changes(latest token) = %+v, want nothing", again)
		}
		// An older token (before any deletion) stays valid and sees it all.
		old := e.changes(t, work, tok1)
		if !slices.Equal(changedUIDs(old), []string{"n@x", "s@x"}) || !slices.Equal(old.Deleted, []string{"a@x"}) || old.Token != cs.Token {
			t.Errorf("Changes(older token) = %+v", old)
		}
		if got := e.objects(t, work, "a@x")["a@x"]; !got.Missing {
			t.Errorf("deleted a@x = %+v, want Missing", got)
		}
		// A deletion recorded without its UID (early M2 files) cannot be
		// named: a token from before it is refused, a later one is not.
		raw, err := sql.Open("sqlite", cache.Path(e.dirs.State))
		if err != nil {
			t.Fatal(err)
		}
		_, err = raw.ExecContext(t.Context(), `UPDATE tombstones SET uid = ''`)
		_ = raw.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Changes(t.Context(), work, tok1); !errors.Is(err, service.ErrInvalidSyncToken) {
			t.Errorf("Changes(token before an unnamed deletion) = %v, want ErrInvalidSyncToken", err)
		}
		if _, err := e.svc.Changes(t.Context(), work, cs.Token); err != nil {
			t.Errorf("Changes(token after the unnamed deletion) = %v", err)
		}
	})

	t.Run("TestObjectETagChangesWithDefaultNotifications", func(t *testing.T) {
		before, beforeTok := e.listing(t, work)
		beforeCTag := e.collection(t, work).CTag
		e.f.SetDefaultNotifications(work, []protontest.Notification{{Type: 1, Trigger: "-PT30M"}}, nil)
		// The settings are read with the keys, once per process: a new one.
		e.svc = e.open(top)
		after, _ := e.listing(t, work)
		afterCTag := e.collection(t, work).CTag
		if after["s@x"] == before["s@x"] || afterCTag == beforeCTag {
			t.Errorf("same rows, new default notifications: ETag %q -> %q, CTag %q -> %q; want both changed", before["s@x"], after["s@x"], beforeCTag, afterCTag)
		}
		contains(t, "s@x", e.objects(t, work, "s@x")["s@x"].Data, "TRIGGER:-PT30M\r\n")
		if cs := e.changes(t, work, beforeTok); len(cs.Changed) != len(after) {
			t.Errorf("Changes(token before the change) = %+v, want all %d objects", cs, len(after))
		}
	})

	t.Run("KeyRotationChangesETagsAndGeneration", func(t *testing.T) {
		before, beforeTok := e.listing(t, work)
		e.f.RotateCalendarKey(work)
		e.svc = e.open(top) // a restart reads the bootstrap again
		after, tok := e.listing(t, work)
		if after["s@x"] == before["s@x"] || tok == beforeTok {
			t.Errorf("after a key rotation: ETag %q -> %q, token %q -> %q; want both changed", before["s@x"], after["s@x"], beforeTok, tok)
		}
		if cs := e.changes(t, work, beforeTok); len(cs.Changed) != len(after) {
			t.Errorf("Changes(token before the rotation) = %+v, want all %d objects", cs, len(after))
		}
		contains(t, "s@x", e.objects(t, work, "s@x")["s@x"].Data, "SUMMARY:Standup\r\n")
	})

	// Ruling M3-R16: a key that opens now under the same ID (reactivation)
	// changes the render inputs.
	t.Run("KeyReactivationChangesETagsAndGeneration", func(t *testing.T) {
		before, beforeTok := e.listing(t, other)
		e.f.ReactivateCalendarKey(other)
		e.svc = e.open(top) // a restart reads the bootstrap again
		after, tok := e.listing(t, other)
		if after["o1@x"] == before["o1@x"] || tok == beforeTok {
			t.Errorf("after a key reactivation: ETag %q -> %q, token %q -> %q; want both changed", before["o1@x"], after["o1@x"], beforeTok, tok)
		}
	})

	t.Run("DuplicateCountFollowsTheRows", func(t *testing.T) {
		contains(t, "d@x", e.objects(t, work, "d@x")["d@x"].Data, "SUMMARY:New\r\n")
		e.objects(t, work, "e@x", "f@x")
		if n := e.svc.RenderCounts().DuplicateMasters; n != 3 {
			t.Errorf("DuplicateMasters = %d, want 3 (d@x, e@x, f@x)", n)
		}
		e.f.DeleteEvent(work, oldDup)
		e.sync(t, false)
		e.objects(t, work, "d@x")
		if n := e.svc.RenderCounts().DuplicateMasters; n != 2 {
			t.Errorf("DuplicateMasters after d@x's duplicate is gone = %d, want 2", n)
		}
		// A UID deleted whole stops counting once a read sees it gone: a
		// listing, or a delta (sync-collection).
		deleteUID := func(uid string) {
			t.Helper()
			for _, ev := range e.events(t, service.EventsRequest{Calendar: work}) {
				if ev.UID == uid {
					e.f.DeleteEvent(work, ev.ID)
				}
			}
			e.sync(t, false)
		}
		deleteUID("e@x")
		_, tok := e.listing(t, work)
		if n := e.svc.RenderCounts().DuplicateMasters; n != 1 {
			t.Errorf("DuplicateMasters after a listing without e@x = %d, want 1 (f@x)", n)
		}
		deleteUID("f@x")
		if cs := e.changes(t, work, tok); !slices.Equal(cs.Deleted, []string{"f@x"}) {
			t.Fatalf("Changes = %+v, want f@x deleted", cs)
		}
		if n := e.svc.RenderCounts().DuplicateMasters; n != 0 {
			t.Errorf("DuplicateMasters after a delta named f@x deleted = %d, want 0", n)
		}
	})

	t.Run("TestKeysDroppedOnLogout", func(t *testing.T) {
		e.objects(t, work, "s@x") // keys and the rendered object are held
		if !e.svc.HasKeys() {
			t.Fatal("no keys held after a read")
		}
		// Reads running while the logout drops the keys (go test -race): they
		// answer or fail, never use cleared keys.
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 5 {
					_, _ = e.svc.ObjectsByUID(t.Context(), work, []string{"s@x", "d@x"})
				}
			})
		}
		if _, err := e.svc.Logout(t.Context()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if e.svc.HasKeys() {
			t.Error("keys held after logout")
		}
		_, err := e.svc.ObjectsByUID(t.Context(), work, []string{"s@x"})
		_ = wantExit(t, err, exitcode.MustAct)
		e.clk.advance(61 * time.Second) // the login guard
		e.login(t, e.req())
		e.sync(t, false)
		boots, decrypts := e.f.Calls("GET", "/calendar/v2/"+work+"/bootstrap"), e.svc.Decrypts()
		contains(t, "s@x", e.objects(t, work, "s@x")["s@x"].Data, "SUMMARY:Standup\r\n")
		if n := e.f.Calls("GET", "/calendar/v2/"+work+"/bootstrap") - boots; n != 1 {
			t.Errorf("the read after a new login fetched the bootstrap %d times, want 1", n)
		}
		if e.svc.Decrypts() == decrypts {
			t.Error("the read after a new login decrypted nothing: the rendered objects outlived the logout")
		}
	})

	t.Run("CloseDropsKeysAndALoadRacingItInstallsNothing", func(t *testing.T) {
		gate := &bootstrapGate{entered: make(chan struct{}), release: make(chan struct{})}
		svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
			HTTP: &http.Client{Transport: gate}})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := svc.ObjectsByUID(t.Context(), work, []string{"s@x"})
			done <- err
		}()
		<-gate.entered // the load is fetching the bootstrap
		if err := svc.Close(); err != nil {
			t.Fatal(err)
		}
		close(gate.release)
		_ = wantExit(t, <-done, exitcode.Blocked)
		if svc.HasKeys() {
			t.Error("a load that raced Close installed its keys")
		}
		// Close completing before a load reads the generation: the load
		// must still install nothing.
		late, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep})
		if err != nil {
			t.Fatal(err)
		}
		late.BeforeKeyLoad(func() { _ = late.Close() })
		_, err = late.ObjectsByUID(t.Context(), work, []string{"s@x"})
		_ = wantExit(t, err, exitcode.Blocked)
		if late.HasKeys() {
			t.Error("a load starting after Close installed its keys")
		}
		e.objects(t, work, "s@x")
		if err := e.svc.Close(); err != nil {
			t.Fatal(err)
		}
		if e.svc.HasKeys() {
			t.Error("keys held after Close")
		}
	})
}

// bootstrapGate holds the first calendar bootstrap request until release.
type bootstrapGate struct {
	once             sync.Once
	entered, release chan struct{}
}

func (g *bootstrapGate) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/bootstrap") {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
	return http.DefaultTransport.RoundTrip(r)
}
