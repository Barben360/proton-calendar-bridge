package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// testDSN is a crash report address no test reaches: a build that has one.
const testDSN = "https://key@reports.example/7"

const notice = "pcb kept a crash report: pcb crash-reports show"

// reportStore is the rig's crash report store.
func reportStore(r *rig) report.Store {
	return report.Store{StateDir: r.dirs.State, Env: func(k string) string { return r.env[k] }, Now: r.now}
}

// keepReports saves n reports of distinct causes in the rig's state
// directory, as a crash or an unexpected error would, and returns their ids.
func keepReports(t *testing.T, r *rig, n int) []string {
	t.Helper()
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil { // as after the first login
		t.Fatal(err)
	}
	st := reportStore(r)
	var ids []string
	for i := range n {
		frames := []report.Frame{{Function: fmt.Sprintf("%s/internal/cli.step%d", module, i), Filename: "internal/cli/step.go", Lineno: 1, InApp: true}}
		e := report.ErrorEvent(errors.New("boom"), frames, report.Meta{Binary: "pcb", Command: "pcb sync", Version: "v0.1.0"}, r.now().Add(time.Duration(i)*time.Second))
		if err := st.Save(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID())
	}
	return ids
}

func pendingIDs(t *testing.T, r *rig) []string {
	t.Helper()
	pend, err := reportStore(r).Pending()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range pend {
		ids = append(ids, p.ID)
	}
	return ids
}

type crashReportsData struct {
	Mode            string `json:"mode"`
	EnvOverride     string `json:"env_override"`
	Pending         int    `json:"pending"`
	BuildHasAddress bool   `json:"build_has_address"`
}

