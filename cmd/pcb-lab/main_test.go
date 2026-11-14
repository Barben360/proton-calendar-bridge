package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli/clitest"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

// labEnv logs into the fake and points the CLI at it; it returns the state dir.
func labEnv(t *testing.T, f *labtest.Front, app string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := session.Login(t.Context(), session.Options{
		BaseURL: f.URL, AppVersion: app, StateDir: dir,
		Creds: session.Creds{Username: labtest.User, Password: labtest.Pass},
		Now:   func() time.Time { return time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	s.Close()
	t.Setenv("PCB_LAB_BASEURL", f.URL)
	t.Setenv("PCB_LAB_APPVERSION", app)
	return dir
}

// run executes the CLI and decodes the single JSON envelope on stdout.
func run(t *testing.T, args ...string) (exitcode.Code, map[string]any) {
	t.Helper()
	var out, errb bytes.Buffer
	code := execute(append(args, "--json"), &out, &errb) // stderr is not asserted
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out.String())
	}
	return code, env
}

func TestProbeCalendarAccessPrintsCodesAndRecords(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{Replies: map[string]labtest.Reply{
		"GET /calendar/v1":       labtest.R(403, `{"Code":9100,"Error":"insufficient scope"}`),
		"GET /settings/calendar": labtest.R(200, `{"Code":1000}`),
	}})
	dir := labEnv(t, f, "external-drive-x@1.0.0")
	rec := t.TempDir()
	code, env := run(t, "probe", "calendar-access", "--state-dir", dir, "--record-dir", rec)
	if code != exitcode.Blocked || env["ok"] != false {
		t.Fatalf("code %d, envelope %v; want Blocked: the calendar list was refused", code, env)
	}
	if msg := env["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "/calendar/v1") || strings.Contains(msg, "/settings/calendar") {
		t.Errorf("error message = %q, want it to name the failed endpoints only", msg)
	}
	data := env["data"].(map[string]any)
	if data["app_version"] != "external-drive-x@1.0.0" {
		t.Errorf("app_version = %v", data["app_version"])
	}
	res := data["results"].([]any)
	first := res[0].(map[string]any)
	if first["status"] != float64(403) || first["code"] != float64(9100) || len(res) != 3 {
		t.Errorf("results = %v", res)
	}
	if m, _ := filepath.Glob(filepath.Join(rec, "external-drive-x_1.0.0-*.json")); len(m) != 2 {
		t.Errorf("recordings = %v, want 2 (list, settings)", m)
	}
}

func TestProbeWithoutSavedSessionIsMustAct(t *testing.T) {
	t.Setenv("PCB_LAB_BASEURL", "http://127.0.0.1:1")
	code, env := run(t, "probe", "calendar-access", "--state-dir", t.TempDir())
	if code != exitcode.MustAct || env["ok"] != false {
		t.Fatalf("code %d, envelope %v", code, env)
	}
}

func TestProbeRefreshReportsRotationAndOldToken(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	dir := labEnv(t, f, "")
	code, env := run(t, "probe", "refresh", "--state-dir", dir)
	if code != exitcode.OK {
		t.Fatalf("code %d, envelope %v", code, env)
	}
	d := env["data"].(map[string]any)
	if d["refresh_rotated"] != true || d["old_token_retried"] != true || d["old_token_works"] != false {
		t.Errorf("data = %v", d)
	}
}

func TestProbeAliveAppendsASampleAndNeverLogsIn(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	dir := labEnv(t, f, "")
	logins := f.AuthCalls
	code, env := run(t, "probe", "alive", "--state-dir", dir)
	if code != exitcode.OK || env["data"].(map[string]any)["ok"] != true {
		t.Fatalf("code %d, envelope %v", code, env)
	}
	b, err := os.ReadFile(filepath.Join(dir, "e01.jsonl"))
	if err != nil || !bytes.Contains(b, []byte(`"ok":true`)) {
		t.Fatalf("e01.jsonl = %q, %v", b, err)
	}
	if f.AuthCalls != logins {
		t.Error("alive logged in")
	}
}

