package calendar_test

import (
	"net/http"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// session logs in on a fresh fake and returns the fake, a client and the
// unlocked keys.
func session(t *testing.T) (*protontest.Fake, *api.Client, *keys.Unlocked) {
	t.Helper()
	return sessionOn(t, protontest.Options{})
}

// sessionOn is session on a fake built with o.
func sessionOn(t *testing.T, o protontest.Options) (*protontest.Fake, *api.Client, *keys.Unlocked) {
	t.Helper()
	f := protontest.New(t, o)
	m := auth.NewManager(f.URL, "Other")
	t.Cleanup(m.Close)
	p, err := auth.Begin(t.Context(), m, f.Username, []byte(f.Password), "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(p.Close)
	c := api.New(p.Tokens, api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	})
	return f, c, unlockKeys(t, c, f.Password)
}

func unlockKeys(t *testing.T, c *api.Client, password string) *keys.Unlocked {
	t.Helper()
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	salts, err := keys.FetchSalts(t.Context(), c, nil)
	if err != nil {
		t.Fatalf("FetchSalts: %v", err)
	}
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(password))
	if err != nil {
		t.Fatalf("DeriveKeyPass: %v", err)
	}
	u, err := keys.Unlock(user, addrs, pass)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(u.Close)
	return u
}

func TestListResolvesMemberMetadata(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso", Description: "mine", Color: "#8080FF", Type: 0})
	f.AddCalendar(f.Username, protontest.Calendar{Name: "Feed", Color: "#FF0000", Type: 1})
	f.AddCalendar(f.Username, protontest.Calendar{Name: "Fêtes", Type: 2})
	cals, err := calendar.List(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 3 {
		t.Fatalf("got %d calendars, want 3", len(cals))
	}
	want := []struct {
		name, color, typ string
		readOnly         bool
	}{{"Perso", "#8080FF", "normal", false}, {"Feed", "#FF0000", "subscribed", true}, {"Fêtes", "", "holidays", true}}
	for i, w := range want {
		g := cals[i]
		if g.Name != w.name || g.Color != w.color || g.Type.String() != w.typ || g.Type.ReadOnly() != w.readOnly {
			t.Errorf("calendar %d = %+v (%s, readonly %v), want %+v", i, g, g.Type, g.Type.ReadOnly(), w)
		}
		if g.ID == "" || g.MemberID == "" || g.AddressID == "" || g.Email == "" || g.Permissions == 0 {
			t.Errorf("calendar %d lacks its member identity: %+v", i, g)
		}
	}
	if cals[0].Description != "mine" {
		t.Errorf("description = %q", cals[0].Description)
	}
}

func TestListLegacyTopLevelFallback(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	f.AddCalendar(f.Username, protontest.Calendar{Name: "Old", Description: "d", Color: "#123456", LegacyTopLevel: true})
	cals, err := calendar.List(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].Name != "Old" || cals[0].Description != "d" || cals[0].Color != "#123456" {
		t.Fatalf("got %+v", cals)
	}
	if cals[0].MemberID == "" {
		t.Error("the member identity must still come from Members[0]")
	}
}

func TestListInventsNothing(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	f.AddCalendar(f.Username, protontest.Calendar{})
	cals, err := calendar.List(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if cals[0].Name != "" || cals[0].Color != "" || cals[0].Description != "" {
		t.Fatalf("invented metadata: %+v", cals[0])
	}
}

func TestUnknownTypeIsReadOnly(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	f.AddCalendar(f.Username, protontest.Calendar{Name: "Odd", Type: 7})
	cals, err := calendar.List(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got := cals[0].Type; got.String() != "type 7" || !got.ReadOnly() {
		t.Fatalf("type 7 = %q, readonly %v", got, got.ReadOnly())
	}
}

func TestDefaultID(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	got, err := calendar.DefaultID(t.Context(), c)
	if err != nil || got != "" {
		t.Fatalf("unset: %q, %v", got, err)
	}
	f.SetDefaultCalendar(f.Username, id)
	got, err = calendar.DefaultID(t.Context(), c)
	if err != nil || got != id {
		t.Fatalf("set: %q, %v, want %q", got, err, id)
	}
}

func TestListIsNotCached(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	if _, err := calendar.List(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	f.AddCalendar(f.Username, protontest.Calendar{Name: "B"})
	cals, err := calendar.List(t.Context(), c)
	if err != nil || len(cals) != 2 {
		t.Fatalf("got %d calendars, %v; want the fresh list of 2", len(cals), err)
	}
}
