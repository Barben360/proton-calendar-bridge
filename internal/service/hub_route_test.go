package service_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// routeEnv is a Hub with accounts a and b logged in, and the password of
// each one's default CalDAV client.
func routeEnv(t *testing.T) (h *service.Hub, a, b, passA, passB string) {
	t.Helper()
	e := newHubEnv(t)
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
	return h, a, b, pass(a), pass(b)
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
