//go:build windows

package tlsutil_test

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

// newSelfSigned generates the certificate of a new tls directory and
// returns its options, for a reuse.
func newSelfSigned(t *testing.T) tlsutil.Options {
	t.Helper()
	o := tlsutil.Options{Dir: filepath.Join(t.TempDir(), "tls"), BindIP: net.ParseIP("192.168.1.5"), Now: clock(&now), InterfaceAddrs: noInterfaces(t)}
	if _, err := tlsutil.SelfSigned(o); err != nil {
		t.Fatal(err)
	}
	return o
}

// The Windows twin of TestSelfSignedRepairsModesOnReuse: an access list
// that lets Everyone in, on the key or the directory, is refused (Windows
// repairs nothing) and the key is kept.
func TestSecureRefusesAnOpenKey(t *testing.T) {
	for _, open := range []string{"key", "directory"} {
		t.Run(open, func(t *testing.T) {
			o := newSelfSigned(t)
			target := map[string]string{"key": tlsutil.KeyPath(o.Dir), "directory": o.Dir}[open]
			acltest.Allow(t, target, "S-1-1-0", windows.GENERIC_READ, windows.NO_INHERITANCE)
			before, _ := os.ReadFile(tlsutil.KeyPath(o.Dir))
			_, err := tlsutil.SelfSigned(o)
			if err == nil || !strings.Contains(err.Error(), "access list") || !strings.Contains(err.Error(), acl.AccountName(acltest.Everyone)) ||
				!strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), "pcb doctor") {
				t.Fatalf("SelfSigned with an open %s = %v, want the access list refused", open, err)
			}
			if after, _ := os.ReadFile(tlsutil.KeyPath(o.Dir)); !slices.Equal(before, after) {
				t.Error("the refused start replaced the key")
			}
		})
	}
}

// The Windows twin of TestSelfSignedRefusesFilesOfAnotherUser, for the
// directory: one owned by an account pcb does not accept is refused.
func TestSecureRefusesADirectoryOfAnotherOwner(t *testing.T) {
	o := newSelfSigned(t)
	defer tlsutil.SetOwnerSID(func(string) (string, error) { return acl.SIDSystem, nil })()
	_, err := tlsutil.SelfSigned(o)
	if err == nil || !strings.Contains(err.Error(), "owned by another user") || !strings.Contains(err.Error(), acl.AccountName(acl.SIDSystem)) {
		t.Fatalf("SelfSigned in a SYSTEM-owned directory = %v, want a refusal naming SYSTEM", err)
	}
}

// Review Focus 4: a private key file owned by another account (what an
// elevated run leaves: Administrators) is still used by a normal run; only
// its access list is checked.
func TestSecureAcceptsAnAdministratorsOwnedPrivateFile(t *testing.T) {
	t.Run("owner injected", func(t *testing.T) {
		o := newSelfSigned(t)
		user, err := acl.UserSID()
		if err != nil {
			t.Fatal(err)
		}
		key, _ := filepath.Abs(tlsutil.KeyPath(o.Dir))
		defer tlsutil.SetOwnerSID(func(path string) (string, error) {
			if p, _ := filepath.Abs(path); strings.EqualFold(p, key) {
				return acl.SIDAdministrators, nil
			}
			return user, nil
		})()
		if res, err := tlsutil.SelfSigned(o); err != nil || res.Generated {
			t.Fatalf("reuse of an Administrators-owned key: generated %v, %v", res.Generated, err)
		}
	})
	t.Run("owner set", func(t *testing.T) {
		o := newSelfSigned(t)
		// SYSTEM: an owner pcb never accepts, even elevated.
		acltest.SetOwner(t, tlsutil.KeyPath(o.Dir), acl.SIDSystem)
		if res, err := tlsutil.SelfSigned(o); err != nil || res.Generated {
			t.Fatalf("reuse of a SYSTEM-owned key: generated %v, %v", res.Generated, err)
		}
	})
}
