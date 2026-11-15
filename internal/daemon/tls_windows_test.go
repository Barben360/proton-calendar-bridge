//go:build windows

package daemon

import (
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// A TLS directory whose access list lets another account in is the
// person's to fix (exit 10, as docs/troubleshooting.md says), and pcb
// doctor gives the icacls line: not "make sure it is writable", exit 20.
func TestListenerTLSRefusesAnOpenAccessListWithMustAct(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if err := acl.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	acltest.Allow(t, dir, acltest.Everyone, windows.GENERIC_READ, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	_, _, err := listenerTLS(slog.New(slog.DiscardHandler), io.Discard, net.IPv4zero, dir, config.CalDAV{Listen: "0.0.0.0:8443"})
	if exitcode.From(err) != exitcode.MustAct || exitcode.HintOf(err) != "run pcb doctor for the fix" {
		t.Fatalf("listenerTLS = %v (exit %d, hint %q), want exit 10 and the hint to run pcb doctor", err, exitcode.From(err), exitcode.HintOf(err))
	}
	if want := "lets " + acl.AccountName(acltest.Everyone) + " (" + acltest.Everyone + ") in"; !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q, want the directory and %q", err, want)
	}
}
