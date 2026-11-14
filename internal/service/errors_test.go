package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

// TestMapError pins the exit code of every failure kind, by type (R23a).
func TestMapError(t *testing.T) {
	t.Parallel()
	const keyFile = "/k/secrets.key"
	guard := &exitcode.Error{Code: exitcode.Blocked, Err: &loginguard.TooSoon{Wait: 50 * time.Second}, Hint: "retry in 50s"}
	cases := []struct {
		name     string
		err      error
		code     exitcode.Code
		hintHas  string
		wantText string
	}{
		{"wrong password", fmt.Errorf("x: %w", auth.ErrBadCredentials), exitcode.MustAct, "login", "password"},
		{"totp rejected", fmt.Errorf("x: %w", auth.ErrTOTPRejected), exitcode.MustAct, "code", "2FA"},
		{"captcha rejected", auth.ErrCaptchaRejected, exitcode.MustAct, "CAPTCHA", "CAPTCHA"},
		{"captcha required", &auth.CaptchaError{Token: "t", URL: "https://verify.proton.me/?token=t"}, exitcode.MustAct, "verify.proton.me", "CAPTCHA"},
		{"two password", auth.ErrTwoPassword, exitcode.MustAct, "single-password", "not supported"},
		{"fido2", auth.ErrFIDO2, exitcode.MustAct, "authenticator", "not supported"},
		{"keys locked", keys.ErrKeysLocked, exitcode.MustAct, "password", ""},
		{"session ended", fmt.Errorf("x: %w", api.ErrSessionEnded), exitcode.MustAct, "login", ""},
		{"unsupported verification", &auth.UnsupportedVerificationError{Methods: []string{"sms"}}, exitcode.Blocked, "", "not support"},
		{"server proof", auth.ErrServerProof, exitcode.Blocked, "", ""},
		{"guard passes through", guard, exitcode.Blocked, "retry in 50s", ""},
		{"raw too soon", &loginguard.TooSoon{Wait: 50 * time.Second}, exitcode.Blocked, "50s", ""},
		{"429", &api.Error{Status: 429, Code: 85131, Message: "slow down", RetryAfter: 30 * time.Second}, exitcode.Blocked, "30s", ""},
		{"503", &api.Error{Status: 503, Message: "down"}, exitcode.Blocked, "later", "Proton"},
		{"proton 4xx", &api.Error{Status: 400, Code: 2000, Message: "bad"}, exitcode.Blocked, "", "2000"},
		{"network", &net.OpError{Op: "dial", Err: errors.New("refused")}, exitcode.Blocked, "network", "reach Proton"},
		// ux-I3: a key or state file fault is the person's to repair: 10 in every command.
		{"no key", fmt.Errorf("%w: %s", seal.ErrNoKey, keyFile), exitcode.MustAct, "restore", keyFile},
		{"corrupt store", seal.ErrCorrupt, exitcode.MustAct, "pcb logout", keyFile},
		{"key of a wrong size", &seal.KeySizeError{Path: keyFile, Size: 5}, exitcode.MustAct, "pcb logout", keyFile},
		{"cache newer schema", fmt.Errorf("reading the cache: %w", cache.ErrNewerSchema), exitcode.Blocked, "upgrade pcb", "newer pcb"},
		{"cache corrupt", fmt.Errorf("x: %w", cache.ErrCorrupt), exitcode.MustAct, "pcb sync --full", "damaged"},
		{"cache permission", fmt.Errorf("x: %w", &cache.IOError{Err: &fs.PathError{Op: "open", Path: "/s/pcb.db", Err: fs.ErrPermission}}), exitcode.Blocked, "permissions", "permission denied"},
		{"cache refusing a write", &syncer.StoreError{Err: errors.New("constraint failed hunter2")}, exitcode.Blocked, "pcb doctor", "event cache"},
		{"cache damaged during a pass", &syncer.StoreError{Err: fmt.Errorf("x: %w", cache.ErrCorrupt)}, exitcode.MustAct, "pcb sync --full", "damaged"},
		{"cache changed while reading", fmt.Errorf("x: %w", cache.ErrChanged), exitcode.Blocked, "run the command again", "changed while reading"},
		// A path error of another file (the key file, a state file) is not the cache's.
		{"non-cache path error", &fs.PathError{Op: "open", Path: "/k/secrets.key", Err: fs.ErrPermission}, exitcode.Blocked, "doctor", "unexpected"},
		{"cancelled", context.Canceled, exitcode.Blocked, "", ""},
		{"unexpected", errors.New("secret-looking detail hunter2"), exitcode.Blocked, "", "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapError(tc.err, keyFile)
			var ee *exitcode.Error
			if !errors.As(got, &ee) {
				t.Fatalf("mapError = %#v, want an *exitcode.Error", got)
			}
			if ee.Code != tc.code {
				t.Errorf("code = %d, want %d", ee.Code, tc.code)
			}
			if ee.Hint == "" || !strings.Contains(ee.Hint, tc.hintHas) {
				t.Errorf("hint = %q, want non-empty containing %q", ee.Hint, tc.hintHas)
			}
			if !strings.Contains(ee.Error(), tc.wantText) {
				t.Errorf("message = %q, want it to contain %q", ee.Error(), tc.wantText)
			}
			if strings.Contains(ee.Error()+ee.Hint, "hunter2") {
				t.Error("an unknown error's text leaked into the message")
			}
		})
	}
	if mapError(nil, keyFile) != nil {
		t.Error("mapError(nil) != nil")
	}
}

