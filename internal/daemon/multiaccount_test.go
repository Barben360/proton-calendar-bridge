package daemon_test

// Two accounts through the real daemon (listener, control socket, Hub) on
// one fake Proton, and an M5 install upgraded in place: go-webdav is the
// independent client (test-only dependency), Thunderbird's own request body
// is sent verbatim for the sync-collection.

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	webdavcaldav "github.com/emersion/go-webdav/caldav"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const (
	userB     = "second-account"
	passwordB = "second-account-password"
	// base is a time in 2027: the events' start.
	base = int64(1_800_000_000)
)

// card is the VEVENT body of an event the fake serves.
func card(uid, summary string) string {
	return strings.Join([]string{"UID:" + uid, "DTSTAMP:20270115T080000Z", "DTSTART:20270115T080000Z", "DTEND:20270115T090000Z", "SUMMARY:" + summary}, "\r\n")
}

func seed(f *protontest.Fake, username, name string, uids ...string) (calID string) {
	calID = f.AddCalendar(username, protontest.Calendar{Name: name})
	for _, uid := range uids {
		f.AddEvent(calID, protontest.Event{UID: uid, Start: base, End: base + 3600, Card: card(uid, name+" "+uid)})
	}
	return calID
}

// clockEnv is an env whose Hub's clock moves, so that two logins clear the
// login guard's interval. The fake holds the first user and userB.
func clockEnv(t *testing.T) (*env, *atomic.Int64) {
	t.Helper()
	f := protontest.New(t, protontest.Options{})
	f.AddUser(userB, passwordB)
	root, err := os.MkdirTemp("", "pcbd") // short: the control socket's path must fit sun_path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	e := &env{f: f, dirs: paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "state")}}
	e.cfg = config.Defaults(e.dirs)
	e.cfg.Proton.BaseURL = f.URL
	e.cfg.CalDAV.Listen = "127.0.0.1:0"
	return e, new(atomic.Int64)
}

func (e *env) openHub(t *testing.T, offset *atomic.Int64) {
	t.Helper()
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: e.dirs, Config: e.cfg,
		Now:   func() time.Time { return time.Now().Add(time.Duration(offset.Load())) },
		Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	e.hub = hub
}

