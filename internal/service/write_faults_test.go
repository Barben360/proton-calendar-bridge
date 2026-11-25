package service_test

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// switchAccount makes the stored login another Proton account's, as a
// pcb login in another process does while pcb serve keeps running, and puts
// the first one back when the test ends (before Serve is stopped).
func (r *rig) switchAccount(t *testing.T) {
	t.Helper()
	const other = "another-account"
	st := r.loadState(t)
	if err := r.secrets().Update(t.Context(), func(a map[string]store.Account) error {
		a[other] = store.Account{ID: other, Username: "other", UID: "other-uid", AccessToken: "x", RefreshToken: "y"} // gitleaks:allow placeholder tokens of a stored account the write never uses
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	moved := st
	moved.AccountID = other
	if err := store.SaveState(r.dirs.State, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.SaveState(r.dirs.State, st); err != nil {
			t.Error(err)
		}
	})
}

func wantAnotherAccount(t *testing.T, err error) {
	t.Helper()
	ee := wantExit(t, err, exitcode.Blocked)
	if !strings.Contains(ee.Error(), "another Proton account than pcb serve started with") || ee.Hint != "restart pcb serve" {
		t.Errorf("error %q, hint %q, want the account switch and the restart hint", ee.Error(), ee.Hint)
	}
}

// A write whose login moved to another Proton account while pcb serve kept
// the first one's cache is refused at the UID listing, and nothing is sent.
func TestWriteAfterAnAccountSwitchIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, nil)
	var unlock func()
	// The write has passed the cache's account check and waits for the
	// calendar's lock: switch there, then let it go on.
	r.svc.OnWriteWait(func(string) {
		r.switchAccount(t)
		unlock()
	})
	r.start(t)
	unlock, err := r.svc.LockCalendar(t.Context(), r.cal)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.put(t, "n@x.ics", newEvent("n@x", "New"), service.Precondition{IfNoneMatchAny: true})
	wantAnotherAccount(t, err)
	if n := r.sendCalls(); n != 0 {
		t.Errorf("%d requests reached the sync endpoint", n)
	}
	if n := len(r.proton(t, "n@x")); n != 0 {
		t.Errorf("Proton holds %d rows", n)
	}
}

// A row cached with a field pcb cannot send back as Proton served it (here
// IsOrganizer, which is a number) is no reason to guess: the edit fails as a
// fault of the preparation, with nothing sent, and without the event's
// content in the message.
func TestWriteThatCannotBePlannedIsAFault(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
		f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Color: green, Card: card("a@x", "Lunch", base)})
	})
	organizer := regexp.MustCompile(`"IsOrganizer"\s*:\s*\d+`)
	r.tr.set(func(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
		resp, err := send()
		if err != nil || req.Method != http.MethodGet || !strings.HasPrefix(req.URL.Path, "/calendar/v1/"+r.cal+"/events") {
			return resp, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		body = organizer.ReplaceAll(body, []byte(`"IsOrganizer":"yes"`))
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Del("Content-Length")
		return resp, nil
	})
	r.start(t) // the first pass stores the row as the hook rewrites it
	obj := r.object(t, "a@x")

	_, err := r.put(t, "a@x.ics", edit(t, obj.Data, "SUMMARY:Lunch", "SUMMARY:Dinner"), service.Precondition{IfMatch: obj.ETag})
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Error() != "the write could not be prepared" || !strings.Contains(ee.Hint, "pcb doctor") {
		t.Errorf("error %q, hint %q, want the preparation fault and the doctor hint", ee.Error(), ee.Hint)
	}
	if strings.Contains(ee.Error()+ee.Hint, "Dinner") {
		t.Error("the fault carries the event's content")
	}
	if n := r.sendCalls(); n != 0 {
		t.Errorf("%d requests reached the sync endpoint", n)
	}
}
