package access_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
)

func newTokenStore(t *testing.T) (*access.TokenStore, *clock, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	c := newClock()
	return access.NewTokenStore(dir, c.now), c, dir
}

func addToken(t *testing.T, s *access.TokenStore, tok access.Token) string {
	t.Helper()
	secret, err := s.Add(context.Background(), tok)
	if err != nil {
		t.Fatalf("Add(%+v): %v", tok, err)
	}
	return secret
}

func readTokenFile(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "mcp-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeTokenFile(t *testing.T, dir string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A new file renamed over the old, as a writer does: a new inode, so
	// the store reloads even within one timestamp tick.
	tmp := filepath.Join(dir, "mcp-tokens.json.test")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "mcp-tokens.json")); err != nil {
		t.Fatal(err)
	}
}

func TestTokenAddStoresOnlyAHash(t *testing.T) {
	t.Parallel()
	s, _, dir := newTokenStore(t)
	secret := addToken(t, s, access.Token{Name: "claude", Caps: []string{"read"}})
	if !strings.HasPrefix(secret, access.TokenPrefix) || len(secret) != len(access.TokenPrefix)+43 {
		t.Fatalf("secret %q: want %s + 43 characters", secret, access.TokenPrefix)
	}
	body := strings.TrimPrefix(secret, access.TokenPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) != 32 {
		t.Fatalf("the secret's body is not 32 bytes of base64url: %d bytes, %v", len(raw), err)
	}
	file := readTokenFile(t, dir)
	for what, b := range map[string][]byte{
		"the secret":                       []byte(secret),
		"its body":                         []byte(body),
		"its raw bytes":                    raw,
		"its raw bytes in standard base64": []byte(base64.StdEncoding.EncodeToString(raw)),
		"its raw bytes in hex":             []byte(hex.EncodeToString(raw)),
	} {
		if bytes.Contains(file, b) {
			t.Errorf("the token file holds %s:\n%s", what, file)
		}
	}
	// What it does hold checks the secret: a salted SHA-256 of it.
	var f struct {
		Version int `json:"version"`
		Tokens  []struct {
			Salt, Hash string
		} `json:"tokens"`
	}
	if err := json.Unmarshal(file, &f); err != nil || f.Version != 1 || len(f.Tokens) != 1 {
		t.Fatalf("file = %s (%v), want version 1 with one token", file, err)
	}
	salt, _ := base64.StdEncoding.DecodeString(f.Tokens[0].Salt)
	sum := sha256.Sum256(append(salt, secret...))
	if len(salt) == 0 || f.Tokens[0].Hash != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Errorf("hash %q is not sha256(salt ‖ secret)", f.Tokens[0].Hash)
	}
	// Two tokens never share a secret or a salt.
	other := addToken(t, s, access.Token{Name: "other", Caps: []string{"read"}})
	if other == secret {
		t.Error("two tokens got the same secret")
	}
}