// add logs username in as a new account of the Hub, gives it a CalDAV client
// and fills its cache: its local ID and the client's password.
func (e *env) add(t *testing.T, offset *atomic.Int64, username, password string) (id, clientPassword string) {
	t.Helper()
	offset.Add(int64(loginguard.MinInterval + time.Second))
	res, err := e.hub.Login(t.Context(), service.LoginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("Login(%s): %v", username, err)
	}
	a, _, err := e.hub.Account(res.Account)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := a.AddClient(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(t.Context(), service.SyncRequest{}); err != nil {
		t.Fatalf("Sync(%s): %v", username, err)
	}
	return res.Account, sec.Password
}

func davClient(t *testing.T, url, password string) *webdavcaldav.Client {
	t.Helper()
	c, err := webdavcaldav.NewClient(webdav.HTTPClientWithBasicAuth(http.DefaultClient, "thunderbird", password), url)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// calendars discovers the account's calendars from its base URL, as a client
// given only that URL does.
func calendars(t *testing.T, c *webdavcaldav.Client) []webdavcaldav.Calendar {
	t.Helper()
	p, err := c.FindCurrentUserPrincipal(t.Context())
	if err != nil {
		t.Fatalf("FindCurrentUserPrincipal: %v", err)
	}
	hs, err := c.FindCalendarHomeSet(t.Context(), p)
	if err != nil {
		t.Fatalf("FindCalendarHomeSet: %v", err)
	}
	cals, err := c.FindCalendars(t.Context(), hs)
	if err != nil {
		t.Fatalf("FindCalendars: %v", err)
	}
	return cals
}

func names(cals []webdavcaldav.Calendar) []string {
	var out []string
	for _, c := range cals {
		out = append(out, c.Name)
	}
	slices.Sort(out)
	return out
}

func parse(t *testing.T, text string) *ical.Calendar {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(text)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

func TestTwoAccountsSideBySide(t *testing.T) {
	t.Parallel()
	e, offset := clockEnv(t)
	alpha := seed(e.f, e.f.Username, "Alpha", "a1@x")
	beta := seed(e.f, userB, "Beta", "b1@x")
	e.openHub(t, offset)
	idA, pwA := e.add(t, offset, e.f.Username, e.f.Password)
	idB, pwB := e.add(t, offset, userB, passwordB)
	if idA == idB || pwA == pwB {
		t.Fatalf("the accounts share an identity: ids %q %q", idA, idB)
	}
	r := e.start(t, e.hub, nil)
	a, b := davClient(t, r.info.AccountURL(idA), pwA), davClient(t, r.info.AccountURL(idB), pwB)

	// Each URL shows only its own account's calendars.
	calsA, calsB := calendars(t, a), calendars(t, b)
	if got := names(calsA); !slices.Equal(got, []string{"Alpha"}) {
		t.Fatalf("a's calendars = %v, want [Alpha]", got)
	}
	if got := names(calsB); !slices.Equal(got, []string{"Beta"}) {
		t.Fatalf("b's calendars = %v, want [Beta]", got)
	}
	// A password opens its own account's URL only.
	for _, c := range []struct{ url, password string }{{r.info.AccountURL(idA), pwB}, {r.info.AccountURL(idB), pwA}} {
		resp, err := r.propfind(t.Context(), c.url, c.password)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("PROPFIND %s with the other account's password = %d, want 401", c.url, resp.StatusCode)
		}
	}

	// A PUT through a reaches a's Proton calendar and never b's.
	put := calsA[0].Path + "new-from-a@x.ics"
	if _, err := a.PutCalendarObject(t.Context(), put, parse(t, strings.Join([]string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN", "BEGIN:VEVENT",
		"UID:new-from-a@x", "DTSTAMP:20270115T080000Z", "DTSTART:20270116T080000Z", "DTEND:20270116T090000Z", "SUMMARY:Put by a",
		"END:VEVENT", "END:VCALENDAR", ""}, "\r\n"))); err != nil {
		t.Fatalf("PUT through a: %v", err)
	}
	var written []string
	for _, c := range e.f.SyncCalls() {
		written = append(written, c.CalendarID)
	}
	if !slices.Equal(written, []string{alpha}) {
		t.Errorf("Proton received writes for calendars %v, want only a's [%s] (b's is %s)", written, alpha, beta)
	}
	got, err := a.GetCalendarObject(t.Context(), put)
	if err != nil {
		t.Fatalf("GET of the new event through a: %v", err)
	}
	if s, _ := got.Data.Events()[0].Props.Text(ical.PropSummary); s != "Put by a" {
		t.Errorf("the new event's summary = %q", s)
	}
	inB := syncCollection(t, r.info.Base, calsB[0].Path, pwB, "")
	if len(inB.etags) != 1 || !strings.HasSuffix(slices.Collect(maps.Keys(inB.etags))[0], "b1@x.ics") {
		t.Errorf("b's calendar holds %v, want only its own b1@x", inB.etags)
	}

	// b signed out over the control socket: a keeps answering, b's URL is 503.
	c, err := ctl.Dial(t.Context(), e.dirs.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Logout(t.Context(), idB, service.LogoutRequest{}); err != nil {
		t.Fatalf("sign b out: %v", err)
	}
	resp, err := r.propfind(t.Context(), r.info.AccountURL(idA), pwA)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("a's URL after b signed out = %d, want 207", resp.StatusCode)
	}
	if got := names(calendars(t, a)); !slices.Equal(got, []string{"Alpha"}) {
		t.Errorf("a's calendars after b signed out = %v", got)
	}
	resp, err = r.propfind(t.Context(), r.info.AccountURL(idB), pwB)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("b's URL after it signed out = %d, want 503", resp.StatusCode)
	}
}

// m5Router serves one Local on the compatibility URLs, as M5's pcb did.
type m5Router struct{ l *service.Local }

func (r m5Router) Route(ctx context.Context, accountID, password, remote string) (service.API, string, time.Duration, error) {
	if ok, wait := r.l.CheckClient(ctx, password, remote); !ok {
		return nil, "", wait, nil
	}
	return r.l, "m5", 0, nil
}

