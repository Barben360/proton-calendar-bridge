package doctor_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

func TestPlatform(t *testing.T) {
	for _, tt := range []struct {
		goos, goarch string
		want         doctor.Status
		message      string
	}{
		{"linux", "amd64", doctor.StatusOK, "linux/amd64"},
		{"linux", "arm64", doctor.StatusOK, "linux/arm64"},
		{"windows", "amd64", doctor.StatusWarn, "windows/amd64: runs, untested"},
		{"windows", "arm64", doctor.StatusWarn, "windows/arm64: runs, unsupported"},
		{"darwin", "arm64", doctor.StatusWarn, "darwin/arm64: runs, unsupported"},
		{"linux", "riscv64", doctor.StatusWarn, "linux/riscv64: runs, unsupported"},
	} {
		got := doctor.PlatformCheck{GOOS: tt.goos, GOARCH: tt.goarch}.Run(t.Context())
		if got.Name != "platform" || got.Status != tt.want || got.Message != tt.message {
			t.Errorf("%s/%s: %+v, want %s %q", tt.goos, tt.goarch, got, tt.want, tt.message)
		}
	}
}

func TestElevatedCheck(t *testing.T) {
	if got := (doctor.ElevatedCheck{}).Run(t.Context()); got.Name != "elevated" || got.Status != doctor.StatusOK || got.Message != "not elevated" {
		t.Errorf("not elevated: %+v", got)
	}
	got := doctor.ElevatedCheck{Elevated: true}.Run(t.Context())
	if got.Name != "elevated" || got.Status != doctor.StatusWarn ||
		got.Message != "pcb runs elevated (as administrator): what it creates is owned by Administrators" ||
		got.Fix != "run pcb from a normal (not elevated) terminal" {
		t.Errorf("elevated: %+v", got)
	}
}

func TestAutostartCheck(t *testing.T) {
	beside := `C:\pcb\pcb-ui.exe`
	check := func(data string, present bool, err error, exists bool) doctor.AutostartCheck {
		return doctor.AutostartCheck{
			Read:   func() (string, bool, error) { return data, present, err },
			Beside: beside,
			Exists: func(string) bool { return exists },
		}
	}
	if name := (doctor.AutostartCheck{}).Name(); name != "start at login" {
		t.Errorf("Name() = %q", name)
	}
	ctx := t.Context()
	if got := check("", false, nil, true).Run(ctx); got.Status != doctor.StatusSkip || got.Message != "Start at login is off" {
		t.Errorf("absent: %+v", got)
	}
	if got := check(runkey.Data(beside), true, nil, true).Run(ctx); got.Status != doctor.StatusOK || got.Message != "starts "+beside {
		t.Errorf("beside: %+v", got)
	}
	// The path compares as Windows does: case and a doubled separator
	// aside.
	if got := check(runkey.Data(`c:\PCB\\pcb-UI.exe`), true, nil, true).Run(ctx); got.Status != doctor.StatusOK {
		t.Errorf("beside, other spelling: %+v", got)
	}
	old := `C:\old\pcb-ui.exe`
	var asked string
	gone := check(runkey.Data(old), true, nil, false)
	gone.Exists = func(p string) bool { asked = p; return false }
	got := gone.Run(ctx)
	if got.Status != doctor.StatusError || !strings.Contains(got.Message, old) || !strings.Contains(got.Message, "does not exist") ||
		got.Fix != "turn Start at login off and on again in pcb-ui" || asked != old {
		t.Errorf("gone: %+v (asked about %q)", got, asked)
	}
	other := `D:\apps\pcb-ui.exe`
	if got := check(runkey.Data(other), true, nil, true).Run(ctx); got.Status != doctor.StatusWarn ||
		!strings.Contains(got.Message, other) || !strings.Contains(got.Message, beside) {
		t.Errorf("another pcb-ui: %+v", got)
	}
	if got := check(`"C:\unclosed --background`, true, nil, true).Run(ctx); got.Status != doctor.StatusError ||
		got.Fix != "turn Start at login off and on again in pcb-ui" {
		t.Errorf("no program: %+v", got)
	}
	// Where pcb.exe lies is unknown (os.Executable failed): a warning.
	noBeside := check(runkey.Data(other), true, nil, true)
	noBeside.Beside = ""
	if got := noBeside.Run(ctx); got.Status != doctor.StatusWarn || !strings.Contains(got.Message, other) {
		t.Errorf("no beside: %+v", got)
	}
	if got := check("", false, errors.New("access is denied"), true).Run(ctx); got.Status != doctor.StatusWarn || !strings.Contains(got.Message, "access is denied") {
		t.Errorf("read error: %+v", got)
	}
}