// The timer is the E01 long probe: every 6 h, catching up missed runs, running
// only `probe alive` on its own dedicated state dir (R15), never the lab's.
func TestTimerUnitsMatchTheProtocol(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "e01-timer", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	timer, svc := read("pcb-lab-e01.timer"), read("pcb-lab-e01.service")
	for _, want := range []string{"OnCalendar=*-*-* 00/6:00:00", "Persistent=true", "Unit=pcb-lab-e01.service"} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer lacks %q", want)
		}
	}
	var exec []string
	for _, l := range strings.Split(svc, "\n") {
		if strings.HasPrefix(l, "ExecStart") {
			exec = append(exec, l)
		}
	}
	if len(exec) != 1 || exec[0] != "ExecStart=%h/.local/bin/pcb-lab probe alive --state-dir %S/pcb-lab-e01" {
		t.Errorf("service must run only `probe alive`, got %q", exec)
	}
}

func TestProbeCalendarAccessSucceedsWhenAllEndpointsAnswer(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{Replies: map[string]labtest.Reply{
		"GET /calendar/v1":              labtest.R(200, `{"Code":1000,"Calendars":[{"ID":"c1"}]}`),
		"GET /calendar/v1/c1/bootstrap": labtest.R(200, `{"Code":1000}`),
		"GET /settings/calendar":        labtest.R(200, `{"Code":1000}`),
	}})
	dir := labEnv(t, f, "")
	if code, env := run(t, "probe", "calendar-access", "--state-dir", dir); code != exitcode.OK || env["ok"] != true {
		t.Fatalf("code %d, envelope %v", code, env)
	}
}

func TestProbeRefreshFailureIsBlockedWithoutTokens(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	dir := labEnv(t, f, "")
	f.Mu.Lock()
	f.Replies = map[string]labtest.Reply{"POST /auth/v4/refresh": labtest.R(422, `{"Code":10013,"Error":"Invalid refresh token","AccessToken":"LEAKY-ACCESS-TOKEN-VALUE"}`)}
	f.Mu.Unlock()
	code, env := run(t, "probe", "refresh", "--state-dir", dir)
	if code != exitcode.Blocked || env["ok"] != false {
		t.Fatalf("code %d, envelope %v; a refused refresh must exit non-zero", code, env)
	}
	b, _ := json.Marshal(env)
	if !strings.Contains(string(b), "10013") || strings.Contains(string(b), "LEAKY") {
		t.Errorf("envelope %s: want the Proton code and no token", b)
	}
}

func TestRawCallsIdentifyAsPcbLab(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	dir := labEnv(t, f, "")
	run(t, "probe", "alive", "--state-dir", dir)
	run(t, "probe", "refresh", "--state-dir", dir)
	f.Mu.Lock()
	defer f.Mu.Unlock()
	n := 0
	for _, s := range f.Log {
		if s.Path == "/core/v4/users" || s.Path == "/auth/v4/refresh" {
			n++
			if !strings.HasPrefix(s.UserAgent, "pcb-lab/") {
				t.Errorf("%s %s sent User-Agent %q, want pcb-lab/<version>", s.Method, s.Path, s.UserAgent)
			}
		}
	}
	if n < 3 {
		t.Fatalf("only %d raw calls seen", n)
	}
}

func TestUsageContractOfEveryLabCommand(t *testing.T) {
	clitest.CheckUsageContract(t, newRoot)
}

func TestNestedCommandNameAgreesOnSuccessAndFailure(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	dir := labEnv(t, f, "external-drive-x@1.0.0")
	_, ok := run(t, "probe", "alive", "--state-dir", dir)
	if ok["command"] != "probe alive" {
		t.Errorf("success command = %v, want %q", ok["command"], "probe alive")
	}
	code, failed := run(t, "probe", "alive", "--state-dir", t.TempDir())
	if code == exitcode.OK || failed["command"] != "probe alive" {
		t.Errorf("failure: code %d command %v, want a failure named %q", code, failed["command"], "probe alive")
	}
}

func TestWaitForEditsIgnoresEOFAndEndsOnALine(t *testing.T) {
	const wait = 150 * time.Millisecond
	start := time.Now()
	if err := waitForEdits(t.Context(), strings.NewReader(""), wait, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < wait {
		t.Errorf("empty stdin cut the wait short after %s, want the full %s", d, wait)
	}
	start = time.Now()
	if err := waitForEdits(t.Context(), strings.NewReader("\n"), time.Minute, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("a line on stdin did not end the wait (%s)", d)
	}
}
