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

type renderStatusStub struct{ stubAPI }

func (*renderStatusStub) Status(context.Context) (service.Status, error) {
	return service.Status{Phase: "ready", Secrets: "sealed", Sync: &service.SyncStatus{
		Calendars:     []service.CalendarStatus{{ID: "c1", Name: "Locked", KeysError: "the calendar keys do not unlock with the account keys"}},
		Undecryptable: 3, DuplicateMasters: 1,
	}}, nil
}

// What the read API could not render is in the text, not only in --json.
func TestStatusTextNamesPlaceholders(t *testing.T) {
	t.Parallel()
	r := stubRig(t, nil)
	r.svc = &renderStatusStub{}
	r.want(exitcode.OK, "status")
	got := r.out.String()
	for _, want := range []string{
		"keys: the calendar keys do not unlock with the account keys (its events are shown as placeholders)\n",
		"undecryptable: 3 events served as placeholders\n", "duplicate_masters: 1 left out",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
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

type neverSyncedStub struct{ stubAPI }

func (*neverSyncedStub) Status(context.Context) (service.Status, error) {
	return service.Status{Phase: "ready", Username: "alice", AccountID: "id1", Secrets: "sealed"}, nil
}

// Logged in but never synced: status says what to run.
func TestStatusNeverSynced(t *testing.T) {
	t.Parallel()
	r := stubRig(t, nil)
	r.svc = &neverSyncedStub{}
	r.want(exitcode.OK, "status")
	if !strings.Contains(r.out.String(), "sync: never (run pcb sync)\n") {
		t.Errorf("status = %q, want the never-synced line", r.out.String())
	}
	r.svc = &statusStub{} // no account: no sync line
	r.want(exitcode.OK, "status")
	if strings.Contains(r.out.String(), "sync:") {
		t.Errorf("logged-out status shows a sync line: %q", r.out.String())
	}
}
