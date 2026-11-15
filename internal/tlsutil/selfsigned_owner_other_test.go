//go:build !windows

package tlsutil_test

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

// Unix UIDs; the Windows twins are TestSecureRefusesADirectoryOfAnotherOwner
// and TestSecureAcceptsAnAdministratorsOwnedPrivateFile.
func TestSelfSignedRefusesFilesOfAnotherUser(t *testing.T) {
	for _, foreign := range []string{"directory", "key"} {
		t.Run(foreign, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "tls")
			o := tlsutil.Options{Dir: dir, BindIP: net.ParseIP("192.168.1.5"), Now: clock(&now), InterfaceAddrs: noInterfaces(t)}
			if _, err := tlsutil.SelfSigned(o); err != nil {
				t.Fatal(err)
			}
			target := map[string]string{"directory": dir, "key": tlsutil.KeyPath(dir)}[foreign]
			target, _ = filepath.Abs(target)
			restore := tlsutil.SetOwner(func(fi fs.FileInfo) (int, bool) {
				if fi.Name() == filepath.Base(target) {
					return os.Getuid() + 1, true
				}
				return os.Getuid(), true
			})
			defer restore()
			before, _ := os.ReadFile(tlsutil.KeyPath(dir))
			_, err := tlsutil.SelfSigned(o)
			if err == nil || !strings.Contains(err.Error(), "owned by another user") {
				t.Fatalf("SelfSigned with a foreign %s = %v, want a refusal", foreign, err)
			}
			if after, _ := os.ReadFile(tlsutil.KeyPath(dir)); !slices.Equal(before, after) {
				t.Error("the refused start replaced the key")
			}
		})
	}
}
