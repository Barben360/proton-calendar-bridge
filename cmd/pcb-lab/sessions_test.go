package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

// savedUID is the session UID a lab state dir holds.
func savedUID(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st struct{ UID string }
	if err := json.Unmarshal(b, &st); err != nil || st.UID == "" {
		t.Fatalf("session.json: %v", err)
	}
	return st.UID
}

// otherSession logs a second session of the lab account into its own dir.
func otherSession(t *testing.T, f *labtest.Front) session.Options {
	t.Helper()
	o := session.Options{
		BaseURL: f.URL, StateDir: filepath.Join(t.TempDir(), "other"),
		Creds: session.Creds{Username: labtest.User, Password: labtest.Pass},
		Now:   func() time.Time { return time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC) },
	}
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return o
}

func sessionsOf(t *testing.T, env map[string]any) []map[string]any {
	t.Helper()
	data, _ := env["data"].(map[string]any)
	list, _ := data["sessions"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		out = append(out, s.(map[string]any))
	}
	return out
}

// One rig, in order: the listing, a refused --revoke-others without --yes,
// then the revoke itself.
func TestProbeSessionsListsThenRevokesOthersNeverItself(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	dir := labEnv(t, f, "")
	other := otherSession(t, f)

	code, env := run(t, "probe", "sessions", "--state-dir", dir)
	if code != exitcode.OK {
		t.Fatalf("list: code %d, envelope %v", code, env)
	}
	list := sessionsOf(t, env)
	self := 0
	for _, s := range list {
		if s["self"] == true {
			self++
		}
	}
	if len(list) < 2 || self != 1 {
		t.Fatalf("sessions %v: want both sessions, exactly one marked self", list)
	}
	b, _ := json.Marshal(env)
	secrets := append(f.Backend.AccessTokens(), savedUID(t, dir), savedUID(t, other.StateDir))
	for _, s := range secrets {
		if strings.Contains(string(b), s) {
			t.Errorf("the envelope holds a token or a whole session UID: %s", b)
		}
	}
	var out, errb bytes.Buffer
	if c := execute([]string{"probe", "sessions", "--state-dir", dir}, &out, &errb); c != exitcode.OK {
		t.Fatalf("text mode: code %d, %s", c, errb.String())
	}
	if n := strings.Count(out.String(), "created "); n != len(list) {
		t.Errorf("text output %q: want one line per session (%d)", out.String(), len(list))
	}

	if code, env := run(t, "probe", "sessions", "--state-dir", dir, "--revoke-others"); code != exitcode.Usage {
		t.Fatalf("--revoke-others without --yes: code %d, envelope %v; want a usage error", code, env)
	}
	s, err := session.Resume(t.Context(), other)
	if err != nil {
		t.Fatalf("the other session was revoked without --yes: %v", err)
	}
	s.Close()

	code, env = run(t, "probe", "sessions", "--state-dir", dir, "--revoke-others", "--yes")
	if code != exitcode.OK {
		t.Fatalf("revoke: code %d, envelope %v", code, env)
	}
	if _, err := session.Resume(t.Context(), other); err == nil {
		t.Error("the other session survived --revoke-others")
	}
	if code, env := run(t, "whoami", "--state-dir", dir); code != exitcode.OK {
		t.Fatalf("the probe's own session is gone: code %d, %v", code, env)
	}
	for _, s := range sessionsOf(t, env) {
		rev, _ := s["revoke"].(map[string]any)
		switch {
		case s["self"] == true && (rev != nil || s["left"] != "this session"):
			t.Errorf("own session %v: want left as this session, never revoked", s)
		case s["self"] != true && (rev["http_status"] != float64(200) || rev["code"] != float64(1000)):
			t.Errorf("other session %v: want the DELETE's answer, HTTP 200 code 1000", s)
		}
	}
	if d := env["data"].(map[string]any); d["after"] != float64(1) {
		t.Errorf("after = %v, want 1 session left", d["after"])
	}
}

// The revoke line follows what Proton answered, never what the probe hoped
// (ruling L5): a refused DELETE is printed with its status and code, a
// session Proton marks not revocable is left alone.
func TestProbeSessionsReportsWhatProtonAnswers(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	dir := labEnv(t, f, "")
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   exitcode.Code
		line   string
	}{
		{"accepted", 200, `{"Code":1000}`, exitcode.OK, "revoke: HTTP 200, code 1000"},
		{"refused", 403, `{"Code":9101,"Error":"Access token does not have sufficient scope"}`, exitcode.Blocked, "revoke: HTTP 403, code 9101"},
		{"session ended", 401, `{"Code":401,"Error":"Invalid access token"}`, exitcode.Blocked, "revoke: HTTP 401, code 401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := fmt.Sprintf(`{"Code":1000,"Sessions":[
				{"UID":%q,"CreateTime":1794650000,"ClientID":"Other","LocalizedClientName":"","Revocable":1},
				{"UID":"otherrevocable0001","CreateTime":1794640000,"ClientID":"web-calendar","LocalizedClientName":"Proton Calendar","Revocable":1},
				{"UID":"otherfixed0002","CreateTime":1794630000,"ClientID":"web-account","LocalizedClientName":"Proton Account","Revocable":0}]}`, savedUID(t, dir))
			f.Mu.Lock()
			from := len(f.Log)
			f.Replies = map[string]labtest.Reply{
				"GET /auth/v4/sessions":                       labtest.R(200, list),
				"DELETE /auth/v4/sessions/otherrevocable0001": labtest.R(tc.status, tc.body),
			}
			f.Mu.Unlock()
			var out, errb bytes.Buffer
			code := execute([]string{"probe", "sessions", "--state-dir", dir, "--revoke-others", "--yes"}, &out, &errb)
			if code != tc.want {
				t.Fatalf("code %d, want %d; stdout %q stderr %q", code, tc.want, out.String(), errb.String())
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			var revocable, fixed, self string
			for _, l := range lines {
				switch {
				case strings.Contains(l, `"Proton Calendar"`):
					revocable = l
				case strings.Contains(l, `"Proton Account"`):
					fixed = l
				case strings.Contains(l, "this session"):
					self = l
				}
			}
			if strings.Contains(out.String(), "otherrevocable0001") {
				t.Errorf("output %q holds a whole session UID", out.String())
			}
			if !strings.Contains(revocable, tc.line) || !strings.Contains(fixed, "left: not revocable") || self == "" {
				t.Errorf("lines %q: want %q on the revocable one, the fixed one left, ours marked", lines, tc.line)
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			for _, s := range f.Log[from:] {
				if s.Method == "DELETE" && s.Path != "/auth/v4/sessions/otherrevocable0001" {
					t.Errorf("DELETE %s: only the revocable other session may be revoked", s.Path)
				}
			}
		})
	}
}
