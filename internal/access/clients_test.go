package access_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
)

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)} }

func newStore(t *testing.T) (*access.Store, *clock, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	c := newClock()
	return access.NewStore(dir, c.now), c, dir
}

func TestAddedPasswordChecksAndIsNeverStored(t *testing.T) {
	t.Parallel()
	s, _, dir := newStore(t)
	id, pw, err := s.Add(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(pw)
	if err != nil || len(raw) != 24 {
		t.Fatalf("password %q: want 24 bytes of unpadded base64url (decode err %v, %d bytes)", pw, err, len(raw))
	}
	c, ok := s.Check(pw)
	if !ok || c.ID != id || c.Name != "Thunderbird" {
		t.Fatalf("Check(password) = %+v, %v; want the added client", c, ok)
	}
	if _, ok := s.Check(pw + "x"); ok {
		t.Error("a wrong password passed")
	}
	if _, ok := s.Check(""); ok {
		t.Error("an empty password passed")
	}
	path := filepath.Join(dir, "clients.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(pw)) || bytes.Contains(data, raw) {
		t.Error("clients.json contains the password")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("clients.json mode = %v, want 0600", st.Mode().Perm())
	}
	// A new store on the same directory (a new process) accepts it.
	if _, ok := access.NewStore(dir, newClock().now).Check(pw); !ok {
		t.Error("a fresh store refused the stored client")
	}
}

func TestSaltsDifferPerClient(t *testing.T) {
	t.Parallel()
	s, _, dir := newStore(t)
	if _, _, err := s.Add(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add(t.Context(), "b"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "clients.json"))
	var f struct {
		Clients []struct{ Salt, Hash string } `json:"clients"`
	}
	if err := jsonUnmarshal(data, &f); err != nil || len(f.Clients) != 2 {
		t.Fatalf("file = %s (%v)", data, err)
	}
	if f.Clients[0].Salt == "" || f.Clients[0].Salt == f.Clients[1].Salt || f.Clients[0].Hash == f.Clients[1].Hash {
		t.Errorf("clients share a salt or a hash: %+v", f.Clients)
	}
}

func TestRemovedClientIsRefused(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	id, pw, _ := s.Add(t.Context(), "Phone")
	_, pw2, _ := s.Add(t.Context(), "Laptop")
	got, err := s.Remove(t.Context(), id)
	if err != nil || got.Name != "Phone" {
		t.Fatalf("Remove(id) = %+v, %v", got, err)
	}
	if _, ok := s.Check(pw); ok {
		t.Error("a removed client's password still passes")
	}
	if _, ok := s.Check(pw2); !ok {
		t.Error("removing one client broke another")
	}
	if got, err := s.Remove(t.Context(), "LAPTOP"); err != nil || got.Name != "Laptop" {
		t.Fatalf("Remove by name, case-insensitive = %+v, %v", got, err)
	}
	if _, err := s.Remove(t.Context(), "nobody"); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("Remove(unknown) = %v, want ErrNotFound", err)
	}
}

