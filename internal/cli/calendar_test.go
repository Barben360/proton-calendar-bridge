package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func mustUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
}

func TestEndToEndCalendarList(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	work := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	feed := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Feed", Type: 1})
	hol := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Holidays", Type: 2})
	r.f.SetDefaultCalendar(r.f.Username, work)
	r.env["PCB_TOTP"] = r.f.TOTPURI

	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	if e := r.envelope(); !e.OK {
		t.Fatalf("login envelope = %+v", e)
	}

	r.want(exitcode.OK, "calendar", "list", "--json")
	var data struct {
		Calendars []service.Calendar `json:"calendars"`
	}
	e := r.envelope()
	mustUnmarshal(t, e.Data, &data)
	if e.Command != "calendar list" || len(data.Calendars) != 3 {
		t.Fatalf("envelope = %+v, %d calendars", e, len(data.Calendars))
	}
	for i, want := range []struct {
		id, typ string
		ro, def bool
	}{{work, "normal", false, true}, {feed, "subscribed", true, false}, {hol, "holidays", true, false}} {
		c := data.Calendars[i]
		if c.ID != want.id || c.Type != want.typ || c.ReadOnly != want.ro || c.Default != want.def || c.Keys != "ok" {
			t.Errorf("calendar %d = %+v, want %+v", i, c, want)
		}
	}
	for _, s := range []string{r.f.Password, r.f.TOTPURI} {
		if strings.Contains(r.out.String()+r.errb.String(), s) {
			t.Error("calendar list output contains a secret")
		}
	}

	r.want(exitcode.OK, "status", "--json")
	var st service.Status
	mustUnmarshal(t, r.envelope().Data, &st)
	if st.Phase != "ready" || st.Username != r.f.Username {
		t.Errorf("status = %+v", st)
	}

	r.want(exitcode.OK, "logout", "--json")
	var lo service.LogoutResult
	mustUnmarshal(t, r.envelope().Data, &lo)
	if !lo.WasLoggedIn || !lo.Revoked {
		t.Errorf("logout = %+v", lo)
	}

	r.want(exitcode.MustAct, "calendar", "list", "--json")
	if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Hint, "pcb login") {
		t.Errorf("error = %+v, want hint pcb login", e.Error)
	}
}

func TestCalendarListText(t *testing.T) {
	t.Parallel()
	r := stubRig(t, nil)
	r.svc = &calStub{}
	r.want(exitcode.OK, "calendar", "list")
	lines := strings.Split(strings.TrimRight(r.out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("output = %q", r.out.String())
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "NAME TYPE ACCESS DEFAULT KEYS ID" {
		t.Errorf("header = %q", lines[0])
	}
	if f := strings.Fields(lines[1]); strings.Join(f, " ") != "Work normal rw * ok c1" {
		t.Errorf("row 1 = %q", lines[1])
	}
	if f := strings.Fields(lines[2]); strings.Join(f, " ") != "Feed subscribed ro - ok c2" {
		t.Errorf("row 2 = %q", lines[2])
	}
}

func TestCalendarListJSONEmptyIsAnArray(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	r.want(exitcode.OK, "calendar", "list", "--json")
	if !strings.Contains(r.out.String(), `"calendars": []`) {
		t.Errorf("stdout = %q, want an empty array", r.out.String())
	}
}

type calStub struct{ stubAPI }

func (*calStub) Calendars(context.Context) ([]service.Calendar, error) {
	return []service.Calendar{
		{ID: "c1", Name: "Work", Type: "normal", Default: true, Keys: "ok"},
		{ID: "c2", Name: "Feed", Type: "subscribed", ReadOnly: true, Keys: "ok"},
	}, nil
}
