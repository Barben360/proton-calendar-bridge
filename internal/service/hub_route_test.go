package service_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// routeEnv is a Hub with accounts a and b logged in, and the password of
// each one's default CalDAV client.
func routeEnv(t *testing.T) (h *service.Hub, a, b, passA, passB string) {
	t.Helper()
	_, h, a, b, passA, passB = routeEnvDirs(t)
	return h, a, b, passA, passB
}

// routeEnvDirs is routeEnv with its env.
func routeEnvDirs(t *testing.T) (e *hubEnv, h *service.Hub, a, b, passA, passB string) {
	t.Helper()
	e = newHubEnv(t)
	h = e.open(t)
	a, b = e.loginBoth(t, h)
	pass := func(id string) string {
		t.Helper()
		api, _, err := h.Account(id)
		if err != nil {
			t.Fatal(err)
		}
		creds, err := api.ShowClient(t.Context(), "default")
		if err != nil || creds.Password == "" {
			t.Fatalf("ShowClient(%s) = %v, %v", id, creds.Password != "", err)
		}
		return creds.Password
	}
	return e, h, a, b, pass(a), pass(b)
}

// The Hub itself (not only the handler's check of the local ID it
// answers) never routes a request on one account's URLs to another
// account: b's password on a's URLs is a refusal.
func TestRouteKeepsEachAccountToItsOwnURLs(t *testing.T) {
	t.Parallel()
	h, a, b, passA, passB := routeEnv(t)
	for _, tc := range []struct {
		url, who, pass, want string
	}{{a, "a", passA, a}, {b, "b", passB, b}, {a, "b", passB, ""}, {b, "a", passA, ""}, {"", "a", passA, a}, {"", "b", passB, b}, {"", "nobody", "wrong", ""}} {
		api, id, wait, err := h.Route(t.Context(), tc.url, tc.pass, "192.0.2.1:1")
		if err != nil || wait != 0 || id != tc.want || (api != nil) != (tc.want != "") {
			t.Errorf("Route(%q, %s's password) = %v, %q, %v, %v; want account %q", tc.url, tc.who, api != nil, id, wait, err, tc.want)
			continue
		}
		if api == nil {
			continue
		}
		// The API handed out is that account's.
		st, err := api.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		mine, _, err := h.Account(tc.want)
		if err != nil {
			t.Fatal(err)
		}
		own, err := mine.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if st.AccountID == "" || st.AccountID != own.AccountID {
			t.Errorf("Route(%q) answered the API of Proton user %q, want %q's (%q)", tc.url, st.AccountID, tc.want, own.AccountID)
		}
	}
}

// A refusal on the compatibility URLs is charged once, however many
// accounts were tried: five wrong passwords leave the sixth request
// examined, and only it is blocked after.
func TestRouteChargesTheLimiterOncePerRequest(t *testing.T) {
	t.Parallel()
	h, a, _, passA, _ := routeEnv(t)
	const remote = "192.0.2.9:9"
	for i := range 5 {
		if api, _, wait, err := h.Route(t.Context(), "", "wrong", remote); api != nil || wait != 0 || err != nil {
			t.Fatalf("wrong password %d: %v, %v, %v; want a refusal", i+1, api != nil, wait, err)
		}
	}
	if api, _, wait, _ := h.Route(t.Context(), a, passA, remote); api != nil || wait <= 0 {
		t.Errorf("after five failures: %v, wait %v; want blocked", api != nil, wait)
	}
	if api, id, _, _ := h.Route(t.Context(), "", passA, "192.0.2.10:9"); api == nil || id != a {
		t.Errorf("another remote: %v, %q; want a", api != nil, id)
	}
}