func TestClientNamesUnique(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	if _, _, err := s.Add(t.Context(), "Phone"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add(t.Context(), "pHONE"); !errors.Is(err, access.ErrDuplicate) {
		t.Errorf("duplicate name differing by case: %v, want ErrDuplicate", err)
	}
	if l, _ := s.List(); len(l) != 1 {
		t.Errorf("%d clients after a refused add, want 1", len(l))
	}
}

func TestClientNamesAreValidated(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	for name, ok := range map[string]bool{
		"":                           false,
		" lead":                      false,
		"trail ":                     false,
		"tab\there":                  false,
		"nl\nx":                      false,
		"nul\x00":                    false,
		"del\x7f":                    false,
		strings.Repeat("a", 65):      false,
		strings.Repeat("é", 65):      false,
		strings.Repeat("a", 64):      true,
		strings.Repeat("é", 64):      true, // 64 characters, 128 bytes
		"my phone (work) - été":      true,
		"a":                          true,
		"two  inner spaces are fine": true,
	} {
		_, _, err := s.Add(t.Context(), name)
		if ok && err != nil {
			t.Errorf("Add(%q) = %v, want ok", name, err)
		}
		if !ok && !errors.Is(err, access.ErrInvalidName) {
			t.Errorf("Add(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestListHasNoSecretsAndKeepsOrder(t *testing.T) {
	t.Parallel()
	s, c, _ := newStore(t)
	s.Add(t.Context(), "first") //nolint:errcheck // checked by List
	c.add(time.Hour)
	_, pw, _ := s.Add(t.Context(), "second")
	l, err := s.List()
	if err != nil || len(l) != 2 || l[0].Name != "first" || l[1].Name != "second" {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if l[1].Created != c.now() || !l[0].LastUsed.IsZero() {
		t.Errorf("times: %+v", l)
	}
	if strings.Contains(l[0].ID+l[0].Name+l[1].ID, pw) {
		t.Error("List leaks a password")
	}
}

func TestTouchIsThrottledToOncePerMinute(t *testing.T) {
	t.Parallel()
	s, c, dir := newStore(t)
	id, _, _ := s.Add(t.Context(), "Phone")
	path := filepath.Join(dir, "clients.json")
	if err := s.Touch(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	first := c.now()
	if l, _ := s.List(); !l[0].LastUsed.Equal(first) {
		t.Fatalf("LastUsed = %v, want %v", l[0].LastUsed, first)
	}
	before, _ := os.ReadFile(path)
	c.add(59 * time.Second)
	if err := s.Touch(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("a touch within a minute rewrote the file")
	}
	c.add(time.Second)
	if err := s.Touch(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(); !l[0].LastUsed.Equal(c.now()) {
		t.Errorf("LastUsed after a minute = %v, want %v", l[0].LastUsed, c.now())
	}
	if err := s.Touch(t.Context(), "unknown-id"); err != nil {
		t.Errorf("touching a removed client = %v, want nil", err)
	}
}

// Two Store values on one directory are two processes: pcb serve and pcb client.
func TestRunningStoreSeesAddAndRemoveFromAnotherStore(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	c := newClock()
	serve := access.NewStore(dir, c.now)
	cli := access.NewStore(dir, c.now)
	if _, ok := serve.Check("whatever"); ok { // loads the (missing) file
		t.Fatal("empty store accepted a password")
	}
	_, pw, err := cli.Add(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := serve.Check(pw); !ok {
		t.Fatal("the running store did not see a client added by another store")
	}
	if _, err := cli.Remove(t.Context(), "Phone"); err != nil {
		t.Fatal(err)
	}
	if _, ok := serve.Check(pw); ok {
		t.Fatal("the running store still accepts a client removed by another store")
	}
}

func TestConcurrentAddsFromTwoStoresKeepEveryClient(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	c := newClock()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := access.NewStore(dir, c.now) // one store per "process"
			if _, _, err := s.Add(t.Context(), "client-"+string(rune('a'+i))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if l, err := access.NewStore(dir, c.now).List(); err != nil || len(l) != 8 {
		t.Fatalf("%d clients after 8 concurrent adds (%v), want 8", len(l), err)
	}
}

func TestAddWaitsForTheLockAndHonoursContext(t *testing.T) {
	t.Parallel()
	s, _, dir := newStore(t)
	if _, _, err := s.Add(t.Context(), "seed"); err != nil {
		t.Fatal(err)
	}
	hold, ok := holdLock(t, filepathJoin(dir, "clients.lock"))
	if !ok {
		t.Skip("no file locks on this system")
	}
	defer hold()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.Add(ctx, "blocked"); !errors.Is(err, context.Canceled) {
		t.Errorf("Add while the lock is held, ctx cancelled = %v, want context.Canceled", err)
	}
	if l, _ := s.List(); len(l) != 1 {
		t.Errorf("%d clients, want 1: the blocked add must not write", len(l))
	}
}

func TestCorruptFileFailsClosed(t *testing.T) {
	t.Parallel()
	s, _, dir := newStore(t)
	_, pw, _ := s.Add(t.Context(), "Phone")
	if err := os.WriteFile(filepath.Join(dir, "clients.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Check(pw); ok {
		t.Error("Check passed on an unreadable client file")
	}
	if _, err := s.List(); err == nil {
		t.Error("List of a corrupt file returned no error")
	}
	if _, _, err := s.Add(t.Context(), "x"); err == nil {
		t.Error("Add over a corrupt file succeeded and would have destroyed it")
	}
}
