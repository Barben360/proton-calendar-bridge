// Package access holds the CalDAV app passwords: who may read the bridge.
// Each client (a Thunderbird, a phone) has a name and a random password
// made by Add. <state>/clients.json (0600, written atomically) keeps
// sha256(salt ‖ password) for the constant-time check and, since decision
// 0013, a copy of the password sealed by the Store's Sealer (the key of
// secrets.sealed), so that it can be shown again (Reveal).
package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// The causes of the errors a Store returns for a bad request; anything else
// is the file's or the system's fault.
var (
	ErrInvalidName = errors.New("invalid client name")
	ErrDuplicate   = errors.New("a client with this name already exists")
	ErrNotFound    = errors.New("no such client")
	// ErrNotRecoverable is Reveal's refusal: the cause it wraps says why
	// (ErrNoSealedCopy, or the sealer's error: seal.ErrNoKey, seal.ErrCorrupt...).
	ErrNotRecoverable = errors.New("the client's password cannot be shown")
	// ErrNoSealedCopy: the client was made before pcb kept a sealed copy
	// (a version 1 record), or with no Sealer; only rotating it gives one.
	ErrNoSealedCopy = errors.New("no sealed copy of it is kept")
	// ErrStale: the sealed copy opens but is not the password the hash
	// checks (a hand-edited file).
	ErrStale = errors.New("its sealed copy does not match its hash")
	// ErrSeal: a new password could not be sealed (the Sealer's error is
	// wrapped too); nothing was stored.
	ErrSeal = errors.New("the client's password could not be sealed")
)

// Sealer seals a client's password beside its hash; seal.Sealer satisfies
// it. Name is in the AAD.
type Sealer interface {
	Name() string
	Seal(plaintext, aad []byte) ([]byte, error)
}

// aad binds a sealed password to its sealer and its client: a sealed copy
// moved to another record does not open.
func aad(sealer, id string) []byte { return []byte("pcb-clients/v2/" + sealer + "/" + id) }

const (
	fileName    = "clients.json"
	lockName    = "clients.lock"
	fileVersion = 2 // written; version 1 (no sealed copies) still reads
	maxNameLen  = 64
	passwordLen = 24 // bytes of entropy
	saltLen     = 16
	touchEvery  = time.Minute
)

// Client is a client as List shows it. LastUsed is zero until its first
// successful check is recorded.
type Client struct {
	ID       string
	Name     string
	Created  time.Time
	LastUsed time.Time
}

type record struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Salt string `json:"salt"` // base64
	Hash string `json:"hash"` // base64 of sha256(salt ‖ password)
	// Sealed is base64 of the password sealed with aad(sealer, ID); empty
	// in a version 1 record, which stays valid but cannot be shown.
	Sealed   string    `json:"sealed,omitempty"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`

	salt, hash, sealed []byte // decoded
}

type fileFormat struct {
	Version int      `json:"version"`
	Clients []record `json:"clients"`
}

// fileState identifies the version of the file a Store has loaded.
// A write by atomicfile replaces the file, so its identity (device and
// inode, with the change time where the system has one) changes even when a
// remove and an add of equal size land in one timestamp tick.
type fileState struct {
	exists bool
	mtime  time.Time
	size   int64
	ctime  int64       // change time in ns, 0 where unknown
	info   os.FileInfo // for os.SameFile
}

func (a fileState) same(b fileState) bool {
	if a.exists != b.exists || !a.mtime.Equal(b.mtime) || a.size != b.size || a.ctime != b.ctime {
		return false
	}
	return !a.exists || os.SameFile(a.info, b.info)
}

// Store is the client list over <dir>/clients.json. Check reloads the file
// when its modification time or size changed, so a running process sees
// clients added or removed by another one without a restart. Mutations take
// <dir>/clients.lock between processes. Safe for concurrent use.
type Store struct {
	dir string
	now func() time.Time

	// Sealer, set before first use, seals each new password beside its
	// hash (Add, AddFirst, Rotate); nil keeps the hash only.
	Sealer Sealer

	mu      sync.Mutex
	loaded  bool
	state   fileState
	clients []record
	loadErr error
}

// processMu stands in for the file lock where there is none (non-Unix
// systems: filelock.ErrUnsupported): it serialises the writers of one
// process only, so two processes writing at the same moment may lose an
// update there. On Unix it is held too, which is harmless.
var processMu sync.Mutex

