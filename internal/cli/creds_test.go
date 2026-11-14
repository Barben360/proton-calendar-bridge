//go:build unix

package cli_test

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// pipeFD returns the read end of a pipe holding content, as a descriptor the
// CLI owns (it closes it).
func pipeFD(t *testing.T, content string) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wr.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(rd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Close(); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(fd)
}

func TestPasswordFDStripsOneNewline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"pa ss\n", "pa ss"},
		{"pass\r\n", "pass"},
		{"pass\n\n", "pass\n"},
		{"pass", "pass"},
		{" pa ss ", " pa ss "},
		{"pass\r", "pass\r"},
	} {
		s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		r := stubRig(t, s)
		r.env["PCB_PASSWORD"] = "from-env"
		r.want(exitcode.OK, "login", "--non-interactive", "--password-fd", pipeFD(t, tc.in))
		if got := s.logins[0].Password; got != tc.want {
			t.Errorf("fd content %q gave password %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPasswordFDRefusesEmptyBeforeAnyCall(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "\n", "\r\n"} {
		s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		r := stubRig(t, s)
		r.want(exitcode.Usage, "login", "--non-interactive", "--password-fd", pipeFD(t, in))
		if len(s.logins) != 0 {
			t.Errorf("an empty password (%q) reached the service", in)
		}
	}
}

func TestPasswordFDRefusesTooLong(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	r.want(exitcode.Usage, "login", "--non-interactive", "--password-fd", pipeFD(t, strings.Repeat("x", 4097)))
	if len(s.logins) != 0 {
		t.Error("an oversized password reached the service")
	}
}

func TestPasswordFDNotOpenIsUsage(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	r.want(exitcode.Usage, "login", "--non-interactive", "--password-fd", "987")
	if len(s.logins) != 0 {
		t.Error("an unreadable fd reached the service")
	}
}
