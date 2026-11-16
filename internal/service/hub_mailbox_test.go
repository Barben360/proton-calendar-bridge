package service_test

import (
	"errors"
	"slices"
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
	e.f.AddUser(userB, passwordB)
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

// A selector naming an account held at its 2FA step is refused: nothing waits
// for a mailbox password there, and the account stays held for its code.
func TestHubMailboxPasswordRefusedAtTheTOTPStep(t *testing.T) {
	t.Parallel()
	e := mailboxHubEnv(t)
	h := e.open(t)
	uri := e.f.SetTwoFA(userA, true)
	r, err := e.login(t, h, userA)
	if err != nil || r.State != service.StateNeedsTOTP {
		t.Fatalf("Login = %+v, %v; want needs-totp", r, err)
	}
	_, err = h.SubmitMailboxPassword(t.Context(), r.Account, e.f.MailboxPassword)
	ee := wantExit(t, err, exitcode.MustAct)
	if ee.Error() != "no login is waiting for a mailbox password" {
		t.Errorf("error = %q, want no login is waiting for a mailbox password", ee.Error())
	}
	if info := find(h.Accounts(), r.Account); info == nil || !info.Pending || info.Phase != string(service.StateNeedsTOTP) {
		t.Fatalf("account after the refusal = %+v, want still pending at needs-totp", info)
	}
	res, err := h.SubmitTOTP(t.Context(), r.Account, code(t, uri, e.clk.Now()))
	if err != nil || res.State != service.StateNeedsMailboxPassword {
		t.Fatalf("SubmitTOTP = %+v, %v; want its code still accepted, then needs-mailbox-password", res, err)
	}
}

// With two logins held and no selector, the mailbox password is not guessed
// onto one: the several-accounts refusal names both.
func TestHubMailboxPasswordSeveralHeldNeedsSelector(t *testing.T) {
	t.Parallel()
	e := mailboxHubEnv(t)
	h := e.open(t)
	e.f.SetTwoFA(userA, true)
	ra, err := e.login(t, h, userA)
	if err != nil || ra.State != service.StateNeedsTOTP {
		t.Fatalf("Login(a) = %+v, %v; want needs-totp", ra, err)
	}
	rb, err := e.login(t, h, userB)
	if err != nil || rb.State != service.StateNeedsMailboxPassword {
		t.Fatalf("Login(b) = %+v, %v; want needs-mailbox-password", rb, err)
	}
	_, err = h.SubmitMailboxPassword(t.Context(), "", e.f.MailboxPassword)
	ee := wantExit(t, err, exitcode.Usage)
	var several *service.SeveralAccountsError
	if !errors.As(ee, &several) || len(several.Accounts) != 2 {
		t.Fatalf("error = %#v, want a SeveralAccountsError of 2 accounts", ee)
	}
	got := []string{several.Accounts[0].ID, several.Accounts[1].ID}
	want := []string{ra.Account, rb.Account}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("error names %v, want %v", got, want)
	}
	if info := find(h.Accounts(), rb.Account); info == nil || !info.Pending || info.Phase != string(service.StateNeedsMailboxPassword) {
		t.Errorf("b after the refusal = %+v, want still held", info)
	}
}