// crashStatus runs pcb crash-reports status --json and returns its data.
func crashStatus(t *testing.T, r *rig) crashReportsData {
	t.Helper()
	r.want(exitcode.OK, "crash-reports", "status", "--json")
	var d crashReportsData
	if err := json.Unmarshal(r.envelope().Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCrashReportsStatus(t *testing.T) {
	r := newRig(t, nil)
	r.want(exitcode.OK, "crash-reports", "status")
	if got, want := r.out.String(), "crash reports: ask; 0 waiting\nthis build has no crash report address\n"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if d := crashStatus(t, r); d != (crashReportsData{Mode: "ask", Pending: 0}) {
		t.Errorf("no address: %+v", d)
	}

	r.reportDSN = testDSN
	keepReports(t, r, 2)
	r.env["PCB_CRASH_REPORTS"] = "Always"
	r.want(exitcode.OK, "crash-reports", "status")
	if got, want := r.out.String(), "crash reports: always (set by PCB_CRASH_REPORTS); 2 waiting\n"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if d := crashStatus(t, r); d != (crashReportsData{Mode: "always", EnvOverride: "always", Pending: 2, BuildHasAddress: true}) {
		t.Errorf("env always: %+v", d)
	}
}

func TestCrashReportsModes(t *testing.T) {
	r := newRig(t, nil)
	r.reportDSN = testDSN
	keepReports(t, r, 2)
	r.want(exitcode.OK, "crash-reports", "always")
	if got := strings.TrimSpace(r.out.String()); got != "crash reports: always; 2 waiting" {
		t.Errorf("always = %q", got)
	}
	if d := crashStatus(t, r); d.Mode != "always" || d.Pending != 2 {
		t.Errorf("after always: %+v", d)
	}
	r.want(exitcode.OK, "crash-reports", "ask")
	if d := crashStatus(t, r); d.Mode != "ask" || d.Pending != 2 {
		t.Errorf("after ask: %+v", d)
	}
	r.want(exitcode.OK, "crash-reports", "never", "--json")
	var d crashReportsData
	if err := json.Unmarshal(r.envelope().Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Mode != "never" || d.Pending != 0 || len(pendingIDs(t, r)) != 0 {
		t.Errorf("never keeps reports: %+v, %v", d, pendingIDs(t, r))
	}
	if got := reportStore(r).Mode(); got != report.Never {
		t.Errorf("mode kept = %q, want never", got)
	}
}

func TestCrashReportsModeMakesTheStateDirectory(t *testing.T) {
	r := newRig(t, nil)
	r.want(exitcode.OK, "crash-reports", "always")
	if got := reportStore(r).Mode(); got != report.Always {
		t.Errorf("mode kept = %q, want always", got)
	}
}

func TestCrashReportsRefusedAsRootOnUserState(t *testing.T) {
	if !onUnixInstall() {
		t.Skip("the root check is Unix's")
	}
	r := newRig(t, nil)
	r.reportDSN = testDSN
	ids := keepReports(t, r, 1)
	r.asRoot = true
	for _, verb := range []string{"always", "never", "discard", "send"} {
		if code := r.run("crash-reports", verb, "--json"); code != exitcode.MustAct {
			t.Errorf("%s as root: exit %d, want %d", verb, code, exitcode.MustAct)
		}
	}
	r.asRoot = false
	if got := pendingIDs(t, r); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("pending after refused verbs = %v, want %v", got, ids)
	}
	if got := reportStore(r).Mode(); got != report.Ask {
		t.Errorf("mode = %q after refused verbs, want ask", got)
	}
}

func TestCrashReportsShow(t *testing.T) {
	r := newRig(t, nil)
	r.want(exitcode.OK, "crash-reports", "show")
	if got := strings.TrimSpace(r.out.String()); got != "no crash report waiting" {
		t.Errorf("show with none = %q", got)
	}
	r.want(exitcode.OK, "crash-reports", "show", "--json")
	if got := strings.TrimSpace(string(r.envelope().Data)); got != "[]" {
		t.Errorf("show --json with none: data = %s, want []", got)
	}

	ids := keepReports(t, r, 2)
	r.want(exitcode.OK, "crash-reports", "show")
	out := r.out.String()
	for _, id := range ids {
		if !strings.Contains(out, `  "event_id": "`+id+`"`) {
			t.Errorf("show lacks report %s, pretty: %s", id, out)
		}
	}
	r.want(exitcode.OK, "crash-reports", "show", "--json")
	var events []struct {
		EventID string `json:"event_id"`
		Level   string `json:"level"`
	}
	if err := json.Unmarshal(r.envelope().Data, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].EventID != ids[0] || events[1].EventID != ids[1] || events[0].Level != "error" {
		t.Errorf("show --json = %+v, want the reports %v oldest first", events, ids)
	}
	if got := pendingIDs(t, r); len(got) != 2 {
		t.Errorf("show changed the reports: %v", got)
	}
}

func TestCrashReportsDiscard(t *testing.T) {
	r := newRig(t, nil)
	r.want(exitcode.OK, "crash-reports", "discard") // none: nothing to do, no state directory made
	if _, err := os.Stat(r.dirs.State); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("discard with none made the state directory: %v", err)
	}
	keepReports(t, r, 2)
	r.want(exitcode.OK, "crash-reports", "discard")
	if got := strings.TrimSpace(r.out.String()); got != "2 crash reports discarded\ncrash reports: ask; 0 waiting\nthis build has no crash report address" {
		t.Errorf("discard = %q", got)
	}
	if got := pendingIDs(t, r); len(got) != 0 {
		t.Errorf("pending after discard = %v", got)
	}
}

// reportServer is a Sentry endpoint answering status(n) to its n-th
// request (from 1); it counts them.
func reportServer(t *testing.T, r *rig, status func(n int32) int) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		if req.URL.Path != "/api/7/envelope/" {
			t.Errorf("request to %s", req.URL.Path)
		}
		w.WriteHeader(status(n.Add(1)))
	}))
	t.Cleanup(srv.Close)
	r.reportDSN = "http://key@" + srv.Listener.Addr().String() + "/7"
	return &n
}

type sendData struct {
	crashReportsData
	Results []struct {
		ID    string `json:"id"`
		Sent  bool   `json:"sent"`
		Error string `json:"error"`
	} `json:"results"`
}

