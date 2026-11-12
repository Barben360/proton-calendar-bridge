package protontest_test

import (
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// twoFA sends one POST /auth/v4/2fa with code and returns the status.
func twoFA(t *testing.T, f *protontest.Fake, a gpa.Auth, code string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.URL+"/auth/v4/2fa",
		strings.NewReader(`{"TwoFactorCode":"`+code+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("x-pm-uid", a.UID)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestProcessRunsCleanupsLastFirstOnce(t *testing.T) {
	t.Parallel()
	var p protontest.Process
	var order []int
	for i := range 3 {
		p.Cleanup(func() { order = append(order, i) })
	}
	p.Close()
	p.Close()
	if want := []int{2, 1, 0}; !slices.Equal(order, want) {
		t.Fatalf("cleanups ran %v, want %v (last first, once)", order, want)
	}
	p.Cleanup(func() { order = append(order, 9) })
	if order[len(order)-1] != 9 {
		t.Fatalf("a cleanup registered after Close did not run at once: %v", order)
	}
}

func TestProcessFatalIsAnErrorRecoverReturns(t *testing.T) {
	t.Parallel()
	var p protontest.Process
	for name, fail := range map[string]func(){
		"Fatalf": func() { p.Fatalf("protontest: unknown calendar %q", "c1") },
		"Fatal":  func() { p.Fatal(`protontest: unknown calendar "c1"`) },
	} {
		err := func() (err error) {
			defer protontest.Recover(&err)
			fail()
			return nil
		}()
		var fl *protontest.Failure
		if !errors.As(err, &fl) || err.Error() != `protontest: unknown calendar "c1"` {
			t.Errorf("%s: Recover gave %v, want the *Failure with the message", name, err)
		}
	}
}

func TestRecoverLetsOtherPanicsThrough(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v, want the original panic", r)
		}
	}()
	var err error
	func() {
		defer protontest.Recover(&err)
		panic("boom")
	}()
	t.Fatal("the panic did not go on")
}

func TestProcessErrorfReports(t *testing.T) {
	t.Parallel()
	var got []string
	p := protontest.Process{Report: func(msg string) { got = append(got, msg) }}
	p.Errorf("protontest: event parts: %v", "bad")
	if !slices.Equal(got, []string{"protontest: event parts: bad"}) {
		t.Fatalf("reported %q", got)
	}
}

// A fake held by a Process serves on the listener it is given, with the
// TOTP secret it is given, until the Process closes.
func TestFakeRunsUnderAProcess(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var p protontest.Process
	t.Cleanup(p.Close)
	const secret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	f := protontest.New(&p, protontest.Options{TwoFA: true, TOTPSecret: secret, Listener: l})
	if want := "http://" + l.Addr().String(); f.URL != want {
		t.Fatalf("URL %q, want %q (the given listener)", f.URL, want)
	}
	if !strings.HasSuffix(f.TOTPURI, "secret="+secret) {
		t.Fatalf("TOTPURI %q, want the given secret", f.TOTPURI)
	}
	o, err := otp.Parse("otpauth://totp/x?secret=" + secret)
	if err != nil {
		t.Fatal(err)
	}
	a := login(t, f, f.Username, f.Password)
	if got := twoFA(t, f, a, o.Code(time.Now())); got != http.StatusOK {
		t.Fatalf("2FA with the given secret's code: %d, want 200", got)
	}
	p.Close()
	c := &http.Client{Timeout: 2 * time.Second}
	if resp, err := c.Get(f.URL + "/tests/ping"); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the fake still answers after Close: %d", resp.StatusCode)
	}
}
