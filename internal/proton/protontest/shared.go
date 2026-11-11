package protontest

import (
	"crypto/rand"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ProtonMail/go-proton-api/server"
	"github.com/ProtonMail/go-proton-api/server/backend"
)

// shared holds the backends of every fake without Options.Private. An account
// costs a real SRP verifier and real keys (bcrypt and 2048-bit modular
// exponentiations: seconds under -race), so each username is created once per
// backend and test binary and reused by every fake on that backend. What a
// test changes lives in its own Fake (the sessions it handed out and revoked,
// calendars, challenges, failures); nothing a fake does changes a shared
// account.
//
// A backend runs every SRP step under one lock, so the fakes are spread over
// GOMAXPROCS backends: parallel tests do not queue behind each other's logins.
var shared struct {
	once     sync.Once
	password string // the default account's
	next     atomic.Uint64
	shards   []*shard
}

// shard is one shared backend and its accounts, started on first use.
type shard struct {
	once     sync.Once
	srv      *server.Server
	mu       sync.Mutex
	accounts map[string]*sharedAccount // username → account
}

type sharedAccount struct {
	once     sync.Once
	password string
	id       string
	err      error
}

// fastKeys makes the backend reuse one precomputed key instead of generating
// one per user and address (seconds per test). Insecure; test use only.
var fastKeys sync.Once

func useFastKeys() {
	fastKeys.Do(func() { backend.GenerateKey = backend.FastGenerateKey })
}

// nextShard returns the next shared backend, round robin. The backends live
// until the test binary exits.
func nextShard() *shard {
	useFastKeys()
	shared.once.Do(func() {
		shared.password = "fake-" + rand.Text()
		shared.shards = make([]*shard, max(1, runtime.GOMAXPROCS(0)/4))
		for i := range shared.shards {
			shared.shards[i] = &shard{}
		}
	})
	s := shared.shards[(shared.next.Add(1)-1)%uint64(len(shared.shards))]
	s.once.Do(func() {
		s.srv = server.New(server.WithTLS(false))
		s.accounts = map[string]*sharedAccount{}
	})
	return s
}

// user returns the ID of username's account, creating it on first use. A
// username is bound to the first password it was asked with.
func (s *shard) user(username, password string) (string, error) {
	s.mu.Lock()
	a := s.accounts[username]
	if a == nil {
		a = &sharedAccount{password: password}
		s.accounts[username] = a
	}
	s.mu.Unlock()
	if a.password != password {
		return "", fmt.Errorf("the shared backend already has user %q with another password; use Options.Private", username)
	}
	a.once.Do(func() { a.id, _, a.err = s.srv.CreateUser(username, []byte(password)) })
	return a.id, a.err
}
