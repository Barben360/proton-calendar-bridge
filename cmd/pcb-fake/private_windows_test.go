package main

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// Windows has no mode bits: an account file whose access list lets
// Everyone read it is refused, with the icacls lines that fix it.
func TestAccountFileWithAnOpenACLIsRefused(t *testing.T) {
	t.Parallel()
	const secretish = "s3cret-value-never-echoed"
	p := accountFile(t, "password = \""+secretish+"\"\n", 0o600)
	acltest.Allow(t, p, "S-1-1-0", windows.GENERIC_READ, windows.NO_INHERITANCE)
	code, out, errb := run(t, "serve", "--account", p)
	if code != exitcode.MustAct || !strings.Contains(errb, "others can read it") || !strings.Contains(errb, "icacls") {
		t.Fatalf("exit %d, want %d refusing the open access list\nstdout: %s\nstderr: %s", code, exitcode.MustAct, out, errb)
	}
	if strings.Contains(out+errb, secretish) {
		t.Fatalf("a value of the account file was echoed: %s%s", out, errb)
	}
}
