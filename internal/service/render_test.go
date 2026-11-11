package service_test

import (
	"slices"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
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
	})

	t.Run("TestObjectETagChangesWithDefaultNotifications", func(t *testing.T) {
		before, _ := e.listing(t, work)
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
	})

	t.Run("TestKeysDroppedOnLogout", func(t *testing.T) {
		e.objects(t, work, "s@x") // keys and the rendered object are held
		if !e.svc.HasKeys() {
			t.Fatal("no keys held after a read")
		}
		if _, err := e.svc.Logout(t.Context()); err != nil {
			t.Fatal(err)
		}
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
}