// NewStore returns the Store of dir (the state directory) on now (nil:
// time.Now). It reads and creates nothing until used.
func NewStore(dir string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{dir: dir, now: now}
}

func (s *Store) path() string { return filepath.Join(s.dir, fileName) }

func stateOf(path string) (fileState, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileState{}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	return fileState{exists: true, mtime: fi.ModTime(), size: fi.Size(), ctime: changeTime(fi), info: fi}, nil
}

// refresh loads the file when it changed since the last load (or always,
// with force). On failure the list is emptied, so Check fails closed, and
// the error is kept until the file changes. s.mu must be held.
func (s *Store) refresh(force bool) error {
	st, err := stateOf(s.path())
	if err != nil {
		s.loaded, s.clients, s.loadErr = false, nil, fmt.Errorf("client list unreadable: %w", err)
		return s.loadErr
	}
	if s.loaded && !force && st.same(s.state) {
		return s.loadErr
	}
	s.loaded, s.state, s.clients, s.loadErr = true, st, nil, nil
	if !st.exists {
		return nil
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		s.loaded, s.loadErr = false, fmt.Errorf("client list unreadable: %w", err)
		return s.loadErr
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		s.loadErr = errors.New("client list is not valid JSON")
		return s.loadErr
	}
	if f.Version != 1 && f.Version != fileVersion {
		s.loadErr = fmt.Errorf("client list has format version %d, this pcb reads 1 and %d", f.Version, fileVersion)
		return s.loadErr
	}
	for i := range f.Clients {
		r := &f.Clients[i]
		var e1, e2, e3 error
		r.salt, e1 = base64.StdEncoding.DecodeString(r.Salt)
		r.hash, e2 = base64.StdEncoding.DecodeString(r.Hash)
		r.sealed, e3 = base64.StdEncoding.DecodeString(r.Sealed)
		if f.Version == 1 && r.Sealed != "" {
			e3 = errors.New("a sealed copy in a version 1 file")
		}
		if e1 != nil || e2 != nil || e3 != nil || len(r.hash) != sha256.Size {
			s.loadErr = errors.New("client list holds a malformed entry")
			return s.loadErr
		}
	}
	s.clients = f.Clients
	return nil
}

// save writes the list; s.mu and the lock must be held.
func (s *Store) save(clients []record) error {
	out := fileFormat{Version: fileVersion, Clients: clients}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := atomicfile.Write(s.path(), append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// mutate runs fn on a fresh copy of the file under both locks and writes
// the result when fn returns true.
func (s *Store) mutate(ctx context.Context, fn func(clients []record) ([]record, bool, error)) error {
	processMu.Lock()
	defer processMu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	unlock, err := filelock.Lock(ctx, filepath.Join(s.dir, lockName))
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		// No file lock here: processMu is all there is.
	case err != nil:
		return err
	default:
		defer unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(true); err != nil {
		return err // never overwrite a file we cannot read
	}
	next, write, err := fn(s.clients)
	if err != nil || !write {
		return err
	}
	if err := s.save(next); err != nil {
		return err
	}
	return s.refresh(true)
}

// ValidateName checks a client name: 1 to 64 characters, no control
// character, no leading or trailing space.
func ValidateName(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case n == 0:
		return fmt.Errorf("%w: it is empty", ErrInvalidName)
	case n > maxNameLen:
		return fmt.Errorf("%w: %d characters, at most %d", ErrInvalidName, n, maxNameLen)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidName)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return fmt.Errorf("%w: it holds a control character", ErrInvalidName)
	case idShape.MatchString(name):
		return fmt.Errorf("%w: it looks like a client ID, which remove would take for one", ErrInvalidName)
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("%w: it starts or ends with a space", ErrInvalidName)
	}
	return nil
}

// idShape is what Add generates for IDs; Remove tries IDs before names.
var idShape = regexp.MustCompile(`^(?i:c[0-9a-f]{16})$`)

func digest(salt []byte, password string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(password))
	return h.Sum(nil)
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// credential is a new password with its salt, hash and sealed copy for
// the client id.
type credential struct {
	password           string
	salt, hash, sealed string // base64, as stored
}

