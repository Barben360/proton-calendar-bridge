package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

func TestDirModeCheckFlagsGroupReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil { // defeat the umask
		t.Fatal(err)
	}
	got := doctor.DirModeCheck{Label: "config dir", Path: dir, Want: 0o700}.Run(context.Background())
	if got.Status != doctor.StatusError || !strings.Contains(got.Fix, "chmod 700 "+dir) {
		t.Fatalf("got %+v", got)
	}
}

func TestDirModeCheckFixQuotesThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my dir's")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	got := doctor.DirModeCheck{Label: "config dir", Path: dir, Want: 0o700}.Run(context.Background())
	want := "chmod 700 '" + strings.ReplaceAll(dir, "'", `'\''`) + "'"
	if got.Fix != want {
		t.Fatalf("fix = %q, want %q", got.Fix, want)
	}
}

func TestDirModeCheckSkipsMissing(t *testing.T) {
	got := doctor.DirModeCheck{Label: "config dir", Path: filepath.Join(t.TempDir(), "nope"), Want: 0o700}.Run(context.Background())
	if got.Status != doctor.StatusSkip {
		t.Fatalf("got %+v", got)
	}
}

func TestDirModeCheckOK(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	got := doctor.DirModeCheck{Label: "config dir", Path: dir, Want: 0o700}.Run(context.Background())
	if got.Status != doctor.StatusOK || got.Fix != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestPlatformCheck(t *testing.T) {
	for _, tt := range []struct {
		goos, goarch string
		want         doctor.Status
	}{
		{"linux", "amd64", doctor.StatusOK},
		{"linux", "arm64", doctor.StatusOK},
		{"windows", "amd64", doctor.StatusWarn},
		{"linux", "riscv64", doctor.StatusWarn},
	} {
		got := doctor.PlatformCheck{GOOS: tt.goos, GOARCH: tt.goarch}.Run(context.Background())
		if got.Status != tt.want {
			t.Errorf("%s/%s: got %+v, want %s", tt.goos, tt.goarch, got, tt.want)
		}
		if tt.want == doctor.StatusWarn && !strings.Contains(got.Message, "unsupported") {
			t.Errorf("%s/%s: message %q lacks \"unsupported\"", tt.goos, tt.goarch, got.Message)
		}
	}
}

func TestRunKeepsOrderAndWorst(t *testing.T) {
	checks := []doctor.Check{
		doctor.PlatformCheck{GOOS: "linux", GOARCH: "amd64"},
		doctor.PlatformCheck{GOOS: "plan9", GOARCH: "386"},
	}
	res := doctor.Run(context.Background(), checks, nil)
	if len(res) != 2 || res[0].Status != doctor.StatusOK || res[1].Status != doctor.StatusWarn {
		t.Fatalf("got %+v", res)
	}
	for _, tt := range []struct {
		in   []doctor.Status
		want doctor.Status
	}{
		{nil, doctor.StatusOK},
		{[]doctor.Status{doctor.StatusSkip, doctor.StatusOK}, doctor.StatusOK},
		{[]doctor.Status{doctor.StatusOK, doctor.StatusWarn, doctor.StatusSkip}, doctor.StatusWarn},
		{[]doctor.Status{doctor.StatusWarn, doctor.StatusError, doctor.StatusOK}, doctor.StatusError},
	} {
		var rs []doctor.Result
		for _, s := range tt.in {
			rs = append(rs, doctor.Result{Status: s})
		}
		if got := doctor.Worst(rs); got != tt.want {
			t.Errorf("Worst(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

type gatedCheck struct {
	name string
	gate chan struct{} // nil: no wait
}

func (c gatedCheck) Name() string { return c.name }
func (c gatedCheck) Run(context.Context) doctor.Result {
	if c.gate != nil {
		<-c.gate
	}
	return doctor.Result{Name: c.name, Status: doctor.StatusOK}
}

// recv receives from ch, failing the test instead of hanging when nothing comes.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run")
		panic("unreachable")
	}
}

func TestRunStreamsEachResult(t *testing.T) {
	release := make(chan struct{})
	seen := make(chan string)
	done := make(chan []doctor.Result)
	go func() {
		done <- doctor.Run(context.Background(),
			[]doctor.Check{gatedCheck{name: "first"}, gatedCheck{name: "second", gate: release}},
			func(r doctor.Result) { seen <- r.Name })
	}()
	// The second check is still blocked: the first result must already be out.
	if got := recv(t, seen); got != "first" {
		t.Fatalf("first streamed result = %q", got)
	}
	select {
	case r := <-done:
		t.Fatalf("Run returned before the second check was released: %+v", r)
	default:
	}
	close(release)
	if got := recv(t, seen); got != "second" {
		t.Fatalf("second streamed result = %q", got)
	}
	if res := recv(t, done); len(res) != 2 || res[0].Name != "first" || res[1].Name != "second" {
		t.Fatalf("results = %+v", res)
	}
}

func TestFailedCheck(t *testing.T) {
	got := doctor.FailedCheck{Label: "config dir", Message: "boom", Fix: "set HOME"}.Run(context.Background())
	if got.Status != doctor.StatusError || got.Name != "config dir" || got.Fix != "set HOME" {
		t.Fatalf("got %+v", got)
	}
}

func TestDoctorConfigCheckReportsBadFile(t *testing.T) {
	dirs := paths.Dirs{Config: t.TempDir(), State: t.TempDir()}
	p := config.Path(dirs)
	if err := os.WriteFile(p, []byte("[proton]\nappversion = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := doctor.ConfigCheck{Path: p, Dirs: dirs}.Run(context.Background())
	if got.Status != doctor.StatusError || !strings.Contains(got.Message, "appversion") || got.Fix != "edit "+p {
		t.Fatalf("got %+v", got)
	}
}

func TestDoctorConfigCheckOK(t *testing.T) {
	dirs := paths.Dirs{Config: t.TempDir(), State: t.TempDir()}
	p := config.Path(dirs)
	got := doctor.ConfigCheck{Path: p, Dirs: dirs}.Run(context.Background())
	if got.Status != doctor.StatusOK || got.Message != "defaults, no file" || got.Fix != "" {
		t.Fatalf("missing file: %+v", got)
	}
	if err := os.WriteFile(p, []byte("[proton]\napp_version = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = doctor.ConfigCheck{Path: p, Dirs: dirs}.Run(context.Background())
	if got.Status != doctor.StatusOK || got.Message != p {
		t.Fatalf("present file: %+v", got)
	}
}

func TestFileModeCheckFlagsGroupReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.key")
	if err := os.WriteFile(path, []byte("k"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil { // defeat the umask
		t.Fatal(err)
	}
	got := doctor.FileModeCheck{Label: "key file", Path: path, Want: 0o600}.Run(context.Background())
	if got.Status != doctor.StatusError || got.Fix != "chmod 600 "+path {
		t.Fatalf("got %+v", got)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (doctor.FileModeCheck{Label: "key file", Path: path, Want: 0o600}).Run(context.Background()); got.Status != doctor.StatusOK {
		t.Fatalf("0600: %+v", got)
	}
}

func TestFileModeCheckSkipsMissingAndRejectsDirs(t *testing.T) {
	dir := t.TempDir()
	if got := (doctor.FileModeCheck{Label: "f", Path: filepath.Join(dir, "nope"), Want: 0o600}).Run(context.Background()); got.Status != doctor.StatusSkip {
		t.Fatalf("missing: %+v", got)
	}
	if got := (doctor.FileModeCheck{Label: "f", Path: dir, Want: 0o600}).Run(context.Background()); got.Status != doctor.StatusError {
		t.Fatalf("dir: %+v", got)
	}
}

// sealedSetup creates a state dir holding secrets.sealed and a key file.
func sealedSetup(t *testing.T) (state, key string) {
	t.Helper()
	root := t.TempDir()
	state, key = filepath.Join(root, "state"), filepath.Join(root, "cfg", "secrets.key")
	for path, dir := range map[string]string{filepath.Join(state, "secrets.sealed"): state, key: filepath.Dir(key)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return state, key
}

func TestSecretsCheckWarnsKeyFile(t *testing.T) {
	state, key := sealedSetup(t)
	restore := doctor.SetDevice(func(p string) (uint64, bool) { return uint64(len(p)), true }) // distinct volumes
	defer restore()
	got := doctor.SecretsCheck{StateDir: state, KeyFile: key}.Run(context.Background())
	if got.Status != doctor.StatusWarn || got.Message != "secrets sealed with a key file (headless fallback)" {
		t.Fatalf("got %+v", got)
	}
}

func TestSecretsCheckSkipsWhenNothingIsSealed(t *testing.T) {
	root := t.TempDir()
	got := doctor.SecretsCheck{StateDir: filepath.Join(root, "state"), KeyFile: filepath.Join(root, "cfg", "secrets.key")}.Run(context.Background())
	if got.Status != doctor.StatusSkip {
		t.Fatalf("got %+v", got)
	}
}

func TestSecretsCheckWarnsUnattended(t *testing.T) {
	state, key := sealedSetup(t)
	if err := store.SaveState(state, store.State{Phase: store.PhaseReady, Unattended: true}); err != nil {
		t.Fatal(err)
	}
	got := doctor.SecretsCheck{StateDir: state, KeyFile: key}.Run(context.Background())
	want := "unattended relogin is on: whoever can read " + key + " and " + filepath.Join(state, "secrets.sealed") + " owns the Proton account"
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestSecretsCheckWarnsSameVolume(t *testing.T) {
	state, key := sealedSetup(t)
	restore := doctor.SetDevice(func(string) (uint64, bool) { return 7, true })
	defer restore()
	got := doctor.SecretsCheck{StateDir: state, KeyFile: key}.Run(context.Background())
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, "the key and the sealed data are on the same volume, so a backup or copy of that volume exposes both") {
		t.Fatalf("got %+v", got)
	}
}

func TestSecretsCheckErrorsWhenKeyIsMissing(t *testing.T) {
	state, key := sealedSetup(t)
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	got := doctor.SecretsCheck{StateDir: state, KeyFile: key}.Run(context.Background())
	if got.Status != doctor.StatusError || !strings.Contains(got.Message, "sealed secrets exist but the key file is missing") || got.Fix != "pcb logout" {
		t.Fatalf("got %+v", got)
	}
	if _, err := os.Stat(key); err == nil {
		t.Fatal("the check created a key")
	}
}

func TestSecretsCheckFailsOnMalformedState(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := doctor.SecretsCheck{StateDir: state, KeyFile: filepath.Join(t.TempDir(), "k")}.Run(context.Background())
	if got.Status != doctor.StatusError {
		t.Fatalf("got %+v", got)
	}
}