func TestTokenVerify(t *testing.T) {
	t.Parallel()
	s, c, dir := newTokenStore(t)
	exp := c.now().Add(48 * time.Hour)
	want := access.Token{Name: "claude", Caps: []string{"read", "write"}, Accounts: []string{"alice@example.com"},
		Calendars: []string{"cal-1"}, Expires: &exp}
	secret := addToken(t, s, want)
	addToken(t, s, access.Token{Name: "other", Caps: []string{"freebusy"}})

	got, ok, err := s.Verify(secret)
	if err != nil || !ok {
		t.Fatalf("Verify(the secret) = %v, %v", ok, err)
	}
	if got.Name != "claude" || !slices.Equal(got.Caps, want.Caps) || !slices.Equal(got.Accounts, want.Accounts) ||
		!slices.Equal(got.Calendars, want.Calendars) || got.Expires == nil || !got.Expires.Equal(exp) ||
		!got.Created.Equal(c.now()) || !got.LastUsed.IsZero() {
		t.Errorf("Verify = %+v, want %+v created %v", got, want, c.now())
	}
	// Another store on the same directory (another process) agrees.
	if got, ok, err := access.NewTokenStore(dir, c.now).Verify(secret); err != nil || !ok || got.Name != "claude" {
		t.Errorf("a second store: Verify = %+v, %v, %v", got, ok, err)
	}
	for _, bad := range []string{"", access.TokenPrefix, secret[:len(secret)-1], secret + "A", secret[:len(secret)-1] + flip(secret[len(secret)-1])} {
		if got, ok, err := s.Verify(bad); ok || err != nil || got.Name != "" {
			t.Errorf("Verify(%q) = %+v, %v, %v; want refused, no token", bad, got, ok, err)
		}
	}
	// Touch records the use, once a minute at most.
	if err := s.Touch(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 || !list[0].LastUsed.Equal(c.now()) || !list[1].LastUsed.IsZero() {
		t.Fatalf("after Touch: %+v, %v", list, err)
	}
	c.add(30 * time.Second)
	_ = s.Touch(context.Background(), "claude")
	if list, _ := s.List(); !list[0].LastUsed.Equal(c.now().Add(-30 * time.Second)) {
		t.Errorf("a second Touch within the minute moved last used to %v", list[0].LastUsed)
	}
	c.add(time.Minute)
	_ = s.Touch(context.Background(), "claude")
	if list, _ := s.List(); !list[0].LastUsed.Equal(c.now()) {
		t.Errorf("a Touch a minute later left last used at %v, want %v", list[0].LastUsed, c.now())
	}
	if err := s.Touch(context.Background(), "gone"); err != nil {
		t.Errorf("Touch of an unknown token: %v, want nil", err)
	}
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}

// The pcbm_ prefix keeps a CalDAV password from passing as a token, even
// with a record whose hash checks it.
func TestTokenWrongPrefixRefused(t *testing.T) {
	t.Parallel()
	s, _, dir := newTokenStore(t)
	addToken(t, s, access.Token{Name: "claude", Caps: []string{"read"}})
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	caldav := base64.RawURLEncoding.EncodeToString(raw) // what access.Store makes
	rawTok := make([]byte, 32)
	_, _ = rand.Read(rawTok)
	unprefixed := base64.RawURLEncoding.EncodeToString(rawTok) // a token's body alone
	for _, pw := range []string{caldav, unprefixed, "pcbx_" + unprefixed, strings.ToUpper(access.TokenPrefix) + unprefixed} {
		rehash(t, dir, pw)
		if got, ok, err := s.Verify(pw); ok || err != nil {
			t.Errorf("Verify(%q) with a record that hashes it = %+v, %v, %v; want refused", pw, got, ok, err)
		}
	}
	// The same doctoring for a well-formed token passes: the refusals
	// above are the prefix's.
	good := access.TokenPrefix + unprefixed
	rehash(t, dir, good)
	if _, ok, err := s.Verify(good); !ok || err != nil {
		t.Fatalf("Verify of a doctored well-formed token = %v, %v; want accepted", ok, err)
	}
}

// rehash rewrites the only record of dir's token file to check pw.
func rehash(t *testing.T, dir, pw string) {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal(readTokenFile(t, dir), &f); err != nil {
		t.Fatal(err)
	}
	rec := f["tokens"].([]any)[0].(map[string]any)
	salt := []byte("0123456789abcdef")
	sum := sha256.Sum256(append(append([]byte(nil), salt...), pw...))
	rec["salt"], rec["hash"] = base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(sum[:])
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	writeTokenFile(t, dir, b)
}

func TestTokenExpired(t *testing.T) {
	t.Parallel()
	s, c, _ := newTokenStore(t)
	exp := c.now().Add(time.Hour)
	secret := addToken(t, s, access.Token{Name: "short", Caps: []string{"read"}, Expires: &exp})
	c.add(time.Hour - time.Second)
	if _, ok, err := s.Verify(secret); !ok || err != nil {
		t.Fatalf("a second before expiry: Verify = %v, %v", ok, err)
	}
	c.add(time.Second)
	if got, ok, err := s.Verify(secret); ok || err != nil || got.Name != "" {
		t.Errorf("at expiry: Verify = %+v, %v, %v; want refused", got, ok, err)
	}
	// List still shows it, with its expiry, so a person can see why.
	if list, err := s.List(); err != nil || len(list) != 1 || list[0].Expires == nil || !list[0].Expires.Equal(exp) {
		t.Errorf("List = %+v, %v", list, err)
	}
}

func TestTokenRemove(t *testing.T) {
	t.Parallel()
	s, _, dir := newTokenStore(t)
	secret := addToken(t, s, access.Token{Name: "Claude", Caps: []string{"read"}})
	keep := addToken(t, s, access.Token{Name: "keep", Caps: []string{"read"}})
	gone, err := s.Remove(context.Background(), "claude") // names are case-insensitive
	if err != nil || gone.Name != "Claude" {
		t.Fatalf("Remove = %+v, %v", gone, err)
	}
	if _, ok, _ := s.Verify(secret); ok {
		t.Error("a removed token still verifies")
	}
	if _, ok, _ := access.NewTokenStore(dir, nil).Verify(secret); ok {
		t.Error("a removed token still verifies in another store")
	}
	if _, ok, _ := s.Verify(keep); !ok {
		t.Error("removing one token refused another")
	}
	if _, err := s.Remove(context.Background(), "claude"); !errors.Is(err, access.ErrTokenNotFound) {
		t.Errorf("removing it again: %v, want ErrTokenNotFound", err)
	}
	// Its name is free again.
	addToken(t, s, access.Token{Name: "claude", Caps: []string{"read"}})
}

func TestTokenAddRefusals(t *testing.T) {
	t.Parallel()
	s, _, dir := newTokenStore(t)
	addToken(t, s, access.Token{Name: "claude", Caps: []string{"read"}})
	before := readTokenFile(t, dir)
	for _, c := range []struct {
		tok  access.Token
		want error
	}{
		{access.Token{Name: "CLAUDE", Caps: []string{"read"}}, access.ErrTokenExists},
		{access.Token{Name: "", Caps: []string{"read"}}, access.ErrInvalidTokenName},
		{access.Token{Name: " x", Caps: []string{"read"}}, access.ErrInvalidTokenName},
		{access.Token{Name: strings.Repeat("x", 65), Caps: []string{"read"}}, access.ErrInvalidTokenName},
		{access.Token{Name: "x\n", Caps: []string{"read"}}, access.ErrInvalidTokenName},
		{access.Token{Name: "x"}, access.ErrInvalidCaps},
		{access.Token{Name: "x", Caps: []string{"login"}}, access.ErrInvalidCaps},
		{access.Token{Name: "x", Caps: []string{"read", "manage"}}, access.ErrInvalidCaps},
		{access.Token{Name: "x", Caps: []string{"admin"}}, access.ErrInvalidCaps},
	} {
		if secret, err := s.Add(context.Background(), c.tok); !errors.Is(err, c.want) || secret != "" {
			t.Errorf("Add(%+v) = %q, %v; want %v", c.tok, secret, err, c.want)
		}
	}
	if after := readTokenFile(t, dir); !bytes.Equal(before, after) {
		t.Errorf("a refused Add changed the file:\n%s", after)
	}
	if want := []string{"freebusy", "read", "write", "delete"}; !slices.Equal(access.TokenCaps, want) {
		t.Errorf("TokenCaps = %v, want %v", access.TokenCaps, want)
	}
}

func TestTokenFileStrictDecoding(t *testing.T) {
	t.Parallel()
	s, _, dir := newTokenStore(t)
	secret := addToken(t, s, access.Token{Name: "claude", Caps: []string{"read"}})
	good := readTokenFile(t, dir)
	edit := func(fn func(f, rec map[string]any)) []byte {
		var g map[string]any
		_ = json.Unmarshal(good, &g)
		fn(g, g["tokens"].([]any)[0].(map[string]any))
		b, _ := json.Marshal(g)
		return b
	}
	for what, b := range map[string][]byte{
		"an unknown top-level field": edit(func(f, _ map[string]any) { f["extra"] = true }),
		"an unknown token field":     edit(func(_, rec map[string]any) { rec["sealed"] = "AAAA" }),
		"version 2":                  edit(func(f, _ map[string]any) { f["version"] = 2 }),
		"no version":                 edit(func(f, _ map[string]any) { delete(f, "version") }),
		"a login capability":         edit(func(_, rec map[string]any) { rec["caps"] = []string{"read", "login"} }),
		"an unknown capability":      edit(func(_, rec map[string]any) { rec["caps"] = []string{"admin"} }),
		"no capability":              edit(func(_, rec map[string]any) { rec["caps"] = []string{} }),
		"a short hash":               edit(func(_, rec map[string]any) { rec["hash"] = "AAAA" }),
		"a hash that is not base64":  edit(func(_, rec map[string]any) { rec["hash"] = "!!" }),
		"an invalid name":            edit(func(_, rec map[string]any) { rec["name"] = "" }),
		"trailing data":              append(append([]byte(nil), good...), []byte(`{}`)...),
		"not JSON":                   []byte("version = 1\n"),
	} {
		writeTokenFile(t, dir, b)
		_, ok, err := s.Verify(secret)
		if ok || err == nil {
			t.Errorf("%s: Verify = %v, %v; want refused with an error", what, ok, err)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error holds the secret", what)
		}
		if _, err := s.List(); err == nil {
			t.Errorf("%s: List succeeded", what)
		}
		if _, err := s.Add(context.Background(), access.Token{Name: "new", Caps: []string{"read"}}); err == nil {
			t.Errorf("%s: Add wrote over a file it cannot read", what)
		}
		if got := readTokenFile(t, dir); !bytes.Equal(got, b) {
			t.Errorf("%s: the unreadable file was rewritten", what)
		}
	}
	writeTokenFile(t, dir, good)
	if _, ok, err := s.Verify(secret); !ok || err != nil {
		t.Errorf("the file restored: Verify = %v, %v", ok, err)
	}
}

func TestTokenFileMode0600(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	s, _, dir := newTokenStore(t)
	addToken(t, s, access.Token{Name: "claude", Caps: []string{"read"}})
	fi, err := os.Stat(filepath.Join(dir, "mcp-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if di, err := os.Stat(dir); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, %v; want 0700", di.Mode().Perm(), err)
	}
}

func TestTokenListEmptyWithoutAFile(t *testing.T) {
	t.Parallel()
	s, _, dir := newTokenStore(t)
	list, err := s.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if _, ok, err := s.Verify(access.TokenPrefix + strings.Repeat("A", 43)); ok || err != nil {
		t.Errorf("Verify with no file = %v, %v", ok, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading created the state directory: %v", err)
	}
}