// newCredential makes a random password for client id, sealed with s.Sealer
// when there is one. A sealing failure is returned: a client is never
// stored without the copy the Store was asked to keep.
func (s *Store) newCredential(id string) (credential, error) {
	raw, err := randomBytes(passwordLen)
	if err != nil {
		return credential{}, err
	}
	salt, err := randomBytes(saltLen)
	if err != nil {
		return credential{}, err
	}
	c := credential{
		password: base64.RawURLEncoding.EncodeToString(raw),
		salt:     base64.StdEncoding.EncodeToString(salt),
	}
	c.hash = base64.StdEncoding.EncodeToString(digest(salt, c.password))
	if s.Sealer != nil {
		sealed, err := s.Sealer.Seal([]byte(c.password), aad(s.Sealer.Name(), id))
		if err != nil {
			return credential{}, fmt.Errorf("%w: %w", ErrSeal, err)
		}
		c.sealed = base64.StdEncoding.EncodeToString(sealed)
	}
	return c, nil
}

func newID() (string, error) {
	idb, err := randomBytes(8)
	if err != nil {
		return "", err
	}
	return "c" + hex.EncodeToString(idb), nil
}

// newRecord is a client named name with a new credential.
func (s *Store) newRecord(name string) (record, string, error) {
	id, err := newID()
	if err != nil {
		return record{}, "", err
	}
	c, err := s.newCredential(id)
	if err != nil {
		return record{}, "", err
	}
	return record{
		ID: id, Name: name, Created: s.now().UTC().Truncate(time.Second),
		Salt: c.salt, Hash: c.hash, Sealed: c.sealed,
	}, c.password, nil
}

// Add creates a client named name and returns its ID and its password. The
// file keeps a salted hash and, with a Sealer, a sealed copy (Reveal).
func (s *Store) Add(ctx context.Context, name string) (id, password string, err error) {
	if err := ValidateName(name); err != nil {
		return "", "", err
	}
	rec, password, err := s.newRecord(name)
	if err != nil {
		return "", "", err
	}
	err = s.mutate(ctx, func(cs []record) ([]record, bool, error) {
		for _, c := range cs {
			if strings.EqualFold(c.Name, name) {
				return nil, false, fmt.Errorf("%w: %q", ErrDuplicate, c.Name)
			}
		}
		return append(cs[:len(cs):len(cs)], rec), true, nil
	})
	if err != nil {
		return "", "", err
	}
	return rec.ID, password, nil
}

// AddFirst adds a client named name only when the list holds no client,
// deciding under the clients lock, so racing callers (in any process) add
// it once. added is false, with no error, when a client exists.
func (s *Store) AddFirst(ctx context.Context, name string) (id, password string, added bool, err error) {
	if err := ValidateName(name); err != nil {
		return "", "", false, err
	}
	rec, password, err := s.newRecord(name)
	if err != nil {
		return "", "", false, err
	}
	err = s.mutate(ctx, func(cs []record) ([]record, bool, error) {
		if len(cs) > 0 {
			return nil, false, nil
		}
		added = true
		return []record{rec}, true, nil
	})
	if err != nil || !added {
		return "", "", false, err
	}
	return rec.ID, password, true, nil
}

// find is the index of the client whose ID is idOrName, or else whose name
// is (case-insensitively); -1 when none.
func find(cs []record, idOrName string) int {
	for i, c := range cs {
		if c.ID == idOrName {
			return i
		}
	}
	for i, c := range cs {
		if strings.EqualFold(c.Name, idOrName) {
			return i
		}
	}
	return -1
}

// Get returns the client whose ID is idOrName, or else whose name is
// (case-insensitively).
func (s *Store) Get(idOrName string) (Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(false); err != nil {
		return Client{}, err
	}
	at := find(s.clients, idOrName)
	if at < 0 {
		return Client{}, fmt.Errorf("%w: %q", ErrNotFound, idOrName)
	}
	return s.clients[at].client(), nil
}

