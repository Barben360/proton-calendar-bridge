package protontest

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// The reuse tests are not parallel: nothing else borrows meanwhile, so the
// backend a test gives back is the next one lent.

func TestBackendAndAccountAreReusedAfterTheTest(t *testing.T) {
	var first *Fake
	t.Run("first", func(t *testing.T) { first = New(t, Options{}) })
	t.Run("next", func(t *testing.T) {
		next := New(t, Options{})
		if next.pool != first.pool {
			t.Fatal("the next fake did not reuse the backend given back")
		}
		if id := next.UserID(next.Username); id == "" || id != first.UserID(first.Username) {
			t.Fatalf("account %q, want the reused %q", id, first.UserID(first.Username))
		}
		login(t, next, next.Username, next.Password) // the reused account still logs in
	})
}

func TestBackendHasOneHolderAtATime(t *testing.T) {
	t.Parallel()
	a, b := New(t, Options{}), New(t, Options{})
	if a.pool == b.pool {
		t.Fatal("two live fakes share a backend")
	}
}

func TestChangedAccountIsNotReused(t *testing.T) {
	var spoilt *pooled
	t.Run("adds an address", func(t *testing.T) {
		f := New(t, Options{})
		f.AddAddress(f.Username, "second@proton.local")
		spoilt = f.pool
	})
	t.Run("next", func(t *testing.T) {
		if f := New(t, Options{}); f.pool == spoilt {
			t.Fatal("a backend whose account was changed was lent again")
		}
	})
}

func TestAddUserWithAnotherPasswordReplacesTheAccount(t *testing.T) {
	t.Parallel()
	f := New(t, Options{})
	f.AddUser("replaced", "first-pass")
	id := f.AddUser("replaced", "second-pass")
	if a := login(t, f, "replaced", "second-pass"); a.UserID != id {
		t.Fatalf("logged in as %q, want %q", a.UserID, id)
	}
}

func TestLoginSessionIsLiveAndReusedOnceGivenBack(t *testing.T) {
	var first api.Tokens
	t.Run("first", func(t *testing.T) {
		f := New(t, Options{})
		first = f.Login(t, f.Username)
		if got := send(t, f, http.MethodGet, "/core/v4/users", first); got != http.StatusOK {
			t.Fatalf("GET /core/v4/users with the session = %d, want 200", got)
		}
		if n := f.Calls("POST", "/auth/"); n != 0 {
			t.Fatalf("the fake saw %d auth calls, want none: the test's own calls stay countable", n)
		}
		if !slices.Contains(f.AccessTokens(), first.AccessToken) {
			t.Fatal("AccessTokens misses the session's token")
		}
	})
	t.Run("next", func(t *testing.T) {
		f := New(t, Options{})
		if got := f.Login(t, f.Username); got != first {
			t.Fatal("the next fake did not reuse the unspent session")
		}
		if got := send(t, f, http.MethodGet, "/core/v4/users", first); got != http.StatusOK {
			t.Fatalf("the reused session answers %d, want 200", got)
		}
	})
}

func TestSpentLoginSessionIsNotReused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spend func(t *testing.T, f *Fake, tok api.Tokens)
	}{
		{"logged out", func(t *testing.T, f *Fake, tok api.Tokens) { send(t, f, http.MethodDelete, "/auth/v4", tok) }},
		{"refreshed", func(t *testing.T, f *Fake, tok api.Tokens) { send(t, f, http.MethodPost, "/auth/v4/refresh", tok) }},
		{"refreshed by the manager", func(t *testing.T, f *Fake, tok api.Tokens) {
			// go-proton-api names the session in the body only, not in x-pm-uid.
			m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
			defer m.Close()
			c, _, err := m.NewClientWithRefresh(t.Context(), tok.UID, tok.RefreshToken)
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			c.Close()
		}},
		{"revoked", func(_ *testing.T, f *Fake, _ api.Tokens) { f.RevokeSessions() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var spent api.Tokens
			t.Run("spends", func(t *testing.T) {
				f := New(t, Options{})
				spent = f.Login(t, f.Username)
				tc.spend(t, f, spent)
			})
			t.Run("next", func(t *testing.T) {
				f := New(t, Options{})
				if got := f.Login(t, f.Username); got.UID == spent.UID {
					t.Fatal("a spent session was handed out again")
				}
			})
		})
	}
}

func TestRevokeSessionsAlsoEndsTheIdleOnes(t *testing.T) {
	t.Run("gives one back", func(t *testing.T) {
		f := New(t, Options{})
		f.Login(t, f.Username)
	})
	t.Run("revokes", func(t *testing.T) {
		f := New(t, Options{})
		f.RevokeSessions() // the backend ends every session of the account
		tok := f.Login(t, f.Username)
		if got := send(t, f, http.MethodGet, "/core/v4/users", tok); got != http.StatusOK {
			t.Fatalf("Login after RevokeSessions answers %d, want a live session", got)
		}
	})
}

func TestLoginSessionOfAReplacedAccountIsNotReused(t *testing.T) {
	var old api.Tokens
	t.Run("first", func(t *testing.T) {
		f := New(t, Options{})
		f.AddUser("replaced-login", "first-pass")
		old = f.Login(t, "replaced-login")
	})
	t.Run("next", func(t *testing.T) {
		f := New(t, Options{})
		f.AddUser("replaced-login", "second-pass")
		tok := f.Login(t, "replaced-login")
		if tok.UID == old.UID {
			t.Fatal("the replaced account's session was handed out")
		}
		if got := send(t, f, http.MethodGet, "/core/v4/users", tok); got != http.StatusOK {
			t.Fatalf("the new account's session answers %d, want 200", got)
		}
	})
}

func TestExpiredLoginSessionAnswers401(t *testing.T) {
	t.Parallel()
	f := New(t, Options{})
	tok := f.Login(t, f.Username)
	f.ExpireAccessTokens()
	if got := send(t, f, http.MethodGet, "/core/v4/users", tok); got != http.StatusUnauthorized {
		t.Fatalf("an expired session's token answers %d, want 401", got)
	}
}

// send sends one request to f with tok and returns the status. A refresh
// carries tok's refresh token.
func send(t *testing.T, f *Fake, method, path string, tok api.Tokens) int {
	t.Helper()
	var body *strings.Reader
	if path == "/auth/v4/refresh" {
		body = strings.NewReader(`{"UID":"` + tok.UID + `","RefreshToken":"` + tok.RefreshToken + `","ResponseType":"token","GrantType":"refresh_token","RedirectURI":"https://protonmail.ch"}`)
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("x-pm-uid", tok.UID)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func login(t *testing.T, f *Fake, user, pass string) gpa.Auth {
	t.Helper()
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	c, a, err := m.NewClientWithLogin(t.Context(), user, []byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	return a
}
