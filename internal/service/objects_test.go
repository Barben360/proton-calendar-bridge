package service_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

var (
	ctagRE  = regexp.MustCompile(`^([0-9a-f]{32}):([0-9]+):[0-9a-f]{16}$`)
	tokenRE = regexp.MustCompile(`^urn:pcb:sync:1:([0-9a-f]{32}):([0-9]+):([0-9a-f]{8})$`)
	etagRE  = regexp.MustCompile(`^"v1-[0-9a-f]{32}"$`)
)

// stamp is unix as an iCalendar UTC date-time.
func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format("20060102T150405Z") }

// richCard has a property of every part kind, unknown and X-PM-* ones
// included.
const richCard = "UID:rich@x\r\nDTSTAMP:20270115T080000Z\r\nDTSTART:20270115T080000Z\r\nDTEND:20270115T090000Z\r\n" +
	"SUMMARY:Lunch with Zoé\r\nX-UNKNOWN-PROP;X-PARAM=1:keep me\r\nX-PM-FOO:bar\r\nCOMMENT:note\r\nSTATUS:CONFIRMED\r\n" +
	"ATTENDEE;CN=Bob;PARTSTAT=ACCEPTED;X-PM-TOKEN=tok1:mailto:bob@example.org\r\n"

func card(uid, summary string, start int64, extra ...string) string {
	lines := []string{"UID:" + uid, "DTSTAMP:20270115T080000Z", "DTSTART:" + stamp(start), "DTEND:" + stamp(start+3600), "SUMMARY:" + summary}
	return strings.Join(append(lines, extra...), "\r\n")
}

func (e *env) objects(t *testing.T, cal string, uids ...string) map[string]service.Object {
	t.Helper()
	objs, err := e.svc.ObjectsByUID(t.Context(), cal, uids)
	if err != nil {
		t.Fatalf("ObjectsByUID: %v", err)
	}
	if len(objs) != len(uids) {
		t.Fatalf("ObjectsByUID returned %d objects for %d UIDs", len(objs), len(uids))
	}
	out := map[string]service.Object{}
	for i, o := range objs {
		if o.UID != uids[i] {
			t.Fatalf("object %d is %q, want %q (request order)", i, o.UID, uids[i])
		}
		out[o.UID] = o
	}
	return out
}

func (e *env) listing(t *testing.T, cal string) (map[string]string, string) {
	t.Helper()
	infos, token, err := e.svc.Objects(t.Context(), cal)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	etags := map[string]string{}
	for _, o := range infos {
		if !etagRE.MatchString(o.ETag) {
			t.Errorf("ETag of %s = %q", o.UID, o.ETag)
		}
		etags[o.UID] = o.ETag
	}
	if !tokenRE.MatchString(token) {
		t.Errorf("token = %q", token)
	}
	return etags, token
}

func contains(t *testing.T, what string, data []byte, want ...string) {
	t.Helper()
	for _, w := range want {
		if !bytes.Contains(data, []byte(w)) {
			t.Errorf("%s lacks %q:\n%s", what, w, data)
		}
	}
}

func lacks(t *testing.T, what string, data []byte, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if bytes.Contains(data, []byte(b)) {
			t.Errorf("%s holds %q:\n%s", what, b, data)
		}
	}
}

