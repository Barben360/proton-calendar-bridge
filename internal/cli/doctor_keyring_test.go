package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// memKeyring is an in-memory desktop keyring (absent: no Secret Service,
// locked: a locked default collection).
type memKeyring struct {
	mu     sync.Mutex
	absent bool
	locked bool
	items  map[string][]byte
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
	b, _ := json.Marshal(attrs)
	v, ok := k.items[string(b)]
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
	b, _ := json.Marshal(attrs)
	k.items[string(b)] = slices.Clone(v)
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

type doctorResult struct{ Name, Status, Message, Fix string }

// doctorResults runs pcb doctor --json (exit 0) and returns its results by name.
func doctorResults(t *testing.T, r *rig) map[string]doctorResult {
	t.Helper()
	r.want(exitcode.OK, "doctor", "--json")
	var data struct {
		Results []doctorResult `json:"results"`
	}
	mustUnmarshal(t, r.envelope().Data, &data)
	out := map[string]doctorResult{}
	for _, res := range data.Results {
		out[res.Name] = res
	}
	return out
}

// keyringRig is a rig logged in on a state the desktop keyring seals.
func keyringRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, &protontest.Options{})
	r.keyring = &memKeyring{}
	toml := "[proton]\nbase_url = \"" + r.f.URL + "\"\n[secrets]\nbackend = \"secret-service\"\n"
	if err := os.WriteFile(filepath.Join(r.dirs.Config, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.OK, "login", "--non-interactive")
	return r
}

func TestDoctorChecksSecretsWithTheKeyring(t *testing.T) {
	t.Parallel()
	r := keyringRig(t)
	res := doctorResults(t, r)
	if got := res["sealer"]; got.Status != "ok" || got.Message != "secret-service" {
		t.Errorf("sealer = %+v", got)
	}
	if got := res["keyring"]; got.Status != "ok" {
		t.Errorf("keyring = %+v", got)
	}
	if _, ok := res["key file"]; ok {
		t.Errorf("a key file check ran for a keyring state: %+v", res["key file"])
	}
	acct := r.f.Username + ": "
	if got := res[acct+"secrets"]; got.Status != "ok" || !strings.Contains(got.Message, "desktop keyring") {
		t.Errorf("secrets = %+v", got)
	}
	for name, got := range res {
		if got.Status == "error" {
			t.Errorf("%s fails: %+v", name, got)
		}
	}
	if _, err := os.Stat(filepath.Join(r.dirs.Config, "secrets.key")); err == nil {
		t.Error("a key file exists beside a keyring state")
	}
}

func TestDoctorWarnsLockedKeyring(t *testing.T) {
	t.Parallel()
	r := keyringRig(t)
	r.keyring.setLocked(true)
	res := doctorResults(t, r) // a warning: no exit 10
	got := res["keyring"]
	if got.Status != "warn" || got.Fix != service.KeyringFix {
		t.Errorf("keyring = %+v; want a warning with the alert's fix", got)
	}
	for name, g := range res {
		if g.Status == "error" {
			t.Errorf("%s fails while the keyring is only locked: %+v", name, g)
		}
	}
}

func TestDoctorReportsAKeyFileStateAsKeyFile(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive")
	res := doctorResults(t, r)
	if got := res["sealer"]; got.Message != "keyfile" || got.Status != "ok" {
		t.Errorf("sealer = %+v", got)
	}
	if _, ok := res["key file"]; !ok {
		t.Error("no key file check for a key-file state")
	}
	if _, ok := res["keyring"]; ok {
		t.Error("a keyring check ran for a key-file state")
	}
}

// desktopRig is a rig with pcb-ui.desktop under XDG_DATA_HOME when ui is
// set and a pcb user unit installed when unit is.
func desktopRig(t *testing.T, ui, unit bool) *rig {
	t.Helper()
	r := stubRig(t, &stubAPI{})
	r.hostRoot = t.TempDir()
	data := t.TempDir()
	r.env["XDG_DATA_HOME"] = data
	write := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ui {
		write(filepath.Join(data, "applications", "pcb-ui.desktop"))
	}
	if unit {
		write(filepath.Join(r.hostRoot, r.env["XDG_CONFIG_HOME"], "systemd", "user", "pcb.service"))
	}
	return r
}

func TestDoctorWarnsBothInstalled(t *testing.T) {
	t.Parallel()
	r := desktopRig(t, true, true)
	res := doctorResults(t, r)
	got := res["pcb-ui"]
	if got.Status != "warn" || got.Message != "pcb-ui and the pcb service are both installed: only one can serve this state directory" ||
		got.Fix != "uninstall one (pcb uninstall, or remove pcb-ui.desktop)" {
		t.Errorf("pcb-ui = %+v", got)
	}
}

func TestDoctorQuietWithOnlyOne(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]*rig{"ui only": desktopRig(t, true, false), "unit only": desktopRig(t, false, true), "neither": desktopRig(t, false, false)} {
		if _, ok := doctorResults(t, c)["pcb-ui"]; ok {
			t.Errorf("%s: pcb doctor says something about pcb-ui", name)
		}
	}
}
