package protontest

import (
	"crypto/rand"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-proton-api/server"
	"github.com/ProtonMail/go-proton-api/server/backend"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// An account costs a real SRP verifier and real keys (bcrypt and 2048-bit
// modular exponentiations: about 1.5 s under -race). So a fake borrows its
// backend from a pool for the test binary and gives it back when its test
// ends, accounts included: the next fake reuses them instead of creating its
// own. A backend has one holder at a time (it runs every SRP step under one
// lock, so sharing it would make parallel logins queue). What a test changes
// lives in its Fake (calendars, challenges, failures, issued tokens) and the
// sessions it opened die with it; a backend whose account a test changed
// (AddAddress) is closed instead of given back. Accounts other tests created
// stay on a reused backend: a test must never assume an unknown username is
// absent. Pooled backends are not closed at exit; the process ends anyway.
var pool struct {
	mu   sync.Mutex
	idle []*pooled
}

// defaultPassword is the default account's, the same for every fake so its
// account can be reused.
var defaultPassword = sync.OnceValue(func() string { return "fake-" + rand.Text() })

// pooled is a backend and the accounts created on it.
type pooled struct {
	srv      *server.Server
	mu       sync.Mutex
	accounts map[string]account      // username → account
	sessions map[string][]api.Tokens // username → live sessions no test holds
	dirty    bool                    // an account was changed: never reused
}

type account struct{ id, password string }

// fastKeys makes the backend reuse precomputed keys instead of generating
// one per user and address (seconds per test). Insecure; test use only.
var fastKeys sync.Once

// generateKey replaces the backend's key generator: an account's user key
// and first address take FastGenerateKey's precomputed key (signer), every
// other address otherAddressKey, so a part signed by the wrong address does
// not verify (research A.4). The backend names the key after the username
// and the address.
func generateKey(name, email string, pass []byte, keyType string, bits int) (string, error) {
	if !strings.Contains(email, "@") || strings.HasPrefix(email, name+"@") {
		return backend.FastGenerateKey(name, email, pass, keyType, bits)
	}
	key, err := otherAddressKey()
	if err != nil {
		return "", err
	}
	locked, err := key.Lock(pass)
	if err != nil {
		return "", err
	}
	return locked.Armor()
}

// secondAddress is the email of the second address every account has.
func secondAddress(username, domain string) string { return "second-" + username + "@" + domain }

// borrow takes an idle backend, or starts one, and gives it back when t ends.
func borrow(t testing.TB) *pooled {
	fastKeys.Do(func() { backend.GenerateKey = generateKey })
	pool.mu.Lock()
	var p *pooled
	if n := len(pool.idle); n > 0 {
		p, pool.idle = pool.idle[n-1], pool.idle[:n-1]
	}
	pool.mu.Unlock()
	if p == nil {
		p = &pooled{srv: server.New(server.WithTLS(false)), accounts: map[string]account{}, sessions: map[string][]api.Tokens{}}
	}
	t.Cleanup(func() {
		p.mu.Lock()
		dirty := p.dirty
		p.mu.Unlock()
		if dirty {
			p.srv.Close()
			return
		}
		pool.mu.Lock()
		pool.idle = append(pool.idle, p)
		pool.mu.Unlock()
	})
	return p
}

// user returns the ID of username's account, created on first use with two
// addresses: username@domain, then secondAddress with its own key. Asked
// with another password, the account is replaced.
func (p *pooled) user(username, password string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.accounts[username]; ok {
		if a.password == password {
			return a.id, nil
		}
		if err := p.srv.RemoveUser(a.id); err != nil {
			return "", err
		}
		delete(p.accounts, username)
		delete(p.sessions, username) // they died with the account
	}
	id, _, err := p.srv.CreateUser(username, []byte(password))
	if err != nil {
		return "", err
	}
	if _, err := p.srv.CreateAddress(id, secondAddress(username, p.srv.GetDomain()), []byte(password), true); err != nil {
		return "", err
	}
	p.accounts[username] = account{id, password}
	return id, nil
}

// takeSession takes an idle live session of username's account.
func (p *pooled) takeSession(username string) (api.Tokens, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.sessions[username]
	if len(q) == 0 {
		return api.Tokens{}, false
	}
	p.sessions[username] = q[:len(q)-1]
	return q[len(q)-1], true
}

// putSession gives back a live session of username's account. A session
// lives an hour on the backend, longer than a test binary may run.
func (p *pooled) putSession(username string, tok api.Tokens) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[username] = append(p.sessions[username], tok)
}

// dropSessions forgets the idle sessions of username's account.
func (p *pooled) dropSessions(username string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, username)
}

// spoil marks the backend as changed by its holder: it is closed, not reused.
func (p *pooled) spoil() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
}