// Rotate gives the client whose ID is idOrName, or else whose name is, a
// new password: a new salt, hash and sealed copy in one write. The old
// password fails the next check of every Store (they reload on change). A
// version 1 record becomes recoverable.
func (s *Store) Rotate(ctx context.Context, idOrName string) (Client, string, error) {
	var out Client
	var password string
	err := s.mutate(ctx, func(cs []record) ([]record, bool, error) {
		at := find(cs, idOrName)
		if at < 0 {
			return nil, false, fmt.Errorf("%w: %q", ErrNotFound, idOrName)
		}
		c, err := s.newCredential(cs[at].ID)
		if err != nil {
			return nil, false, err
		}
		next := append([]record(nil), cs...)
		next[at].Salt, next[at].Hash, next[at].Sealed = c.salt, c.hash, c.sealed
		out, password = next[at].client(), c.password
		return next, true, nil
	})
	if err != nil {
		return Client{}, "", err
	}
	return out, password, nil
}

// Reveal opens the sealed copy of client id's password with open (the
// Store's Sealer's Open). It fails with ErrNotFound for an unknown ID, and
// with ErrNotRecoverable wrapping the cause for a record with no sealed
// copy (ErrNoSealedCopy), one the key cannot open (open's error), or one
// that is not the password the hash checks. Errors never carry a password.
func (s *Store) Reveal(id string, open func(sealed, aad []byte) ([]byte, error)) (password string, err error) {
	s.mu.Lock()
	if err := s.refresh(false); err != nil {
		s.mu.Unlock()
		return "", err
	}
	at := find(s.clients, id)
	if at < 0 || s.clients[at].ID != id {
		s.mu.Unlock()
		return "", fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	r := s.clients[at]
	s.mu.Unlock()
	if len(r.sealed) == 0 || s.Sealer == nil {
		return "", fmt.Errorf("%w: %w", ErrNotRecoverable, ErrNoSealedCopy)
	}
	plain, err := open(r.sealed, aad(s.Sealer.Name(), r.ID))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotRecoverable, err)
	}
	password = string(plain)
	clear(plain)
	if subtle.ConstantTimeCompare(digest(r.salt, password), r.hash) != 1 {
		return "", fmt.Errorf("%w: %w", ErrNotRecoverable, ErrStale)
	}
	return password, nil
}

func (r record) client() Client {
	return Client{ID: r.ID, Name: r.Name, Created: r.Created, LastUsed: r.LastUsed}
}

// List returns the clients in creation order.
func (s *Store) List() ([]Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(false); err != nil {
		return nil, err
	}
	out := make([]Client, len(s.clients))
	for i, r := range s.clients {
		out[i] = r.client()
	}
	return out, nil
}

// Remove deletes the client whose ID is idOrName, or else whose name is
// (case-insensitively), and returns it.
func (s *Store) Remove(ctx context.Context, idOrName string) (Client, error) {
	var gone Client
	err := s.mutate(ctx, func(cs []record) ([]record, bool, error) {
		at := find(cs, idOrName)
		if at < 0 {
			return nil, false, fmt.Errorf("%w: %q", ErrNotFound, idOrName)
		}
		gone = cs[at].client()
		next := append(append([]record(nil), cs[:at]...), cs[at+1:]...)
		return next, true, nil
	})
	return gone, err
}

// Check reports the client whose password this is. It compares against
// every client in constant time and does not return early, so neither the
// answer's time nor the position of the match tells anything. An unreadable
// client file refuses everyone.
func (s *Store) Check(password string) (Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(false); err != nil {
		return Client{}, false
	}
	var found Client
	ok := 0
	for _, r := range s.clients {
		match := subtle.ConstantTimeCompare(digest(r.salt, password), r.hash)
		if match == 1 {
			found = r.client()
		}
		ok |= match
	}
	return found, ok == 1
}

// Touch records that the client id was just used. It is throttled to once a
// minute per client (a skipped call touches no file), and a client that no
// longer exists is not an error.
func (s *Store) Touch(ctx context.Context, id string) error {
	s.mu.Lock()
	var last time.Time
	known := false
	for _, r := range s.clients {
		if r.ID == id {
			last, known = r.LastUsed, true
		}
	}
	s.mu.Unlock()
	now := s.now().UTC().Truncate(time.Second)
	if known && now.Sub(last) < touchEvery {
		return nil
	}
	return s.mutate(ctx, func(cs []record) ([]record, bool, error) {
		next := append([]record(nil), cs...)
		for i := range next {
			if next[i].ID == id && now.Sub(next[i].LastUsed) >= touchEvery {
				next[i].LastUsed = now
				return next, true, nil
			}
		}
		return nil, false, nil // gone, or another process touched it just now
	})
}
