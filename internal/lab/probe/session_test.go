package probe_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

func count(f *labtest.Front, method, path string) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	n := 0
	for _, s := range f.Log {
		if s.Method == method && s.Path == path {
			n++
		}
	}
	return n
}

func TestRefreshRotatesAndOldRefreshTokenIsRefused(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := loggedIn(t, f, "")
	logins := f.AuthCalls

	res, err := probe.Refresh(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RefreshRotated || !res.AccessRotated {
		t.Errorf("rotated: refresh %v access %v, want both true", res.RefreshRotated, res.AccessRotated)
	}
	if !res.Retried || res.OldTokenWorks || res.Retry.Status == 200 {
		t.Errorf("old token: retried %v works %v status %d, want retried, refused", res.Retried, res.OldTokenWorks, res.Retry.Status)
	}
	if f.AuthCalls != logins {
		t.Error("the refresh probe logged in")
	}
	if n := count(f, "POST", "/auth/v4/refresh"); n != 2 {
		t.Errorf("refresh calls = %d, want exactly 2 (one refresh, one retry)", n)
	}
	// The saved pair is the new one: the session still resumes.
	s, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatalf("session unusable after the probe: %v", err)
	}
	s.Close()
}

func TestRefreshReportsExpiresInAndAcceptedOldToken(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := loggedIn(t, f, "")
	f.Mu.Lock()
	f.Replies = map[string]labtest.Reply{"POST /auth/v4/refresh": labtest.R(200,
		`{"Code":1000,"UID":"u","AccessToken":"ACC-NEW","RefreshToken":"REF-NEW","ExpiresIn":3600}`)}
	f.Mu.Unlock()

	res, err := probe.Refresh(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want 3600", res.ExpiresIn)
	}
	if !res.OldTokenWorks {
		t.Error("OldTokenWorks = false although the retry was answered 200")
	}
	b, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	if !bytes.Contains(b, []byte("REF-NEW")) {
		t.Errorf("session.json does not hold the newest refresh token: %s", b)
	}
}

func TestRefreshFailureSkipsTheRetry(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := loggedIn(t, f, "")
	f.Mu.Lock()
	f.Replies = map[string]labtest.Reply{"POST /auth/v4/refresh": labtest.R(422, `{"Code":10013,"Error":"Invalid refresh token"}`)}
	f.Mu.Unlock()
	res, err := probe.Refresh(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	n := count(f, "POST", "/auth/v4/refresh")
	if res.Retried || res.First.Code != 10013 || n != 1 {
		t.Errorf("retried %v, code %d, calls %d; want no retry after a failed refresh", res.Retried, res.First.Code, n)
	}
}

func TestRefreshRecordsWithoutTokens(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := loggedIn(t, f, "")
	before, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	var st struct{ AccessToken, RefreshToken string }
	_ = json.Unmarshal(before, &st)
	dir := t.TempDir()
	o.Recorder = &record.Recorder{Dir: dir}
	if _, err := probe.Refresh(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 2 {
		t.Fatalf("recorded %v, want refresh and refresh-old-token", files)
	}
	after, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	var st2 struct{ AccessToken, RefreshToken string }
	_ = json.Unmarshal(after, &st2)
	for _, p := range files {
		b, _ := os.ReadFile(p)
		for _, secret := range []string{st.AccessToken, st.RefreshToken, st2.AccessToken, st2.RefreshToken} {
			if secret != "" && strings.Contains(string(b), secret) {
				t.Errorf("%s contains a token", p)
			}
		}
	}
}

func readLog(t *testing.T, o session.Options) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(o.StateDir, "e01.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestAliveAppendsOneLinePerRunWithoutRefreshing(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := loggedIn(t, f, "")
	for i := range 2 {
		o.Now = func() time.Time { return t0.Add(time.Duration(i) * 6 * time.Hour) }
		r, err := probe.Alive(t.Context(), o)
		if err != nil {
			t.Fatal(err)
		}
		if !r.OK || r.Code != 1000 || r.Over {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
	lines := readLog(t, o)
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}
	if lines[0]["at"] != "2026-11-11T10:00:00Z" || lines[1]["at"] != "2026-11-11T16:00:00Z" ||
		lines[0]["ok"] != true || lines[0]["code"] != float64(1000) {
		t.Errorf("lines = %v", lines)
	}
	if n := count(f, "POST", "/auth/v4/refresh"); n != 0 {
		t.Errorf("alive refreshed %d times, want none", n)
	}
}

func TestAliveRecordsAnExpiredSession(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := loggedIn(t, f, "")
	f.Mu.Lock()
	f.Replies = map[string]labtest.Reply{"GET /core/v4/users": labtest.R(401, `{"Code":401,"Error":"Invalid access token"}`)}
	f.Mu.Unlock()
	r, err := probe.Alive(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Code != 401 {
		t.Fatalf("result %+v, want not ok, code 401", r)
	}
	if l := readLog(t, o); len(l) != 1 || l[0]["ok"] != false || l[0]["code"] != float64(401) {
		t.Errorf("log = %v", l)
	}
}

func TestAliveStopsAfterTheFourteenDayWindow(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := loggedIn(t, f, "")
	if _, err := probe.Alive(t.Context(), o); err != nil { // first sample at t0
		t.Fatal(err)
	}
	calls := f.Requests

	o.Now = func() time.Time { return t0.Add(probe.AliveWindow - time.Minute) }
	if r, err := probe.Alive(t.Context(), o); err != nil || r.Over {
		t.Fatalf("inside the window: %+v, %v", r, err)
	}
	o.Now = func() time.Time { return t0.Add(probe.AliveWindow + time.Second) }
	r, err := probe.Alive(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Over {
		t.Fatal("Over = false after 14 days")
	}
	if got := f.Requests; got != calls+1 { // only the in-window sample
		t.Errorf("requests = %d, want %d: no call after the window", got, calls+1)
	}
	if l := readLog(t, o); len(l) != 2 {
		t.Errorf("log has %d lines, want 2 (nothing appended after the window)", len(l))
	}
}
