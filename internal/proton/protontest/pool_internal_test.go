package protontest

import (
	"testing"

	gpa "github.com/ProtonMail/go-proton-api"
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