// thunderbirdSync is Thunderbird's sync-collection body.
func thunderbirdSync(token string) string {
	tok := "<sync-token/>"
	if token != "" {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(token))
		tok = "<sync-token>" + b.String() + "</sync-token>"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<sync-collection xmlns="DAV:">` + tok + `
<sync-level>1</sync-level><prop><getcontenttype/><getetag/></prop></sync-collection>`
}

type syncAnswer struct {
	status int
	token  string
	etags  map[string]string // href → ETag of the changed objects
	hrefs  int               // every response, deletions included
}

func syncCollection(t *testing.T, base, calPath, password, token string) syncAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "REPORT", base+calPath, strings.NewReader(thunderbirdSync(token)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	req.SetBasicAuth("thunderbird", password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var ms struct {
		Token     string `xml:"sync-token"`
		Responses []struct {
			Href     string `xml:"href"`
			Propstat []struct {
				ETag string `xml:"prop>getetag"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(body, &ms); err != nil {
		t.Fatalf("sync-collection answered %d with %q: %v", resp.StatusCode, body, err)
	}
	out := syncAnswer{status: resp.StatusCode, token: ms.Token, etags: map[string]string{}, hrefs: len(ms.Responses)}
	for _, r := range ms.Responses {
		for _, p := range r.Propstat {
			if p.ETag != "" {
				out.etags[r.Href] = p.ETag
			}
		}
	}
	return out
}

func TestMigratedThunderbirdKeepsItsTokens(t *testing.T) {
	t.Parallel()
	e, offset := clockEnv(t)
	seed(e.f, e.f.Username, "Work", "w1@x", "w2@x")

	// An M5 install: one Local on the state root, which is M5's layout
	// (state.json, secrets.sealed, clients.json and pcb.db at the root). M5
	// never wrote the address to state.json.
	l, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg,
		Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Login(t.Context(), service.LoginRequest{Username: e.f.Username, Password: e.f.Password}); err != nil {
		t.Fatalf("M5 login: %v", err)
	}
	sec, err := l.AddClient(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Sync(t.Context(), service.SyncRequest{}); err != nil {
		t.Fatalf("M5 sync: %v", err)
	}
	stateFile := filepath.Join(e.dirs.State, "state.json")
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]json.RawMessage
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	delete(st, "address")
	if raw, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// What Thunderbird holds: the calendar's URL, its token and ETags, from
	// M5's compatibility URLs.
	m5 := httptest.NewServer(caldav.Handler(m5Router{l}, caldav.Options{}))
	t.Cleanup(m5.Close)
	// M5's home set: Thunderbird was configured with it, whatever / says now.
	cals, err := davClient(t, m5.URL, sec.Password).FindCalendars(t.Context(), "/dav/calendars/me/")
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].Name != "Work" {
		t.Fatalf("M5 calendars = %v", names(cals))
	}
	calPath := cals[0].Path
	before := syncCollection(t, m5.URL, calPath, sec.Password, "")
	if before.status != http.StatusMultiStatus || before.token == "" || len(before.etags) != 2 {
		t.Fatalf("M5 sync-collection = %+v, want a token and two ETags", before)
	}
	m5.Close()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// The upgrade: M6 opens the same root (it migrates) and serves it.
	e.openHub(t, offset)
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Fatalf("the root state.json is still there after the upgrade: %v", err)
	}
	r := e.start(t, e.hub, nil)
	after := syncCollection(t, r.info.Base, calPath, sec.Password, before.token)
	if after.status != http.StatusMultiStatus || after.hrefs != 0 {
		t.Errorf("sync-collection with the M5 token after the upgrade = %+v, want 207 and the empty delta", after)
	}
	// The empty delta is the token's doing: a token nobody issued is refused.
	if bogus := syncCollection(t, r.info.Base, calPath, sec.Password, "http://pcb/sync/bogus"); bogus.status == http.StatusMultiStatus {
		t.Errorf("a made-up token answered %+v, want a refusal", bogus)
	}
	fresh := syncCollection(t, r.info.Base, calPath, sec.Password, "")
	if !maps.Equal(fresh.etags, before.etags) {
		t.Errorf("ETags after the upgrade = %v, want the M5 ones %v", fresh.etags, before.etags)
	}
}
