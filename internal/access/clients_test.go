package access_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
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
		"c0123456789abcdef":          false, // shaped like a client ID: remove NAME would hit another client
		"C0123456789ABCDEF":          false,
		"c0123456789abcde":           true,
		"c0123456789abcdefg":         true,
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

// A Touch from one process interleaved with Add/Remove from another must not
// lose either (ruling M3-R18): every write re-reads the file under the lock.
func TestTouchAndAddRemoveFromTwoStoresLoseNothing(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	c := newClock()
	serve, cli := access.NewStore(dir, c.now), access.NewStore(dir, c.now)
	keep, _, _ := serve.Add(t.Context(), "keep")
	gone, gonePW, _ := serve.Add(t.Context(), "gone")
	if _, err := cli.List(); err != nil { // cli now holds a copy
		t.Fatal(err)
	}
	c.add(2 * time.Minute)
	if err := serve.Touch(t.Context(), keep); err != nil { // serve writes LastUsed...
		t.Fatal(err)
	}
	if _, _, err := cli.Add(t.Context(), "new"); err != nil { // ...cli writes from its stale copy
		t.Fatal(err)
	}
	if _, err := cli.Remove(t.Context(), gone); err != nil {
		t.Fatal(err)
	}
	c.add(2 * time.Minute)
	if err := serve.Touch(t.Context(), gone); err != nil { // serve touches a client cli removed
		t.Fatal(err)
	}
	l, err := access.NewStore(dir, c.now).List()
	if err != nil || len(l) != 2 || l[0].Name != "keep" || l[1].Name != "new" {
		t.Fatalf("clients = %+v (%v), want keep and new: the removed client must not come back", l, err)
	}
	if l[0].LastUsed.IsZero() {
		t.Error("cli's Add from a stale copy dropped serve's LastUsed")
	}
	if _, ok := serve.Check(gonePW); ok {
		t.Error("a touch resurrected a removed client")
	}
}