func TestCrashReportsSend(t *testing.T) {
	r := newRig(t, nil)
	n := reportServer(t, r, func(int32) int { return http.StatusOK })
	ids := keepReports(t, r, 2)
	r.want(exitcode.OK, "crash-reports", "send", "--json")
	var d sendData
	if err := json.Unmarshal(r.envelope().Data, &d); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 || len(d.Results) != 2 || d.Results[0].ID != ids[0] || !d.Results[0].Sent || !d.Results[1].Sent || d.Pending != 0 {
		t.Errorf("send: %d requests, %+v", n.Load(), d)
	}
	if got := pendingIDs(t, r); len(got) != 0 {
		t.Errorf("pending after send = %v", got)
	}
	r.want(exitcode.OK, "crash-reports", "send")
	if got := strings.TrimSpace(r.out.String()); got != "no crash report waiting" || n.Load() != 2 {
		t.Errorf("send with none = %q, %d requests", got, n.Load())
	}
}

func TestSendPartial(t *testing.T) {
	r := newRig(t, nil)
	reportServer(t, r, func(n int32) int {
		if n == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	ids := keepReports(t, r, 2)
	r.want(exitcode.Partial, "crash-reports", "send", "--json")
	env := r.envelope()
	if env.OK || env.Error == nil || env.Error.Message != "1 of 2 crash reports not sent" {
		t.Errorf("envelope = %+v", env.Error)
	}
	var d sendData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Results) != 2 || d.Results[0].Sent || d.Results[0].Error != "crash reports: HTTP 500" || !d.Results[1].Sent || d.Pending != 1 {
		t.Errorf("results = %+v", d)
	}
	if got := pendingIDs(t, r); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("pending after a partial send = %v, want [%s]", got, ids[0])
	}
	r.want(exitcode.OK, "crash-reports", "send")
	if got := r.out.String(); !strings.Contains(got, ids[0]+": sent") {
		t.Errorf("second send = %q", got)
	}
}

// A stored Retry-After sends nothing: exit 40, and the hint names the
// server's wait as well as the daily limit.
func TestSendDuringRetryAfterNamesTheWait(t *testing.T) {
	r := newRig(t, nil)
	n := reportServer(t, r, func(int32) int { return http.StatusOK })
	keepReports(t, r, 1)
	if err := reportStore(r).Edit(t.Context(), func(st *report.State) { st.RetryAfter = r.now().Add(time.Hour) }); err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.Partial, "crash-reports", "send", "--json")
	e := r.envelope().Error
	if e == nil || !strings.Contains(e.Hint, "Retry-After") || !strings.Contains(e.Hint, "sent a day") {
		t.Errorf("error %+v: want a hint naming Retry-After and the daily limit", e)
	}
	if n.Load() != 0 || len(pendingIDs(t, r)) != 1 {
		t.Errorf("%d requests, pending %v; want none sent, the report kept", n.Load(), pendingIDs(t, r))
	}
}

func TestCrashReportsSendWithoutAddress(t *testing.T) {
	r := newRig(t, nil)
	r.want(exitcode.OK, "crash-reports", "send")
	keepReports(t, r, 1)
	r.want(exitcode.Blocked, "crash-reports", "send", "--json")
	if e := r.envelope().Error; e == nil || e.Message != "this build has no crash report address" {
		t.Errorf("error = %+v", e)
	}
	if got := pendingIDs(t, r); len(got) != 1 {
		t.Errorf("pending = %v, want the report kept", got)
	}
}

func TestCrashReportsBadVerb(t *testing.T) {
	r := newRig(t, nil)
	r.want(exitcode.Usage, "crash-reports", "maybe", "--json")
	if e := r.envelope().Error; e == nil || !strings.Contains(e.Message, `"maybe"`) {
		t.Errorf("error = %+v", e)
	}
}