func TestZoneCheck(t *testing.T) {
	ctx := t.Context()
	got := doctor.ZoneCheck{Key: "W. Europe Standard Time", KeyOK: true}.Run(ctx)
	if got.Name != "time zone" || got.Status != doctor.StatusOK || got.Message != "W. Europe Standard Time → Europe/Berlin" {
		t.Errorf("mapped: %+v", got)
	}
	got = doctor.ZoneCheck{Key: "Mars Standard Time", KeyOK: true}.Run(ctx)
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, "Mars Standard Time") || !strings.Contains(got.Message, "UTC offsets") ||
		!strings.Contains(got.Fix, "[mcp] timezone") {
		t.Errorf("unmapped: %+v", got)
	}
	if got := (doctor.ZoneCheck{}).Run(ctx); got.Status != doctor.StatusWarn || got.Message != "the Windows time zone cannot be read" {
		t.Errorf("unread: %+v", got)
	}
}

// dpapiLike writes a file that starts as a DPAPI blob does (version 1,
// then the provider GUID), so that the check reaches the unprotect step
// on every OS.
func dpapiLike(t *testing.T, path string) {
	t.Helper()
	blob := append([]byte{1, 0, 0, 0, 0xd0, 0x8c, 0x9d, 0xdf, 0x01, 0x15, 0xd1, 0x11, 0x8c, 0x7a, 0x00, 0xc0, 0x4f, 0xc2, 0x97, 0xeb}, make([]byte, 200)...)
	if !seal.HasDPAPIHeader(blob) {
		t.Fatal("the test blob has no DPAPI header")
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A key Windows cannot unprotect in this session (a logon without the
// password) is an error that may pass by itself, with the sign-in fix,
// never a logout.
func TestDPAPIKeyCheckKeyUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), seal.DPAPIKeyName)
	dpapiLike(t, path)
	cause := errors.New("access is denied")
	defer doctor.SetDPAPICheck(func(p string) error {
		if p != path {
			t.Errorf("checked %q, want %q", p, path)
		}
		return &seal.ProtectError{Unprotect: true, Err: cause}
	})()
	got := doctor.DPAPIKeyCheck{Path: path}.Run(t.Context())
	if got.Name != "dpapi key" || got.Status != doctor.StatusError || !got.Transient || got.Fix != service.DPAPIUnprotectFix ||
		!strings.Contains(got.Message, path) || !strings.Contains(got.Message, "access is denied") || strings.Contains(got.Message+got.Fix, "logout") {
		t.Errorf("key unavailable: %+v", got)
	}
}

func TestDPAPIKeyCheckRefusesAFileThatIsNoDPAPIBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), seal.DPAPIKeyName)
	if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil { // a raw key
		t.Fatal(err)
	}
	defer doctor.SetDPAPICheck(func(string) error {
		t.Error("a file with no DPAPI header was given to Windows to unprotect")
		return nil
	})()
	got := doctor.DPAPIKeyCheck{Path: path}.Run(t.Context())
	if got.Status != doctor.StatusError || got.Transient || !strings.Contains(got.Message, "not a DPAPI-protected key") ||
		!strings.Contains(got.Message, "cannot be opened by this Windows user") ||
		got.Fix != "restore it from a backup; with no secrets.sealed, remove it and run pcb login" {
		t.Errorf("raw key: %+v", got)
	}
}

