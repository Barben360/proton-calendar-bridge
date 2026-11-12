package caldav_test

// The routing of design §4.4 through a real Hub of two accounts on one fake
// Proton: each account under its own URLs (/dav/<local-id>/, realm
// "pcb <local-id>"), the compatibility URLs routed by the password alone,
// one failure limiter for both, and the accounts that cannot answer.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

const (
	routeUserB = "routing-b@x"
	routePassB = "routing-test-password-b"
	// routeWait bounds a wait for the Hub's serve loops.
	routeWait = 30 * time.Second
)

// skewClock is the wall clock moved ahead by hand: the second login of a
// rig passes the login guard's interval without waiting for it.
type skewClock struct {
	mu   sync.Mutex
	skew time.Duration
}

func (c *skewClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.skew)
}

func (c *skewClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skew += d
}

// waitSleep is a Sleep that lasts until its context ends: a serve loop runs
// one pass, then waits.
func waitSleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// hubRig is a Hub of two accounts on one fake Proton, a and b, each logged
// in and synced, with a calendar of its own and a CalDAV client, behind
// caldav.Handler.
type hubRig struct {
	hub          *service.Hub
	root         string // the state root
	h            http.Handler
	a, b         string // local IDs
	passA, passB string // their CalDAV clients' passwords
	calA, calB   string // their calendars' IDs
}

func newHubRig(t *testing.T) *hubRig {
	t.Helper()
	clk := &skewClock{}
	f := protontest.New(t, protontest.Options{Now: clk.Now})
	f.AddUser(routeUserB, routePassB)
	r := &hubRig{calA: f.AddCalendar(f.Username, protontest.Calendar{Name: "Calendar of A"}),
		calB: f.AddCalendar(routeUserB, protontest.Calendar{Name: "Calendar of B"})}
	f.AddEvent(r.calA, protontest.Event{UID: "a@x", Start: base, End: base + 3600, Card: card("a@x", "Event of A", base)})
	f.AddEvent(r.calB, protontest.Event{UID: "b@x", Start: base, End: base + 3600, Card: card("b@x", "Event of B", base)})
	dir := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "state")}
	cfg := config.Defaults(dirs)
	cfg.Proton.BaseURL = f.URL
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: dirs, Config: cfg, Now: clk.Now, Sleep: waitSleep}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	r.hub, r.root = hub, dirs.State
	add := func(username, password string) (id, pass string) {
		t.Helper()
		clk.advance(loginguard.MinInterval + time.Second)
		res, err := hub.Login(t.Context(), service.LoginRequest{Username: username, Password: password})
		if err != nil {
			t.Fatalf("Login(%s): %v", username, err)
		}
		api, _, err := hub.Account(res.Account)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := api.Sync(t.Context(), service.SyncRequest{}); err != nil {
			t.Fatalf("Sync(%s): %v", username, err)
		}
		sec, err := api.AddClient(t.Context(), "Thunderbird")
		if err != nil {
			t.Fatal(err)
		}
		return res.Account, sec.Password
	}
	r.a, r.passA = add(f.Username, f.Password)
	r.b, r.passB = add(routeUserB, routePassB)
	r.h = caldav.Handler(hub, caldav.Options{})
	return r
}

