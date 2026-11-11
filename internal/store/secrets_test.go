package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

type fixture struct {
	state, key string
	secrets    *store.Secrets
}

func newFixture(t *testing.T) fixture {
	root := t.TempDir()
	f := fixture{state: filepath.Join(root, "state"), key: filepath.Join(root, "cfg", "secrets.key")}
	f.secrets = store.NewSecrets(f.state, &seal.KeyFile{Path: f.key})
	return f
}

func fullAccount(id string) store.Account {
	return store.Account{
		ID: id, Username: "alice-" + id + "@proton.me", UID: "uid-" + id,
		AccessToken: "access-token-" + id, RefreshToken: "refresh-token-" + id,
		KeyPass: []byte("salted-keypass-" + id),
		Relogin: &store.Relogin{Password: "pw-" + id, TOTPURI: "otpauth://totp/x?secret=TOTPSECRET" + id},
	}
}

func put(t *testing.T, s *store.Secrets, accts ...store.Account) {
	t.Helper()
	err := s.Update(ctx(t), func(m map[string]store.Account) error {
		for _, a := range accts {
			m[a.ID] = a
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSecretsRoundTripTwoAccounts(t *testing.T) {
	f := newFixture(t)
	if m, err := f.secrets.Load(); err != nil || len(m) != 0 {
		t.Fatalf("missing store: %v, %v", m, err)
	}
	a, b := fullAccount("userA"), fullAccount("userB")
	b.Relogin = nil
	put(t, f.secrets, a, b)
	got, err := f.secrets.Load()
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if g := got["userA"]; g.AccessToken != a.AccessToken || string(g.KeyPass) != string(a.KeyPass) ||
		g.Relogin == nil || g.Relogin.Password != a.Relogin.Password || g.Relogin.TOTPURI != a.Relogin.TOTPURI {
		t.Fatalf("account A = %+v", g)
	}
	if g := got["userB"]; g.RefreshToken != b.RefreshToken || g.Relogin != nil {
		t.Fatalf("account B = %+v", g)
	}
	if info, _ := os.Stat(f.secrets.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

func TestSealedFileHoldsNoPlaintext(t *testing.T) {
	f := newFixture(t)
	a := fullAccount("userA")
	put(t, f.secrets, a)
	raw, err := os.ReadFile(f.secrets.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{a.Username, a.UID, a.AccessToken, a.RefreshToken, string(a.KeyPass), a.Relogin.Password, a.Relogin.TOTPURI, "TOTPSECRET"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("sealed file contains %q", secret)
		}
	}
	if !strings.Contains(string(raw), `"sealer":"keyfile"`) || !strings.Contains(string(raw), `"version":1`) {
		t.Fatalf("envelope: %s", raw)
	}
}

func TestMissingKeyNeverOverwritesStore(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	before, _ := os.ReadFile(f.secrets.Path)
	if err := os.Remove(f.key); err != nil {
		t.Fatal(err)
	}
	called := false
	err := f.secrets.Update(ctx(t), func(m map[string]store.Account) error { called = true; return nil })
	if !errors.Is(err, seal.ErrNoKey) || called {
		t.Fatalf("err = %v, fn called = %v", err, called)
	}
	after, _ := os.ReadFile(f.secrets.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("sealed store was modified")
	}
	if _, err := os.Stat(f.key); err == nil {
		t.Fatal("a new key was created")
	}
	if _, err := f.secrets.Load(); !errors.Is(err, seal.ErrNoKey) {
		t.Fatalf("Load err = %v", err)
	}
}

func TestWipeRemovesUnreadableStore(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	if err := os.Remove(f.key); err != nil {
		t.Fatal(err)
	}
	if err := f.secrets.Wipe(); err != nil {
		t.Fatal(err)
	}
	if m, err := f.secrets.Load(); err != nil || len(m) != 0 {
		t.Fatalf("after wipe: %v, %v", m, err)
	}
	if err := f.secrets.Wipe(); err != nil { // nothing left: not an error
		t.Fatal(err)
	}
}

func TestEmptyUpdateRemovesFile(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	err := f.secrets.Update(ctx(t), func(m map[string]store.Account) error { delete(m, "userA"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.secrets.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still there: %v", err)
	}
}

func TestUpdateKeepsStoreWhenFnFails(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	before, _ := os.ReadFile(f.secrets.Path)
	boom := errors.New("boom")
	err := f.secrets.Update(ctx(t), func(m map[string]store.Account) error { delete(m, "userA"); return boom })
	after, _ := os.ReadFile(f.secrets.Path)
	if !errors.Is(err, boom) || !bytes.Equal(before, after) {
		t.Fatalf("err = %v, changed = %v", err, !bytes.Equal(before, after))
	}
}

func TestSealerMismatchIsAnError(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	other := store.NewSecrets(f.state, fakeSealer{})
	if _, err := other.Load(); err == nil || !strings.Contains(err.Error(), "keyfile") {
		t.Fatalf("err = %v", err)
	}
}

type fakeSealer struct{}

func (fakeSealer) Name() string                     { return "other" }
func (fakeSealer) Seal(p, _ []byte) ([]byte, error) { return p, nil }
func (fakeSealer) Open(s, _ []byte) ([]byte, error) { return s, nil }

type aadRecorder struct {
	seal.KeyFile
	aads []string
}

func (r *aadRecorder) Seal(p, aad []byte) ([]byte, error) {
	r.aads = append(r.aads, string(aad))
	return r.KeyFile.Seal(p, aad)
}

func (r *aadRecorder) Open(s, aad []byte) ([]byte, error) {
	r.aads = append(r.aads, string(aad))
	return r.KeyFile.Open(s, aad)
}

func TestSecretsBindTheSealerInTheAAD(t *testing.T) {
	f := newFixture(t)
	rec := &aadRecorder{KeyFile: seal.KeyFile{Path: f.key}}
	s := store.NewSecrets(f.state, rec)
	put(t, s, fullAccount("userA"))
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if len(rec.aads) != 2 || rec.aads[0] != "pcb-secrets/v1/keyfile" || rec.aads[1] != rec.aads[0] {
		t.Fatalf("aads = %q", rec.aads)
	}
}

func TestConcurrentUpdatesSerialize(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	for _, id := range []string{"userA", "userB", "userC", "userD"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.secrets.Update(ctx(t), func(m map[string]store.Account) error {
				m[id] = fullAccount(id)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := f.secrets.Load()
	if err != nil || len(got) != 4 {
		t.Fatalf("got %d accounts, %v", len(got), err)
	}
}

func TestUpdateHonoursCancelledContext(t *testing.T) {
	f := newFixture(t)
	c, cancel := context.WithCancel(context.Background())
	cancel()
	unlock := lockFor(t, f)
	defer unlock()
	if err := f.secrets.Update(c, func(map[string]store.Account) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestAccountNeverPrintsSecrets(t *testing.T) {
	a := fullAccount("userA")
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "account", a, "ptr", &a, "relogin", a.Relogin)
	outs := []string{
		fmt.Sprintf("%v", a), fmt.Sprintf("%+v", a), fmt.Sprintf("%#v", a),
		fmt.Sprintf("%v", &a), fmt.Sprintf("%+v", &a), fmt.Sprintf("%#v", &a),
		fmt.Sprintf("%v", a.Relogin), fmt.Sprintf("%+v", *a.Relogin), fmt.Sprintf("%#v", a.Relogin),
		fmt.Sprintf("%v", map[string]store.Account{"userA": a}),
		logged.String(),
	}
	secrets := []string{a.Username, a.UID, a.AccessToken, a.RefreshToken, string(a.KeyPass), a.Relogin.Password, a.Relogin.TOTPURI, "TOTPSECRET", "userA@"}
	for _, out := range outs {
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%q leaked in %q", s, out)
			}
		}
	}
	if got := fmt.Sprintf("%v", a); got != "store.Account(id=user…, redacted)" {
		t.Errorf("got %q", got)
	}
}
