package protontest

import (
	"crypto/rand"
	"sync"
	"testing"

	"github.com/ProtonMail/go-proton-api/server"
	"github.com/ProtonMail/go-proton-api/server/backend"
)

// An account costs a real SRP verifier and real keys (bcrypt and 2048-bit
// modular exponentiations: about 1.5 s under -race). So a fake borrows its
// backend from a pool for the test binary and gives it back when its test
// ends, accounts included: the next fake reuses them instead of creating its
// own. A backend has one holder at a time (it runs every SRP step under one
// lock, so sharing it would make parallel logins queue). What a test changes
// lives in its Fake (calendars, challenges, failures, issued tokens) and the
// sessions it opened die with it; a backend whose account a test changed
// (AddAddress) is closed instead of given back.
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
	accounts map[string]account // username → account
	dirty    bool               // an account was changed: never reused
}

type account struct{ id, password string }

// fastKeys makes the backend reuse one precomputed key instead of generating
// one per user and address (seconds per test). Insecure; test use only.
var fastKeys sync.Once

// borrow takes an idle backend, or starts one, and gives it back when t ends.
func borrow(t testing.TB) *pooled {
	fastKeys.Do(func() { backend.GenerateKey = backend.FastGenerateKey })
	pool.mu.Lock()
	var p *pooled
	if n := len(pool.idle); n > 0 {
		p, pool.idle = pool.idle[n-1], pool.idle[:n-1]
	}
	pool.mu.Unlock()
	if p == nil {
		p = &pooled{srv: server.New(server.WithTLS(false)), accounts: map[string]account{}}
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

// user returns the ID of username's account, created on first use. Asked
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
	}
	id, _, err := p.srv.CreateUser(username, []byte(password))
	if err != nil {
		return "", err
	}
	p.accounts[username] = account{id, password}
	return id, nil
}

// spoil marks the backend as changed by its holder: it is closed, not reused.
func (p *pooled) spoil() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
}
