package caldav_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Final review, minor 5: Thunderbird's flow on Serve's own cache handle
// (ruling M3-R6), with readers hammering sync-collection and PROPFIND while
// the passes write: the home set, an empty-token sync, a multiget, then an
// edit and a deletion in Proton, and a delta that holds exactly the edited
// object (a new ETag, the one a multiget serves) and the deleted one as a
// response-level 404; the next delta is empty. Run with -race.
func TestServeFlowOnItsOwnHandle(t *testing.T) {
	t.Parallel()
	var cal, goneID, editID string
	r := newUnsyncedRig(t, func(f *protontest.Fake) {
		cal = f.AddCalendar(f.Username, protontest.Calendar{Name: "Flow"})
		editID = f.AddEvent(cal, protontest.Event{UID: "edit@x", Start: base, End: base + 3600, Card: card("edit@x", "Before", base)})
		goneID = f.AddEvent(cal, protontest.Event{UID: "gone@x", Start: base, End: base + 3600, Card: card("gone@x", "Gone soon", base)})
	})
	// Each pass waits in OnPass until the test takes it: a pass taken after
	// an edit in Proton began after it.
	ctx, cancel := context.WithCancel(t.Context())
	passes, done := make(chan service.WatchPass), make(chan error, 1)
	go func() {
		done <- r.svc.Serve(ctx, service.ServeOptions{OnPass: func(p service.WatchPass) {
			select {
			case passes <- p:
			case <-ctx.Done():
			}
		}})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve after the test cancelled = %v, want nil", err)
		}
	})
	pass := func() {
		t.Helper()
		select {
		case p := <-passes:
			if p.Err != nil {
				t.Fatalf("pass: %v", p.Err)
			}
		case err := <-done:
			t.Fatalf("Serve returned %v", err)
		}
	}
	pass() // the first pass synced the calendar

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if rec := r.do("REPORT", calPath(cal), tbSync(""), "Depth", "1"); rec.Code != http.StatusMultiStatus {
					t.Errorf("background sync-collection: %d", rec.Code)
				}
				rec := r.do("PROPFIND", calPath(cal), `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/"><D:prop><CS:getctag/></D:prop></D:propfind>`, "Depth", "0")
				if rec.Code != http.StatusMultiStatus {
					t.Errorf("background PROPFIND: %d", rec.Code)
				}
			}
		})
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()

	homeSet := r.do("PROPFIND", home, `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/><D:displayname/></D:prop></D:propfind>`, "Depth", "1")
	if homeSet.Code != http.StatusMultiStatus || !strings.Contains(homeSet.Body.String(), "Flow") {
		t.Fatalf("home set: %d\n%s", homeSet.Code, homeSet.Body)
	}
	first := r.syncReport(t, cal, "")
	before, _ := listed(t, first)
	if len(before) != 2 {
		t.Fatalf("empty-token sync lists %v, want edit@x and gone@x", before)
	}
	if d := r.multiget(t, cal, "edit@x", "gone@x").get(t, objPath(cal, "edit@x")).prop(t, nsC, "calendar-data").Text; !strings.Contains(d, "SUMMARY:Before\r\n") {
		t.Fatalf("edit@x before the edit:\n%s", d)
	}

	r.f.UpdateEvent(cal, editID, protontest.Event{UID: "edit@x", Start: base, End: base + 3600, Card: card("edit@x", "After", base)})
	r.f.DeleteEvent(cal, goneID)
	pass() // may have run before the edits
	pass() // began after them

	delta := r.syncReport(t, cal, *first.token)
	changed, deleted := listed(t, delta)
	edited := objPath(cal, "edit@x")
	if len(changed) != 1 || changed[edited] == "" || changed[edited] == before[edited] {
		t.Errorf("delta changed %v, want edit@x alone with a new ETag (was %q)", changed, before[edited])
	}
	if len(deleted) != 1 || deleted[0] != objPath(cal, "gone@x") {
		t.Errorf("delta deleted %v, want gone@x", deleted)
	}
	got := r.multiget(t, cal, "edit@x").get(t, edited)
	if d := got.prop(t, nsC, "calendar-data").Text; !strings.Contains(d, "SUMMARY:After\r\n") {
		t.Errorf("edit@x after the edit:\n%s", d)
	}
	if e := got.prop(t, nsD, "getetag").Text; e != changed[edited] {
		t.Errorf("multiget ETag %q, delta ETag %q", e, changed[edited])
	}
	again, gone := listed(t, r.syncReport(t, cal, *delta.token))
	if len(again) != 0 || len(gone) != 0 {
		t.Errorf("the next delta lists %v, deleted %v; want nothing", again, gone)
	}
}
