package ctl_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const mailboxPW = "mbx-S3CRET-correct-horse"

// The mailbox password goes to the account whose path carries it, as sent,
// and the result comes back; a refusal crosses with its exit code and hint.
func TestMailboxPasswordRoundTrip(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{})
	hub.infos = append(hub.infos, service.AccountInfo{ID: idB, Address: addrB})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	c := dial(t, s.path)
	res, err := c.SubmitMailboxPassword(t.Context(), idB, mailboxPW)
	if err != nil || res.State != service.StateReady || res.Account != idA {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want the stub's ready result", res, err)
	}
	if _, err := c.SubmitMailboxPassword(t.Context(), addrA, "other-pw"); err != nil { // an address is a selector too
		t.Fatal(err)
	}
	hub.mu.Lock()
	if !slices.Equal(hub.mailboxFor, []string{idB, addrA}) || !slices.Equal(hub.mailboxes, []string{mailboxPW, "other-pw"}) {
		t.Errorf("the hub got passwords %v for %v", hub.mailboxes, hub.mailboxFor)
	}
	hub.mu.Unlock()

	_, err = c.SubmitMailboxPassword(t.Context(), idA, "wrong")
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.MustAct || ee.Hint != "2 tries left" {
		t.Errorf("a refused mailbox password = %#v; want exit 10 with its hint", err)
	}
	if _, err := c.SubmitMailboxPassword(t.Context(), "zzzzzzzz", "x"); exitcode.From(err) != exitcode.Usage {
		t.Errorf("unknown account = %v; want exit 2", err)
	}
}

// The password travels in the body only: the daemon's debug log carries the
// route, never the password; a malformed body is a 400 that never reaches
// the Hub, and so is one over 64 KiB (413).
func TestMailboxPasswordBodyAndLogs(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	c := dial(t, s.path)
	if _, err := c.SubmitMailboxPassword(t.Context(), idA, mailboxPW); err != nil {
		t.Fatal(err)
	}
	target := "/v2/accounts/" + idA + "/login/mailbox-password"
	resp, env := raw(t, s.path, http.MethodPost, target, strings.NewReader(`{"password":`))
	if resp.StatusCode != http.StatusBadRequest || env["ok"] != false {
		t.Errorf("malformed body: HTTP %d, envelope %v; want 400", resp.StatusCode, env)
	}
	big := `{"password":"` + strings.Repeat("x", 64<<10) + `"}`
	resp, _ = raw(t, s.path, http.MethodPost, target, strings.NewReader(big))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: HTTP %d; want 413", resp.StatusCode)
	}
	if resp, _ := raw(t, s.path, http.MethodGet, target, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: HTTP %d; want 405", resp.StatusCode)
	}
	hub.mu.Lock()
	n := len(hub.mailboxes)
	hub.mu.Unlock()
	if n != 1 {
		t.Errorf("the hub got %d mailbox passwords, want only the valid request", n)
	}
	logs := s.logs.String()
	if strings.Contains(logs, mailboxPW) || strings.Contains(logs, "xxxx") {
		t.Errorf("the logs carry the password:\n%s", logs)
	}
	if !strings.Contains(logs, "path="+target+" ") {
		t.Errorf("the logs lack the route:\n%s", logs)
	}
}

// Only one login runs at a time: a mailbox password during a login is refused
// at once, like a 2FA code.
func TestMailboxPasswordRefusedDuringLogin(t *testing.T) {
	t.Parallel()
	api := newHub(service.Status{})
	api.entered, api.release = make(chan struct{}), make(chan struct{})
	s := serve(t, &ctl.Server{Hub: api, Daemon: daemon()})
	first := make(chan error, 1)
	go func() {
		c, err := ctl.Dial(t.Context(), s.path)
		if err != nil {
			first <- err
			return
		}
		defer func() { _ = c.Close() }()
		_, err = c.Login(t.Context(), service.LoginRequest{Username: "alice", Password: "pw"})
		first <- err
	}()
	<-api.entered
	_, err := dial(t, s.path).SubmitMailboxPassword(t.Context(), idA, mailboxPW)
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.MustAct || ee.Error() != "a login is already in progress" {
		t.Errorf("SubmitMailboxPassword during a login = %#v; want exit 10, a login is already in progress", err)
	}
	close(api.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.mailboxes) != 0 {
		t.Errorf("the hub got %d mailbox passwords during the login", len(api.mailboxes))
	}
}

// The request type never prints its password, by any verb or slog.
func TestMailboxPasswordRequestIsRedacted(t *testing.T) {
	t.Parallel()
	r := ctl.MailboxPasswordRequest{Password: mailboxPW}
	var b strings.Builder
	l := slog.New(slog.NewTextHandler(&b, nil))
	l.Info("x", "req", r, "ptr", &r)
	for _, out := range []string{fmt.Sprint(r), fmt.Sprintf("%v %+v %#v %s", r, r, r, r), fmt.Sprint(&r), b.String()} {
		if strings.Contains(out, mailboxPW) || !strings.Contains(out, "redacted") {
			t.Errorf("a rendering of the request leaks or is not marked redacted: %q", out)
		}
	}
}