// TestObjectReads shares one logged-in rig: the subtests run in order, and
// only read (the decrypt and request counters they compare are the rig's).
func TestObjectReads(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work", Color: "#3CBB3A", Description: "Team"})
	hol := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
	locked := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Locked", BrokenPassphrase: true})
	e.f.AddEvent(work, protontest.Event{UID: "rich@x", Start: base, End: base + 3600, Card: richCard, Color: "#FF0000"})
	rid := base + day
	e.f.AddEvent(work, protontest.Event{UID: "series@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY",
		Card: card("series@x", "Standup", base, "RRULE:FREQ=DAILY")})
	e.f.AddEvent(work, protontest.Event{UID: "series@x", Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid,
		Card: card("series@x", "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid)), Notifications: []protontest.Notification{}})
	// An exception that does not decrypt is a placeholder that keeps its
	// RECURRENCE-ID; an exception whose master is gone is served alone.
	rid2 := base + 2*day
	e.f.AddEvent(work, protontest.Event{UID: "series@x", Start: rid2, End: rid2 + 3600, RecurrenceID: &rid2,
		BrokenPart: protontest.SharedEncryptedPart, Card: card("series@x", "Hidden change", rid2, "RECURRENCE-ID:"+stamp(rid2))})
	e.f.AddEvent(work, protontest.Event{UID: "orphan@x", Start: rid, End: rid + 3600, RecurrenceID: &rid,
		Card: card("orphan@x", "Orphan", rid, "RECURRENCE-ID:"+stamp(rid))})
	e.f.AddEvent(work, protontest.Event{UID: "allday@x", Start: base - 8*3600, End: base - 8*3600 + day, FullDay: true})
	e.f.AddEvent(work, protontest.Event{UID: "broken@x", Start: base, End: base + 3600, BrokenPart: protontest.SharedEncryptedPart,
		Card: card("broken@x", "Secret plan", base)})
	// Duplicate masters (M3-R3): the most recently edited one wins, whatever
	// the row order.
	old := e.f.AddEvent(work, protontest.Event{UID: "dup@x", Start: base, End: base + 60, Card: card("dup@x", "Old", base)})
	e.f.AddEvent(work, protontest.Event{UID: "dup@x", Start: base, End: base + 60, Card: card("dup@x", "Mid", base)})
	e.f.UpdateEvent(work, old, protontest.Event{UID: "dup@x", Start: base, End: base + 60, Card: card("dup@x", "Newest", base)})
	e.f.AddEvent(locked, protontest.Event{UID: "locked@x", Start: base - 8*3600, End: base - 8*3600 + 2*day, FullDay: true})
	// A recurring event that does not decrypt: its placeholder master keeps
	// the row's plaintext RRULE (final review, minor 3).
	e.f.AddEvent(locked, protontest.Event{UID: "lockedseries@x", Start: base, End: base + 3600, RRule: "FREQ=WEEKLY;BYDAY=FR"})
	e.f.AddEvent(locked, protontest.Event{UID: "lockedseries@x", Start: rid + 3600, End: rid + 7200, RecurrenceID: &rid})
	// Signatures that do not verify (ruling M3-R33): a whole event, and one
	// exception of a series whose master verifies.
	e.f.AddEvent(hol, protontest.Event{UID: "forged@x", Start: base, End: base + 3600, ForeignKey: true, Card: card("forged@x", "Forged", base)})
	// Ruling M3-R36: an own shared part sent unsigned marks the event; a
	// part authored by an address not ours (unverifiable in v1), or an own
	// attendees part sent unsigned, does not.
	e.f.AddEvent(hol, protontest.Event{UID: "unsigned@x", Start: base, End: base + 3600, Unsigned: protontest.SharedSignedPart,
		Card: card("unsigned@x", "Unsigned", base)})
	e.f.AddEvent(hol, protontest.Event{UID: "stranger@x", Start: base, End: base + 3600, Author: "stranger@example.org", ForeignKey: true,
		Card: card("stranger@x", "Invitation", base)})
	e.f.AddEvent(hol, protontest.Event{UID: "attendees@x", Start: base, End: base + 3600, Unsigned: protontest.AttendeesPart,
		Card: card("attendees@x", "Meeting", base, "ATTENDEE;CN=Bob;PARTSTAT=ACCEPTED;X-PM-TOKEN=tok1:mailto:bob@example.org")})
	e.f.AddEvent(hol, protontest.Event{UID: "mixed@x", Start: base, End: base + 3600, RRule: "FREQ=DAILY",
		Card: card("mixed@x", "Mixed", base, "RRULE:FREQ=DAILY")})
	e.f.AddEvent(hol, protontest.Event{UID: "mixed@x", Start: rid, End: rid + 3600, RecurrenceID: &rid, ForeignKey: true,
		Card: card("mixed@x", "Mixed moved", rid, "RECURRENCE-ID:"+stamp(rid))})
	e.login(t, e.req())

	t.Run("NeverSyncedIsMustAct", func(t *testing.T) {
		calls := e.f.Calls("", "/")
		_, err := e.svc.Collections(t.Context())
		_ = wantExit(t, err, exitcode.MustAct)
		if n := e.f.Calls("", "/") - calls; n != 0 {
			t.Errorf("Collections without a cache made %d requests", n)
		}
	})
	e.sync(t, false)

	t.Run("TransientKeyFailureIsAnErrorNotAPlaceholder", func(t *testing.T) {
		for range 3 { // the client retries a 503 twice
			e.f.FailNext("GET", "/calendar/v2/"+work+"/bootstrap", 503, 0, 0)
		}
		_, err := e.svc.ObjectsByUID(t.Context(), work, []string{"rich@x"})
		_ = wantExit(t, err, exitcode.Blocked)
		// Not kept: the next read unlocks the keys and decrypts.
		contains(t, "rich@x", e.objects(t, work, "rich@x")["rich@x"].Data, "SUMMARY:Lunch with Zoé")
	})

	var colls map[string]service.Collection
	t.Run("Collections", func(t *testing.T) {
		list, err := e.svc.Collections(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		colls = map[string]service.Collection{}
		for _, c := range list {
			colls[c.ID] = c
		}
		w, h, l := colls[work], colls[hol], colls[locked]
		if len(list) != 3 || w.Name != "Work" || w.Color != "#3CBB3A" || w.Description != "Team" || w.ReadOnly ||
			h.Name != "Holidays" || !h.ReadOnly || l.Name != "Locked" {
			t.Fatalf("collections = %+v", list)
		}
		for _, c := range list {
			m, tm := ctagRE.FindStringSubmatch(c.CTag), tokenRE.FindStringSubmatch(c.SyncToken)
			if m == nil || tm == nil || m[1] != tm[1] || m[2] != tm[2] {
				t.Errorf("%s: CTag %q, sync token %q: want one epoch and seq", c.Name, c.CTag, c.SyncToken)
			}
		}
		if h.CTag == w.CTag {
			t.Errorf("two calendars share the CTag %q", w.CTag)
		}
		// Keys are kept: a second listing calls nobody.
		calls := e.f.Calls("", "/")
		if again, err := e.svc.Collections(t.Context()); err != nil || len(again) != 3 || again[0] != list[0] {
			t.Errorf("second Collections = %+v, %v", again, err)
		}
		if n := e.f.Calls("", "/") - calls; n != 0 {
			t.Errorf("a second Collections made %d requests", n)
		}
	})

	t.Run("TestObjectsRenderDecryptedEvents", func(t *testing.T) {
		etags, token := e.listing(t, work)
		if token != colls[work].SyncToken {
			t.Errorf("Objects token %q, collection's %q", token, colls[work].SyncToken)
		}
		if len(etags) != 6 {
			t.Errorf("listing = %v, want the 6 UIDs", etags)
		}
		got := e.objects(t, work, "rich@x", "series@x", "allday@x", "orphan@x", "nobody@x")
		rich := got["rich@x"].Data
		contains(t, "rich@x", rich, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//pcb//proton-calendar-bridge//EN\r\n",
			"SUMMARY:Lunch with Zoé\r\n", "X-UNKNOWN-PROP;X-PARAM=1:keep me\r\n", "X-PM-FOO:bar\r\n", "COMMENT:note\r\n",
			"STATUS:CONFIRMED\r\n", "COLOR:#FF0000\r\n", "PARTSTAT=ACCEPTED", "mailto:bob@example.org",
			// Notifications null: the calendar's part-day default.
			"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT1H\r\n")
		lacks(t, "rich@x", rich, "X-PM-TOKEN", "X-PM-SESSION-KEY", "X-PCB-PLACEHOLDER")
		if !bytes.HasSuffix(rich, []byte("END:VCALENDAR\r\n")) {
			t.Errorf("rich@x does not end with END:VCALENDAR CRLF")
		}
		series := got["series@x"].Data
		if bytes.Count(series, []byte("BEGIN:VEVENT")) != 3 || bytes.Index(series, []byte("SUMMARY:Standup\r\n")) > bytes.Index(series, []byte("SUMMARY:Standup moved")) {
			t.Errorf("series@x: want the master then its exceptions:\n%s", series)
		}
		contains(t, "series@x", series, "RECURRENCE-ID:"+stamp(rid), "RRULE:FREQ=DAILY",
			// The undecryptable exception: a placeholder still bound to its occurrence.
			"RECURRENCE-ID:"+stamp(rid2)+"\r\nSUMMARY:(pcb could not decrypt this event)\r\nX-PCB-PLACEHOLDER:1\r\n")
		lacks(t, "series@x", series, "Hidden change")
		orphan := got["orphan@x"].Data
		contains(t, "orphan@x", orphan, "RECURRENCE-ID:"+stamp(rid)+"\r\n", "SUMMARY:Orphan\r\n")
		if bytes.Count(orphan, []byte("BEGIN:VEVENT")) != 1 {
			t.Errorf("orphan@x: want its one exception, served alone:\n%s", orphan)
		}
		if bytes.Count(series, []byte("RRULE")) != 1 || bytes.Count(series, []byte("BEGIN:VALARM")) != 1 {
			t.Errorf("series@x: want one RRULE (the master's) and one alarm (the exception has none):\n%s", series)
		}
		contains(t, "allday@x", got["allday@x"].Data, "TRIGGER:-PT15H") // the full-day default
		for uid, o := range got {
			if uid == "nobody@x" {
				if !o.Missing || o.Data != nil || o.ETag != "" {
					t.Errorf("unknown UID = %+v, want Missing", o)
				}
				continue
			}
			if o.Missing || o.ETag != etags[uid] {
				t.Errorf("%s: ETag %q, listing %q, missing %v", uid, o.ETag, etags[uid], o.Missing)
			}
		}
	})

	t.Run("TestUndecryptableRowIsAPlaceholder", func(t *testing.T) {
		etags, _ := e.listing(t, work)
		got := e.objects(t, work, "broken@x", "rich@x")
		b := got["broken@x"]
		contains(t, "broken@x", b.Data, "BEGIN:VEVENT\r\nUID:broken@x\r\n", "DTSTART:"+stamp(base)+"\r\n", "DTEND:"+stamp(base+3600)+"\r\n",
			"SUMMARY:(pcb could not decrypt this event)\r\n", "X-PCB-PLACEHOLDER:1\r\n")
		lacks(t, "broken@x", b.Data, "Secret plan")
		comps, err := ical.Parse(b.Data)
		if err != nil || len(comps) != 1 || len(comps[0].Subs("VEVENT")) != 1 {
			t.Errorf("the placeholder does not parse as one VCALENDAR with one VEVENT: %v", err)
		}
		if b.ETag != etags["broken@x"] || b.Missing {
			t.Errorf("placeholder ETag %q, listing %q", b.ETag, etags["broken@x"])
		}
		contains(t, "rich@x beside it", got["rich@x"].Data, "SUMMARY:Lunch with Zoé")
		// A calendar whose keys fail: every object is a placeholder, with
		// the row's all-day dates.
		l := e.objects(t, locked, "locked@x")["locked@x"].Data
		contains(t, "locked@x", l, "DTSTART;VALUE=DATE:20270115\r\n", "DTEND;VALUE=DATE:20270117\r\n", "X-PCB-PLACEHOLDER:1\r\n")
		rc := e.svc.RenderCounts()
		if rc.Undecryptable != 3 {
			t.Errorf("Undecryptable = %d, want 3 (broken@x, series@x's second exception and locked@x)", rc.Undecryptable)
		}
		if !slices.Equal(rc.KeysErrors, []string{locked}) {
			t.Errorf("KeysErrors = %v, want the Locked calendar's ID only", rc.KeysErrors)
		}
	})

	t.Run("PlaceholderMasterKeepsTheRRule", func(t *testing.T) {
		d := string(e.objects(t, locked, "lockedseries@x")["lockedseries@x"].Data)
		vevents := strings.Split(d, "BEGIN:VEVENT")
		if len(vevents) != 3 || !strings.Contains(vevents[1], "\r\nRRULE:FREQ=WEEKLY;BYDAY=FR\r\n") || strings.Contains(vevents[2], "RRULE") ||
			strings.Count(d, "X-PCB-PLACEHOLDER:1\r\n") != 2 || !strings.Contains(vevents[2], "RECURRENCE-ID:"+stamp(rid)+"\r\n") {
			t.Errorf("lockedseries@x: want a placeholder master with the row's RRULE, then its placeholder exception without:\n%s", d)
		}
	})

	t.Run("TestUnverifiedSignatureIsMarked", func(t *testing.T) {
		got := e.objects(t, hol, "forged@x", "mixed@x")
		forged := got["forged@x"].Data
		// Marked, never dropped: the event is still served in full.
		contains(t, "forged@x", forged, "SUMMARY:Forged\r\n", "X-PCB-INTEGRITY:unverified\r\n")
		mixed := string(got["mixed@x"].Data)
		vevents := strings.Split(mixed, "BEGIN:VEVENT")
		if len(vevents) != 3 || strings.Contains(vevents[1], "X-PCB-INTEGRITY") || !strings.Contains(vevents[2], "X-PCB-INTEGRITY:unverified\r\n") {
			t.Errorf("mixed@x: want the marker on the exception's VEVENT only:\n%s", mixed)
		}
		lacks(t, "rich@x (verified)", e.objects(t, work, "rich@x")["rich@x"].Data, "X-PCB-INTEGRITY")
		more := e.objects(t, hol, "unsigned@x", "stranger@x", "attendees@x")
		contains(t, "unsigned@x (own shared part, no signature)", more["unsigned@x"].Data, "SUMMARY:Unsigned\r\n", "X-PCB-INTEGRITY:unverified\r\n")
		lacks(t, "stranger@x (authored by an address not ours)", more["stranger@x"].Data, "X-PCB-INTEGRITY")
		contains(t, "stranger@x", more["stranger@x"].Data, "SUMMARY:Invitation\r\n")
		lacks(t, "attendees@x (own attendees part, no signature)", more["attendees@x"].Data, "X-PCB-INTEGRITY")
		contains(t, "attendees@x", more["attendees@x"].Data, "mailto:bob@example.org")
		if n := e.svc.RenderCounts().Unverified; n != 3 {
			t.Errorf("Unverified = %d, want 3 (forged@x, mixed@x's exception, unsigned@x)", n)
		}
	})

	t.Run("TestLRUServesWithoutDecrypting", func(t *testing.T) {
		first := e.objects(t, work, "rich@x", "series@x")
		decrypts, calls := e.svc.Decrypts(), e.f.Calls("", "/")
		second := e.objects(t, work, "rich@x", "series@x")
		if n := e.svc.Decrypts() - decrypts; n != 0 {
			t.Errorf("a second read decrypted %d rows, want 0", n)
		}
		if n := e.f.Calls("", "/") - calls; n != 0 {
			t.Errorf("a second read made %d requests", n)
		}
		for uid := range first {
			if !bytes.Equal(first[uid].Data, second[uid].Data) || first[uid].ETag != second[uid].ETag {
				t.Errorf("%s differs between reads", uid)
			}
		}
	})

	t.Run("TestDuplicateMastersMostRecentWins", func(t *testing.T) {
		d := e.objects(t, work, "dup@x")["dup@x"].Data
		contains(t, "dup@x", d, "SUMMARY:Newest\r\n")
		lacks(t, "dup@x", d, "SUMMARY:Mid", "SUMMARY:Old")
		if n := bytes.Count(d, []byte("BEGIN:VEVENT")); n != 1 {
			t.Errorf("dup@x has %d VEVENTs, want 1", n)
		}
		if n := e.svc.RenderCounts().DuplicateMasters; n != 1 {
			t.Errorf("DuplicateMasters = %d; want 1 duplicate master left out", n)
		}
	})

	t.Run("TestInvalidTokenIsTyped", func(t *testing.T) {
		_, token := e.listing(t, work)
		m := tokenRE.FindStringSubmatch(token)
		bad := []string{
			"urn:pcb:sync:1:" + strings.Repeat("0", 32) + ":" + m[2] + ":" + m[3], // another epoch
			"urn:pcb:sync:1:" + m[1] + ":99999:" + m[3],                           // above the current seq
			"urn:pcb:sync:1:" + m[1] + ":" + m[2],                                 // the format before generations
			"urn:pcb:sync:1:" + m[1] + ":-1:" + m[3],
			"urn:pcb:sync:1:" + m[1] + ":" + m[2] + ":XYZ",
			"urn:pcb:sync:2:" + m[1] + ":" + m[2] + ":" + m[3],
			"http://example.org/sync/1",
			"garbage",
		}
		for _, tok := range bad {
			_, err := e.svc.Changes(t.Context(), work, tok)
			_ = wantExit(t, err, exitcode.Usage)
			if !errors.Is(err, service.ErrInvalidSyncToken) {
				t.Errorf("Changes(%q) = %v, want ErrInvalidSyncToken", tok, err)
			}
		}
		cs, err := e.svc.Changes(t.Context(), work, token)
		if err != nil || len(cs.Changed) != 0 || len(cs.Deleted) != 0 || cs.Token != token {
			t.Errorf("Changes(current token) = %+v, %v; want nothing and the same token", cs, err)
		}
		full, err := e.svc.Changes(t.Context(), work, "")
		if err != nil || len(full.Changed) != 6 || len(full.Deleted) != 0 || full.Token != token {
			t.Errorf("Changes(empty token) = %+v, %v; want every object", full, err)
		}
	})

	t.Run("UnknownCalendar", func(t *testing.T) {
		calls := e.f.Calls("", "/")
		_, _, err := e.svc.Objects(t.Context(), "no-such-calendar")
		_ = wantExit(t, err, exitcode.Usage)
		if !errors.Is(err, service.ErrNoSuchCalendar) {
			t.Errorf("Objects(unknown) = %v, want ErrNoSuchCalendar", err)
		}
		if n := e.f.Calls("", "/") - calls; n != 0 {
			t.Errorf("an unknown calendar made %d requests", n)
		}
	})
}

// serving is a Serve running in the background, one pass at a time: each
// pass waits in OnPass until the test resumes it.
type serving struct {
	passes chan service.WatchPass
	resume chan struct{}
	done   chan error
	cancel context.CancelFunc
}

func (e *env) serve(t *testing.T) *serving {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := &serving{passes: make(chan service.WatchPass), resume: make(chan struct{}), done: make(chan error, 1), cancel: cancel}
	go func() {
		s.done <- e.svc.Serve(ctx, service.ServeOptions{OnPass: func(p service.WatchPass) {
			s.passes <- p
			<-s.resume
		}})
	}()
	t.Cleanup(cancel)
	return s
}

// next lets the paused pass go on (but before the first) and waits for the
// next one.
func (s *serving) next(t *testing.T, first bool) service.WatchPass {
	t.Helper()
	if !first {
		s.resume <- struct{}{}
	}
	select {
	case p := <-s.passes:
		if p.Err != nil {
			t.Fatalf("pass: %v", p.Err)
		}
		return p
	case err := <-s.done:
		t.Fatalf("Serve returned %v before its pass", err)
	}
	return service.WatchPass{}
}

func (s *serving) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	close(s.resume)
	if err := <-s.done; err != nil {
		t.Errorf("Serve after the caller cancelled = %v, want nil", err)
	}
}

// TestServeReadsOnItsOwnHandle: Serve holds the sync lock, syncs on its
// writer handle and serves the reads from it (M3-R6); once it ends, the reads
// open the cache per call again.
func TestServeReadsOnItsOwnHandle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(work, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	s := e.serve(t)
	if p := s.next(t, true); calSync(t, p.Result, work).Mode != "initial" {
		t.Errorf("first pass = %+v", p.Result)
	}
	_, err := e.svc.Sync(t.Context(), service.SyncRequest{})
	if ee := wantExit(t, err, exitcode.Blocked); !strings.Contains(ee.Error(), "a sync is already running") {
		t.Errorf("Sync during Serve = %v", err)
	}
	_, tok := e.listing(t, work)
	path := cache.Path(e.dirs.State)
	unreadable := func(t *testing.T) {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("root reads a file of mode 0: this check needs another user")
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	}
	t.Run("ReadsUseServesOpenHandle", func(t *testing.T) {
		unreadable(t) // the file no longer opens: only Serve's open handle reads it
		contains(t, "a@x", e.objects(t, work, "a@x")["a@x"].Data, "UID:a@x")
		if _, again := e.listing(t, work); again != tok {
			t.Errorf("token %q, then %q", tok, again)
		}
	})
	e.f.AddEvent(work, protontest.Event{UID: "n@x", Start: base, End: base + 60})
	s.next(t, false)
	if cs := e.changes(t, work, tok); !slices.Equal(changedUIDs(cs), []string{"n@x"}) {
		t.Errorf("Changes after Serve's second pass = %+v, want n@x", cs)
	}
	s.stop(t)
	t.Run("AfterServeReadsOpenTheFile", func(t *testing.T) {
		unreadable(t)
		_, _, err := e.svc.Objects(t.Context(), work)
		_ = wantExit(t, err, exitcode.Blocked)
	})
	if etags, _ := e.listing(t, work); len(etags) != 2 {
		t.Errorf("listing after Serve = %v", etags)
	}
	e.sync(t, false) // the lock is free again
}

// Ruling M3-R34: each pass of Serve carries the render counters of its
// process (pcb serve logs them): the rows served as placeholders, the
// duplicate masters left out and the calendars whose keys failed, by ID.
func TestServePassesCarryTheRenderCounters(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	locked := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Locked", BrokenPassphrase: true})
	e.f.AddEvent(work, protontest.Event{UID: "ok@x", Start: base, End: base + 60})
	e.f.AddEvent(work, protontest.Event{UID: "broken@x", Start: base, End: base + 60, BrokenPart: protontest.SharedEncryptedPart})
	e.f.AddEvent(work, protontest.Event{UID: "dup@x", Start: base, End: base + 60})
	e.f.AddEvent(work, protontest.Event{UID: "dup@x", Start: base, End: base + 60})
	e.f.AddEvent(locked, protontest.Event{UID: "l@x", Start: base, End: base + 60})
	e.login(t, e.req())
	s := e.serve(t)
	if p := s.next(t, true); p.Render == nil || !p.Render.Zero() {
		t.Fatalf("first pass Render = %+v, want zero counters (nothing rendered yet)", p.Render)
	}
	e.objects(t, work, "ok@x", "broken@x", "dup@x")
	e.objects(t, locked, "l@x")
	p := s.next(t, false)
	if p.Render == nil {
		t.Fatal("a pass of Serve without Render")
	}
	if rc := *p.Render; rc.Undecryptable != 2 || rc.DuplicateMasters != 1 || !slices.Equal(rc.KeysErrors, []string{locked}) {
		t.Errorf("Render = %+v, want 2 placeholders (broken@x, l@x), 1 duplicate master, keys errors [%s]", rc, locked)
	}
	s.stop(t)
}

// Ruling M3-R10/R11: Serve reads the calendars' bootstraps again every 10
// minutes; a changed default notification, or keys that now unlock, change
// the ETags, and a token of the former generation lists every object.
func TestServeRefreshesCalendarSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	other := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Other"})
	e.f.AddEvent(work, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.f.AddEvent(work, protontest.Event{UID: "b@x", Start: base, End: base + 60})
	c := e.f.AddEvent(work, protontest.Event{UID: "c@x", Start: base, End: base + 60})
	e.f.AddEvent(other, protontest.Event{UID: "o@x", Start: base, End: base + 60})
	e.login(t, e.req())
	s := e.serve(t)
	s.next(t, true)
	e.f.FailNext("GET", "/calendar/v2/"+other+"/bootstrap", 422, 2501, 0) // Other's keys fail at first
	etags, tokW := e.listing(t, work)
	_, tokO := e.listing(t, other)
	contains(t, "o@x", e.objects(t, other, "o@x")["o@x"].Data, "X-PCB-PLACEHOLDER:1")
	boots := e.f.Calls("GET", "/calendar/v2/")
	e.f.SetDefaultNotifications(work, []protontest.Notification{{Type: 1, Trigger: "-PT30M"}}, nil)

	s.next(t, false) // not due yet
	if n := e.f.Calls("GET", "/calendar/v2/") - boots; n != 0 {
		t.Errorf("a pass before the refresh is due read %d bootstraps", n)
	}
	if again, tok := e.listing(t, work); again["a@x"] != etags["a@x"] || tok != tokW {
		t.Errorf("before the refresh: ETag %q -> %q, token %q -> %q", etags["a@x"], again["a@x"], tokW, tok)
	}

	// c@x is deleted in the pass that also refreshes the settings.
	e.f.DeleteEvent(work, c)
	e.clk.advance(10*time.Minute + time.Second)
	s.next(t, false)
	if n := e.f.Calls("GET", "/calendar/v2/") - boots; n != 2 {
		t.Errorf("the refresh read %d bootstraps, want one per calendar (2)", n)
	}
	after, tok := e.listing(t, work)
	if after["a@x"] == etags["a@x"] || after["b@x"] == etags["b@x"] || tok == tokW {
		t.Errorf("after the refresh: ETags %v (were %v), token %q (was %q); want all changed", after, etags, tok, tokW)
	}
	contains(t, "a@x", e.objects(t, work, "a@x")["a@x"].Data, "TRIGGER:-PT30M\r\n")
	// a@x and b@x did not change, yet a client holding the former token
	// fetches them all, and still learns that c@x is gone.
	if cs := e.changes(t, work, tokW); !slices.Equal(changedUIDs(cs), []string{"a@x", "b@x"}) || !slices.Equal(cs.Deleted, []string{"c@x"}) || cs.Token != tok {
		t.Errorf("Changes(token before the settings change) = %+v, want a@x and b@x changed, c@x deleted", cs)
	}
	// Other's keys now unlock: its objects decrypt, and its former token
	// lists them again.
	lacks(t, "o@x", e.objects(t, other, "o@x")["o@x"].Data, "X-PCB-PLACEHOLDER")
	if cs := e.changes(t, other, tokO); !slices.Equal(changedUIDs(cs), []string{"o@x"}) {
		t.Errorf("Changes(token of the keys-unavailable state) = %+v, want o@x", cs)
	}

	// A refresh that fails keeps the working keys: a calendar's own refusal
	// (422), then Proton unavailable (503, retried at the next pass).
	held, heldTok := e.listing(t, work)
	unchanged := func(when string) {
		t.Helper()
		if again, tok := e.listing(t, work); !maps.Equal(again, held) || tok != heldTok {
			t.Errorf("%s: ETags %v (were %v), token %q (was %q); want unchanged", when, again, held, tok, heldTok)
		}
		d := e.objects(t, work, "a@x")["a@x"].Data
		contains(t, "a@x "+when, d, "TRIGGER:-PT30M\r\n")
		lacks(t, "a@x "+when, d, "X-PCB-PLACEHOLDER")
	}
	e.clk.advance(10*time.Minute + time.Second)
	e.f.FailNext("GET", "/calendar/v2/"+work+"/bootstrap", 422, 2501, 0)
	s.next(t, false)
	unchanged("after a refresh refused for the calendar")
	e.clk.advance(10*time.Minute + time.Second)
	for range 3 { // the client retries a 503 twice
		e.f.FailNext("GET", "/calendar/v2/"+work+"/bootstrap", 503, 0, 0)
	}
	s.next(t, false)
	unchanged("after a refresh that could not reach Proton")
	boots = e.f.Calls("GET", "/calendar/v2/")
	s.next(t, false) // no time passed: only the failed refresh is due again
	if n := e.f.Calls("GET", "/calendar/v2/") - boots; n != 2 {
		t.Errorf("the pass after a failed refresh read %d bootstraps, want it retried (2)", n)
	}
	unchanged("after the retried refresh")

	// A calendar gone from Proton (and so from the cache) is no longer
	// refreshed.
	e.f.RemoveCalendar(e.f.Username, other)
	e.clk.advance(10*time.Minute + time.Second)
	boots = e.f.Calls("GET", "/calendar/v2/")
	s.next(t, false)
	if n := e.f.Calls("GET", "/calendar/v2/") - boots; n != 1 {
		t.Errorf("the refresh after Other was removed read %d bootstraps, want 1 (Work only)", n)
	}
	s.stop(t)
}

// A read unlocking keys (a Proton call) holds no lock Serve needs: Serve
// stops while the read waits for Proton.
func TestServeStopsWhileAReadUnlocksKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	gate := &bootstrapGate{entered: make(chan struct{}), release: make(chan struct{})}
	svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
		HTTP: &http.Client{Transport: gate}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	e.svc = svc
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(work, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	s := e.serve(t)
	s.next(t, true)
	read := make(chan error, 1)
	go func() {
		_, _, err := e.svc.Objects(t.Context(), work)
		read <- err
	}()
	<-gate.entered // the read waits for the calendar's bootstrap
	stopped := make(chan struct{})
	go func() {
		s.stop(t)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second): // a failure bound, not a synchronisation
		close(gate.release)
		t.Fatal("Serve did not stop while a read was unlocking keys")
	}
	close(gate.release)
	if err := <-read; err != nil {
		t.Errorf("the read = %v, want it served (from the file once Serve ended)", err)
	}
}

// Would fail if ReadOnly were still "type is normal" (M3-R15), ignored the
// Write bit, the passphrase verification or a calendar whose keys do not
// unlock.
func TestCollectionReadOnlyFollowsTheGate(t *testing.T) {
	t.Parallel()
	t.Run("permissions and types", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, protontest.Options{})
		owner := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Owner"})
		editor := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Editor", Permissions: 8})
		viewer := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Viewer", Permissions: 2 | 4})
		busy := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Busy", Permissions: 1})
		hol := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
		sub := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Feed", Type: 1})
		odd := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Odd", Type: 7})
		locked := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Locked", BrokenPassphrase: true})
		e.login(t, e.req())
		e.sync(t, false)
		for id, want := range map[string]bool{owner: false, editor: false, viewer: true, busy: true, hol: true, sub: true, odd: true, locked: true} {
			if got := e.collection(t, id).ReadOnly; got != want {
				t.Errorf("calendar %s: ReadOnly = %v, want %v", id, got, want)
			}
		}
		// The CLI listing follows the same gate, row by row.
		want := map[string]bool{owner: false, editor: false, viewer: true, busy: true, hol: true, sub: true, odd: true, locked: true}
		got := e.calendars(t)
		if len(got) != len(want) {
			t.Fatalf("calendar list has %d rows, want %d", len(got), len(want))
		}
		for _, c := range got {
			if c.ReadOnly != want[c.ID] {
				t.Errorf("pcb calendar %s (%s, keys %s): ReadOnly = %v, want %v", c.ID, c.Name, c.Keys, c.ReadOnly, want[c.ID])
			}
		}
	})
	t.Run("bad passphrase signature", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, protontest.Options{BadPassphraseSignature: true})
		id := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
		e.f.AddEvent(id, protontest.Event{UID: "a@x", Start: base, End: base + 60})
		e.login(t, e.req())
		e.sync(t, false)
		if !e.collection(t, id).ReadOnly {
			t.Error("a calendar whose passphrase signature fails is writable")
		}
		// Still readable: the objects decrypt.
		lacks(t, "a@x", e.objects(t, id, "a@x")["a@x"].Data, "X-PCB-PLACEHOLDER")
	})
}

