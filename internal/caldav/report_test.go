package caldav_test

// The reports against the real service (protontest fake, real encrypted
// cards, the real cache): what the stub cannot show.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// base is 2027-01-15 08:00 UTC, the start of the test events.
const (
	base = int64(1_800_000_000)
	day  = int64(24 * 3600)
)

func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format("20060102T150405Z") }

func card(uid, summary string, start int64, extra ...string) string {
	lines := []string{"UID:" + uid, "DTSTAMP:20270115T080000Z", "DTSTART:" + stamp(start), "DTEND:" + stamp(start+3600), "SUMMARY:" + summary}
	return strings.Join(append(lines, extra...), "\r\n")
}

// rig is a logged-in, synced service on a fake Proton, its handler and a
// client's app password.
type rig struct {
	f    *protontest.Fake
	svc  *service.Local
	dirs paths.Dirs
	h    http.Handler
	pass string
}

func noSleep(context.Context, time.Duration) error { return nil }

// newRig builds the rig; setup adds the account's calendars and events
// before the login and the first sync.
func newRig(t *testing.T, setup func(f *protontest.Fake)) *rig {
	t.Helper()
	r := &rig{f: protontest.New(t, protontest.Options{})}
	root := t.TempDir()
	r.dirs = paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(r.dirs)
	cfg.Proton.BaseURL = r.f.URL
	svc, err := service.NewLocal(service.Options{Dirs: r.dirs, Config: cfg, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	r.svc = svc
	setup(r.f)
	if _, err := svc.Login(t.Context(), service.LoginRequest{Username: r.f.Username, Password: r.f.Password}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	r.sync(t)
	sec, err := svc.AddClient(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	r.pass = sec.Password
	r.h = caldav.Handler(svc, caldav.Options{})
	return r
}

func (r *rig) sync(t *testing.T) {
	t.Helper()
	if _, err := r.svc.Sync(t.Context(), service.SyncRequest{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func (r *rig) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	req := call(method, target, body, hdr...)
	req.SetBasicAuth("user@proton.me", r.pass)
	return serve(r.h, req)
}

// syncReport runs Thunderbird's sync-collection with token.
func (r *rig) syncReport(t *testing.T, calID, token string) multistatus {
	t.Helper()
	ms := parseMS(t, r.do("REPORT", calPath(calID), tbSync(token), "Depth", "1", "Content-Type", "text/xml; charset=utf-8"))
	if ms.token == nil || *ms.token == "" {
		t.Fatalf("no sync-token in:\n%s", ms.raw)
	}
	return ms
}

// multiget fetches uids the way Thunderbird does.
func (r *rig) multiget(t *testing.T, calID string, uids ...string) multistatus {
	t.Helper()
	hrefs := make([]string, len(uids))
	for i, u := range uids {
		hrefs[i] = tbHref(calID, u)
	}
	return parseMS(t, r.do("REPORT", calPath(calID), tbMultiget(hrefs...), "Depth", "1"))
}

// listed returns the hrefs a sync listed (200) and deleted (404).
func listed(t *testing.T, ms multistatus) (changed map[string]string, deleted []string) {
	t.Helper()
	changed = map[string]string{}
	for path, resp := range ms.byPath {
		switch {
		case resp.status == "HTTP/1.1 404 Not Found" && resp.propstats == 0:
			deleted = append(deleted, path)
		case resp.status == "":
			changed[path] = resp.prop(t, nsD, "getetag").Text
		default:
			t.Errorf("%s: status %q", path, resp.status)
		}
	}
	return changed, deleted
}

// TestReportsOverTheService shares one logged-in rig; the subtests run in
// order and each builds on the state the previous one left.
func TestReportsOverTheService(t *testing.T) {
	t.Parallel()
	const (
		keep   = "keep@x"
		gone   = "gone@x"
		series = "series@x"
		broken = "broken@x"
		odd    = "odd uid/é"
	)
	var work, keepID, excID, goneID string
	rid := base + day
	r := newRig(t, func(f *protontest.Fake) {
		work = f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"})
		keepID = f.AddEvent(work, protontest.Event{UID: keep, Start: base, End: base + 3600, Card: card(keep, "Keep me", base)})
		goneID = f.AddEvent(work, protontest.Event{UID: gone, Start: base, End: base + 3600, Card: card(gone, "Gone soon", base)})
		f.AddEvent(work, protontest.Event{UID: series, Start: base, End: base + 3600, RRule: "FREQ=DAILY",
			Card: card(series, "Standup", base, "RRULE:FREQ=DAILY")})
		excID = f.AddEvent(work, protontest.Event{UID: series, Start: rid + 1800, End: rid + 5400, RecurrenceID: &rid,
			Card: card(series, "Standup moved", rid+1800, "RECURRENCE-ID:"+stamp(rid))})
		f.AddEvent(work, protontest.Event{UID: broken, Start: base, End: base + 3600, BrokenPart: protontest.SharedEncryptedPart,
			Card: card(broken, "Secret plan", base)})
		f.AddEvent(work, protontest.Event{UID: odd, Start: base, End: base + 3600, Card: card(odd, "Odd one", base)})
	})
	all := []string{keep, gone, series, broken, odd}
	var token string
	etags := map[string]string{}

	// Review focus 3: one event with a part that does not decrypt; every
	// other event still syncs, that one is the placeholder.
	t.Run("TestUndecryptableEventDoesNotFailTheReport", func(t *testing.T) {
		ms := r.syncReport(t, work, "")
		changed, deleted := listed(t, ms)
		if len(changed) != len(all) || len(deleted) != 0 {
			t.Fatalf("empty-token sync listed %v, deleted %v; want the %d UIDs", changed, deleted, len(all))
		}
		for _, uid := range all {
			etags[uid] = changed[objPath(work, uid)]
		}
		token = *ms.token
		got := r.multiget(t, work, all...)
		for _, uid := range all {
			resp := got.get(t, objPath(work, uid))
			if resp.status != "" {
				t.Fatalf("%s: status %q", uid, resp.status)
			}
			if e := resp.prop(t, nsD, "getetag").Text; e != etags[uid] {
				t.Errorf("%s: multiget ETag %q, listing %q", uid, e, etags[uid])
			}
		}
		data := func(uid string) string { return got.get(t, objPath(work, uid)).prop(t, nsC, "calendar-data").Text }
		if d := data(broken); !strings.Contains(d, "X-PCB-PLACEHOLDER:1\r\n") || !strings.Contains(d, "SUMMARY:(pcb could not decrypt this event)\r\n") ||
			strings.Contains(d, "Secret plan") {
			t.Errorf("the undecryptable event:\n%q", d)
		}
		for uid, summary := range map[string]string{keep: "Keep me", odd: "Odd one", series: "Standup moved"} {
			if d := data(uid); !strings.Contains(d, "SUMMARY:"+summary+"\r\n") || strings.Contains(d, "X-PCB-PLACEHOLDER") {
				t.Errorf("%s:\n%q", uid, d)
			}
		}
		st, err := r.svc.Status(t.Context())
		if err != nil || st.Sync == nil || st.Sync.Undecryptable == 0 {
			t.Errorf("the placeholder is not counted: %+v, %v", st.Sync, err)
		}
	})

	// E08 MUST 12: an edit changes the object's ETag, and the delta, the
	// multiget, GET and PROPFIND agree on the new one.
	t.Run("TestMust12ETagsChangeWithContentAndAgree", func(t *testing.T) {
		r.f.UpdateEvent(work, keepID, protontest.Event{UID: keep, Start: base, End: base + 3600, Card: card(keep, "Kept and edited", base)})
		r.sync(t)
		ms := r.syncReport(t, work, token)
		changed, deleted := listed(t, ms)
		p := objPath(work, keep)
		if len(changed) != 1 || changed[p] == "" || len(deleted) != 0 {
			t.Fatalf("delta after one edit: changed %v, deleted %v", changed, deleted)
		}
		if changed[p] == etags[keep] {
			t.Errorf("the ETag did not change with the content: %s", changed[p])
		}
		got := r.multiget(t, work, keep).get(t, p)
		one := parseMS(t, r.do("PROPFIND", p, tbEtags, "Depth", "0")).get(t, p)
		head := r.do("GET", p, "")
		for what, e := range map[string]string{"multiget": got.prop(t, nsD, "getetag").Text, "PROPFIND": one.prop(t, nsD, "getetag").Text, "GET": head.Header().Get("ETag")} {
			if e != changed[p] {
				t.Errorf("%s ETag %q, delta %q", what, e, changed[p])
			}
		}
		if !strings.Contains(head.Body.String(), "SUMMARY:Kept and edited\r\n") {
			t.Errorf("GET:\n%s", head.Body)
		}
		token, etags[keep] = *ms.token, changed[p]
	})

	// Review focus 2: an event deleted in Proton is its href's response-
	// level 404, exactly once; a deleted exception changes its UID's object.
	t.Run("TestSyncCollectionReportsDeletionsPerUID", func(t *testing.T) {
		r.f.DeleteEvent(work, goneID)
		r.f.DeleteEvent(work, excID)
		r.sync(t)
		ms := r.syncReport(t, work, token)
		changed, deleted := listed(t, ms)
		if len(deleted) != 1 || deleted[0] != objPath(work, gone) {
			t.Errorf("deleted %v, want only %s", deleted, objPath(work, gone))
		}
		if len(changed) != 1 || changed[objPath(work, series)] == "" || changed[objPath(work, series)] == etags[series] {
			t.Errorf("changed %v, want %s with a new ETag", changed, series)
		}
		if d := r.multiget(t, work, series).get(t, objPath(work, series)).prop(t, nsC, "calendar-data").Text; strings.Contains(d, "RECURRENCE-ID") {
			t.Errorf("the exception is still served:\n%s", d)
		}
		if rec := r.do("GET", objPath(work, gone), ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET of the deleted object: %d", rec.Code)
		}
		token = *ms.token
		again, gone2 := listed(t, r.syncReport(t, work, token))
		if len(again) != 0 || len(gone2) != 0 {
			t.Errorf("the next sync repeats: changed %v, deleted %v", again, gone2)
		}
	})

	// Review focus 1: Thunderbird holds a token of a cache since recreated
	// (another epoch): 403 valid-sync-token, then its empty-token sync lists
	// every live object once, and nothing deleted.
	t.Run("TestSyncTokenOfAnotherEpochIsRefused", func(t *testing.T) {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(cache.Path(r.dirs.State) + suffix); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		r.sync(t)
		rec := r.do("REPORT", calPath(work), tbSync(token), "Depth", "1")
		wantStatus(t, rec, http.StatusForbidden)
		wantBody(t, rec, validSyncTokenBody)
		ms := r.syncReport(t, work, "")
		changed, deleted := listed(t, ms)
		want := []string{keep, series, broken, odd}
		if len(changed) != len(want) || len(deleted) != 0 || len(ms.responses) != len(want) {
			t.Fatalf("full sync: %d responses, changed %v, deleted %v; want %v once each", len(ms.responses), changed, deleted, want)
		}
		for _, uid := range want {
			if changed[objPath(work, uid)] == "" {
				t.Errorf("%s not listed", uid)
			}
		}
		if *ms.token == token {
			t.Error("the new life kept the old token")
		}
	})
}