func TestNoticeOncePerDay(t *testing.T) {
	r := newRig(t, nil)
	r.reportDSN = testDSN
	r.stderrTerm = true
	keepReports(t, r, 1)
	r.want(exitcode.OK, "version")
	if got := r.errb.String(); got != notice+"\n" {
		t.Errorf("first run: stderr = %q, want the notice", got)
	}
	r.want(exitcode.OK, "version")
	if got := r.errb.String(); got != "" {
		t.Errorf("second run the same day: stderr = %q, want nothing", got)
	}
	r.advance(24 * time.Hour)
	r.want(exitcode.OK, "crash-reports", "status")
	if got := r.errb.String(); got != "" {
		t.Errorf("pcb crash-reports: stderr = %q, want nothing", got)
	}
	r.want(exitcode.OK, "version", "--json")
	if got := r.errb.String(); got != "" {
		t.Errorf("--json: stderr = %q, want nothing", got)
	}
	r.stderrTerm = false
	r.want(exitcode.OK, "version")
	if got := r.errb.String(); got != "" {
		t.Errorf("stderr not a terminal: stderr = %q, want nothing", got)
	}
	r.stderrTerm = true
	r.env["PCB_CRASH_REPORTS"] = "always"
	r.want(exitcode.OK, "version")
	if got := r.errb.String(); got != "" {
		t.Errorf("mode always: stderr = %q, want nothing", got)
	}
	delete(r.env, "PCB_CRASH_REPORTS")
	r.want(exitcode.OK, "version")
	if got := r.errb.String(); got != notice+"\n" {
		t.Errorf("next day: stderr = %q, want the notice", got)
	}
}

func TestNoNoticeWithoutReportOrAddress(t *testing.T) {
	r := newRig(t, nil)
	r.stderrTerm = true
	r.reportDSN = testDSN
	r.want(exitcode.OK, "version")
	if got := r.errb.String(); got != "" {
		t.Errorf("no report: stderr = %q", got)
	}
	keepReports(t, r, 1)
	r.reportDSN = ""
	r.want(exitcode.OK, "version")
	if got := r.errb.String(); got != "" {
		t.Errorf("no address: stderr = %q", got)
	}
}

// doctorLine runs pcb doctor --json and returns its exit code and the
// line named name.
func doctorLine(t *testing.T, r *rig, name string) (exitcode.Code, map[string]string) {
	t.Helper()
	code := r.run("doctor", "--json")
	var d struct {
		Results []map[string]string `json:"results"`
	}
	if err := json.Unmarshal(r.envelope().Data, &d); err != nil {
		t.Fatal(err)
	}
	for _, res := range d.Results {
		if res["name"] == name {
			return code, res
		}
	}
	t.Fatalf("no %s line in %v", name, d.Results)
	return 0, nil
}

func TestDoctorCrashReportsLine(t *testing.T) {
	r := newRig(t, nil)
	if _, res := doctorLine(t, r, "crash reports"); res["status"] != "skip" || res["message"] != "this build has no crash report address" {
		t.Errorf("no address: %v", res)
	}
	r.reportDSN = testDSN
	none, res := doctorLine(t, r, "crash reports")
	if res["status"] != "ok" || res["message"] != "none waiting" {
		t.Errorf("none waiting: %v", res)
	}
	keepReports(t, r, 2)
	waiting, res := doctorLine(t, r, "crash reports")
	if res["status"] != "warn" || res["message"] != "2 waiting: pcb crash-reports show, then send or discard" || res["fix"] != "pcb crash-reports show" {
		t.Errorf("waiting: %v", res)
	}
	if waiting != none {
		t.Errorf("exit %d with reports waiting, %d without: a warn must not change it", waiting, none)
	}
	r.env["PCB_CRASH_REPORTS"] = "always"
	if _, res := doctorLine(t, r, "crash reports"); res["status"] != "ok" || res["message"] != "always" {
		t.Errorf("always: %v", res)
	}
	r.env["PCB_CRASH_REPORTS"] = "never"
	if _, res := doctorLine(t, r, "crash reports"); res["status"] != "ok" || res["message"] != "never" {
		t.Errorf("never: %v", res)
	}
}