// Would fail if the write gate were not among the render inputs: a permission
// change would leave ETags, CTag and token alone and clients would keep a
// stale privilege set.
func TestWritabilityChangeIsSeenByClients(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	id := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(id, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	s := e.serve(t)
	s.next(t, true)
	before := e.collection(t, id)
	etags, tok := e.listing(t, id)
	if before.ReadOnly {
		t.Fatal("precondition: the calendar starts writable")
	}
	e.f.SetPermissions(id, 2) // the owner downgrades us to a viewer
	e.clk.advance(10*time.Minute + time.Second)
	s.next(t, false)
	after := e.collection(t, id)
	etags2, tok2 := e.listing(t, id)
	if !after.ReadOnly {
		t.Error("the downgrade did not make the collection read only")
	}
	if after.CTag == before.CTag || after.SyncToken == before.SyncToken || tok2 == tok || etags2["a@x"] == etags["a@x"] {
		t.Errorf("a writability change left CTag %q->%q, token %q->%q, ETag %q->%q",
			before.CTag, after.CTag, tok, tok2, etags["a@x"], etags2["a@x"])
	}
	s.stop(t)
}

// The fake's own address keys unlock through keys.Fetch and keys.Unlock and
// every key tags as unlocked (it would not with the armor parse of M3).
func TestAddressKeyTagsOfTheFakesKeysAreUnlocked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	c, err := e.svc.SessionClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	u, err := keys.Unlock(user, addrs, e.account(t).KeyPass)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	tags := service.AddressKeyTags(u)
	if len(tags) == 0 {
		t.Fatal("no address key tagged")
	}
	for _, tag := range tags {
		if !strings.HasSuffix(tag, ":unlocked") {
			t.Errorf("tag %q: a key of the fake unlocked", tag)
		}
	}
}