// as serves method on target with password (none when empty) from remote
// ("" keeps httptest's).
func (r *hubRig) as(password, remote, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	req := call(method, target, body, hdr...)
	if password != "" {
		req.SetBasicAuth("anyone", password)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	return serve(r.h, req)
}

// space is the namespace of local ID id: its home set and its calendars.
func space(id string) urlBase {
	dav := service.AccountPath(id)
	return urlBase{root: dav, dav: dav, principal: dav + "principals/me/", home: dav + "calendars/me/", challenge: realmOf(id)}
}

func realmOf(id string) string { return `Basic realm="pcb ` + id + `", charset="UTF-8"` }

// homeSet answers PROPFIND Depth 1 on home with password: the hrefs it
// lists and the body, which must be a 207.
func (r *hubRig) homeSet(t *testing.T, password, home string) (map[string]bool, string) {
	t.Helper()
	ms := parseMS(t, r.as(password, "", "PROPFIND", home, tbHomeSet, "Depth", "1"))
	out := map[string]bool{}
	for _, res := range ms.responses {
		out[res.href] = true
	}
	return out, ms.raw
}

// want401 checks a refusal: 401, the challenge of realm, no body.
func want401(t *testing.T, rec *httptest.ResponseRecorder, what, realm string) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("%s: status %d, want 401; body:\n%s", what, rec.Code, rec.Body)
		return
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != realm {
		t.Errorf("%s: WWW-Authenticate %q, want %q", what, got, realm)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("%s: body %q, want none", what, rec.Body)
	}
}

