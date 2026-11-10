package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

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
	if code != exitcode.OK || env["ok"] != true {
		t.Fatalf("code %d, envelope %v", code, env)
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
