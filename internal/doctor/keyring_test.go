package doctor_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// memKeyring is an in-memory keyring: absent stands for no Secret Service,
// locked for a locked default collection.
type memKeyring struct {
	mu     sync.Mutex
	absent bool
	locked bool
	items  map[string][]byte
}

func attrKey(attrs map[string]string) string {
	b, _ := json.Marshal(attrs)
	return string(b)
}

func (k *memKeyring) gate() error {
	switch {
	case k.absent:
		return seal.ErrNoService
	case k.locked:
		return seal.ErrLocked
	}
	return nil
}

func (k *memKeyring) Get(_ context.Context, attrs map[string]string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.gate(); err != nil {
		return nil, err
	}
	v, ok := k.items[attrKey(attrs)]
	if !ok {
		return nil, seal.ErrNoKey
	}
	return slices.Clone(v), nil
}

func (k *memKeyring) Set(_ context.Context, _ string, attrs map[string]string, v []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.gate(); err != nil {
		return err
	}
	if k.items == nil {
		k.items = map[string][]byte{}
	}
	k.items[attrKey(attrs)] = slices.Clone(v)
	return nil
}

func (k *memKeyring) Locked(context.Context) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.absent {
		return false, seal.ErrNoService
	}
	return k.locked, nil
}

func (k *memKeyring) setLocked(v bool) {
	k.mu.Lock()
	k.locked = v
	k.mu.Unlock()
}

