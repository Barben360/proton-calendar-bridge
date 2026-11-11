package service_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

var (
	ctagRE  = regexp.MustCompile(`^([0-9a-f]{32}):([0-9]+):[0-9a-f]{16}$`)
	tokenRE = regexp.MustCompile(`^urn:pcb:sync:1:([0-9a-f]{32}):([0-9]+)$`)
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
	e.f.AddEvent(work, protontest.Event{UID: "allday@x", Start: base - 8*3600, End: base - 8*3600 + day, FullDay: true})
	e.f.AddEvent(work, protontest.Event{UID: "broken@x", Start: base, End: base + 3600, BrokenPart: protontest.SharedEncryptedPart,
		Card: card("broken@x", "Secret plan", base)})
	// Duplicate masters (M3-R3): the most recently edited one wins, whatever
	// the row order.
	old := e.f.AddEvent(work, protontest.Event{UID: "dup@x", Start: base, End: base + 60, Card: card("dup@x", "Old", base)})
	e.f.AddEvent(work, protontest.Event{UID: "dup@x", Start: base, End: base + 60, Card: card("dup@x", "Mid", base)})
	e.f.UpdateEvent(work, old, protontest.Event{UID: "dup@x", Start: base, End: base + 60, Card: card("dup@x", "Newest", base)})
	e.f.AddEvent(locked, protontest.Event{UID: "locked@x", Start: base - 8*3600, End: base - 8*3600 + 2*day, FullDay: true})
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
		if len(etags) != 5 {
			t.Errorf("listing = %v, want the 5 UIDs", etags)
		}
		got := e.objects(t, work, "rich@x", "series@x", "allday@x", "nobody@x")
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
		if bytes.Count(series, []byte("BEGIN:VEVENT")) != 2 || bytes.Index(series, []byte("SUMMARY:Standup\r\n")) > bytes.Index(series, []byte("SUMMARY:Standup moved")) {
			t.Errorf("series@x: want the master then its exception:\n%s", series)
		}
		contains(t, "series@x", series, "RECURRENCE-ID:"+stamp(rid), "RRULE:FREQ=DAILY")
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
		if b.ETag != etags["broken@x"] || b.Missing {
			t.Errorf("placeholder ETag %q, listing %q", b.ETag, etags["broken@x"])
		}
		contains(t, "rich@x beside it", got["rich@x"].Data, "SUMMARY:Lunch with Zoé")
		// A calendar whose keys fail: every object is a placeholder, with
		// the row's all-day dates.
		l := e.objects(t, locked, "locked@x")["locked@x"].Data
		contains(t, "locked@x", l, "DTSTART;VALUE=DATE:20270115\r\n", "DTEND;VALUE=DATE:20270117\r\n", "X-PCB-PLACEHOLDER:1\r\n")
		st, err := e.svc.Status(t.Context())
		if err != nil || st.Sync == nil {
			t.Fatalf("Status = %+v, %v", st, err)
		}
		if st.Sync.Undecryptable != 2 {
			t.Errorf("Undecryptable = %d, want 2 (broken@x and locked@x)", st.Sync.Undecryptable)
		}
		for _, c := range st.Sync.Calendars {
			switch {
			case c.ID == locked && c.KeysError != "the calendar keys do not unlock with the account keys":
				t.Errorf("Locked calendar status = %+v, want its keys error", c)
			case c.ID != locked && c.KeysError != "":
				t.Errorf("calendar %s has keys error %q", c.Name, c.KeysError)
			}
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
		st, err := e.svc.Status(t.Context())
		if err != nil || st.Sync == nil || st.Sync.DuplicateMasters != 1 {
			t.Errorf("Status = %+v, %v; want 1 duplicate master left out", st.Sync, err)
		}
	})

	t.Run("TestInvalidTokenIsTyped", func(t *testing.T) {
		_, token := e.listing(t, work)
		m := tokenRE.FindStringSubmatch(token)
		bad := []string{
			"urn:pcb:sync:1:" + strings.Repeat("0", 32) + ":" + m[2], // another epoch
			"urn:pcb:sync:1:" + m[1] + ":99999",                      // above the current seq
			"urn:pcb:sync:1:" + m[1] + ":-1",
			"urn:pcb:sync:2:" + m[1] + ":" + m[2],
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
		if err != nil || len(full.Changed) != 5 || len(full.Deleted) != 0 || full.Token != token {
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

// TestServeReadsOnItsOwnHandle: Serve holds the sync lock, syncs on its
// writer handle and serves the reads from it (M3-R6); once it ends, the reads
// open the cache per call again.
func TestServeReadsOnItsOwnHandle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(work, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	passes, resume, done := make(chan service.WatchPass), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- e.svc.Serve(ctx, service.ServeOptions{OnPass: func(p service.WatchPass) {
			passes <- p
			<-resume
		}})
	}()
	next := func() service.WatchPass {
		t.Helper()
		select {
		case p := <-passes:
			if p.Err != nil {
				t.Fatalf("pass: %v", p.Err)
			}
			return p
		case err := <-done:
			t.Fatalf("Serve returned %v before its pass", err)
		}
		return service.WatchPass{}
	}
	if p := next(); calSync(t, p.Result, work).Mode != "initial" {
		t.Errorf("first pass = %+v", p.Result)
	}
	_, err := e.svc.Sync(t.Context(), service.SyncRequest{})
	if ee := wantExit(t, err, exitcode.Blocked); !strings.Contains(ee.Error(), "a sync is already running") {
		t.Errorf("Sync during Serve = %v", err)
	}
	// The cache file no longer opens: only Serve's open handle reads it.
	path := cache.Path(e.dirs.State)
	unreadable := os.Geteuid() != 0
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0o600) }()
	_, tok := e.listing(t, work)
	contains(t, "a@x", e.objects(t, work, "a@x")["a@x"].Data, "UID:a@x")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	e.f.AddEvent(work, protontest.Event{UID: "n@x", Start: base, End: base + 60})
	resume <- struct{}{}
	next()
	if cs := e.changes(t, work, tok); !slices.Equal(changedUIDs(cs), []string{"n@x"}) {
		t.Errorf("Changes after Serve's second pass = %+v, want n@x", cs)
	}
	cancel()
	close(resume)
	if err := <-done; err != nil {
		t.Errorf("Serve after the caller cancelled = %v, want nil", err)
	}
	if unreadable {
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		_, _, err := e.svc.Objects(t.Context(), work)
		_ = wantExit(t, err, exitcode.Blocked)
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if etags, _ := e.listing(t, work); len(etags) != 2 {
		t.Errorf("listing after Serve = %v", etags)
	}
	e.sync(t, false) // the lock is free again
}
