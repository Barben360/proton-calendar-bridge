package access

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

// The MCP bearer tokens (design §5.2): who may use the remote /mcp. Each
// token has a name, the capabilities and scope it grants, and a random
// secret made by Add and shown once. <state>/mcp-tokens.json (0600,
// written atomically, format version 1) keeps sha256(salt ‖ secret) only:
// never a sealed copy, so a lost token is removed and added again.

// The causes of the errors a TokenStore returns for a bad request; anything
// else is the file's or the system's fault.
var (
	ErrInvalidTokenName = errors.New("invalid MCP token name")
	ErrTokenExists      = errors.New("an MCP token with this name already exists")
	ErrTokenNotFound    = errors.New("no MCP token")
	ErrInvalidCaps      = errors.New("invalid MCP token capabilities")
)

// TokenPrefix starts every MCP token, so that a CalDAV password never
// passes as one (nor the reverse).
const TokenPrefix = "pcbm_"

// TokenCaps are the capabilities a token may grant, in the order they are
// shown. login and manage are stdio only (design §5.4): never a token's.
var TokenCaps = []string{"freebusy", "read", "write", "delete"}

const (
	tokenFile        = "mcp-tokens.json"
	tokenLock        = "mcp-tokens.lock"
	tokenFileVersion = 1
	tokenBytes       = 32 // of entropy: 43 base64url characters
)

// tokenLen is the length of a token: its prefix and its base64url body.
var tokenLen = len(TokenPrefix) + base64.RawURLEncoding.EncodedLen(tokenBytes)

// Token is an MCP token as List and Verify show it: never its secret.
// Accounts name accounts by address or local ID (none: every account);
// Calendars name calendar IDs (none: every calendar). LastUsed is zero
// until its first recorded use; Expires nil never expires.
type Token struct {
	Name      string     `json:"name"`
	Caps      []string   `json:"caps"`
	Accounts  []string   `json:"accounts,omitempty"`
	Calendars []string   `json:"calendars,omitempty"`
	Created   time.Time  `json:"created"`
	LastUsed  time.Time  `json:"last_used,omitzero"`
	Expires   *time.Time `json:"expires,omitempty"`
}

// clone is t with slices of its own, so a caller cannot change a store's.
func (t Token) clone() Token {
	t.Caps = slices.Clone(t.Caps)
	t.Accounts = slices.Clone(t.Accounts)
	t.Calendars = slices.Clone(t.Calendars)
	if t.Expires != nil {
		e := *t.Expires
		t.Expires = &e
	}
	return t
}

// expired reports whether t no longer verifies at now.
func (t Token) expired(now time.Time) bool { return t.Expires != nil && !now.Before(*t.Expires) }

type tokenRecord struct {
	Token
	Salt string `json:"salt"` // base64
	Hash string `json:"hash"` // base64 of sha256(salt ‖ secret)

	salt, hash []byte // decoded
}

type tokenFileFormat struct {
	Version *int          `json:"version"`
	Tokens  []tokenRecord `json:"tokens"`
}

// TokenStore is the MCP token list over <stateRoot>/mcp-tokens.json. Verify
// reloads the file when it changed, so a running daemon sees tokens added
// or removed by pcb mcp token without a restart. Mutations take
// <stateRoot>/mcp-tokens.lock between processes. Safe for concurrent use.
type TokenStore struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	loaded  bool
	state   fileState
	tokens  []tokenRecord
	loadErr error
}

// NewTokenStore returns the TokenStore of stateRoot on now (nil: time.Now).
// It reads and creates nothing until used.
func NewTokenStore(stateRoot string, now func() time.Time) *TokenStore {
	if now == nil {
		now = time.Now
	}
	return &TokenStore{dir: stateRoot, now: now}
}

func (s *TokenStore) path() string { return filepath.Join(s.dir, tokenFile) }