// keyringState is a state root whose account (under accounts/a1) holds a
// secrets.sealed sealed by the keyring, with the sealer that opens it.
func keyringState(t *testing.T, kr *memKeyring) (root, acct string, sealer seal.Sealer) {
	t.Helper()
	root = t.TempDir()
	acct = filepath.Join(root, "accounts", "a1")
	if err := os.MkdirAll(acct, 0o700); err != nil {
		t.Fatal(err)
	}
	sealer, err := service.NewSealer(root, "secret-service", filepath.Join(root, "no.key"), kr)
	if err != nil {
		t.Fatal(err)
	}
	err = store.NewSecrets(acct, sealer).Update(t.Context(), func(a map[string]store.Account) error {
		a["u"] = store.Account{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveState(acct, store.State{Phase: store.PhaseReady, Address: "a@example.org"}); err != nil {
		t.Fatal(err)
	}
	return root, acct, sealer
}

// A relative state root names the same keyring item as its absolute path:
// doctor builds its sealer through service.NewSealer, as the daemon does.
func TestDoctorSealerForARelativeStateDirUsesTheAbsoluteKeyringItem(t *testing.T) {
	kr := &memKeyring{}
	t.Chdir(t.TempDir())
	if err := os.Mkdir("state", 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := service.NewSealer("state", "secret-service", "/k", kr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seal([]byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("state")
	want := attrKey(map[string]string{"application": "pcb", "state": abs})
	if _, ok := kr.items[want]; len(kr.items) != 1 || !ok {
		t.Errorf("keyring items = %v, want one for %s", kr.items, abs)
	}
}

func TestSealerCheckOnAFreshStateDoesNotClaimTheKeyFile(t *testing.T) {
	ctx := t.Context()
	for _, backend := range []string{"auto", "secret-service"} {
		got := doctor.SealerCheck{Sealer: "keyfile", Fresh: true, Backend: backend}.Run(ctx)
		want := "not chosen yet: the first login picks it ([secrets] backend = " + backend + ")"
		if got.Status != doctor.StatusOK || got.Message != want {
			t.Errorf("%s: %+v, want ok %q", backend, got, want)
		}
	}
	if got := (doctor.SealerCheck{Sealer: "keyfile", Fresh: true, Backend: "keyfile"}).Run(ctx); got.Message != "keyfile" {
		t.Errorf("backend keyfile: %+v", got)
	}
	if got := (doctor.SealerCheck{Sealer: "secret-service"}).Run(ctx); got.Message != "secret-service" {
		t.Errorf("recorded: %+v", got)
	}
}

func TestSecretsCheckWithTheKeyringNeedsNoKeyFile(t *testing.T) {
	kr := &memKeyring{}
	_, acct, sealer := keyringState(t, kr)
	key := filepath.Join(t.TempDir(), "absent.key")
	got := doctor.SecretsCheck{StateDir: acct, KeyFile: key, Sealer: sealer}.Run(t.Context())
	if got.Status != doctor.StatusOK || strings.Contains(got.Message, "key file") {
		t.Fatalf("got %+v; want ok and no word of a key file", got)
	}
	if got.Message == "" {
		t.Errorf("no message: %+v", got)
	}
	if _, err := os.Stat(key); err == nil {
		t.Fatal("the check created a key file")
	}
	// Unattended names the keyring, not a key file.
	if err := store.SaveState(acct, store.State{Phase: store.PhaseReady, Unattended: true}); err != nil {
		t.Fatal(err)
	}
	got = doctor.SecretsCheck{StateDir: acct, KeyFile: key, Sealer: sealer}.Run(t.Context())
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, doctor.UnattendedKeyringWarning) || strings.Contains(got.Message, key) {
		t.Errorf("unattended: %+v", got)
	}
}

func TestSecretsCheckWithAKeyringMissingItsKeyErrors(t *testing.T) {
	kr := &memKeyring{}
	_, acct, _ := keyringState(t, kr)
	empty, _ := service.NewSealer(t.TempDir(), "secret-service", "/k", kr) // another state root: another item
	got := doctor.SecretsCheck{StateDir: acct, Sealer: empty}.Run(t.Context())
	if got.Status != doctor.StatusError || !strings.Contains(got.Message, "desktop keyring") || got.Fix != "pcb logout" {
		t.Fatalf("got %+v", got)
	}
}

func TestSecretsCheckWithALockedKeyringLeavesItToTheKeyringCheck(t *testing.T) {
	kr := &memKeyring{}
	_, acct, sealer := keyringState(t, kr)
	kr.setLocked(true)
	got := doctor.SecretsCheck{StateDir: acct, Sealer: sealer}.Run(t.Context())
	if got.Status == doctor.StatusError {
		t.Fatalf("got %+v; a locked keyring is the keyring check's warning", got)
	}
}

func TestSealedClientsCheckWithTheKeyring(t *testing.T) {
	ctx := t.Context()
	kr := &memKeyring{}
	root, acct, sealer := keyringState(t, kr)
	st := access.NewStore(acct, nil)
	st.Sealer = sealer
	if _, _, err := st.Add(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	check := doctor.SealedClientsCheck{StateDir: acct, KeyFile: filepath.Join(root, "no.key"), Sealer: sealer}
	if got := check.RunAll(ctx); len(got) != 0 {
		t.Fatalf("the keyring opens it: %+v, want no result", got)
	}
	kr.setLocked(true)
	if got := check.RunAll(ctx); len(got) != 0 {
		t.Errorf("locked keyring: %+v, want no result (the keyring check warns)", got)
	}
	kr.setLocked(false)
	// A new key in the keyring: the sealed copy no longer opens.
	other, _ := service.NewSealer(root, "secret-service", "/k", kr)
	if err := kr.Set(ctx, "x", map[string]string{"application": "pcb", "state": root}, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	want := []doctor.Result{{Name: "caldav client phone", Status: doctor.StatusWarn, Message: "password cannot be shown", Fix: "pcb client rotate phone"}}
	if got := (doctor.SealedClientsCheck{StateDir: acct, Sealer: other}).RunAll(ctx); !slices.Equal(got, want) {
		t.Errorf("another key: %+v, want %+v", got, want)
	}
}

func TestKeyringCheck(t *testing.T) {
	kr := &memKeyring{}
	c := doctor.KeyringCheck{Keyring: kr}
	if got := c.Run(t.Context()); got.Name != "keyring" || got.Status != doctor.StatusOK {
		t.Errorf("unlocked: %+v", got)
	}
	kr.setLocked(true)
	got := c.Run(t.Context())
	if got.Status != doctor.StatusWarn || got.Fix != service.KeyringFix || !strings.Contains(got.Message, "locked") {
		t.Errorf("locked: %+v; want a warning with the alert's fix", got)
	}
	kr.setLocked(false)
	kr.absent = true
	if got := c.Run(t.Context()); got.Status != doctor.StatusError || got.Fix == "" {
		t.Errorf("no service: %+v; want an error with a fix", got)
	}
}

func TestAccountsReportsTheRecordedSealer(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "layout.json"), `{"version": 2, "sealer": "secret-service"}`)
	l, err := doctor.Accounts(root)
	if err != nil || l.Sealer != "secret-service" {
		t.Fatalf("got %q, %v; want secret-service", l.Sealer, err)
	}
	writeFile(t, filepath.Join(root, "layout.json"), `{"version": 2}`)
	if l, err = doctor.Accounts(root); err != nil || l.Sealer != "keyfile" {
		t.Fatalf("no sealer named: got %q, %v; want keyfile", l.Sealer, err)
	}
	if l.Fresh {
		t.Error("a recorded layout is not fresh")
	}
	if l, err = doctor.Accounts(t.TempDir()); err != nil || l.Sealer != "keyfile" || !l.Fresh {
		t.Fatalf("no layout: got %q fresh=%v, %v; want keyfile, fresh", l.Sealer, l.Fresh, err)
	}
}

func TestDesktopCheckWarnsWhenBothAreInstalled(t *testing.T) {
	apps := t.TempDir()
	writeFile(t, filepath.Join(apps, "pcb-ui.desktop"), "[Desktop Entry]\n")
	c := doctor.DesktopCheck{AppDirs: []string{filepath.Join(t.TempDir(), "none"), apps}, ServiceInstalled: func() bool { return true }}
	rs := c.RunAll(t.Context())
	want := doctor.Result{Name: "pcb-ui", Status: doctor.StatusWarn,
		Message: "pcb-ui and the pcb service are both installed: only one can serve this state directory",
		Fix:     "uninstall one (pcb uninstall, or remove pcb-ui.desktop)"}
	if len(rs) != 1 || rs[0] != want {
		t.Fatalf("got %+v, want [%+v]", rs, want)
	}
}

func TestDesktopCheckIsQuietWithOnlyOne(t *testing.T) {
	apps := t.TempDir()
	writeFile(t, filepath.Join(apps, "pcb-ui.desktop"), "[Desktop Entry]\n")
	cases := map[string]doctor.DesktopCheck{
		"ui only":      {AppDirs: []string{apps}, ServiceInstalled: func() bool { return false }},
		"service only": {AppDirs: []string{t.TempDir()}, ServiceInstalled: func() bool { return true }},
		"neither":      {AppDirs: []string{t.TempDir()}, ServiceInstalled: func() bool { return false }},
	}
	for name, c := range cases {
		if rs := c.RunAll(t.Context()); len(rs) != 0 {
			t.Errorf("%s: %+v, want no result", name, rs)
		}
	}
}
