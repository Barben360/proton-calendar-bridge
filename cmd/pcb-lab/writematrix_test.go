package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// matrixEnv is a lab session on a fake with a pcb-lab calendar, and the
// test account's credentials in the environment. The salts are not locked:
// the engine's tests cover the scope elevation.
func matrixEnv(t *testing.T) (*labtest.Front, string, string) {
	t.Helper()
	f := labtest.NewFront(t, &labtest.Front{})
	cal := f.Backend.AddCalendar(labtest.User, protontest.Calendar{Name: "pcb-lab"})
	dir := labEnv(t, f, "")
	t.Setenv("PCB_LAB_USERNAME", labtest.User)
	t.Setenv("PCB_LAB_PASSWORD", labtest.Pass)
	return f, dir, cal
}

var matrixCaseNames = []string{
	"key-packets-on-update", "second-master", "rrule-count-50", "rrule-until-2038", "rrule-bysetpos-5", "rrule-hourly",
	"rrule-interval-1000", "status-tentative", "floating-time", "non-iana-tzid", "duration", "unbumped-time-edit",
	"notifications-absent", "personal-part-left-out",
}

// One rig, in order: bad invocations (nothing sent), a full run in text
// mode, then a run stopped by a 429 in JSON (the engine's own tests check the
// JSON result of full runs).
func TestProbeWriteMatrix(t *testing.T) {
	f, dir, cal := matrixEnv(t)
	usage(t, f, dir, cal)
	var out, errb bytes.Buffer
	if c := execute([]string{"probe", "write-matrix", "--state-dir", dir, "--calendar", cal, "--interval", "0s"}, &out, &errb); c != exitcode.OK {
		t.Fatalf("text mode: code %d, %s", c, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(matrixCaseNames)+3 {
		t.Fatalf("text output: %d lines, want a header, one per case, the cleanup and the count:\n%s", len(lines), out.String())
	}
	for i, name := range matrixCaseNames {
		if !strings.HasPrefix(lines[i+1], name+" (") || !strings.Contains(lines[i+1], "read-back: ") {
			t.Errorf("line %d = %q, want the %s case with its read-back", i+1, lines[i+1], name)
		}
		t.Log(lines[i+1]) // the fake's own reading of each case (the oracle)
	}
	if !strings.HasSuffix(lines[len(lines)-2], "left 0") || strings.Contains(out.String(), labtest.Pass) {
		t.Errorf("cleanup line %q: want nothing left, and no password in the output", lines[len(lines)-2])
	}

	// A failed cleanup is a partial run: Proton answers 500 to the deletes.
	f.Mu.Lock()
	f.Intercept = func(method, path string, body []byte) (labtest.Reply, bool) {
		var req struct {
			Events []struct{ Event json.RawMessage }
		}
		if method != "PUT" || !strings.HasSuffix(path, "/events/sync") || json.Unmarshal(body, &req) != nil {
			return labtest.Reply{}, false
		}
		for _, e := range req.Events {
			if len(e.Event) > 0 {
				return labtest.Reply{}, false
			}
		}
		return labtest.R(500, `{"Code":0,"Error":"injected"}`), true
	}
	f.Mu.Unlock()
	code, env := run(t, "probe", "write-matrix", "--state-dir", dir, "--calendar", cal, "--interval", "0s")
	f.Mu.Lock()
	f.Intercept = nil
	f.Mu.Unlock()
	if code != exitcode.Partial {
		t.Errorf("failed cleanup: code %d, envelope %v; want partial", code, env)
	}
	e, _ := env["error"].(map[string]any)
	d, _ := env["data"].(map[string]any)
	hint, _ := e["hint"].(string)
	cleanup, _ := d["cleanup"].(string)
	if !strings.Contains(hint, "pcb-lab-wm-") || !strings.Contains(cleanup, "HTTP 500") || !strings.Contains(cleanup, "left 10:") || !strings.Contains(cleanup, "-second-master") {
		t.Errorf("failed cleanup: hint %q, cleanup %q; want the prefix hint and the rows left named", hint, cleanup)
	}

	f.Backend.FailNext("PUT", "/calendar/v1/"+cal+"/events/sync", 429, 429, 30)
	code, env = run(t, "probe", "write-matrix", "--state-dir", dir, "--calendar", cal, "--interval", "0s")
	if code != exitcode.Partial {
		t.Fatalf("code %d, envelope %v; want partial after a 429", code, env)
	}
	if hint, _ := env["error"].(map[string]any)["hint"].(string); !strings.Contains(hint, "pcb-lab-wm-") {
		t.Errorf("hint %q: want the UID prefix of what is left", hint)
	}
	b, _ := json.Marshal(env)
	for _, secret := range append(f.Backend.AccessTokens(), labtest.Pass) {
		if strings.Contains(string(b), secret) {
			t.Errorf("the envelope holds a token or the password")
		}
	}
}

// usage: bad invocations exit 2 and send nothing.
func usage(t *testing.T, f *labtest.Front, dir, cal string) {
	t.Helper()
	f.Mu.Lock()
	before := f.Requests
	f.Mu.Unlock()
	for name, args := range map[string][]string{
		"no calendar":  {"--state-dir", dir},
		"no state dir": {"--calendar", cal},
		"record dir":   {"--state-dir", dir, "--calendar", cal, "--record-dir", t.TempDir()},
	} {
		if code, env := run(t, append([]string{"probe", "write-matrix"}, args...)...); code != exitcode.Usage {
			t.Errorf("%s: code %d, envelope %v; want a usage error", name, code, env)
		}
	}
	t.Setenv("PCB_LAB_PASSWORD", "")
	if code, env := run(t, "probe", "write-matrix", "--state-dir", dir, "--calendar", cal); code != exitcode.Usage {
		t.Errorf("no password: code %d, envelope %v; want a usage error", code, env)
	}
	f.Mu.Lock()
	if f.Requests != before {
		t.Errorf("%d requests reached the fake on bad invocations", f.Requests-before)
	}
	f.Mu.Unlock()
	// A wrong password unlocks no key (salts readable, no SRP here): must act, nothing written.
	t.Setenv("PCB_LAB_PASSWORD", "not-the-password")
	if code, env := run(t, "probe", "write-matrix", "--state-dir", dir, "--calendar", cal); code != exitcode.MustAct || len(f.Backend.SyncCalls()) != 0 {
		t.Errorf("wrong password: code %d, envelope %v, %d writes; want must-act and nothing written", code, env, len(f.Backend.SyncCalls()))
	}
	t.Setenv("PCB_LAB_PASSWORD", labtest.Pass)
}

// Only a loopback base URL (a local fake) lifts the request pacing: Proton's
// API is always paced.
func TestLoopbackOnlyForLocalFakes(t *testing.T) {
	for base, want := range map[string]bool{
		"http://127.0.0.1:4321": true, "http://localhost:1": true, "http://[::1]:80": true,
		"": false, "https://mail-api.proton.me": false, "https://127.0.0.1.example.org": false, "http://10.0.0.1": false,
	} {
		if got := loopback(base); got != want {
			t.Errorf("loopback(%q) = %v, want %v", base, got, want)
		}
	}
}
