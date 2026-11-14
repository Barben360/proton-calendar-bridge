//go:build windows

package doctor_test

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// windowsEnv is an environment whose directories lie under a test's own
// %APPDATA% and %LOCALAPPDATA%; it returns the state directory.
func windowsEnv(t *testing.T) (func(string) string, string) {
	root := t.TempDir()
	env := map[string]string{"APPDATA": filepath.Join(root, "Roaming"), "LOCALAPPDATA": filepath.Join(root, "Local")}
	return func(k string) string { return env[k] }, filepath.Join(env["LOCALAPPDATA"], "pcb")
}

func TestChecksOnWindows(t *testing.T) {
	env, state := windowsEnv(t)
	res := doctor.Run(t.Context(), doctor.Checks(env, install.Host{}, nil, nil), nil)
	byName := map[string]doctor.Result{}
	for _, r := range res {
		byName[r.Name] = r
	}
	for _, name := range []string{"platform", "elevated", "time zone", "start at login", "dpapi key"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("no %q result: %+v", name, res)
		}
	}
	for _, name := range []string{"keyring", "pcb-ui", "service", "key file"} {
		if r, ok := byName[name]; ok {
			t.Errorf("a %q check ran on Windows: %+v", name, r)
		}
	}
	if got := byName["platform"]; runtime.GOARCH == "amd64" && (got.Status != doctor.StatusWarn || got.Message != "windows/amd64: runs, untested") {
		t.Errorf("platform: %+v", got)
	}
	// A fresh state under [secrets] backend = auto gets a DPAPI key.
	if got := byName["dpapi key"]; got.Status != doctor.StatusSkip || got.Message != "no key yet: the first login creates it" {
		t.Errorf("fresh state: %+v", got)
	}
	// A dpapi state checks its key in the state directory.
	if err := acl.MkdirAll(state); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLayout(state, service.SealerDPAPI); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(state, seal.DPAPIKeyName)
	if _, err := (&seal.DPAPI{Path: key}).Seal(nil, nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range doctor.Run(t.Context(), doctor.Checks(env, install.Host{}, nil, nil), nil) {
		if r.Name == "dpapi key" {
			found = true
			if r.Status != doctor.StatusOK || !strings.HasPrefix(r.Message, key+": ") {
				t.Errorf("dpapi state: %+v", r)
			}
		}
		if r.Name == "key file" {
			t.Errorf("a key file check ran for a dpapi state: %+v", r)
		}
	}
	if !found {
		t.Error("no dpapi key check for a dpapi state")
	}
	result := func(name string) doctor.Result {
		t.Helper()
		for _, r := range doctor.Run(t.Context(), doctor.Checks(env, install.Host{}, nil, nil), nil) {
			if r.Name == name {
				return r
			}
		}
		t.Fatalf("no %q result", name)
		return doctor.Result{}
	}
	// An open state directory is the state dir check's error only: one
	// cause, one ✗.
	acltest.Allow(t, state, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE) // the key keeps its own access list
	if got := result("state dir"); got.Status != doctor.StatusError {
		t.Errorf("open state dir: %+v", got)
	}
	if got := result("dpapi key"); got.Status != doctor.StatusOK {
		t.Errorf("dpapi key in an open state dir: %+v, want ok (the state dir check reports it)", got)
	}
	// An account with sealed secrets and no key: the key is lost.
	saveState(t, store.AccountDir(state, "aaaaaaaa"), "a@x")
	writeFile(t, filepath.Join(store.AccountDir(state, "aaaaaaaa"), "secrets.sealed"), "x")
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if got := result("dpapi key"); got.Status != doctor.StatusError || got.Message != key+" does not exist" {
		t.Errorf("key missing beside sealed secrets: %+v", got)
	}
}

// A fresh state under [secrets] backend = keyfile keeps the key file check.
func TestChecksOnWindowsWithTheKeyFileBackend(t *testing.T) {
	env, _ := windowsEnv(t)
	cfg := filepath.Join(env("APPDATA"), "pcb")
	if err := acl.MkdirAll(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("[secrets]\nbackend = \"keyfile\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range doctor.Run(t.Context(), doctor.Checks(env, install.Host{}, nil, nil), nil) {
		names[r.Name] = true
	}
	if !names["key file"] || names["dpapi key"] {
		t.Errorf("checks %v, want the key file's and no dpapi key", names)
	}
}

func TestDPAPIKeyCheck(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "Zoë O'Brien")
	if err := acl.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, seal.DPAPIKeyName)
	if _, err := (&seal.DPAPI{Path: path}).Seal(nil, nil); err != nil {
		t.Fatal(err)
	}
	check := doctor.DPAPIKeyCheck{Path: path}
	if got := check.Run(ctx); got.Status != doctor.StatusOK || got.Message != path+": protected for this Windows user, 32 bytes" {
		t.Fatalf("protected: %+v", got)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Damaged past its header: Windows refuses it.
	damaged := append([]byte(nil), blob...)
	damaged[len(damaged)-1] ^= 0xff
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := check.Run(ctx); got.Status != doctor.StatusError || got.Transient || !strings.HasPrefix(got.Message, path+" cannot be opened by this Windows user: it is damaged or not pcb's: ") ||
		got.Fix != "restore it from a backup; with no secrets.sealed, remove it and run pcb login" {
		t.Errorf("damaged: %+v", got)
	}
	// Random bytes: no DPAPI blob at all.
	junk := make([]byte, len(blob))
	_, _ = rand.Read(junk)
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := check.Run(ctx); got.Status != doctor.StatusError || !strings.Contains(got.Message, "cannot be opened by this Windows user") {
		t.Errorf("junk: %+v", got)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	acltest.Allow(t, path, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE)
	if got := check.Run(ctx); got.Status != doctor.StatusError || got.Fix != acl.Fix(path, "S-1-1-0") {
		t.Errorf("open key: %+v, want the fix %q", got, acl.Fix(path, "S-1-1-0"))
	}
}

// The directory a DPAPI key lies in must be private too: a new key takes
// its access list.
func TestDPAPIKeyCheckJudgesItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := acl.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, seal.DPAPIKeyName)
	if _, err := (&seal.DPAPI{Path: path}).Seal(nil, nil); err != nil {
		t.Fatal(err)
	}
	acltest.Allow(t, dir, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE)
	if got := (doctor.DPAPIKeyCheck{Path: path}).Run(t.Context()); got.Status != doctor.StatusError || !strings.Contains(got.Message, dir) ||
		got.Fix != acl.Fix(dir, acltest.Everyone) {
		t.Errorf("open directory: %+v, want the fix %q", got, acl.Fix(dir, acltest.Everyone))
	}
	// The state directory is the state dir check's: not reported twice.
	if got := (doctor.DPAPIKeyCheck{Path: path, StateDir: dir}).Run(t.Context()); got.Status != doctor.StatusOK {
		t.Errorf("open state directory: %+v, want ok (the state dir check reports it)", got)
	}
}

// A DPAPI key that no longer opens is the dpapi key check's: no client is
// told to rotate (the rotation would fail on the same key).
func TestSealedClientsCheckLeavesABrokenDPAPIKeyAlone(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	key := &seal.DPAPI{Path: filepath.Join(dir, seal.DPAPIKeyName)}
	st := access.NewStore(dir, nil)
	st.Sealer = key
	if _, _, err := st.Add(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key.Path, make([]byte, 300), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (doctor.SealedClientsCheck{StateDir: dir, Sealer: key}).RunAll(ctx); len(got) != 0 {
		t.Errorf("broken key: %+v, want no result", got)
	}
}