// A removed account routes nothing; a closed Hub is an error.
func TestRouteAfterRemoveAndClose(t *testing.T) {
	t.Parallel()
	h, a, b, passA, passB := routeEnv(t)
	if _, err := h.Remove(t.Context(), a, service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{a, ""} {
		if api, _, wait, err := h.Route(t.Context(), url, passA, "192.0.2.1:1"); api != nil || wait != 0 || err != nil {
			t.Errorf("Route(%q) with removed a's password = %v, %v, %v; want a refusal", url, api != nil, wait, err)
		}
	}
	if api, id, _, err := h.Route(t.Context(), "", passB, "192.0.2.1:1"); api == nil || id != b || err != nil {
		t.Errorf("b after a's removal: %v, %q, %v", api != nil, id, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if api, _, _, err := h.Route(t.Context(), "", passB, "192.0.2.1:1"); api != nil || err == nil {
		t.Errorf("Route on a closed Hub = %v, %v; want an error", api != nil, err)
	}
}

// A client of an account with CalDAV off is refused without a charge, on
// either kind of URL (ruling R28): a Thunderbird left polling it from the
// shared localhost IP never blocks the other accounts. A password no
// account holds is still charged on the off account's URL.
func TestRouteCalDAVOffClientIsNotCharged(t *testing.T) {
	t.Parallel()
	h, a, b, passA, passB := routeEnv(t)
	if _, err := h.SetCalDAV(t.Context(), a, false); err != nil {
		t.Fatal(err)
	}
	const remote = "127.0.0.1:5000"
	for i := range 8 {
		url := []string{a, ""}[i%2]
		if api, _, wait, err := h.Route(t.Context(), url, passA, remote); api != nil || wait != 0 || err != nil {
			t.Fatalf("request %d: a's password on %q with CalDAV off = %v, %v, %v; want a refusal", i+1, url, api != nil, wait, err)
		}
	}
	if api, id, wait, _ := h.Route(t.Context(), b, passB, remote); api == nil || id != b {
		t.Fatalf("b's password after a's eight refusals = %v, %q, wait %v; want b", api != nil, id, wait)
	}
	const other = "127.0.0.2:5000"
	for i := range 5 {
		if api, _, wait, _ := h.Route(t.Context(), a, "wrong", other); api != nil || wait != 0 {
			t.Fatalf("wrong password %d on a's URL: %v, wait %v; want a refusal", i+1, api != nil, wait)
		}
	}
	if _, _, wait, _ := h.Route(t.Context(), b, passB, other); wait <= 0 {
		t.Errorf("after five wrong passwords on the off account's URL: not blocked")
	}
}

// A client of an account with CalDAV off is checked but not used: no
// touch of its last_used. b's client, used meanwhile, is touched: the hook
// sees touches.
func TestRouteCalDAVOffMatchDoesNotTouch(t *testing.T) {
	t.Parallel()
	h, a, b, passA, passB := routeEnv(t)
	var mu sync.Mutex
	touched := map[string]int{}
	for _, id := range []string{a, b} {
		h.LocalOf(id).SetTouchHook(func(phase, _ string) {
			if phase == "start" {
				mu.Lock()
				touched[id]++
				mu.Unlock()
			}
		})
	}
	if _, err := h.SetCalDAV(t.Context(), a, false); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{a, ""} {
		if api, _, _, _ := h.Route(t.Context(), url, passA, "192.0.2.1:1"); api != nil {
			t.Fatalf("a's password on %q with CalDAV off was served", url)
		}
	}
	if api, _, _, _ := h.Route(t.Context(), b, passB, "192.0.2.1:1"); api == nil {
		t.Fatal("b's password refused")
	}
	if err := h.Close(); err != nil { // waits for the touches started
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if touched[a] != 0 || touched[b] == 0 {
		t.Errorf("touches: a %d, b %d; want none of a's client, b's touched", touched[a], touched[b])
	}
}

// An account whose state.json cannot be read (its CalDAV switch unknown)
// answers 503, uncharged: once it is readable again, its password passes
// from the remote it refused six times.
func TestRouteUnreadableStateIs503Uncharged(t *testing.T) {
	t.Parallel()
	e, h, a, b, passA, passB := routeEnvDirs(t)
	path := filepath.Join(store.AccountDir(e.dirs.State, a), "state.json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	const remote = "192.0.2.77:7"
	for i := range 6 {
		url := []string{a, ""}[i%2]
		if api, _, wait, err := h.Route(t.Context(), url, passA, remote); api != nil || wait != 30*time.Second || err != nil {
			t.Fatalf("request %d on %q with a's state.json unreadable = %v, %v, %v; want 503 after 30 s", i+1, url, api != nil, wait, err)
		}
	}
	if api, id, _, _ := h.Route(t.Context(), "", passB, remote); api == nil || id != b {
		t.Errorf("b on the compatibility URL with a's state.json unreadable = %v, %q; want b", api != nil, id)
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if api, id, wait, _ := h.Route(t.Context(), a, passA, remote); api == nil || id != a {
		t.Errorf("a once state.json is readable = %v, %q, wait %v; want a (no charge)", api != nil, id, wait)
	}
}
