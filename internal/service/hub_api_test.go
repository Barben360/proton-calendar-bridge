package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The API Hub.Account hands out is a wrapper: each method must run on its
// account's Local, and only on that one. These tests drive every method of
// it and read what happened on the fake (a stub that answered nil would
// pass a test that only looks at the return values), for two accounts.

// apiRig is a Hub with accounts a and b logged in, each owning one calendar
// with seeded events, and a Proton-side client per account to look at the
// fake with.
type apiRig struct {
	e    *hubEnv
	h    *service.Hub
	id   map[string]string // local ID by username
	cal  map[string]string // calendar ID by username
	pc   map[string]*api.Client
	uids map[string][]string // seeded UIDs by username
}

func newAPIRig(t *testing.T) *apiRig {
	t.Helper()
	e := newHubEnv(t)
	e.sleep = blockSleep
	r := &apiRig{e: e, id: map[string]string{}, cal: map[string]string{}, pc: map[string]*api.Client{}, uids: map[string][]string{
		userA: {"a1@x", "a2@x"}, userB: {"b1@x"},
	}}
	for _, user := range []string{userA, userB} {
		r.cal[user] = e.f.AddCalendar(user, protontest.Calendar{Name: "cal of " + user})
		for i, uid := range r.uids[user] {
			start := base + int64(i)*7200
			e.f.AddEvent(r.cal[user], protontest.Event{UID: uid, Start: start, End: start + 3600, Card: card(uid, "Seed "+uid, start)})
		}
		r.pc[user] = api.New(e.f.Session(t, user), api.Options{BaseURL: e.f.URL})
	}
	r.h = e.open(t)
	r.id[userA], r.id[userB] = e.loginBoth(t, r.h)
	return r
}

// account is the API Hub.Account hands out for user.
func (r *apiRig) account(t *testing.T, user string) service.API {
	t.Helper()
	a, _, err := r.h.Account(r.id[user])
	if err != nil {
		t.Fatalf("Account(%s): %v", user, err)
	}
	return a
}

// agenda is the Agenda Hub.Agenda hands out for user.
func (r *apiRig) agenda(t *testing.T, user string) service.Agenda {
	t.Helper()
	g, _, err := r.h.Agenda(r.id[user])
	if err != nil {
		t.Fatalf("Agenda(%s): %v", user, err)
	}
	return g
}

// rows is what Proton holds for uid in user's calendar.
func (r *apiRig) rows(t *testing.T, user, uid string) []events.Row {
	t.Helper()
	rows, err := events.ListByUID(t.Context(), r.pc[user], r.cal[user], uid)
	if err != nil {
		t.Fatalf("ListByUID(%s): %v", uid, err)
	}
	return rows
}

// serveAccount runs the account's Serve (through the wrapper) until its
// first pass; the returned stop ends it.
func (r *apiRig) serveAccount(t *testing.T, a service.API) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	passes := make(chan service.WatchPass, 16)
	done := make(chan error, 1)
	go func() {
		done <- a.Serve(ctx, service.ServeOptions{LoginPoll: time.Hour, OnPass: func(p service.WatchPass) {
			select {
			case passes <- p:
			default:
			}
		}})
	}()
	select {
	case p := <-passes:
		if p.Err != nil {
			t.Fatalf("first pass: %v", p.Err)
		}
	case err := <-done:
		t.Fatalf("Serve returned before its first pass: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("no first pass")
	}
	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve = %v", err)
			}
		case <-time.After(waitLimit):
			t.Error("Serve did not return after its context ended")
		}
	}
	t.Cleanup(stop)
	return stop
}

func uidsOf[T any](list []T, uid func(T) string) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = uid(x)
	}
	slices.Sort(out)
	return out
}