func TestDPAPIKeyCheckSkipsAMissingKey(t *testing.T) {
	got := doctor.DPAPIKeyCheck{Path: filepath.Join(t.TempDir(), seal.DPAPIKeyName)}.Run(t.Context())
	if got.Name != "dpapi key" || got.Status != doctor.StatusSkip || got.Message != "no key yet: the first login creates it" {
		t.Errorf("missing: %+v", got)
	}
}

// fakeSealer is a sealer named dpapi that leaves data as it is, or whose
// Open fails with err. It has no Check method: its failures show at Open.
type fakeSealer struct{ err error }

func (fakeSealer) Name() string { return service.SealerDPAPI }

func (fakeSealer) Seal(p, _ []byte) ([]byte, error) { return append([]byte(nil), p...), nil }

func (f fakeSealer) Open(s, _ []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]byte(nil), s...), nil
}

// unavailable is DPAPI's failure in a logon without the password.
var unavailable = &seal.ProtectError{Unprotect: true, Err: errors.New("access is denied")}

// The secrets of a dpapi state are named as DPAPI's, and a key Windows
// cannot unprotect in this session is never a logout.
func TestSecretsCheckOfADPAPIState(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(dir, "secrets.sealed")
	if err := store.NewSecrets(dir, fakeSealer{}).Update(ctx, func(m map[string]store.Account) error {
		m["a"] = store.Account{ID: "a", Username: "u"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := doctor.SecretsCheck{StateDir: dir, Sealer: fakeSealer{}}.Run(ctx)
	if got.Status != doctor.StatusOK || got.Message != "secrets sealed with a key DPAPI protects for this Windows user" {
		t.Errorf("sealed: %+v", got)
	}
	got = doctor.SecretsCheck{StateDir: dir, Sealer: fakeSealer{err: unavailable}}.Run(ctx)
	if got.Status != doctor.StatusWarn || got.Fix != service.DPAPIUnprotectFix || !strings.Contains(got.Message, "access is denied") ||
		strings.Contains(got.Message+got.Fix, "logout") || strings.Contains(got.Message, "keyring") {
		t.Errorf("key unavailable: %+v", got)
	}
	got = doctor.SecretsCheck{StateDir: dir, Sealer: fakeSealer{err: seal.ErrNoKey}}.Run(ctx)
	if got.Status != doctor.StatusError || !strings.Contains(got.Message, "DPAPI key") || strings.Contains(got.Message, "keyring") || got.Fix != "pcb logout" {
		t.Errorf("no key: %+v", got)
	}
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady, Unattended: true}); err != nil {
		t.Fatal(err)
	}
	got = doctor.SecretsCheck{StateDir: dir, Sealer: fakeSealer{}}.Run(ctx)
	if want := "unattended relogin is on: whoever can sign in as this Windows user and read " + sealed + " owns the Proton account"; got.Status != doctor.StatusWarn || got.Message != want {
		t.Errorf("unattended: %+v, want %q", got, want)
	}
}

// A dpapi key Windows cannot unprotect in this session is the dpapi key
// check's: no client is told to rotate its password for it (a rotation
// would fail the same way).
func TestSealedClientsCheckLeavesAnUnavailableDPAPIKeyAlone(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st := access.NewStore(dir, nil)
	st.Sealer = fakeSealer{}
	if _, _, err := st.Add(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	if got := (doctor.SealedClientsCheck{StateDir: dir, Sealer: fakeSealer{err: unavailable}}).RunAll(ctx); len(got) != 0 {
		t.Errorf("key unavailable: %+v, want no result", got)
	}
	// A key that opens nothing still warns.
	if got := (doctor.SealedClientsCheck{StateDir: dir, Sealer: fakeSealer{err: seal.ErrCorrupt}}).RunAll(ctx); len(got) != 1 || got[0].Fix != "pcb client rotate phone" {
		t.Errorf("damaged key: %+v, want the rotation", got)
	}
}
