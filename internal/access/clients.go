// Package access holds the CalDAV app passwords: who may read the bridge.
// Each client (a Thunderbird, a phone) has a name and a random password
// printed once by Add; only sha256(salt ‖ password) is stored, in
// <state>/clients.json (0600, written atomically).
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
)

const (
	fileName    = "clients.json"
	lockName    = "clients.lock"
	fileVersion = 1
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
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Salt     string    `json:"salt"` // base64
	Hash     string    `json:"hash"` // base64 of sha256(salt ‖ password)
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`

	salt, hash []byte // decoded
}

type fileFormat struct {
	Version int      `json:"version"`
	Clients []record `json:"clients"`
}

// fileState identifies the version of the file a Store has loaded.
type fileState struct {
	exists bool
	mtime  time.Time
	size   int64
}

// Store is the client list over <dir>/clients.json. Check reloads the file
// when its modification time or size changed, so a running process sees
// clients added or removed by another one without a restart. Mutations take
// <dir>/clients.lock between processes. Safe for concurrent use.
type Store struct {
	dir string
	now func() time.Time

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
	return fileState{exists: true, mtime: fi.ModTime(), size: fi.Size()}, nil
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
	if s.loaded && !force && st == s.state {
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
	if f.Version != fileVersion {
		s.loadErr = fmt.Errorf("client list has format version %d, this pcb reads %d", f.Version, fileVersion)
		return s.loadErr
	}
	for i := range f.Clients {
		r := &f.Clients[i]
		var e1, e2 error
		r.salt, e1 = base64.StdEncoding.DecodeString(r.Salt)
		r.hash, e2 = base64.StdEncoding.DecodeString(r.Hash)
		if e1 != nil || e2 != nil || len(r.hash) != sha256.Size {
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
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("%w: it starts or ends with a space", ErrInvalidName)
	}
	return nil
}

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

// Add creates a client named name and returns its ID and its password, which
// is shown this once: only a salted hash is stored.
func (s *Store) Add(ctx context.Context, name string) (id, password string, err error) {
	if err := ValidateName(name); err != nil {
		return "", "", err
	}
	raw, err := randomBytes(passwordLen)
	if err != nil {
		return "", "", err
	}
	salt, err := randomBytes(saltLen)
	if err != nil {
		return "", "", err
	}
	idb, err := randomBytes(8)
	if err != nil {
		return "", "", err
	}
	id, password = "c"+hex.EncodeToString(idb), base64.RawURLEncoding.EncodeToString(raw)
	err = s.mutate(ctx, func(cs []record) ([]record, bool, error) {
		for _, c := range cs {
			if strings.EqualFold(c.Name, name) {
				return nil, false, fmt.Errorf("%w: %q", ErrDuplicate, c.Name)
			}
		}
		h := digest(salt, password)
		return append(cs[:len(cs):len(cs)], record{
			ID: id, Name: name, Created: s.now().UTC().Truncate(time.Second),
			Salt: base64.StdEncoding.EncodeToString(salt), Hash: base64.StdEncoding.EncodeToString(h),
		}), true, nil
	})
	if err != nil {
		return "", "", err
	}
	return id, password, nil
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
		at := -1
		for i, c := range cs {
			if c.ID == idOrName {
				at = i
				break
			}
		}
		if at < 0 {
			for i, c := range cs {
				if strings.EqualFold(c.Name, idOrName) {
					at = i
					break
				}
			}
		}
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