// Every method of the API Hub.Account hands out reaches its account: the
// effects are on the fake (rows written and deleted, sessions revoked), and
// the other account is untouched.
func TestAccountAPIMethodsReachTheirAccount(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t)
	ctx := t.Context()
	A, B := r.account(t, userA), r.account(t, userB)
	G := r.agenda(t, userA)
	calA, calB := r.cal[userA], r.cal[userB]

	// Login, SubmitTOTP, SubmitMailboxPassword.
	r.e.clk.advance(loginguard.MinInterval + time.Second)
	if res, err := A.Login(ctx, service.LoginRequest{Username: userA, Password: r.e.password(userA)}); err != nil || res.State != service.StateReady {
		t.Fatalf("Login = %+v, %v; want ready", res, err)
	}
	if _, err := A.Login(ctx, service.LoginRequest{}); wantExit(t, err, exitcode.Usage).Error() != "a username and a password are required" {
		t.Errorf("Login without credentials = %v, want the Local's refusal", err)
	}
	for name, call := range map[string]func() error{
		"SubmitTOTP":            func() error { _, err := A.SubmitTOTP(ctx, "123456"); return err },
		"SubmitMailboxPassword": func() error { _, err := A.SubmitMailboxPassword(ctx, "pw"); return err },
	} {
		if ee := wantExit(t, call(), exitcode.MustAct); !strings.Contains(ee.Error(), "no login is waiting") {
			t.Errorf("%s with no login waiting = %q, want the Local's refusal", name, ee.Error())
		}
	}
	if st, err := A.Status(ctx); err != nil || st.Address != userA || st.Phase != string(service.StateReady) {
		t.Fatalf("Status = %+v, %v; want a's, ready", st, err)
	}
	if st, err := B.Status(ctx); err != nil || st.Address != userB {
		t.Fatalf("Status of b = %+v, %v", st, err)
	}

	if res, err := A.Sync(ctx, service.SyncRequest{}); err != nil || len(res.Calendars) != 1 {
		t.Errorf("Sync = %+v, %v; want a's one calendar", res, err)
	}
	stopServe := r.serveAccount(t, A)
	r.serveAccount(t, B)

	// Reads.
	if cals, err := A.Calendars(ctx); err != nil || len(cals) != 1 || cals[0].ID != calA {
		t.Errorf("Calendars = %+v, %v; want a's one", cals, err)
	}
	if cols, err := A.Collections(ctx); err != nil || len(cols) != 1 || cols[0].ID != calA {
		t.Errorf("Collections = %+v, %v; want a's one", cols, err)
	}
	if h, err := A.Health(ctx); err != nil || h.Serve != service.ServeServing || len(h.Calendars) != 1 {
		t.Errorf("Health = %+v, %v; want serving with a's calendar", h, err)
	}
	if evs, err := A.Events(ctx, service.EventsRequest{Calendar: calA}); err != nil ||
		!slices.Equal(uidsOf(evs, func(e service.Event) string { return e.UID }), []string{"a1@x", "a2@x"}) {
		t.Errorf("Events = %+v, %v; want a1 and a2", evs, err)
	}
	infos, token, err := A.Objects(ctx, calA)
	if err != nil || !slices.Equal(uidsOf(infos, func(o service.ObjectInfo) string { return o.UID }), []string{"a1@x", "a2@x"}) || token == "" {
		t.Errorf("Objects = %+v, %q, %v; want a1 and a2", infos, token, err)
	}
	objs, err := A.ObjectsByUID(ctx, calA, []string{"a1@x", "a2@x", "nope@x"})
	if err != nil || len(objs) != 3 {
		t.Fatalf("ObjectsByUID = %+v, %v; want 3 answers", objs, err)
	}
	for i, want := range []string{"Seed a1@x", "Seed a2@x"} {
		if objs[i].Missing || !bytes.Contains(objs[i].Data, []byte("SUMMARY:"+want)) || objs[i].ETag == "" {
			t.Errorf("ObjectsByUID[%d] = %+v, want the object with %q", i, objs[i], want)
		}
	}
	if !objs[2].Missing {
		t.Errorf("ObjectsByUID of an unknown UID = %+v, want Missing", objs[2])
	}
	byName, err := A.ObjectsByName(ctx, calA, []string{objs[0].Name})
	if err != nil || len(byName) != 1 || byName[0].UID != "a1@x" || !bytes.Equal(byName[0].Data, objs[0].Data) {
		t.Errorf("ObjectsByName(%q) = %+v, %v; want a1", objs[0].Name, byName, err)
	}
	if cs, err := A.Changes(ctx, calA, ""); err != nil || len(cs.Changed) != 2 || cs.Token == "" {
		t.Errorf("Changes = %+v, %v; want both objects", cs, err)
	}
	window := service.OccurrencesRequest{Calendars: []string{calA}, From: at(base - day), To: at(base + day)}
	if p, err := G.Occurrences(ctx, window); err != nil || len(p.Items) != 2 {
		t.Errorf("Occurrences = %+v, %v; want 2", p, err)
	}
	if fb, _, err := G.FreeBusy(ctx, window); err != nil || len(fb) != 2 {
		t.Errorf("FreeBusy = %+v, %v; want 2", fb, err)
	}
	if d, err := G.Event(ctx, service.EventRef{CalendarID: calA, UID: "a2@x"}); err != nil || d.Data.Summary != "Seed a2@x" {
		t.Errorf("Event = %+v, %v", d.Data, err)
	}

	// PutObject writes to Proton, DeleteObject removes there.
	if len(r.rows(t, userA, "new@x")) != 0 {
		t.Fatal("new@x is on Proton before the PutObject")
	}
	put, err := A.PutObject(ctx, calA, "new@x.ics", vcal([]string{"UID:new@x", "DTSTAMP:20270115T080000Z", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600), "SUMMARY:Put through the hub"}), service.Precondition{})
	if err != nil || !put.Created {
		t.Fatalf("PutObject = %+v, %v", put, err)
	}
	if len(r.rows(t, userA, "new@x")) != 1 {
		t.Error("PutObject did not reach Proton")
	}
	if err := A.DeleteObject(ctx, calA, "a1@x.ics", service.Precondition{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if n := len(r.rows(t, userA, "a1@x")); n != 0 {
		t.Errorf("DeleteObject left %d rows of a1@x on Proton", n)
	}
	if objs, err := A.ObjectsByUID(ctx, calA, []string{"a1@x", "new@x"}); err != nil || len(objs) != 2 || !objs[0].Missing || objs[1].Missing {
		t.Errorf("ObjectsByUID after the delete and the put = %+v, %v; want a1 gone, new there", objs, err)
	}

	// CreateEvent, UpdateEvent (a dry run first), DeleteEvent.
	ref, etag, err := G.CreateEvent(ctx, calA, service.EventFields{
		Summary: ptr("Created"),
		Start:   &service.EventTime{At: at(base + day)}, End: &service.EventTime{At: at(base + day + 3600)},
	})
	if err != nil || len(r.rows(t, userA, ref.UID)) != 1 {
		t.Fatalf("CreateEvent = %+v, %v; want a row on Proton", ref, err)
	}
	dry, err := G.UpdateEvent(ctx, ref, etag, service.ScopeSeries, service.EventFields{Summary: ptr("Renamed")}, true)
	if err != nil || dry.After.Summary != "Renamed" || dry.Before.Summary != "Created" {
		t.Fatalf("UpdateEvent dry run = %+v, %v", dry, err)
	}
	if d, err := G.Event(ctx, ref); err != nil || d.Data.Summary != "Created" {
		t.Fatalf("after a dry run the event is %+v, %v; want it unchanged", d.Data, err)
	}
	upd, err := G.UpdateEvent(ctx, ref, etag, service.ScopeSeries, service.EventFields{Summary: ptr("Renamed")}, false)
	if err != nil || upd.ETag == "" || upd.ETag == etag {
		t.Fatalf("UpdateEvent = %+v, %v; want a new ETag", upd, err)
	}
	if d, err := G.Event(ctx, ref); err != nil || d.Data.Summary != "Renamed" {
		t.Errorf("after UpdateEvent the event is %+v, %v", d.Data, err)
	}
	if err := G.DeleteEvent(ctx, ref, upd.ETag, service.ScopeSeries); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	if n := len(r.rows(t, userA, ref.UID)); n != 0 {
		t.Errorf("DeleteEvent left %d rows on Proton", n)
	}

	// The clients of a's CalDAV.
	sec, err := A.AddClient(ctx, "phone")
	if err != nil || sec.Password == "" {
		t.Fatalf("AddClient = %v, %v", sec.Name, err)
	}
	cl, err := A.Clients(ctx)
	if err != nil || !slices.Contains(uidsOf(cl, func(c service.Client) string { return c.Name }), "phone") {
		t.Errorf("Clients = %+v, %v; want phone listed", cl, err)
	}
	if cr, err := A.ShowClient(ctx, "phone"); err != nil || cr.ID != sec.ID {
		t.Errorf("ShowClient = %+v, %v; want phone", cr.Client, err)
	}
	const remote = "192.0.2.1:1"
	if ok, _ := A.CheckClient(ctx, sec.Password, remote); !ok {
		t.Error("CheckClient refused the new client's password")
	}
	if ok, _ := B.CheckClient(ctx, sec.Password, remote); ok {
		t.Error("b accepted a's client's password")
	}
	rot, err := A.RotateClient(ctx, "phone")
	if err != nil || rot.Password == "" || rot.Password == sec.Password {
		t.Fatalf("RotateClient = %v, %v; want another password", rot.Name, err)
	}
	if ok, _ := A.CheckClient(ctx, sec.Password, remote); ok {
		t.Error("the rotated-away password still passes CheckClient")
	}
	if ok, _ := A.CheckClient(ctx, rot.Password, remote); !ok {
		t.Error("the rotated password does not pass CheckClient")
	}
	if rm, err := A.RemoveClient(ctx, "phone"); err != nil || rm.Name != "phone" {
		t.Fatalf("RemoveClient = %+v, %v", rm, err)
	}
	if ok, _ := A.CheckClient(ctx, rot.Password, remote); ok {
		t.Error("a removed client's password still passes CheckClient")
	}

	// b saw none of it.
	if n := len(r.rows(t, userB, "b1@x")); n != 1 {
		t.Errorf("b's b1@x has %d rows, want 1", n)
	}
	if infos, _, err := B.Objects(ctx, calB); err != nil || len(infos) != 1 || infos[0].UID != "b1@x" {
		t.Errorf("b's Objects = %+v, %v; want b1 only", infos, err)
	}
	if cl, err := B.Clients(ctx); err != nil || slices.Contains(uidsOf(cl, func(c service.Client) string { return c.Name }), "phone") {
		t.Errorf("b's Clients = %+v, %v; want no phone", cl, err)
	}

	// Logout revokes a's session on the fake and leaves b's.
	stopServe()
	tokA, tokB := r.e.session(userA), r.e.session(userB)
	if !sessionAliveOn(t, r.e.f.URL, tokA) {
		t.Fatal("a's session is not alive before the Logout")
	}
	lo, err := A.Logout(ctx, service.LogoutRequest{})
	if err != nil || !lo.WasLoggedIn || !lo.Revoked {
		t.Fatalf("Logout = %+v, %v", lo, err)
	}
	if sessionAliveOn(t, r.e.f.URL, tokA) {
		t.Error("a's session is alive after the Logout")
	}
	if !sessionAliveOn(t, r.e.f.URL, tokB) {
		t.Error("b's session was revoked by a's Logout")
	}
	if st, err := A.Status(ctx); err != nil || st.Phase == string(service.StateReady) {
		t.Errorf("Status after Logout = %+v, %v", st, err)
	}
}

// Once an account is removed, every method of the API handed out for it
// refuses with ErrNoAccount, the long ones included, and the other account
// still answers.
func TestAccountAPIAfterRemoveFindsNoAccount(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t)
	ctx := t.Context()
	A, B := r.account(t, userA), r.account(t, userB)
	G := r.agenda(t, userA)
	cal := r.cal[userA]
	ref := service.EventRef{CalendarID: cal, UID: "a1@x"}
	win := service.OccurrencesRequest{Calendars: []string{cal}, From: at(base - day), To: at(base + day)}
	calls := map[string]func(a service.API, g service.Agenda) error{
		"Login": func(a service.API, _ service.Agenda) error {
			_, err := a.Login(ctx, service.LoginRequest{Username: userA, Password: "x"})
			return err
		},
		"SubmitTOTP":            func(a service.API, _ service.Agenda) error { _, err := a.SubmitTOTP(ctx, "123456"); return err },
		"SubmitMailboxPassword": func(a service.API, _ service.Agenda) error { _, err := a.SubmitMailboxPassword(ctx, "x"); return err },
		"Logout": func(a service.API, _ service.Agenda) error {
			_, err := a.Logout(ctx, service.LogoutRequest{})
			return err
		},
		"Status":    func(a service.API, _ service.Agenda) error { _, err := a.Status(ctx); return err },
		"Calendars": func(a service.API, _ service.Agenda) error { _, err := a.Calendars(ctx); return err },
		"Sync":      func(a service.API, _ service.Agenda) error { _, err := a.Sync(ctx, service.SyncRequest{}); return err },
		"WatchSync": func(a service.API, _ service.Agenda) error {
			return a.WatchSync(ctx, service.SyncRequest{}, time.Hour, func(service.WatchPass) {})
		},
		"Serve":  func(a service.API, _ service.Agenda) error { return a.Serve(ctx, service.ServeOptions{}) },
		"Health": func(a service.API, _ service.Agenda) error { _, err := a.Health(ctx); return err },
		"Events": func(a service.API, _ service.Agenda) error {
			_, err := a.Events(ctx, service.EventsRequest{Calendar: cal})
			return err
		},
		"Collections": func(a service.API, _ service.Agenda) error { _, err := a.Collections(ctx); return err },
		"Objects":     func(a service.API, _ service.Agenda) error { _, _, err := a.Objects(ctx, cal); return err },
		"ObjectsByUID": func(a service.API, _ service.Agenda) error {
			_, err := a.ObjectsByUID(ctx, cal, []string{"a1@x"})
			return err
		},
		"ObjectsByName": func(a service.API, _ service.Agenda) error {
			_, err := a.ObjectsByName(ctx, cal, []string{"a1@x.ics"})
			return err
		},
		"Changes": func(a service.API, _ service.Agenda) error { _, err := a.Changes(ctx, cal, ""); return err },
		"PutObject": func(a service.API, _ service.Agenda) error {
			_, err := a.PutObject(ctx, cal, "n@x.ics", vcal([]string{"UID:n@x", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600)}), service.Precondition{})
			return err
		},
		"DeleteObject": func(a service.API, _ service.Agenda) error {
			return a.DeleteObject(ctx, cal, "a1@x.ics", service.Precondition{})
		},
		"AddClient":    func(a service.API, _ service.Agenda) error { _, err := a.AddClient(ctx, "late"); return err },
		"Clients":      func(a service.API, _ service.Agenda) error { _, err := a.Clients(ctx); return err },
		"RemoveClient": func(a service.API, _ service.Agenda) error { _, err := a.RemoveClient(ctx, "default"); return err },
		"ShowClient":   func(a service.API, _ service.Agenda) error { _, err := a.ShowClient(ctx, "default"); return err },
		"RotateClient": func(a service.API, _ service.Agenda) error { _, err := a.RotateClient(ctx, "default"); return err },
		"Occurrences":  func(_ service.API, g service.Agenda) error { _, err := g.Occurrences(ctx, win); return err },
		"Event":        func(_ service.API, g service.Agenda) error { _, err := g.Event(ctx, ref); return err },
		"FreeBusy":     func(_ service.API, g service.Agenda) error { _, _, err := g.FreeBusy(ctx, win); return err },
		"CreateEvent": func(_ service.API, g service.Agenda) error {
			_, _, err := g.CreateEvent(ctx, cal, service.EventFields{Summary: ptr("x")})
			return err
		},
		"UpdateEvent": func(_ service.API, g service.Agenda) error {
			_, err := g.UpdateEvent(ctx, ref, "", service.ScopeSeries, service.EventFields{Summary: ptr("x")}, false)
			return err
		},
		"DeleteEvent": func(_ service.API, g service.Agenda) error { return g.DeleteEvent(ctx, ref, "", service.ScopeSeries) },
	}
	// Before the removal each answers something else than ErrNoAccount.
	for name, call := range calls {
		if name == "Serve" || name == "WatchSync" || name == "Logout" {
			continue // would run, or end the account
		}
		if err := call(A, G); errors.Is(err, service.ErrNoAccount) {
			t.Errorf("%s before the removal = %v", name, err)
		}
	}
	if _, err := r.h.Remove(ctx, r.id[userA], service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	for name, call := range calls {
		err := call(A, G)
		if !errors.Is(err, service.ErrNoAccount) {
			t.Errorf("%s after Remove = %v, want ErrNoAccount", name, err)
		}
		if err != nil && exitcode.From(err) != exitcode.Usage {
			t.Errorf("%s after Remove: exit code %d, want Usage", name, exitcode.From(err))
		}
	}
	if ok, wait := A.CheckClient(ctx, "anything", "192.0.2.1:1"); ok || wait != 0 {
		t.Errorf("CheckClient after Remove = %v, %v; want a refusal", ok, wait)
	}
	if _, _, err := r.h.Agenda(r.id[userA]); err == nil {
		t.Error("Agenda finds the removed account")
	}
	if st, err := B.Status(ctx); err != nil || st.Address != userB {
		t.Errorf("b after a's removal: %+v, %v", st, err)
	}
}

