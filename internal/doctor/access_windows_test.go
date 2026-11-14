//go:build windows

package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

// The Windows twin of TestDirModeCheckFlagsGroupReadable: an Everyone ACE
// on a private directory is an error whose fix removes it.
func TestDirModeCheckReportsAnOpenACL(t *testing.T) {
	d := filepath.Join(t.TempDir(), "my dir's")
	if err := acl.MkdirAll(d); err != nil {
		t.Fatal(err)
	}
	check := doctor.DirModeCheck{Label: "state dir", Path: d, Want: 0o700}
	if got := check.Run(context.Background()); got.Status != doctor.StatusOK || got.Message != d+" private" {
		t.Fatalf("private: %+v", got)
	}
	acltest.Allow(t, d, acltest.Everyone, windows.GENERIC_READ, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	got := check.Run(context.Background())
	if got.Status != doctor.StatusError || !strings.Contains(got.Message, acltest.Everyone) || !strings.Contains(got.Message, "access list") || got.Fix != acl.Fix(d, acltest.Everyone) {
		t.Fatalf("open: %+v, want the Everyone ACE and its fix %q", got, acl.Fix(d, acltest.Everyone))
	}
}

// A directory owned by an account pcb does not accept is an error (an
// elevated run's directories, Administrators', for a normal run); a file
// is checked by its access list only.
func TestModeChecksJudgeTheOwnerOfDirectoriesOnly(t *testing.T) {
	d := filepath.Join(t.TempDir(), "state")
	if err := acl.MkdirAll(d); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(d, "state.json")
	if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := acl.SIDSystem
	defer doctor.SetOwnerSID(func(string) (string, error) { return owner, nil })()
	got := doctor.DirModeCheck{Label: "state dir", Path: d, Want: 0o700}.Run(context.Background())
	if got.Status != doctor.StatusError || got.Message != d+" belongs to "+acl.AccountName(acl.SIDSystem) ||
		got.Fix != "run pcb as that user, or move "+shellquote.Quote(d)+" aside" {
		t.Errorf("SYSTEM-owned directory: %+v", got)
	}
	owner = acl.SIDAdministrators
	got = doctor.DirModeCheck{Label: "state dir", Path: d, Want: 0o700}.Run(context.Background())
	if wantOK := acl.Elevated(); (got.Status == doctor.StatusOK) != wantOK {
		t.Errorf("Administrators-owned directory, elevated %v: %+v", wantOK, got)
	}
	if got := (doctor.FileModeCheck{Label: "state", Path: f, Want: 0o600}).Run(context.Background()); got.Status != doctor.StatusOK || got.Message != f+" private" {
		t.Errorf("Administrators-owned private file: %+v", got)
	}
	acltest.Allow(t, f, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE)
	if got := (doctor.FileModeCheck{Label: "state", Path: f, Want: 0o600}).Run(context.Background()); got.Status != doctor.StatusError || got.Fix != acl.Fix(f, acltest.Everyone) {
		t.Errorf("open file: %+v", got)
	}
}

func TestCacheAndCertificateChecksUseTheACL(t *testing.T) {
	ctx := context.Background()
	t.Run("cache", func(t *testing.T) {
		dir := t.TempDir()
		path := cache.Path(dir)
		c, err := cache.Open(path, "a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ApplyDelta(ctx, "cal", "c", nil, nil, time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if r := runCache(dir); r.Status != doctor.StatusOK || !strings.HasPrefix(r.Message, path+" private, schema") {
			t.Fatalf("private: %+v", r)
		}
		acltest.Allow(t, path, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE)
		if r := runCache(dir); r.Status != doctor.StatusError || !strings.Contains(r.Message, acltest.Everyone) || r.Fix != acl.Fix(path, acltest.Everyone) {
			t.Errorf("open: %+v", r)
		}
	})
	t.Run("certificate", func(t *testing.T) {
		remote := config.CalDAV{Listen: "0.0.0.0:8443", Hostnames: []string{"pcb.lan"}}
		dir := filepath.Join(t.TempDir(), "tls")
		selfSigned(t, dir, "pcb.lan")
		check := doctor.CertificateCheck{CalDAV: remote, TLSDir: dir, Now: func() time.Time { return day0 }}
		if got := check.Run(ctx); got.Status != doctor.StatusOK {
			t.Fatalf("private: %+v", got)
		}
		key := tlsutil.KeyPath(dir)
		acltest.Allow(t, key, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE)
		if got := check.Run(ctx); got.Status != doctor.StatusError || !strings.Contains(got.Message, acltest.Everyone) || got.Fix != acl.Fix(key, acltest.Everyone) {
			t.Errorf("open key: %+v", got)
		}
	})
}
