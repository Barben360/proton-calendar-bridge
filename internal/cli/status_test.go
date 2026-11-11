package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

type statusStub struct{ stubAPI }

func (*statusStub) Status(context.Context) (service.Status, error) {
	at := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	return service.Status{Phase: "needs-captcha", Reason: "Proton asks for a CAPTCHA", Username: "alice", AccountID: "id1",
		Secrets: "sealed", KeyFile: "/k/secrets.key", CaptchaURL: "https://verify.proton.me/?t=1", LastLogin: &at}, nil
}

func TestStatusTextIsOneKeyValuePerLine(t *testing.T) {
	t.Parallel()
	r := stubRig(t, nil)
	r.svc = &statusStub{}
	r.want(exitcode.OK, "status")
	got := r.out.String()
	for _, want := range []string{
		"phase: needs-captcha\n", "reason: Proton asks for a CAPTCHA\n", "username: alice\n", "account_id: id1\n",
		"secrets: sealed\n", "key_file: /k/secrets.key\n", "unattended: false\n",
		"captcha_url: https://verify.proton.me/?t=1\n", "last_login: 2026-11-11T10:00:00Z\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "last_relogin") {
		t.Errorf("an unset field is printed:\n%s", got)
	}
}

func TestStatusWhenLoggedOutExits0(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "status", "--json")
	var st service.Status
	mustUnmarshal(t, r.envelope().Data, &st)
	if st.Phase != "logged-out" {
		t.Errorf("status = %+v", st)
	}
}

func TestLogoutText(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "logout")
	if !strings.Contains(r.out.String(), "Not logged in") {
		t.Errorf("stdout = %q", r.out.String())
	}
	r.want(exitcode.OK, "login", "--non-interactive")
	r.want(exitcode.OK, "logout")
	if !strings.Contains(r.out.String(), "Logged out") {
		t.Errorf("stdout = %q", r.out.String())
	}
}
