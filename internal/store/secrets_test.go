package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	if !strings.Contains(string(raw), `"sealer":"keyfile"`) || !strings.Contains(string(raw), `"version":2`) {
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
	if _, err := f.secrets.Wipe(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if m, err := f.secrets.Load(); err != nil || len(m) != 0 {
		t.Fatalf("after wipe: %v, %v", m, err)
	}
	if _, err := f.secrets.Wipe(ctx(t)); err != nil { // nothing left: not an error
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

// openRecorder records each Open's AAD and outcome.
type openRecorder struct {
	seal.KeyFile
	opens []struct {
		aad string
		err error
	}
}

func (r *openRecorder) Open(s, aad []byte) ([]byte, error) {
	p, err := r.KeyFile.Open(s, aad)
	r.opens = append(r.opens, struct {
		aad string
		err error
	}{string(aad), err})
	return p, err
}

func TestSecretsBindTheSealerInTheAAD(t *testing.T) {
	f := newFixture(t)
	rec := &aadRecorder{KeyFile: seal.KeyFile{Path: f.key}}
	s := store.NewSecrets(f.state, rec)
	put(t, s, fullAccount("userA"))
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if len(rec.aads) != 2 || rec.aads[0] != "pcb-secrets/v2/keyfile" || rec.aads[1] != rec.aads[0] {
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

func TestWipeWaitsForTheStoreLock(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	unlock := lockFor(t, f)
	c, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := f.secrets.Wipe(c); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(f.secrets.Path); err != nil {
		t.Fatalf("store removed while locked: %v", err)
	}
	unlock()
	if _, err := f.secrets.Wipe(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.secrets.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store still there: %v", err)
	}
}

// envelopeOf reads secrets.sealed's envelope as written on disk.
func envelopeOf(t *testing.T, f fixture) (version int, sealer string, data []byte) {
	t.Helper()
	raw, err := os.ReadFile(f.secrets.Path)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Version int    `json:"version"`
		Sealer  string `json:"sealer"`
		Data    []byte `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.Version, env.Sealer, env.Data
}

// sealedGeneration opens secrets.sealed with the fixture's key under the v2
// AAD and returns the generation sealed in it.
func sealedGeneration(t *testing.T, f fixture) uint64 {
	t.Helper()
	version, sealer, data := envelopeOf(t, f)
	if version != 2 || sealer != "keyfile" {
		t.Fatalf("envelope version %d sealer %q, want 2 keyfile", version, sealer)
	}
	plain, err := (&seal.KeyFile{Path: f.key}).Open(data, []byte("pcb-secrets/v2/keyfile"))
	if err != nil {
		t.Fatalf("open under the v2 AAD: %v", err)
	}
	var p struct {
		Generation uint64                   `json:"generation"`
		Accounts   map[string]store.Account `json:"accounts"`
	}
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("v2 payload: %v", err)
	}
	if len(p.Accounts) == 0 {
		t.Fatal("v2 payload holds no accounts")
	}
	return p.Generation
}

func mirrorOf(t *testing.T, f fixture) uint64 {
	t.Helper()
	st, err := store.LoadState(f.state)
	if err != nil {
		t.Fatal(err)
	}
	return st.SecretsGeneration
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// wantRolledBack checks that err is ErrRolledBack naming both generations.
func wantRolledBack(t *testing.T, err error, sealed, mirror uint64) {
	t.Helper()
	var rb *store.RolledBackError
	if !errors.Is(err, store.ErrRolledBack) || !errors.As(err, &rb) {
		t.Fatalf("err = %v, want store.ErrRolledBack", err)
	}
	if rb.Generation != sealed || rb.Mirror != mirror {
		t.Fatalf("rolled back %d < %d, want %d < %d", rb.Generation, rb.Mirror, sealed, mirror)
	}
	if want := fmt.Sprintf("(generation %d < %d)", sealed, mirror); !strings.Contains(err.Error(), want) {
		t.Fatalf("message %q does not name %s", err, want)
	}
}

func TestGenerationIncrementsAndIsMirrored(t *testing.T) {
	f := newFixture(t)
	for want := uint64(1); want <= 3; want++ {
		put(t, f.secrets, fullAccount("userA"))
		if g := sealedGeneration(t, f); g != want {
			t.Fatalf("sealed generation %d, want %d", g, want)
		}
		if m := mirrorOf(t, f); m != want {
			t.Fatalf("mirror %d, want %d", m, want)
		}
	}
	raw := readFile(t, filepath.Join(f.state, "state.json"))
	if !strings.Contains(string(raw), `"secrets_generation": 3`) {
		t.Fatalf("state.json: %s", raw)
	}
}

func TestRestoredOlderSecretsAreRefused(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	old := readFile(t, f.secrets.Path)
	put(t, f.secrets, fullAccount("userB"))
	writeFile(t, f.secrets.Path, old) // a backup restored over the newer file
	_, err := f.secrets.Load()
	wantRolledBack(t, err, 1, 2)
	called := false
	err = f.secrets.Update(ctx(t), func(map[string]store.Account) error { called = true; return nil })
	wantRolledBack(t, err, 1, 2)
	if called || !bytes.Equal(readFile(t, f.secrets.Path), old) {
		t.Fatalf("Update ran fn (%v) or rewrote the restored file", called)
	}
	if m := mirrorOf(t, f); m != 2 {
		t.Fatalf("mirror %d, want 2", m)
	}
}

func TestFileRemovedByLogoutIsRefusedIfRestored(t *testing.T) {
	for name, remove := range map[string]func(t *testing.T, f fixture){
		"empty update": func(t *testing.T, f fixture) {
			if err := f.secrets.Update(ctx(t), func(m map[string]store.Account) error { clear(m); return nil }); err != nil {
				t.Fatal(err)
			}
		},
		"wipe": func(t *testing.T, f fixture) {
			if _, err := f.secrets.Wipe(ctx(t)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			put(t, f.secrets, fullAccount("userA"))
			old := readFile(t, f.secrets.Path)
			remove(t, f)
			if _, err := os.Stat(f.secrets.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("file still there: %v", err)
			}
			if m := mirrorOf(t, f); m != 2 {
				t.Fatalf("mirror after removal %d, want 2", m)
			}
			writeFile(t, f.secrets.Path, old)
			_, err := f.secrets.Load()
			wantRolledBack(t, err, 1, 2)
			// A new login after the removal seals above the bumped mirror.
			if err := os.Remove(f.secrets.Path); err != nil {
				t.Fatal(err)
			}
			put(t, f.secrets, fullAccount("userB"))
			if g, m := sealedGeneration(t, f), mirrorOf(t, f); g != 3 || m != 3 {
				t.Fatalf("after a new login: sealed %d mirror %d, want 3 3", g, m)
			}
		})
	}
}

// writeV1 writes accounts as an M1-M4 store: version 1, the accounts map
// sealed under the v1 AAD.
func writeV1(t *testing.T, f fixture, accounts map[string]store.Account) []byte {
	t.Helper()
	plain, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := (&seal.KeyFile{Path: f.key}).Seal(plain, []byte("pcb-secrets/v1/keyfile"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "sealer": "keyfile", "data": sealed})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.secrets.Path, raw)
	return raw
}

func TestV1FileIsReadAndUpgraded(t *testing.T) {
	f := newFixture(t)
	a := fullAccount("userA")
	v1 := writeV1(t, f, map[string]store.Account{"userA": a})
	got, err := f.secrets.Load()
	if err != nil || got["userA"].RefreshToken != a.RefreshToken || got["userA"].Relogin == nil {
		t.Fatalf("v1 Load = %v, %v", got, err)
	}
	put(t, f.secrets, fullAccount("userB"))
	if g, m := sealedGeneration(t, f), mirrorOf(t, f); g != 1 || m != 1 {
		t.Fatalf("after the upgrade: sealed %d mirror %d, want 1 1", g, m)
	}
	if got, err := f.secrets.Load(); err != nil || len(got) != 2 || got["userA"].AccessToken != a.AccessToken {
		t.Fatalf("after the upgrade: %v, %v", got, err)
	}
	writeFile(t, f.secrets.Path, v1) // the pre-upgrade file restored: generation 0
	_, err = f.secrets.Load()
	wantRolledBack(t, err, 0, 1)
}

func TestSealedAheadOfMirrorIsAccepted(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	statePath := filepath.Join(f.state, "state.json")
	before := readFile(t, statePath)
	// The mirror write fails (state.json cannot be replaced): Update still
	// succeeds, the sealed file being ahead.
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, f.secrets, fullAccount("userB"))
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	writeFile(t, statePath, before) // the mirror still says 1
	if g, m := sealedGeneration(t, f), mirrorOf(t, f); g != 2 || m != 1 {
		t.Fatalf("sealed %d mirror %d, want 2 1", g, m)
	}
	if got, err := f.secrets.Load(); err != nil || len(got) != 2 {
		t.Fatalf("Load = %d accounts, %v", len(got), err)
	}
	put(t, f.secrets, fullAccount("userC"))
	if g, m := sealedGeneration(t, f), mirrorOf(t, f); g != 3 || m != 3 {
		t.Fatalf("next update: sealed %d mirror %d, want 3 3", g, m)
	}
}

func TestEditedVersionFieldDoesNotDowngrade(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	raw := readFile(t, f.secrets.Path)
	edited := bytes.Replace(raw, []byte(`"version":2`), []byte(`"version":1`), 1)
	if bytes.Equal(edited, raw) {
		t.Fatalf("no version 2 in %s", raw)
	}
	writeFile(t, f.secrets.Path, edited)
	rec := &openRecorder{KeyFile: seal.KeyFile{Path: f.key}}
	if _, err := store.NewSecrets(f.state, rec).Load(); !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("Load err = %v, want seal.ErrCorrupt", err)
	}
	// The sealer itself refuses it: the version is authenticated, so the
	// v2 data does not open under the v1 AAD.
	if len(rec.opens) != 1 || rec.opens[0].aad != "pcb-secrets/v1/keyfile" || !errors.Is(rec.opens[0].err, seal.ErrCorrupt) {
		t.Fatalf("opens = %+v, want one refused under pcb-secrets/v1/keyfile", rec.opens)
	}
	err := f.secrets.Update(ctx(t), func(map[string]store.Account) error { return nil })
	if !errors.Is(err, seal.ErrCorrupt) || !bytes.Equal(readFile(t, f.secrets.Path), edited) {
		t.Fatalf("Update err = %v, or the file was rewritten", err)
	}
}

// A wiped file that was ahead of the mirror is refused if restored too: the
// mirror moves past the file's generation, not only past the mirror.
func TestWipeMovesTheMirrorPastAFileAhead(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	put(t, f.secrets, fullAccount("userA"))
	statePath := filepath.Join(f.state, "state.json")
	st, err := store.LoadState(f.state)
	if err != nil {
		t.Fatal(err)
	}
	st.SecretsGeneration = 1 // the mirror write of generation 2 was lost
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveState(f.state, st); err != nil {
		t.Fatal(err)
	}
	ahead := readFile(t, f.secrets.Path)
	if _, err := f.secrets.Wipe(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if m := mirrorOf(t, f); m != 3 {
		t.Fatalf("mirror after the wipe %d, want 3", m)
	}
	writeFile(t, f.secrets.Path, ahead)
	_, err = f.secrets.Load()
	wantRolledBack(t, err, 2, 3)
}

// Wipe returns the generation it moved the mirror to: the caller seeds a
// state.json it rewrites from scratch with it (logout over a damaged one).
func TestWipeReturnsTheNextGeneration(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	put(t, f.secrets, fullAccount("userA"))
	// A damaged state.json: the mirror cannot be read, nor moved.
	writeFile(t, filepath.Join(f.state, "state.json"), []byte("{"))
	next, err := f.secrets.Wipe(ctx(t))
	if err != nil || next != 3 {
		t.Fatalf("Wipe = %d, %v; want 3 (the file's generation 2 + 1)", next, err)
	}
	if next, err := f.secrets.Wipe(ctx(t)); err != nil || next != 0 {
		t.Fatalf("Wipe with no file = %d, %v; want 0", next, err)
	}
}

func TestFailedMirrorWriteIsLogged(t *testing.T) {
	f := newFixture(t)
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, nil))
	f.secrets.Log = func() *slog.Logger { return log }
	a := fullAccount("userA")
	put(t, f.secrets, a)
	if logged.Len() != 0 {
		t.Fatalf("logged on a good write: %s", logged.String())
	}
	statePath := filepath.Join(f.state, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, f.secrets, a) // still succeeds: the sealed file is ahead
	out := logged.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"generation":2`) || !strings.Contains(out, "could not be recorded in state.json") {
		t.Fatalf("log = %s", out)
	}
	for _, secret := range []string{a.Username, a.UID, a.AccessToken, a.RefreshToken, string(a.KeyPass), a.Relogin.Password, "TOTPSECRET"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log holds %q", secret)
		}
	}
}

// A mirror at the bound (only an edit puts it there) saturates instead of
// moving past what LoadState accepts: one wipe and a new login recover.
func TestGenerationSaturatesAtTheBound(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	if err := store.SaveState(f.state, store.State{Phase: store.PhaseReady, SecretsGeneration: store.MaxSecretsGeneration}); err != nil {
		t.Fatal(err)
	}
	_, err := f.secrets.Load()
	wantRolledBack(t, err, 1, store.MaxSecretsGeneration)
	next, err := f.secrets.Wipe(ctx(t))
	if err != nil || next != store.MaxSecretsGeneration {
		t.Fatalf("Wipe = %d, %v; want the bound", next, err)
	}
	if err := store.SaveState(f.state, store.State{Phase: store.PhaseLoggedOut, SecretsGeneration: next}); err != nil {
		t.Fatal(err)
	}
	if m := mirrorOf(t, f); m != store.MaxSecretsGeneration {
		t.Fatalf("mirror %d", m)
	}
	put(t, f.secrets, fullAccount("userB"))
	if g, m := sealedGeneration(t, f), mirrorOf(t, f); g != store.MaxSecretsGeneration || m != store.MaxSecretsGeneration {
		t.Fatalf("sealed %d mirror %d, want both at the bound", g, m)
	}
	if got, err := f.secrets.Load(); err != nil || len(got) != 1 {
		t.Fatalf("Load = %d accounts, %v", len(got), err)
	}
}

// writeEnvelope replaces secrets.sealed with an envelope of version and
// sealer around data.
func writeEnvelope(t *testing.T, f fixture, version int, sealer string, data []byte) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": version, "sealer": sealer, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.secrets.Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A store this build cannot open names why (state-I1): a newer envelope
// version, or another sealer, each as *UnsupportedError; Update never
// overwrites it.
func TestUnsupportedStoreNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		sealer  string
		open    *store.Secrets // nil: the fixture's
		want    string
	}{
		{name: "newer version", version: 3, sealer: "keyfile", want: "secrets.sealed was written by a newer pcb"},
		{name: "newer version, other sealer", version: 9, sealer: "tpm2", want: "secrets.sealed was written by a newer pcb"},
		{name: "other sealer", version: 2, sealer: "tpm2", want: `secrets.sealed is sealed with "tpm2", which this pcb cannot use`},
		{name: "odd sealer name", version: 2, sealer: "a\x1b[31mb" + strings.Repeat("z", 60),
			want: `secrets.sealed is sealed with "a\x1b[31mb` + strings.Repeat("z", 25) + `…", which this pcb cannot use`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			put(t, f.secrets, fullAccount("userA"))
			_, _, data := envelopeOf(t, f)
			writeEnvelope(t, f, tc.version, tc.sealer, data)
			before, _ := os.ReadFile(f.secrets.Path)
			_, err := f.secrets.Load()
			var ue *store.UnsupportedError
			if !errors.As(err, &ue) || !errors.Is(err, store.ErrUnsupported) {
				t.Fatalf("Load err = %v, want an *UnsupportedError", err)
			}
			if got := ue.Explain(); got != tc.want {
				t.Errorf("Explain() = %q, want %q", got, tc.want)
			}
			if err := f.secrets.Update(ctx(t), func(m map[string]store.Account) error { m["userB"] = fullAccount("userB"); return nil }); !errors.Is(err, store.ErrUnsupported) {
				t.Errorf("Update err = %v, want ErrUnsupported", err)
			}
			if after, _ := os.ReadFile(f.secrets.Path); !bytes.Equal(before, after) {
				t.Error("Update overwrote a store it cannot open")
			}
		})
	}
}