func TestAcquireErrorIsBlocked(t *testing.T) {
	t.Parallel()
	for _, err := range []error{filelock.ErrUnsupported, errors.New("open /state/login.lock: permission denied")} {
		var ee *exitcode.Error
		if got := acquireError(err, "/state"); !errors.As(got, &ee) || ee.Code != exitcode.Blocked || ee.Hint == "" {
			t.Errorf("acquireError(%v) = %#v, want Blocked with a hint", err, got)
		}
	}
	guard := &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("busy"), Hint: "wait for it to finish"}
	if got := acquireError(guard, "/state"); got != guard { //nolint:errorlint // identity is the point
		t.Errorf("a guard refusal was rewrapped: %v", got)
	}
}

// What a watch reads from a mapped error: how long Proton asked to wait,
// and whether retrying can help at all.
func TestWatchReadsTheMappedError(t *testing.T) {
	t.Parallel()
	limited := mapError(&api.Error{Status: 429, RetryAfter: 20 * time.Minute}, "")
	if got := retryAfter(limited); got != 20*time.Minute {
		t.Errorf("retryAfter(429 with Retry-After 20m) = %v", got)
	}
	if h := exitcode.HintOf(limited); h != "retry in 20m0s" {
		t.Errorf("one-shot hint = %q, want the retry-in hint kept", h)
	}
	for _, c := range []struct {
		name string
		err  error
		stop bool
	}{
		{"must act", mapError(api.ErrSessionEnded, ""), true},
		{"newer schema", mapError(fmt.Errorf("x: %w", cache.ErrNewerSchema), ""), true},
		{"no login locks", acquireError(filelock.ErrUnsupported, "/s"), true},
		{"rate limited", limited, false},
		{"Proton down", mapError(&api.Error{Status: 503}, ""), false},
		{"network", mapError(&net.OpError{Op: "dial", Err: errors.New("refused")}, ""), false},
		{"cache permission", mapError(&cache.IOError{Err: &fs.PathError{Op: "open", Path: "/s/pcb.db", Err: fs.ErrPermission}}, ""), false},
		{"partial", partial("1 of 2 calendars failed to sync", "run pcb sync again"), false},
	} {
		if got := stopsWatch(c.err); got != c.stop {
			t.Errorf("%s: stopsWatch = %v, want %v", c.name, got, c.stop)
		}
		if c.name != "rate limited" && retryAfter(c.err) != 0 {
			t.Errorf("%s: retryAfter = %v, want 0", c.name, retryAfter(c.err))
		}
	}
}

// The note of a listing that never settled is shown as is: it holds a count
// only.
func TestCalendarErrorShowsTheUnverifiedNote(t *testing.T) {
	t.Parallel()
	note := &syncer.UnverifiedError{Rows: 2}
	if got := calendarError(context.Background(), fmt.Errorf("x: %w", note)); got != note.Error() {
		t.Errorf("calendarError = %q, want %q", got, note.Error())
	}
}