// refresh loads the file when it changed since the last load (or always,
// with force). On failure the list is emptied, so Verify fails closed, and
// the error is kept until the file changes. s.mu must be held.
func (s *TokenStore) refresh(force bool) error {
	st, err := stateOf(s.path())
	if err != nil {
		s.loaded, s.tokens, s.loadErr = false, nil, fmt.Errorf("MCP token list unreadable: %w", err)
		return s.loadErr
	}
	if s.loaded && !force && st.same(s.state) {
		return s.loadErr
	}
	s.loaded, s.state, s.tokens, s.loadErr = true, st, nil, nil
	if !st.exists {
		return nil
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		s.loaded, s.loadErr = false, fmt.Errorf("MCP token list unreadable: %w", err)
		return s.loadErr
	}
	tokens, err := decodeTokens(data)
	if err != nil {
		s.loadErr = fmt.Errorf("MCP token list %s: %w", s.path(), err)
		return s.loadErr
	}
	s.tokens = tokens
	return nil
}

// decodeTokens reads a token file strictly: no unknown field, no trailing
// data, format version 1, every record well formed. Its errors never quote
// the file.
func decodeTokens(data []byte) ([]tokenRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f tokenFileFormat
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New("is not valid JSON of the expected shape")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("has data after its end")
	}
	if f.Version == nil || *f.Version != tokenFileVersion {
		v := "none"
		if f.Version != nil {
			v = fmt.Sprint(*f.Version)
		}
		return nil, fmt.Errorf("has format version %s, this pcb reads %d", v, tokenFileVersion)
	}
	for i := range f.Tokens {
		r := &f.Tokens[i]
		var e1, e2 error
		r.salt, e1 = base64.StdEncoding.DecodeString(r.Salt)
		r.hash, e2 = base64.StdEncoding.DecodeString(r.Hash)
		if e1 != nil || e2 != nil || len(r.salt) == 0 || len(r.hash) != sha256.Size ||
			ValidateTokenName(r.Name) != nil || checkCaps(r.Caps) != nil {
			return nil, fmt.Errorf("holds a malformed entry (number %d)", i+1)
		}
		for _, o := range f.Tokens[:i] {
			if strings.EqualFold(o.Name, r.Name) {
				return nil, fmt.Errorf("holds two tokens named %q", r.Name)
			}
		}
	}
	return f.Tokens, nil
}

// ValidateTokenName checks an MCP token name: the client name rules (1 to
// 64 characters, no control character, no leading or trailing space). Its
// errors wrap ErrInvalidTokenName.
func ValidateTokenName(name string) error { return checkName(name, ErrInvalidTokenName) }

// checkCaps refuses an empty list, a capability a token cannot hold
// (login, manage: stdio only) and an unknown one.
func checkCaps(caps []string) error {
	if len(caps) == 0 {
		return fmt.Errorf("%w: none given", ErrInvalidCaps)
	}
	for _, c := range caps {
		if !slices.Contains(TokenCaps, c) {
			return fmt.Errorf("%w: %q is not one of %s", ErrInvalidCaps, c, strings.Join(TokenCaps, ", "))
		}
	}
	return nil
}

// save writes the list; s.mu and the lock must be held.
func (s *TokenStore) save(tokens []tokenRecord) error {
	v := tokenFileVersion
	if tokens == nil {
		tokens = []tokenRecord{}
	}
	data, err := json.MarshalIndent(tokenFileFormat{Version: &v, Tokens: tokens}, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path(), append(data, '\n'))
}