// An envelope version below 1 was never written by any pcb: a damaged file,
// not a newer one.
func TestEnvelopeVersionZeroIsCorrupt(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	_, _, data := envelopeOf(t, f)
	writeEnvelope(t, f, 0, "keyfile", data)
	if _, err := f.secrets.Load(); !errors.Is(err, seal.ErrCorrupt) || errors.Is(err, store.ErrUnsupported) {
		t.Fatalf("Load err = %v, want ErrCorrupt", err)
	}
}

// Removing secrets.sealed fsyncs the state directory (state Minor): a power
// loss right after a logout must not bring the file back.
func TestWipeSyncsTheDirectory(t *testing.T) {
	f := newFixture(t)
	put(t, f.secrets, fullAccount("userA"))
	var synced []string
	defer store.OnSyncDir(func(dir string) { synced = append(synced, dir) })()
	if _, err := f.secrets.Wipe(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, f.state) {
		t.Errorf("directories synced after the wipe = %q, want %s", synced, f.state)
	}
	synced = nil
	put(t, f.secrets, fullAccount("userA"))
	synced = nil
	if err := f.secrets.Update(ctx(t), func(m map[string]store.Account) error { clear(m); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, f.state) {
		t.Errorf("directories synced after an emptying update = %q, want %s", synced, f.state)
	}
}