// want503 checks an account that cannot answer: 503, Retry-After 30, no
// body and no challenge.
func want503(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%s: status %d, want 503; body:\n%s", what, rec.Code, rec.Body)
		return
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("%s: Retry-After %q, want 30", what, got)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("%s: body %q, WWW-Authenticate %q, want neither", what, rec.Body, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestPrefixedURLServesOnlyThatAccount(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	for _, tc := range []struct {
		id, pass, mine, other string
	}{{r.a, r.passA, r.calA, r.calB}, {r.b, r.passB, r.calB, r.calA}} {
		sp := space(tc.id)
		hrefs, raw := r.homeSet(t, tc.pass, sp.home)
		if len(hrefs) != 2 || !hrefs[sp.home] || !hrefs[sp.calPath(tc.mine)] {
			t.Errorf("%s's home set lists %v, want its home and its own calendar alone:\n%s", tc.id, hrefs, raw)
		}
		for h := range hrefs {
			if !strings.HasPrefix(h, sp.dav) {
				t.Errorf("%s's home set lists %s, outside %s", tc.id, h, sp.dav)
			}
		}
		// The other account's calendar is not there under this prefix.
		if rec := r.as(tc.pass, "", "PROPFIND", sp.calPath(tc.other), tbServerInfo, "Depth", "0"); rec.Code != http.StatusNotFound {
			t.Errorf("the other account's calendar under %s: %d, want 404", sp.dav, rec.Code)
		}
	}
	// The object of a, read on a's URL with a's password.
	sp := space(r.a)
	rec := r.as(r.passA, "", "GET", sp.objPath(r.calA, "a@x"), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SUMMARY:Event of A") {
		t.Errorf("GET a's object: %d\n%s", rec.Code, rec.Body)
	}
}

// Review focus 2: account B's password on account A's URLs is a 401 with
// A's realm, never B's data and never A's.
func TestOtherAccountsPasswordIsRefusedOnPrefixedURL(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	sa := space(r.a)
	for i, tc := range []struct{ method, path, body string }{
		{"PROPFIND", sa.root, tbDetect},
		{"PROPFIND", strings.TrimSuffix(sa.root, "/"), tbDetect},
		{"PROPFIND", sa.principal, tbDetect},
		{"PROPFIND", sa.home, tbHomeSet},
		{"PROPFIND", sa.calPath(r.calA), tbServerInfo},
		{"PROPFIND", sa.calPath(r.calB), tbServerInfo},
		{"REPORT", sa.calPath(r.calA), tbSync("")},
		{"REPORT", sa.calPath(r.calB), tbSync("")},
		{"GET", sa.objPath(r.calA, "a@x"), ""},
		{"GET", sa.objPath(r.calB, "b@x"), ""},
		{"PUT", sa.objPath(r.calA, "new@x"), string(ics("new@x", "Planted"))},
		{"DELETE", sa.objPath(r.calA, "a@x"), ""},
	} {
		what := "b's password on " + tc.method + " " + tc.path
		// Each from its own IP: the limiter would block the sixth.
		want401(t, r.as(r.passB, fmt.Sprintf("198.51.100.%d:1", i+1), tc.method, tc.path, tc.body, "Depth", "1", "Content-Type", "text/calendar"), what, realmOf(r.a))
		want401(t, r.as("", "", tc.method, tc.path, tc.body, "Depth", "1"), "no credentials on "+tc.method+" "+tc.path, realmOf(r.a))
	}
	// Nothing was written: a's event is still there for a.
	rec := r.as(r.passA, "", "GET", sa.objPath(r.calA, "a@x"), "")
	if rec.Code != http.StatusOK {
		t.Errorf("a's object after b's refused writes: %d", rec.Code)
	}
	if rec := r.as(r.passA, "", "GET", sa.objPath(r.calA, "new@x"), ""); rec.Code != http.StatusNotFound {
		t.Errorf("the object b's refused PUT named: %d, want 404", rec.Code)
	}
}

func TestCompatibilityURLRoutesByPassword(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	for _, tc := range []struct{ who, pass, mine string }{{"a", r.passA, r.calA}, {"b", r.passB, r.calB}} {
		hrefs, raw := r.homeSet(t, tc.pass, home)
		if len(hrefs) != 2 || !hrefs[home] || !hrefs[calPath(tc.mine)] {
			t.Errorf("%s's password on %s lists %v, want the home and %s's own calendar alone:\n%s", tc.who, home, hrefs, tc.who, raw)
		}
	}
	rec := r.as(r.passB, "", "GET", objPath(r.calB, "b@x"), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SUMMARY:Event of B") {
		t.Errorf("GET b's object on the compatibility URL: %d\n%s", rec.Code, rec.Body)
	}
	if rec := r.as(r.passA, "", "GET", objPath(r.calB, "b@x"), ""); rec.Code != http.StatusNotFound {
		t.Errorf("a's password on b's object: %d, want 404", rec.Code)
	}
	want401(t, r.as("", "", "PROPFIND", home, tbHomeSet, "Depth", "1"), "no credentials on "+home, challenge)
	want401(t, r.as("not-a-password", "", "PROPFIND", home, tbHomeSet, "Depth", "1"), "an unknown password on "+home, challenge)
}

func TestRootPointsToPrefixedPrincipal(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	for _, tc := range []struct{ id, pass, path string }{
		{r.b, r.passB, "/"}, {r.a, r.passA, "/"}, {r.b, r.passB, space(r.b).root},
	} {
		ms := parseMS(t, r.as(tc.pass, "", "PROPFIND", tc.path, tbDetect, "Depth", "0"))
		if got := ms.get(t, tc.path).prop(t, nsD, "current-user-principal").hrefOf(t); got != space(tc.id).principal {
			t.Errorf("PROPFIND %s as %s: current-user-principal %q, want %q", tc.path, tc.id, got, space(tc.id).principal)
		}
	}
	rec := r.as(r.passB, "", "PROPFIND", "/.well-known/caldav", tbDetect, "Depth", "0")
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != space(r.b).principal {
		t.Errorf("well-known as b: %d to %q, want 308 to %s", rec.Code, rec.Header().Get("Location"), space(r.b).principal)
	}
}

func TestCalDAVOffAccountIs401(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	if _, err := r.hub.SetCalDAV(t.Context(), r.a, false); err != nil {
		t.Fatal(err)
	}
	want401(t, r.as(r.passA, "", "PROPFIND", space(r.a).home, tbHomeSet, "Depth", "1"), "a's password on a's URL, CalDAV off", realmOf(r.a))
	want401(t, r.as(r.passA, "", "PROPFIND", home, tbHomeSet, "Depth", "1"), "a's password on the compatibility URL, CalDAV off", challenge)
	if hrefs, raw := r.homeSet(t, r.passB, space(r.b).home); !hrefs[space(r.b).calPath(r.calB)] {
		t.Errorf("b's URL with a off:\n%s", raw)
	}
	if hrefs, raw := r.homeSet(t, r.passB, home); !hrefs[calPath(r.calB)] {
		t.Errorf("the compatibility URL as b with a off:\n%s", raw)
	}
	if _, err := r.hub.SetCalDAV(t.Context(), r.a, true); err != nil {
		t.Fatal(err)
	}
	if hrefs, raw := r.homeSet(t, r.passA, space(r.a).home); !hrefs[space(r.a).calPath(r.calA)] {
		t.Errorf("a's URL once CalDAV is on again:\n%s", raw)
	}
}

// An account whose clients.json cannot be loaded answers 503 on its URLs,
// with no limiter charge; on the compatibility URLs the other accounts
// still answer their passwords, and a password no account holds is a 503
// while a store is broken (it may be that account's).
func TestBrokenClientsFile(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	path := filepath.Join(store.AccountDir(r.root, r.a), "clients.json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	const remote = "203.0.113.7:1"
	for i := range 3 {
		want503(t, r.as(r.passA, remote, "PROPFIND", space(r.a).principal, tbDetect, "Depth", "0"), "a's password on a's URL, a's store broken")
		want503(t, r.as(r.passB, remote, "PROPFIND", space(r.a).principal, tbDetect, "Depth", "0"), "b's password on a's URL, a's store broken")
		want503(t, r.as("not-a-password", remote, "PROPFIND", home, tbHomeSet, "Depth", "1"), "an unknown password on the compatibility URL")
		if hrefs, raw := r.homeSet(t, r.passB, home); !hrefs[calPath(r.calB)] {
			t.Fatalf("round %d: b's password on the compatibility URL, a's store broken:\n%s", i, raw)
		}
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	// Nine refusals were not charged to remote: a's password passes from it.
	if hrefs, raw := r.homeSet(t, r.passA, space(r.a).home); !hrefs[space(r.a).calPath(r.calA)] {
		t.Errorf("a's URL once clients.json is back:\n%s", raw)
	}
	if rec := r.as(r.passA, remote, "PROPFIND", space(r.a).principal, tbDetect, "Depth", "0"); rec.Code != http.StatusMultiStatus {
		t.Errorf("a's password from the remote the broken store refused: %d, want 207 (no limiter charge)", rec.Code)
	}
	// With every store sound an unknown password is a 401 again.
	want401(t, r.as("not-a-password", "", "PROPFIND", home, tbHomeSet, "Depth", "1"), "an unknown password, every store sound", challenge)
}

// serveHub runs the rig's Hub.Serve until the test ends; states gets each
// account's loop states.
func (r *hubRig) serveHub(t *testing.T) (states func(id string) []service.ServeState, changed <-chan struct{}) {
	t.Helper()
	var mu sync.Mutex
	got := map[string][]service.ServeState{}
	poke := make(chan struct{}, 1)
	signal := func() {
		select {
		case poke <- struct{}{}:
		default:
		}
	}
	r.hub.OnChange(signal)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- r.hub.Serve(ctx, service.HubServeOptions{
			ServeOptions: service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour},
			OnState: func(id string, s service.ServeState) {
				mu.Lock()
				got[id] = append(got[id], s)
				mu.Unlock()
				signal()
			},
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Hub.Serve = %v, want nil", err)
			}
		case <-time.After(routeWait):
			t.Error("Hub.Serve did not return after its context ended")
		}
	})
	return func(id string) []service.ServeState {
		mu.Lock()
		defer mu.Unlock()
		return append([]service.ServeState(nil), got[id]...)
	}, poke
}

// waitUntil waits for cond, looked at each time poke fires.
func waitUntil(t *testing.T, what string, poke <-chan struct{}, cond func() bool) {
	t.Helper()
	deadline := time.After(routeWait)
	for !cond() {
		select {
		case <-poke:
		case <-deadline:
			t.Fatalf("never: %s", what)
		}
	}
}

func lastIs(states []service.ServeState, s service.ServeState) bool {
	return len(states) > 0 && states[len(states)-1] == s
}

func TestParkedAccountIs503(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	if err := os.WriteFile(cache.Path(store.AccountDir(r.root, r.a)), []byte("not a database, not at all, not even close........"), 0o600); err != nil {
		t.Fatal(err)
	}
	states, poke := r.serveHub(t)
	waitUntil(t, "a parked", poke, func() bool {
		for _, info := range r.hub.Accounts() {
			if info.ID == r.a {
				return info.Stopped != nil
			}
		}
		return false
	})
	waitUntil(t, "b serving", poke, func() bool { return lastIs(states(r.b), service.ServeServing) })
	for _, p := range []string{space(r.a).principal, space(r.a).root, "/", principal} {
		want503(t, r.as(r.passA, "", "PROPFIND", p, tbDetect, "Depth", "0"), "a's password on "+p+", a parked")
	}
	if hrefs, raw := r.homeSet(t, r.passB, space(r.b).home); !hrefs[space(r.b).calPath(r.calB)] {
		t.Errorf("b's URL with a parked:\n%s", raw)
	}
	want401(t, r.as(r.passB, "", "PROPFIND", space(r.a).principal, tbDetect, "Depth", "0"), "b's password on parked a's URL", realmOf(r.a))
}

// A signed-out account keeps its CalDAV clients; while its loop waits for
// a login its password gets a 503, even where no read of the cache is
// needed.
func TestSignedOutAccountIs503(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	states, poke := r.serveHub(t)
	waitUntil(t, "a serving", poke, func() bool { return lastIs(states(r.a), service.ServeServing) })
	if _, err := r.hub.SignOut(t.Context(), r.a, service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a waiting for a login", poke, func() bool { return lastIs(states(r.a), service.ServeWaitingForLogin) })
	for _, p := range []string{space(r.a).principal, space(r.a).root, "/", principal} {
		want503(t, r.as(r.passA, "", "PROPFIND", p, tbDetect, "Depth", "0"), "a's password on "+p+", a signed out")
	}
	if hrefs, raw := r.homeSet(t, r.passB, space(r.b).home); !hrefs[space(r.b).calPath(r.calB)] {
		t.Errorf("b's URL with a signed out:\n%s", raw)
	}
}

// One limiter for every account, charged once per refused request: five
// failures on a's URL block b's from the same IP, and five on the
// compatibility URLs (two accounts tried each time) are five, not ten.
func TestLimiterIsSharedAcrossAccounts(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	const one, two = "192.0.2.10:1", "192.0.2.20:2"
	for range 5 {
		want401(t, r.as("wrong", one, "PROPFIND", space(r.a).principal, tbDetect, "Depth", "0"), "wrong password on a's URL", realmOf(r.a))
		want401(t, r.as("wrong", two, "PROPFIND", principal, tbDetect, "Depth", "0"), "wrong password on the compatibility URL", challenge)
	}
	for _, tc := range []struct{ remote, pass, path string }{
		{one, r.passB, space(r.b).principal}, {two, r.passA, principal}, {two, r.passB, space(r.b).principal},
	} {
		rec := r.as(tc.pass, tc.remote, "PROPFIND", tc.path, tbDetect, "Depth", "0")
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || rec.Header().Get("Retry-After") == "0" {
			t.Errorf("a sixth request from %s on %s: %d, Retry-After %q; want 503 with a wait", tc.remote, tc.path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	// Another IP is not blocked.
	if rec := r.as(r.passB, "192.0.2.30:3", "PROPFIND", space(r.b).principal, tbDetect, "Depth", "0"); rec.Code != http.StatusMultiStatus {
		t.Errorf("b from another IP: %d, want 207", rec.Code)
	}
}

// A local ID no account holds is refused like a wrong password (401 with
// its realm, a limiter charge), whatever the password: a probe cannot tell
// which IDs exist. A first segment that is no local ID is outside every
// account's namespace: a compatibility path, realm pcb, 404 once
// authenticated.
func TestUnknownAccountIs401(t *testing.T) {
	t.Parallel()
	r := newHubRig(t)
	const remote = "192.0.2.40:4"
	if r.a == "0f0f0f0f" || r.b == "0f0f0f0f" || r.a == "00000000" || r.b == "00000000" {
		t.Skip("the Hub drew a local ID the test takes for unknown")
	}
	unknown := space("0f0f0f0f")
	want401(t, r.as("", remote, "PROPFIND", unknown.principal, tbDetect, "Depth", "0"), "no credentials on an unknown ID", realmOf("0f0f0f0f"))
	for _, p := range []string{unknown.principal, unknown.root, unknown.home, space("00000000").home} {
		want401(t, r.as(r.passA, remote, "PROPFIND", p, tbDetect, "Depth", "0"), "a's password on "+p, realmOf(strings.Split(p, "/")[2]))
	}
	for _, p := range []string{"/dav/zzzzzzzz/principals/me/", "/dav/" + strings.ToUpper(r.a) + "/principals/me/", "/dav/" + r.a + "0/principals/me/", "/dav/0f0f0f0/"} {
		want401(t, r.as("", "", "PROPFIND", p, tbDetect, "Depth", "0"), "no credentials on "+p, challenge)
		if rec := r.as(r.passB, "", "PROPFIND", p, tbDetect, "Depth", "0"); rec.Code != http.StatusNotFound {
			t.Errorf("b's password on %s: %d, want 404", p, rec.Code)
		}
	}
	// The four refusals above were charged: one more and remote is blocked.
	want401(t, r.as("wrong", remote, "PROPFIND", principal, tbDetect, "Depth", "0"), "a fifth failure", challenge)
	if rec := r.as(r.passA, remote, "PROPFIND", space(r.a).principal, tbDetect, "Depth", "0"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a's password after five failures from %s: %d, want 503", remote, rec.Code)
	}
}

// liar is a Router that sends every authenticated request to the stub as
// account id, whatever the URL asked for.
type liar struct {
	api service.API
	id  string
}

func (l liar) Route(context.Context, string, string, string) (service.API, string, time.Duration, error) {
	return l.api, l.id, 0, nil
}

// The handler never serves an account's URL with another account's API,
// even from a Router that says so.
func TestRouteToAnotherAccountIsRefused(t *testing.T) {
	t.Parallel()
	s := newStub()
	h := caldav.Handler(liar{api: s, id: "ffffffff"}, caldav.Options{})
	want401(t, do(h, "PROPFIND", account.home, tbHomeSet, "Depth", "1"), "account "+stubID+"'s URL routed to ffffffff", account.challenge)
	if _, reads := s.counts(); reads != 0 {
		t.Errorf("%d service reads for a request routed to another account", reads)
	}
	// On the compatibility URLs the Router's choice stands.
	ms := parseMS(t, do(h, "PROPFIND", "/", tbDetect, "Depth", "0"))
	if got := ms.get(t, "/").prop(t, nsD, "current-user-principal").hrefOf(t); got != "/dav/ffffffff/principals/me/" {
		t.Errorf("current-user-principal %q, want ffffffff's", got)
	}
}

// A multiget names objects in the request's own namespace: an href of the
// other namespace is a response-level 404, as any href that names nothing
// of the calendar.
func TestMultigetHrefsOfAnotherNamespaceAre404(t *testing.T) {
	t.Parallel()
	h := handler(newStub(), caldav.Options{})
	for _, tc := range []struct{ req, other urlBase }{{compat, account}, {account, compat}} {
		ms := parseMS(t, do(h, "REPORT", tc.req.calPath(workID), tbMultiget(tc.req.tbHref(workID, plainUID), tc.other.tbHref(workID, lastUID)), "Depth", "1"))
		if got := ms.get(t, tc.req.objPath(workID, plainUID)); got.status != "" {
			t.Errorf("%s: its own href answered %q", tc.req.name, got.status)
		}
		if got := ms.get(t, tc.other.objPath(workID, lastUID)); got.status != "HTTP/1.1 404 Not Found" || len(got.props) != 0 {
			t.Errorf("%s: the %s href answered %q with %d properties, want a response-level 404", tc.req.name, tc.other.name, got.status, len(got.props))
		}
	}
}