// The cached type reaches the gate on every path that unlocks keys (ruling
// M4-R1): a read that loads the keys before any listing, and the settings
// refresh under Serve. A subscribed calendar with every permission stays
// read only through both.
func TestCachedTypeFeedsTheGateOnEveryKeyLoad(t *testing.T) {
	t.Parallel()
	t.Run("a read loads the keys first", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, protontest.Options{})
		sub := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Feed", Type: 1})
		e.f.AddEvent(sub, protontest.Event{UID: "a@x", Start: base, End: base + 60})
		e.login(t, e.req())
		e.sync(t, false)
		e.objects(t, sub, "a@x") // calendarRead unlocks: no Collections call before
		if !e.collection(t, sub).ReadOnly {
			t.Error("a subscribed calendar became writable after calendarRead loaded its keys")
		}
	})
	t.Run("refreshSettings", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, protontest.Options{})
		sub := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Feed", Type: 1})
		e.f.AddEvent(sub, protontest.Event{UID: "a@x", Start: base, End: base + 60})
		e.login(t, e.req())
		s := e.serve(t)
		s.next(t, true)
		if !e.collection(t, sub).ReadOnly {
			t.Fatal("precondition: a subscribed calendar starts read only")
		}
		// A settings change makes the refresh replace the calendar's keys.
		e.f.SetDefaultNotifications(sub, []protontest.Notification{{Type: 1, Trigger: "-PT30M"}}, nil)
		boots := e.f.Calls("GET", "/calendar/v2/")
		e.clk.advance(10*time.Minute + time.Second)
		s.next(t, false)
		if e.f.Calls("GET", "/calendar/v2/") == boots {
			t.Fatal("the refresh read no bootstrap")
		}
		if !e.collection(t, sub).ReadOnly {
			t.Error("a subscribed calendar became writable after a settings refresh")
		}
		s.stop(t)
	})
}
