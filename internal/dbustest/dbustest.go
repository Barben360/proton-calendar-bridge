// Package dbustest starts a private D-Bus session bus for a test, so no test
// ever touches the user's real session (and its desktop keyring).
package dbustest

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// config is a minimal session bus: no service directory (so nothing is ever
// started by activation, e.g. a real keyring daemon) and open policy.
const config = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:tmpdir=/tmp</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`

// Start runs dbus-daemon on a private session bus and returns its address.
// The daemon stops at cleanup. A missing dbus-daemon skips the test, or
// fails it when CI is set.
func Start(t testing.TB) (addr string) {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("dbus-daemon is not installed")
		}
		t.Skip("dbus-daemon is not installed")
	}
	cfg := filepath.Join(t.TempDir(), "bus.conf")
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config-file="+cfg, "--print-address", "--nofork")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	addr = strings.TrimSpace(line)
	if err != nil || addr == "" {
		t.Fatalf("dbus-daemon printed no address: %v", fmt.Sprint(err))
	}
	return addr
}