// caldavURL is the URL of the object name of calendar cal on the account's
// own URLs (the calendar's segment is the first 16 hex digits of sha256 of
// its ID).
func caldavURL(srv, id, cal, name string) string {
	sum := sha256.Sum256([]byte(cal))
	return srv + service.AccountPath(id) + "calendars/me/" + hex.EncodeToString(sum[:8]) + "/" + name
}

// One request path end to end, over two accounts: the CalDAV server, the
// Hub's Route, the account's API, the Local and the fake. A PUT lands on
// Proton, a multiget reads it back, a DELETE removes it, each on its own
// account, and one account's password does nothing on the other's URLs.
func TestCalDAVThroughTheHubWritesAndReadsEachAccount(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t)
	ctx := t.Context()
	ps := newServeRig(r.h)
	ps.serve(t, ps.options(true))
	for _, id := range r.id {
		ps.waitPassed(t, id, 0)
	}
	pass := func(user string) string {
		t.Helper()
		c, err := r.account(t, user).ShowClient(ctx, "default")
		if err != nil || c.Password == "" {
			t.Fatalf("ShowClient(%s): %v", user, err)
		}
		return c.Password
	}
	passA, passB := pass(userA), pass(userB)
	srv := httptest.NewServer(caldav.Handler(r.h, caldav.Options{}))
	t.Cleanup(srv.Close)
	do := func(method, url, password, body string, hdr ...string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("any", password)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	multiget := func(user, password string, names ...string) (int, string) {
		t.Helper()
		var hrefs strings.Builder
		for _, n := range names {
			hrefs.WriteString("<D:href>" + strings.TrimPrefix(caldavURL(srv.URL, r.id[user], r.cal[user], n), srv.URL) + "</D:href>")
		}
		body := `<?xml version="1.0"?><C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
			`<D:prop><D:getetag/><C:calendar-data/></D:prop>` + hrefs.String() + `</C:calendar-multiget>`
		return do("REPORT", caldavURL(srv.URL, r.id[user], r.cal[user], ""), password, body, "Depth", "1", "Content-Type", "text/xml; charset=utf-8")
	}
	ical := string(vcal([]string{"UID:new@x", "DTSTAMP:20270115T080000Z", "DTSTART:" + stamp(base), "DTEND:" + stamp(base+3600), "SUMMARY:Put by Thunderbird"}))
	ctype := []string{"Content-Type", "text/calendar; charset=utf-8"}

	// A multiget reads the seeded objects, each account its own.
	if code, body := multiget(userA, passA, "a1@x.ics", "a2@x.ics"); code != http.StatusMultiStatus ||
		!strings.Contains(body, "Seed a1@x") || !strings.Contains(body, "Seed a2@x") || strings.Contains(body, "Seed b1@x") {
		t.Errorf("a's multiget = %d\n%s", code, body)
	}
	if code, body := multiget(userB, passB, "b1@x.ics"); code != http.StatusMultiStatus || !strings.Contains(body, "Seed b1@x") {
		t.Errorf("b's multiget = %d\n%s", code, body)
	}

	// A PUT on a's URLs lands on a's calendar, and only there.
	if code, body := do(http.MethodPut, caldavURL(srv.URL, r.id[userA], r.cal[userA], "new@x.ics"), passA, ical, ctype...); code != http.StatusCreated {
		t.Fatalf("PUT = %d\n%s", code, body)
	}
	if n := len(r.rows(t, userA, "new@x")); n != 1 {
		t.Errorf("a's Proton calendar has %d rows of new@x after the PUT, want 1", n)
	}
	if n := len(r.rows(t, userB, "new@x")); n != 0 {
		t.Errorf("b's Proton calendar has %d rows of new@x, want 0", n)
	}
	if code, body := multiget(userA, passA, "new@x.ics"); code != http.StatusMultiStatus || !strings.Contains(body, "Put by Thunderbird") {
		t.Errorf("multiget after the PUT = %d\n%s", code, body)
	}

	// One account's password on the other's URLs writes and deletes nothing.
	if code, _ := do(http.MethodPut, caldavURL(srv.URL, r.id[userB], r.cal[userB], "evil@x.ics"), passA, strings.ReplaceAll(ical, "new@x", "evil@x"), ctype...); code != http.StatusUnauthorized {
		t.Errorf("a's password PUT on b's URLs = %d, want 401", code)
	}
	if code, _ := do(http.MethodDelete, caldavURL(srv.URL, r.id[userA], r.cal[userA], "a2@x.ics"), passB, ""); code != http.StatusUnauthorized {
		t.Errorf("b's password DELETE on a's URLs = %d, want 401", code)
	}
	if n := len(r.rows(t, userB, "evil@x")); n != 0 {
		t.Errorf("b's calendar has %d rows of evil@x", n)
	}
	if n := len(r.rows(t, userA, "a2@x")); n != 1 {
		t.Errorf("a2@x has %d rows after a refused DELETE, want 1", n)
	}

	// A DELETE removes the row on Proton, and the multiget then finds it gone.
	if code, body := do(http.MethodDelete, caldavURL(srv.URL, r.id[userA], r.cal[userA], "a2@x.ics"), passA, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d\n%s", code, body)
	}
	if n := len(r.rows(t, userA, "a2@x")); n != 0 {
		t.Errorf("a2@x has %d rows on Proton after the DELETE, want 0", n)
	}
	if code, body := multiget(userA, passA, "a2@x.ics", "a1@x.ics"); code != http.StatusMultiStatus ||
		strings.Contains(body, "Seed a2@x") || !strings.Contains(body, "Seed a1@x") || !strings.Contains(body, "404") {
		t.Errorf("multiget after the DELETE = %d; want a2 a 404 and a1 still there\n%s", code, body)
	}
	if code, _ := do(http.MethodDelete, caldavURL(srv.URL, r.id[userB], r.cal[userB], "b1@x.ics"), passB, ""); code != http.StatusNoContent {
		t.Errorf("b's DELETE = %d, want 204", code)
	}
	if n := len(r.rows(t, userB, "b1@x")); n != 0 {
		t.Errorf("b1@x has %d rows after b's DELETE, want 0", n)
	}
	if n := len(r.rows(t, userA, "a1@x")); n != 1 {
		t.Errorf("a's a1@x has %d rows after b's DELETE, want 1", n)
	}
}
