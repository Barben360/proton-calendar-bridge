package keys_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

const saltsPath = "/core/v4/keys/salts"

func newClient(f *protontest.Fake, tok api.Tokens) *api.Client {
	return api.New(tok, api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 2 * time.Minute}, // SRP checks run server-side, slow under -race and load
	})
}

// login opens a session on a fresh fake and returns the fake, its client and
// the manager.
func login(t *testing.T, o protontest.Options) (*protontest.Fake, *api.Client, *gpa.Manager) {
	t.Helper()
	f := protontest.New(t, o)
	m := auth.NewManager(f.URL, "Other")
	t.Cleanup(m.Close)
	return f, newClient(f, f.Login(t, f.Username)), m
}

// counting is an Elevator that counts calls around a real auth.Scope.
type counting struct {
	inner         keys.Elevator
	unlocks, lock atomic.Int32
	lockErr       error
}

func (c *counting) Unlock(ctx context.Context) error { c.unlocks.Add(1); return c.inner.Unlock(ctx) }
func (c *counting) Lock(ctx context.Context) error {
	c.lock.Add(1)
	if c.lockErr != nil {
		return c.lockErr
	}
	return c.inner.Lock(ctx)
}

func elevator(f *protontest.Fake, c *api.Client, m *gpa.Manager) *counting {
	return &counting{inner: auth.Scope{M: m, C: c, Username: f.Username, Password: []byte(f.Password)}}
}

func fetchAll(t *testing.T, c *api.Client, e keys.Elevator) (gpa.User, []gpa.Address, gpa.Salts) {
	t.Helper()
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	salts, err := keys.FetchSalts(t.Context(), c, e)
	if err != nil {
		t.Fatalf("FetchSalts: %v", err)
	}
	return user, addrs, salts
}

func TestDeriveKeyPassUnlocksAddressKeys(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	if len(addrs) == 0 {
		t.Fatal("the fake user has no address")
	}
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password))
	if err != nil || len(pass) == 0 {
		t.Fatalf("DeriveKeyPass = %d bytes, %v", len(pass), err)
	}
	u, err := keys.Unlock(user, addrs, pass)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(u.Close)
	kr := u.AddrKRs[addrs[0].ID]
	if kr == nil {
		t.Fatalf("AddrKRs lacks address %s", addrs[0].ID)
	}
	// Encrypt to the address's public key, decrypt with the unlocked keyring.
	priv, err := crypto.NewKey(addrs[0].Keys[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.ToPublic()
	if err != nil {
		t.Fatal(err)
	}
	pubKR, err := crypto.NewKeyRing(pub)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := pubKR.Encrypt(crypto.NewPlainMessageFromString("hello"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kr.Decrypt(msg, nil, crypto.GetUnixTime())
	if err != nil || got.GetString() != "hello" {
		t.Fatalf("Decrypt = %q, %v", got.GetString(), err)
	}
}

func TestDeriveKeyPassWrongPassword(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte("wrong-"+f.Password))
	if !errors.Is(err, keys.ErrKeysLocked) {
		t.Fatalf("err = %v, want ErrKeysLocked", err)
	}
	if pass != nil {
		t.Fatal("a failed derivation returned a passphrase")
	}
	if strings.Contains(err.Error(), f.Password) {
		t.Fatal("the error leaks the password")
	}
}

func TestUnlockWrongPassphrase(t *testing.T) {
	t.Parallel()
	_, c, _ := login(t, protontest.Options{})
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Unlock(user, addrs, []byte("not the passphrase")); !errors.Is(err, keys.ErrKeysLocked) {
		t.Fatalf("Unlock = %v, want ErrKeysLocked", err)
	}
}

func TestFetchSaltsElevatesOnlyOn9101(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		locked        bool
		unlock, lockN int32
	}{
		{"locked salts", true, 1, 1},
		{"default", false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, c, m := login(t, protontest.Options{LockedSalts: tc.locked})
			e := elevator(f, c, m)
			salts, err := keys.FetchSalts(t.Context(), c, e)
			if err != nil || len(salts) == 0 {
				t.Fatalf("FetchSalts = %d salts, %v", len(salts), err)
			}
			if e.unlocks.Load() != tc.unlock || e.lock.Load() != tc.lockN {
				t.Fatalf("Unlock/Lock calls = %d/%d, want %d/%d", e.unlocks.Load(), e.lock.Load(), tc.unlock, tc.lockN)
			}
		})
	}
}

func TestFetchSaltsOther403DoesNotElevate(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{LockedSalts: true})
	e := elevator(f, c, m)
	f.FailNext("GET", saltsPath, http.StatusForbidden, 2011, 0)
	_, err := keys.FetchSalts(t.Context(), c, e)
	if err == nil || !api.IsCode(err, 2011) {
		t.Fatalf("err = %v, want the 2011 error", err)
	}
	if e.unlocks.Load() != 0 || e.lock.Load() != 0 {
		t.Fatalf("Unlock/Lock calls = %d/%d, want 0/0", e.unlocks.Load(), e.lock.Load())
	}
}

func TestFetchSaltsUnlockFailureIsReturned(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{LockedSalts: true, BadUnlockProof: true})
	e := elevator(f, c, m)
	if _, err := keys.FetchSalts(t.Context(), c, e); !errors.Is(err, auth.ErrServerProof) {
		t.Fatalf("err = %v, want ErrServerProof", err)
	}
}

func TestFetchSaltsLockFailureReturnedOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{LockedSalts: true})
	e := elevator(f, c, m)
	e.lockErr = errors.New("lock boom")
	salts, err := keys.FetchSalts(t.Context(), c, e)
	if err == nil || !strings.Contains(err.Error(), "lock boom") {
		t.Fatalf("err = %v, want the lock failure", err)
	}
	if len(salts) == 0 {
		t.Fatal("the fetched salts were dropped with the lock failure")
	}

	// A fetch that failed returns its own error, not the lock's.
	f2, c2, m2 := login(t, protontest.Options{LockedSalts: true})
	e2 := elevator(f2, c2, m2)
	e2.lockErr = errors.New("lock boom")
	f2.FailNext("GET", saltsPath, http.StatusForbidden, api.CodeInsufficientScope, 0)
	f2.FailNext("GET", saltsPath, http.StatusForbidden, 2011, 0)
	_, err = keys.FetchSalts(t.Context(), c2, e2)
	if err == nil || strings.Contains(err.Error(), "lock boom") || !api.IsCode(err, 2011) {
		t.Fatalf("err = %v, want only the fetch failure (2011)", err)
	}
	if e2.lock.Load() != 1 {
		t.Fatalf("Lock calls after a failed fetch = %d, want 1", e2.lock.Load())
	}
}

func TestUnlockWithStoredKeyPassAfterRefresh(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{LockedSalts: true})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password))
	if err != nil {
		t.Fatalf("DeriveKeyPass: %v", err)
	}

	f.ExpireAccessTokens()
	tok, err := auth.Refresh(t.Context(), m, c.Tokens())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	c2 := newClient(f, tok)
	user2, addrs2, err := keys.Fetch(t.Context(), c2)
	if err != nil {
		t.Fatalf("Fetch on the refreshed session: %v", err)
	}
	u, err := keys.Unlock(user2, addrs2, pass)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(u.Close)
	if u.AddrKRs[addrs2[0].ID] == nil {
		t.Fatal("no address keyring after refresh")
	}
}

func TestFetchSortsAddressesByOrder(t *testing.T) {
	t.Parallel()
	_, c, _ := login(t, protontest.Options{})
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil || user.ID == "" {
		t.Fatalf("Fetch = %q, %v", user.ID, err)
	}
	for i := 1; i < len(addrs); i++ {
		if addrs[i-1].Order > addrs[i].Order {
			t.Fatalf("addresses not sorted by Order: %d before %d", addrs[i-1].Order, addrs[i].Order)
		}
	}
}

func TestUnlockedCloseDropsKeyrings(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password))
	if err != nil {
		t.Fatal(err)
	}
	u, err := keys.Unlock(user, addrs, pass)
	if err != nil {
		t.Fatal(err)
	}
	kr := u.AddrKRs[addrs[0].ID]
	if kr.CountDecryptionEntities() == 0 {
		t.Fatal("the keyring is not unlocked before Close")
	}
	priv, err := crypto.NewKey(addrs[0].Keys[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.ToPublic()
	if err != nil {
		t.Fatal(err)
	}
	pubKR, err := crypto.NewKeyRing(pub)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := pubKR.Encrypt(crypto.NewPlainMessageFromString("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := kr.Decrypt(msg, nil, crypto.GetUnixTime()); err != nil || got.GetString() != "secret" {
		t.Fatalf("Decrypt before Close = %q, %v", got.GetString(), err)
	}
	u.Close()
	u.Close() // idempotent
	if len(u.AddrKRs) != 0 {
		t.Fatal("Close left keyrings in AddrKRs")
	}
	if n := kr.CountDecryptionEntities(); n != 0 {
		t.Fatalf("keyring holds %d decryption entities after Close", n)
	}
	if _, err := kr.Decrypt(msg, nil, crypto.GetUnixTime()); err == nil {
		t.Fatal("keyring still decrypts after Close")
	}
	var nilU *keys.Unlocked
	nilU.Close() // nil-safe
}

func TestDeriveKeyPassUsesPrimaryKeySalt(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	real := user.Keys[0]
	real.Primary = true
	decoy := real
	decoy.ID = "decoy-key"
	decoy.Primary = false
	user.Keys = gpa.Keys{decoy, real}
	// Only the primary key has a salt: picking Keys[0] finds none.
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password))
	if err != nil || len(pass) == 0 {
		t.Fatalf("DeriveKeyPass = %d bytes, %v", len(pass), err)
	}
}

func TestDeriveKeyPassFirstKeyWhenNonePrimary(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	user.Keys[0].Primary = false
	if _, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password)); err != nil {
		t.Fatalf("DeriveKeyPass without a primary key: %v", err)
	}
}

func TestDeriveKeyPassUserWithoutKey(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	user.Keys = nil
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password))
	if err == nil || pass != nil {
		t.Fatalf("DeriveKeyPass = %v, %v; want an error and no passphrase", pass, err)
	}
}

// Unlock serialises each address key once, so concurrent copies of one key
// (a Signer built per calendar) write no cache. Would fail under -race if
// Unlock stopped warming the keys (ruling M4-R4).
func TestUnlockedAddressKeysCopyConcurrentlyWithoutRace(t *testing.T) {
	t.Parallel()
	f, c, m := login(t, protontest.Options{})
	user, addrs, salts := fetchAll(t, c, elevator(f, c, m))
	pass, err := keys.DeriveKeyPass(user, addrs, salts, []byte(f.Password))
	if err != nil {
		t.Fatal(err)
	}
	u, err := keys.Unlock(user, addrs, pass)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(u.Close)
	var wg sync.WaitGroup
	for range 8 {
		for _, kr := range u.AddrKRs {
			for _, k := range kr.GetKeys() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := k.Copy(); err != nil {
						t.Error(err)
					}
				}()
			}
		}
	}
	wg.Wait()
}
