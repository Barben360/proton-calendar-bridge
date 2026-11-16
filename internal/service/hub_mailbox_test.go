package service_test

import (
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// mailboxHubEnv is a hubEnv whose fake is a two-password account.
func mailboxHubEnv(t *testing.T) *hubEnv {
	t.Helper()
	e := newHubEnv(t)
	e.f = protontest.New(t, protontest.Options{Now: e.clk.Now, Username: userA, TwoPassword: true})
	e.cfg.Proton.BaseURL = e.f.URL
	return e
}

// An added account held for its mailbox password is listed pending with its
// phase; a wrong one keeps it pending (the login stays held), the right one
// makes it ready. Neither password reaches the log.
func TestHubMailboxPassword(t *testing.T) {
	t.Parallel()
	e := mailboxHubEnv(t)
	h, lb := e.openLogged(t)
	r, err := e.login(t, h, userA)
	if err != nil || r.State != service.StateNeedsMailboxPassword || r.Account == "" {
		t.Fatalf("Login = %+v, %v; want needs-mailbox-password on a new account", r, err)
	}
	if info := find(h.Accounts(), r.Account); info == nil || !info.Pending || info.Phase != string(service.StateNeedsMailboxPassword) {
		t.Fatalf("account while held = %+v, want pending, needs-mailbox-password", info)
	}
	const wrong = "wrong-mailbox-pw-Zq91"
	_, err = h.SubmitMailboxPassword(t.Context(), r.Account, wrong)
	_ = wantExit(t, err, exitcode.MustAct)
	if info := find(h.Accounts(), r.Account); info == nil || !info.Pending || info.Phase != string(service.StateNeedsMailboxPassword) {
		t.Fatalf("account after a wrong mailbox password = %+v, want still pending", info)
	}
	lb.want(t, `msg="login mailbox password"`, "account="+r.Account, "outcome=refused", "exit=10")
	// An empty selector is the only account whose login waits.
	res, err := h.SubmitMailboxPassword(t.Context(), "", e.f.MailboxPassword)
	if err != nil || res.State != service.StateReady || res.Account != r.Account {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready on %s", res, err, r.Account)
	}
	if info := find(h.Accounts(), r.Account); info == nil || info.Pending || info.Phase != string(service.StateReady) {
		t.Fatalf("account after its mailbox password = %+v, want ready, not pending", info)
	}
	lb.want(t, `msg="login mailbox password"`, "account="+r.Account, "outcome=ready")
	lb.wantNoSecret(t, wrong, e.f.MailboxPassword, e.f.Password)
}

// Nothing waits for a mailbox password: a must-act refusal naming it, logged
// without the password.
func TestHubMailboxPasswordWithoutLogin(t *testing.T) {
	t.Parallel()
	e := mailboxHubEnv(t)
	h, lb := e.openLogged(t)
	_, err := h.SubmitMailboxPassword(t.Context(), "", "some-mailbox-pw-77")
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Error(), "mailbox password") {
		t.Errorf("error = %q, want it to name the mailbox password", ee.Error())
	}
	lb.want(t, `msg="login mailbox password"`, "outcome=refused")
	lb.wantNoSecret(t, "some-mailbox-pw-77")
}