// mutate runs fn on a fresh copy of the file under both locks and writes
// the result when fn returns true.
func (s *TokenStore) mutate(ctx context.Context, fn func(tokens []tokenRecord) ([]tokenRecord, bool, error)) error {
	unlock, err := lockDir(ctx, s.dir, tokenLock)
	if err != nil {
		return err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(true); err != nil {
		return err // never overwrite a file we cannot read
	}
	next, write, err := fn(s.tokens)
	if err != nil || !write {
		return err
	}
	if err := s.save(next); err != nil {
		return err
	}
	return s.refresh(true)
}

// Add stores t (its Created set to now, LastUsed cleared) and returns its
// secret, which nothing keeps: the file holds a salted hash only. The name
// follows the client name rules and is unique (case-insensitive); the
// capabilities are among TokenCaps.
func (s *TokenStore) Add(ctx context.Context, t Token) (secret string, err error) {
	if err := ValidateTokenName(t.Name); err != nil {
		return "", err
	}
	if err := checkCaps(t.Caps); err != nil {
		return "", err
	}
	raw, err := randomBytes(tokenBytes)
	if err != nil {
		return "", err
	}
	salt, err := randomBytes(saltLen)
	if err != nil {
		return "", err
	}
	secret = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	clear(raw)
	t = t.clone()
	t.Created, t.LastUsed = s.now().UTC().Truncate(time.Second), time.Time{}
	if t.Expires != nil {
		e := t.Expires.UTC()
		t.Expires = &e
	}
	rec := tokenRecord{Token: t, Salt: base64.StdEncoding.EncodeToString(salt),
		Hash: base64.StdEncoding.EncodeToString(digest(salt, secret))}
	err = s.mutate(ctx, func(ts []tokenRecord) ([]tokenRecord, bool, error) {
		for _, o := range ts {
			if strings.EqualFold(o.Name, t.Name) {
				return nil, false, fmt.Errorf("%w: %q", ErrTokenExists, o.Name)
			}
		}
		return append(ts[:len(ts):len(ts)], rec), true, nil
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// List returns the tokens in creation order, expired ones included.
func (s *TokenStore) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(false); err != nil {
		return nil, err
	}
	out := make([]Token, len(s.tokens))
	for i, r := range s.tokens {
		out[i] = r.clone()
	}
	return out, nil
}

// Remove deletes the token named name (case-insensitively) and returns it.
// Its secret fails the next Verify of every TokenStore.
func (s *TokenStore) Remove(ctx context.Context, name string) (Token, error) {
	var gone Token
	err := s.mutate(ctx, func(ts []tokenRecord) ([]tokenRecord, bool, error) {
		for i, r := range ts {
			if strings.EqualFold(r.Name, name) {
				gone = r.clone()
				return append(append([]tokenRecord(nil), ts[:i]...), ts[i+1:]...), true, nil
			}
		}
		return nil, false, fmt.Errorf("%w %q", ErrTokenNotFound, name)
	})
	return gone, err
}

// Verify reports the token whose secret this is. ok is false for a secret
// without the pcbm_ prefix and length, an unknown one and an expired one.
// It compares against every token in constant time and does not return
// early. err says why the token file could not be loaded (secret-free);
// then no secret passes.
func (s *TokenStore) Verify(secret string) (Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(false); err != nil {
		return Token{}, false, err
	}
	if len(secret) != tokenLen || !strings.HasPrefix(secret, TokenPrefix) {
		return Token{}, false, nil
	}
	var found Token
	ok := 0
	for _, r := range s.tokens {
		match := subtle.ConstantTimeCompare(digest(r.salt, secret), r.hash)
		if match == 1 {
			found = r.Token
		}
		ok |= match
	}
	if ok != 1 || found.expired(s.now()) {
		return Token{}, false, nil
	}
	return found.clone(), true, nil
}

// Touch records that the token named name was just used. It is throttled
// to once a minute per token (a skipped call touches no file), and a token
// that no longer exists is not an error.
func (s *TokenStore) Touch(ctx context.Context, name string) error {
	s.mu.Lock()
	var last time.Time
	known := false
	for _, r := range s.tokens {
		if r.Name == name {
			last, known = r.LastUsed, true
		}
	}
	s.mu.Unlock()
	now := s.now().UTC().Truncate(time.Second)
	if known && now.Sub(last) < touchEvery {
		return nil
	}
	return s.mutate(ctx, func(ts []tokenRecord) ([]tokenRecord, bool, error) {
		next := slices.Clone(ts)
		for i := range next {
			if next[i].Name == name && now.Sub(next[i].LastUsed) >= touchEvery {
				next[i].LastUsed = now
				return next, true, nil
			}
		}
		return nil, false, nil // gone, or another process touched it just now
	})
}
