package seal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// memKeyring is an in-memory Keyring: one item per attribute set.
type memKeyring struct {
	mu     sync.Mutex
	locked bool
	items  map[string][]byte
}

func newMem() *memKeyring { return &memKeyring{items: map[string][]byte{}} }

func memKey(attrs map[string]string) string {
	b, _ := json.Marshal(attrs) // map keys marshal sorted
	return string(b)
}

func (m *memKeyring) Get(_ context.Context, attrs map[string]string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locked {
		return nil, seal.ErrLocked
	}
	v, ok := m.items[memKey(attrs)]
	if !ok {
		return nil, seal.ErrNoKey
	}
	return bytes.Clone(v), nil
}

func (m *memKeyring) Set(_ context.Context, _ string, attrs map[string]string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locked {
		return seal.ErrLocked
	}
	m.items[memKey(attrs)] = bytes.Clone(secret)
	return nil
}

func (m *memKeyring) Delete(_ context.Context, attrs map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locked {
		return seal.ErrLocked
	}
	delete(m.items, memKey(attrs))
	return nil
}

func (m *memKeyring) Locked(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.locked, nil
}

func TestSecretServiceRoundTrip(t *testing.T) {
	k := newMem()
	s := &seal.SecretService{Keyring: k, StateDir: t.TempDir()}
	if s.Name() != "secret-service" {
		t.Fatalf("name = %q", s.Name())
	}
	sealed, err := s.Seal([]byte("hunter2"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("plaintext visible in the sealed blob")
	}
	if len(k.items) != 1 {
		t.Fatalf("items = %d, want 1 created on first Seal", len(k.items))
	}
	// A second sealer on the same keyring and state dir opens it.
	got, err := (&seal.SecretService{Keyring: k, StateDir: s.StateDir}).Open(sealed, []byte("ctx"))
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := s.Open(sealed, []byte("other")); !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("wrong aad: err = %v, want ErrCorrupt", err)
	}
	if _, err := s.Seal([]byte("again"), nil); err != nil || len(k.items) != 1 {
		t.Fatalf("second Seal: err = %v, items = %d (key must be created once)", err, len(k.items))
	}
}

// slowMissKeyring widens the window between "no key" and the write: a Get
// that finds nothing returns only after a pause, so two unsynchronised first
// sealers both see "no key" before either stores one.
type slowMissKeyring struct{ *memKeyring }

func (g slowMissKeyring) Get(ctx context.Context, attrs map[string]string) ([]byte, error) {
	v, err := g.memKeyring.Get(ctx, attrs)
	if errors.Is(err, seal.ErrNoKey) {
		time.Sleep(150 * time.Millisecond)
	}
	return v, err
}

// Two processes sealing for the first time at once must agree on one key.
func TestSecretServiceFirstSealsAgreeOnOneKey(t *testing.T) {
	g := slowMissKeyring{newMem()}
	dir := t.TempDir()
	blobs := make([][]byte, 2)
	var wg sync.WaitGroup
	for i := range blobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := (&seal.SecretService{Keyring: g, StateDir: dir}).Seal([]byte{byte('a' + i)}, nil)
			if err != nil {
				t.Error(err)
			}
			blobs[i] = b
		}()
	}
	wg.Wait()
	third := &seal.SecretService{Keyring: g.memKeyring, StateDir: dir}
	for i, b := range blobs {
		got, err := third.Open(b, nil)
		if err != nil || string(got) != string([]byte{byte('a' + i)}) {
			t.Errorf("blob %d: %q, %v (the second Set replaced the first key)", i, got, err)
		}
	}
}

func TestSecretServiceLockedIsErrLocked(t *testing.T) {
	k := newMem()
	s := &seal.SecretService{Keyring: k, StateDir: t.TempDir()}
	sealed, err := s.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	k.locked = true
	if _, err := s.Open(sealed, nil); !errors.Is(err, seal.ErrLocked) {
		t.Fatalf("Open err = %v, want ErrLocked", err)
	}
	if _, err := s.Seal([]byte("y"), nil); !errors.Is(err, seal.ErrLocked) {
		t.Fatalf("Seal err = %v, want ErrLocked", err)
	}
}

func TestSecretServiceMissingItemIsErrNoKey(t *testing.T) {
	k := newMem()
	s := &seal.SecretService{Keyring: k, StateDir: t.TempDir()}
	if _, err := s.Open(make([]byte, 64), nil); !errors.Is(err, seal.ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if len(k.items) != 0 {
		t.Fatal("Open created the key")
	}
}

func TestSecretServiceRefusesAShortKey(t *testing.T) {
	k := newMem()
	dir := t.TempDir()
	_ = k.Set(context.Background(), "", map[string]string{"application": "pcb", "state": dir}, []byte("short"))
	s := &seal.SecretService{Keyring: k, StateDir: dir}
	if _, err := s.Seal([]byte("x"), nil); !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// The store puts the sealer's name in the AAD, so a store sealed by the key
// file does not open under the keyring even with the same key bytes and the
// envelope's sealer field edited to match.
func TestSecretServiceBindsItsName(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	state := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "secrets.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	kf := store.NewSecrets(state, &seal.KeyFile{Path: keyPath})
	acct := store.Account{ID: "a", Username: "a@proton.me", AccessToken: "tok"}
	if err := kf.Update(context.Background(), func(m map[string]store.Account) error {
		m["a"] = acct
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mem := newMem()
	ss := &seal.SecretService{Keyring: mem, StateDir: state}
	// The keyring item holds the very same key bytes.
	if err := mem.Set(context.Background(), "", map[string]string{"application": "pcb", "state": state}, key); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(kf.Path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env["sealer"] = "secret-service"
	raw, _ = json.Marshal(env)
	if err := os.WriteFile(kf.Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.NewSecrets(state, ss).Load()
	if !errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("Load err = %v, want ErrCorrupt (the AAD carries the sealer name)", err)
	}

	// Control: the keyring sealer's own store round-trips.
	own := store.NewSecrets(t.TempDir(), &seal.SecretService{Keyring: newMem(), StateDir: state})
	if err := own.Update(context.Background(), func(m map[string]store.Account) error {
		m["a"] = acct
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := own.Load()
	if err != nil || got["a"].AccessToken != "tok" {
		t.Fatalf("own store: %v, %v", got, err)
	}
}

// A blob sealed before the AEAD helper was extracted still opens.
func TestKeyFileUnchangedByTheHelper(t *testing.T) {
	key, err := os.ReadFile("testdata/keyfile.key")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile("testdata/keyfile.sealed")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "secrets.key") // git does not keep the 0600 mode
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&seal.KeyFile{Path: path}).Open(sealed, []byte("fixture aad"))
	if err != nil || string(got) != "fixture plaintext" {
		t.Fatalf("got %q, %v", got, err)
	}
}
