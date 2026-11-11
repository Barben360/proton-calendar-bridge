package protontest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"
)

func TestSharedAccountIsCreatedOncePerUsername(t *testing.T) {
	t.Parallel()
	s := nextShard()
	first, err := s.user("shared-once", "pass-1")
	if err != nil || first == "" {
		t.Fatalf("user = %q, %v", first, err)
	}
	if again, err := s.user("shared-once", "pass-1"); err != nil || again != first {
		t.Fatalf("second ask = %q, %v; want the same account %q", again, err, first)
	}
	_, err = s.user("shared-once", "pass-2")
	if err == nil || !strings.Contains(err.Error(), "Options.Private") {
		t.Fatalf("another password: err = %v, want a refusal naming Options.Private", err)
	}
	if strings.Contains(err.Error(), "pass-") {
		t.Errorf("the refusal shows a password: %v", err)
	}
}

func TestPrivateFakeHasItsOwnAccount(t *testing.T) {
	t.Parallel()
	common := New(t, Options{})
	private := New(t, Options{Private: true})
	if private.shard != nil || private.backend == common.backend {
		t.Fatal("a private fake runs on a shared backend")
	}
	if private.UserID(private.Username) == common.UserID(common.Username) {
		t.Fatal("a private fake reuses the shared account")
	}
}

// sameShard returns a new fake on f's shared backend (fakes are spread round
// robin, and other tests take turns too).
func sameShard(t *testing.T, f *Fake) *Fake {
	t.Helper()
	for range 100 * len(shared.shards) {
		if g := New(t, Options{}); g.shard == f.shard {
			return g
		}
	}
	t.Fatal("no second fake on the same backend")
	return nil
}

func TestRevokeSessionsLeavesOtherFakesAlone(t *testing.T) {
	t.Parallel()
	f := New(t, Options{})
	g := sameShard(t, f)
	fa, ga := login(t, f), login(t, g)
	f.RevokeSessions()
	if status := getUser(t, f, fa); status != http.StatusUnauthorized {
		t.Errorf("revoked session answered %d, want 401", status)
	}
	if status, code := refresh(t, f, fa); status != http.StatusUnprocessableEntity || code != 10013 {
		t.Errorf("revoked refresh answered %d/%d, want 422/10013", status, code)
	}
	if status := getUser(t, g, ga); status != http.StatusOK {
		t.Errorf("the other fake's session answered %d, want 200", status)
	}
	if status := getUser(t, f, login(t, f)); status != http.StatusOK {
		t.Errorf("a login after the revocation answered %d, want 200", status)
	}
}

// recorder is a testing.TB whose Fatalf ends the goroutine and records why.
type recorder struct {
	testing.TB
	fatal string
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func TestAddAddressNeedsAPrivateFake(t *testing.T) {
	t.Parallel()
	f := New(t, Options{})
	r := &recorder{TB: t}
	f.t = r
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.AddAddress(f.Username, "second@proton.local")
	}()
	<-done
	if !strings.Contains(r.fatal, "Options.Private") {
		t.Fatalf("AddAddress on a shared account: fatal %q, want a refusal naming Options.Private", r.fatal)
	}
}

func login(t *testing.T, f *Fake) gpa.Auth {
	t.Helper()
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	c, a, err := m.NewClientWithLogin(t.Context(), f.Username, []byte(f.Password))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	return a
}

func getUser(t *testing.T, f *Fake, a gpa.Auth) int {
	t.Helper()
	status, _ := send(t, f, http.MethodGet, "/core/v4/users", a, nil)
	return status
}

func refresh(t *testing.T, f *Fake, a gpa.Auth) (status, code int) {
	t.Helper()
	return send(t, f, http.MethodPost, "/auth/v4/refresh", a, map[string]string{
		"UID": a.UID, "RefreshToken": a.RefreshToken, "ResponseType": "token", "GrantType": "refresh_token",
		"RedirectURI": "https://protonmail.ch", "State": "s",
	})
}

func send(t *testing.T, f *Fake, method, path string, a gpa.Auth, body any) (status, code int) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("Content-Type", "application/json")
	if path != "/auth/v4/refresh" {
		req.Header.Set("x-pm-uid", a.UID)
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	var env struct{ Code int }
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env.Code
}