func TestConcurrentTouchesAndAddsFromTwoStoresKeepEverything(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	c := newClock()
	serve, cli := access.NewStore(dir, c.now), access.NewStore(dir, c.now)
	keep, _, _ := serve.Add(t.Context(), "keep")
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.add(time.Minute) // every touch is due
			if err := serve.Touch(t.Context(), keep); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, _, err := cli.Add(t.Context(), "n"+string(rune('a'+i))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	l, err := access.NewStore(dir, c.now).List()
	if err != nil || len(l) != 7 {
		t.Fatalf("%d clients (%v), want 7: a touch and an add lost each other", len(l), err)
	}
}

func TestFileReplacedByAnotherInodeWithSameSizeAndMtimeIsReloaded(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	c := newClock()
	serve := access.NewStore(dir, c.now)
	_, pw1, _ := access.NewStore(dir, c.now).Add(t.Context(), "aaaa")
	if _, ok := serve.Check(pw1); !ok {
		t.Fatal("setup")
	}
	path := filepath.Join(dir, "clients.json")
	old, _ := os.Stat(path)
	// Another process removes and re-adds a client of the same name length: the
	// file has the same size; force the same mtime too.
	other := access.NewStore(dir, c.now)
	if _, err := other.Remove(t.Context(), "aaaa"); err != nil {
		t.Fatal(err)
	}
	_, pw2, _ := other.Add(t.Context(), "bbbb")
	if err := os.Chtimes(path, old.ModTime(), old.ModTime()); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.Stat(path); now.Size() != old.Size() || !now.ModTime().Equal(old.ModTime()) {
		t.Skipf("could not recreate the same size and mtime (%d vs %d)", now.Size(), old.Size())
	}
	if _, ok := serve.Check(pw1); ok {
		t.Error("the running store still accepts a client removed behind an identical size and mtime")
	}
	if _, ok := serve.Check(pw2); !ok {
		t.Error("the running store missed a client added behind an identical size and mtime")
	}
}

// sealedStore is newStore whose passwords are sealed with a key file kept
// beside the state directory.
func sealedStore(t *testing.T) (*access.Store, *seal.KeyFile, string) {
	t.Helper()
	s, _, dir := newStore(t)
	k := &seal.KeyFile{Path: filepath.Join(filepath.Dir(dir), "secrets.key")}
	s.Sealer = k
	return s, k, dir
}

// clientsFile is clients.json as tests read and edit it.
type clientsFile struct {
	Version int              `json:"version"`
	Clients []map[string]any `json:"clients"`
}

func readClients(t *testing.T, dir string) clientsFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f clientsFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeClients(t *testing.T, dir string, f clientsFile) {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clients.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRevealReturnsTheAddedPassword(t *testing.T) {
	t.Parallel()
	s, k, dir := sealedStore(t)
	id, pw, err := s.Add(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Reveal(id, k.Open)
	if err != nil || got != pw {
		t.Fatalf("Reveal = %d chars, %v; want the added password", len(got), err)
	}
	// Another process (a fresh Store) reveals it too, and the file holds a
	// version 2 record with a sealed copy, never the password itself.
	other := access.NewStore(dir, newClock().now)
	other.Sealer = k
	if got, err := other.Reveal(id, k.Open); err != nil || got != pw {
		t.Fatalf("a fresh store's Reveal = %d chars, %v", len(got), err)
	}
	f := readClients(t, dir)
	if f.Version != 2 || f.Clients[0]["sealed"] == nil || f.Clients[0]["sealed"] == "" {
		t.Errorf("file = %+v, want version 2 with a sealed copy", f)
	}
	if _, err := s.Reveal("c0000000000000000", k.Open); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("Reveal(unknown) = %v, want ErrNotFound", err)
	}
}

func TestRevealRefusesAnotherClientsAAD(t *testing.T) {
	t.Parallel()
	s, k, dir := sealedStore(t)
	idA, pwA, _ := s.Add(t.Context(), "a")
	idB, _, _ := s.Add(t.Context(), "b")
	f := readClients(t, dir)
	f.Clients[1]["sealed"] = f.Clients[0]["sealed"] // a's sealed copy moved to b
	writeClients(t, dir, f)
	got, err := s.Reveal(idB, k.Open)
	if !errors.Is(err, access.ErrNotRecoverable) || !errors.Is(err, seal.ErrCorrupt) || got != "" {
		t.Fatalf("Reveal(b) with a's blob = %d chars, %v; want ErrNotRecoverable caused by seal.ErrCorrupt (the AAD names the client)", len(got), err)
	}
	if strings.Contains(err.Error(), pwA) {
		t.Error("the error carries a password")
	}
	if got, err := s.Reveal(idA, k.Open); err != nil || got != pwA {
		t.Errorf("Reveal(a) = %d chars, %v; want a's password", len(got), err)
	}
}

func TestV1ClientsStillCheckAndAreNotRecoverable(t *testing.T) {
	t.Parallel()
	s, k, dir := sealedStore(t)
	// A file pcb wrote before decision 0013: version 1, salt and hash only.
	salt := []byte("0123456789abcdef")
	const pw = "an-old-password-of-32-characters"
	sum := sha256.Sum256(append(append([]byte(nil), salt...), pw...))
	writeClients(t, dir, clientsFile{Version: 1, Clients: []map[string]any{{
		"id": "c00000000000000aa", "name": "Old", "created": "2026-11-01T10:00:00Z",
		"salt": base64.StdEncoding.EncodeToString(salt), "hash": base64.StdEncoding.EncodeToString(sum[:]),
	}}})
	if c, ok := s.Check(pw); !ok || c.Name != "Old" {
		t.Fatalf("Check(v1 password) = %+v, %v; want the v1 client", c, ok)
	}
	got, err := s.Reveal("c00000000000000aa", k.Open)
	if !errors.Is(err, access.ErrNotRecoverable) || !errors.Is(err, access.ErrNoSealedCopy) || got != "" {
		t.Fatalf("Reveal(v1) = %d chars, %v; want ErrNotRecoverable caused by ErrNoSealedCopy", len(got), err)
	}
	// The next change writes version 2 and keeps the v1 record as it was.
	if _, _, err := s.Add(t.Context(), "New"); err != nil {
		t.Fatal(err)
	}
	f := readClients(t, dir)
	if f.Version != 2 || len(f.Clients) != 2 || f.Clients[0]["sealed"] != nil {
		t.Fatalf("after an add: %+v; want version 2 with the v1 record unsealed", f)
	}
	if _, ok := access.NewStore(dir, newClock().now).Check(pw); !ok {
		t.Fatal("the v1 password stopped working after the rewrite")
	}
	// Rotating it makes it recoverable.
	_, npw, err := s.Rotate(t.Context(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Reveal("c00000000000000aa", k.Open); err != nil || got != npw {
		t.Errorf("Reveal after rotate = %d chars, %v; want the new password", len(got), err)
	}
}

func TestRotateInvalidatesTheOldPassword(t *testing.T) {
	t.Parallel()
	s, k, dir := sealedStore(t)
	id, old, _ := s.Add(t.Context(), "Phone")
	_, other, _ := s.Add(t.Context(), "Laptop")
	before, _ := s.List()
	serve := access.NewStore(dir, newClock().now) // the daemon, holding the old file
	if _, ok := serve.Check(old); !ok {
		t.Fatal("setup")
	}
	c, pw, err := s.Rotate(t.Context(), "PHONE")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != id || c.Name != "Phone" || !c.Created.Equal(before[0].Created) || pw == old || len(pw) != 32 {
		t.Fatalf("Rotate = %+v, %d chars; want the same client with a new 32-character password", c, len(pw))
	}
	if _, ok := serve.Check(old); ok {
		t.Error("the old password still passes after a rotate")
	}
	if got, ok := serve.Check(pw); !ok || got.ID != id {
		t.Errorf("the new password: %+v, %v", got, ok)
	}
	if _, ok := serve.Check(other); !ok {
		t.Error("rotating one client broke another")
	}
	if got, err := s.Reveal(id, k.Open); err != nil || got != pw {
		t.Errorf("Reveal after rotate = %d chars, %v", len(got), err)
	}
	if c2, _, err := s.Rotate(t.Context(), id); err != nil || c2.ID != id {
		t.Errorf("Rotate by ID = %+v, %v", c2, err)
	}
	if _, _, err := s.Rotate(t.Context(), "nobody"); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("Rotate(unknown) = %v, want ErrNotFound", err)
	}
}

func TestRevealWithoutTheKeyIsNotRecoverableAndCheckStillWorks(t *testing.T) {
	t.Parallel()
	s, k, _ := sealedStore(t)
	id, pw, _ := s.Add(t.Context(), "Phone")
	if err := os.Remove(k.Path); err != nil {
		t.Fatal(err)
	}
	got, err := s.Reveal(id, k.Open)
	if !errors.Is(err, access.ErrNotRecoverable) || !errors.Is(err, seal.ErrNoKey) || got != "" {
		t.Fatalf("Reveal without the key = %d chars, %v; want ErrNotRecoverable caused by seal.ErrNoKey", len(got), err)
	}
	if _, ok := s.Check(pw); !ok {
		t.Error("the check needs the key: it must not")
	}
}

func TestAddFirstAddsOnlyToAnEmptyListOnce(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	k := &seal.KeyFile{Path: filepath.Join(filepath.Dir(dir), "secrets.key")}
	var wg sync.WaitGroup
	var mu sync.Mutex
	added := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := access.NewStore(dir, newClock().now) // one store per "process"
			s.Sealer = k
			_, _, ok, err := s.AddFirst(t.Context(), "default")
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				added++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	l, err := access.NewStore(dir, nil).List()
	if err != nil || len(l) != 1 || l[0].Name != "default" || added != 1 {
		t.Fatalf("after 8 racing AddFirst: %+v (%v), %d reported added; want one default", l, err, added)
	}
	// Once any client exists, it adds nothing, even after default is gone.
	s := access.NewStore(dir, nil)
	s.Sealer = k
	if _, _, err := s.Add(t.Context(), "Phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.AddFirst(t.Context(), "default"); ok || err != nil {
		t.Errorf("AddFirst with a client = %v, %v; want nothing added", ok, err)
	}
	if l, _ := s.List(); len(l) != 1 || l[0].Name != "Phone" {
		t.Errorf("clients = %+v, want Phone only", l)
	}
}

// A sealed copy that opens but is not the password the hash checks (a
// hand-edited file) is never shown: show must print what the check accepts.
func TestRevealRefusesASealedCopyThatIsNotThePassword(t *testing.T) {
	t.Parallel()
	s, k, dir := sealedStore(t)
	idA, _, _ := s.Add(t.Context(), "a")
	_, pwB, _ := s.Add(t.Context(), "b")
	f := readClients(t, dir)
	f.Clients[0]["salt"], f.Clients[0]["hash"] = f.Clients[1]["salt"], f.Clients[1]["hash"] // a now checks b's password
	writeClients(t, dir, f)
	if _, ok := s.Check(pwB); !ok {
		t.Fatal("setup: b's password no longer checks")
	}
	got, err := s.Reveal(idA, k.Open)
	if !errors.Is(err, access.ErrNotRecoverable) || errors.Is(err, seal.ErrCorrupt) || got != "" {
		t.Fatalf("Reveal = %d chars, %v; want ErrNotRecoverable for a copy that opens but does not match the hash", len(got), err)
	}
}
