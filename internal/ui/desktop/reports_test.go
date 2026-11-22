package desktop

import (
	"context"
	"errors"
	"slices"
	"testing"

	"fyne.io/fyne/v2/test"
)

// fakeReports records the answers the window gives.
type fakeReports struct {
	n       int
	crashed bool
	text    string
	calls   []string
	err     error
}

func (r *fakeReports) Pending() (int, bool) { return r.n, r.crashed }
func (r *fakeReports) Text() string         { return r.text }
func (r *fakeReports) answer(name string) error {
	r.calls = append(r.calls, name)
	if r.err == nil {
		r.n = 0
	}
	return r.err
}
func (r *fakeReports) Send(context.Context) error       { return r.answer("send") }
func (r *fakeReports) AlwaysSend(context.Context) error { return r.answer("always") }
func (r *fakeReports) DontSend(context.Context) error   { return r.answer("dont") }

func reportsFixture(t *testing.T, crashed bool) (*fixture, *fakeReports) {
	t.Helper()
	f := newFixture(t)
	r := &fakeReports{n: 1, crashed: crashed, text: "{\n  \"event_id\": \"abc\"\n}"}
	f.w.SetReports(r)
	f.w.Update(twoAccounts())
	return f, r
}

func TestReportDialogTitleAndButtons(t *testing.T) {
	for _, tc := range []struct {
		crashed bool
		title   string
	}{{true, "pcb stopped unexpectedly"}, {false, "pcb ran into an unexpected error"}} {
		f, _ := reportsFixture(t, tc.crashed)
		f.w.AskReports(true)
		f.wait()
		if !f.has(tc.title) {
			t.Fatalf("no dialog titled %q: %v", tc.title, f.texts())
		}
		if body := "It holds where pcb's code failed: never calendar content, a name, an address, a secret or a path. Nothing is sent until you agree."; !slices.Contains(f.texts(), body) {
			t.Errorf("the dialog does not say %q: %v", body, f.texts())
		}
		for _, b := range []string{"Show report", "Send", "Always send", "Don't send", "Close"} {
			if !f.hasButton(b) {
				t.Errorf("crashed=%v: no %q button: %v", tc.crashed, b, f.texts())
			}
		}
		if !f.w.visible {
			t.Error("the window stays hidden under the dialog")
		}
	}
}

func TestReportDialogAnswers(t *testing.T) {
	for _, tc := range []struct{ button, call string }{{"Send", "send"}, {"Always send", "always"}, {"Don't send", "dont"}} {
		f, r := reportsFixture(t, true)
		f.w.AskReports(true)
		f.tap(tc.button)
		if !slices.Equal(r.calls, []string{tc.call}) {
			t.Errorf("%s: calls %v, want [%s]", tc.button, r.calls, tc.call)
		}
		if f.w.win.Canvas().Overlays().Top() != nil {
			t.Errorf("%s: the dialog stays open", tc.button)
		}
	}
}

func TestReportDialogCloseAnswersNothing(t *testing.T) {
	f, r := reportsFixture(t, true)
	f.w.AskReports(true)
	f.tap("Close")
	if len(r.calls) != 0 || f.w.win.Canvas().Overlays().Top() != nil {
		t.Errorf("calls %v, overlay %v", r.calls, f.w.win.Canvas().Overlays().Top())
	}
}

func TestReportDialogFailureStaysAndSaysWhy(t *testing.T) {
	f, r := reportsFixture(t, true)
	r.err = errors.New("the report server is down")
	f.w.AskReports(true)
	f.tap("Send")
	if !f.has("the report server is down") {
		t.Errorf("the failure is not shown: %v", f.texts())
	}
}

func TestShowReportIsTheExactTextReadOnly(t *testing.T) {
	f, r := reportsFixture(t, true)
	f.w.AskReports(true)
	f.tap("Show report")
	if !f.has(r.text) {
		t.Fatalf("the report text is not shown: %q", f.texts())
	}
	if len(r.calls) != 0 {
		t.Errorf("Show report answered: %v", r.calls)
	}
}

func TestReportPanel(t *testing.T) {
	f, r := reportsFixture(t, false)
	f.w.Show("")
	if f.hasButton("Send") {
		t.Fatal("panel before any report")
	}
	f.w.ShowReportPanel()
	if !f.has("pcb ran into an unexpected error") {
		t.Fatalf("panel missing: %v", f.texts())
	}
	for _, b := range []string{"Show report", "Send", "Always send", "Don't send"} {
		if !f.hasButton(b) {
			t.Errorf("no %q button", b)
		}
	}
	f.tap("Don't send")
	if !slices.Equal(r.calls, []string{"dont"}) || f.hasButton("Send") {
		t.Errorf("calls %v, panel still shown: %v", r.calls, f.texts())
	}
}

func TestReportPanelNeedsAWaitingReport(t *testing.T) {
	f, r := reportsFixture(t, false)
	f.w.Show("")
	r.n = 0
	f.w.ShowReportPanel()
	if f.hasButton("Send") {
		t.Error("panel shown with no report waiting")
	}
}

func TestSettingsCrashReportsSwitch(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("Settings")
	sw := f.toggle("Send crash reports")
	if sw.On {
		t.Fatal("switch on while the mode is ask")
	}
	test.Tap(sw)
	f.wait()
	if !f.set.reportsOn || !slices.Equal(f.set.calls, []string{"crashreports"}) {
		t.Errorf("calls %v, on %v", f.set.calls, f.set.reportsOn)
	}
}

// PCB_CRASH_REPORTS sets the mode: the switch shows it, disabled, says
// why, and stays disabled after another setting's work ends.
func TestSettingsSwitchSetByTheEnvironment(t *testing.T) {
	f := newFixture(t)
	f.set.reportsOn, f.set.reportsBy = true, "PCB_CRASH_REPORTS"
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("Settings")
	sw := f.toggle("Send crash reports")
	if !sw.On || !sw.Disabled() {
		t.Fatalf("switch on %v, disabled %v; want on and disabled", sw.On, sw.Disabled())
	}
	f.line("set by PCB_CRASH_REPORTS")
	test.Tap(sw)
	f.wait()
	if slices.Contains(f.set.calls, "crashreports") || !sw.On {
		t.Errorf("the disabled switch changed the mode: calls %v, on %v", f.set.calls, sw.On)
	}
	test.Tap(f.toggle("Check for updates"))
	f.wait()
	if !sw.Disabled() {
		t.Error("the switch came back enabled after another setting's work")
	}
	test.Tap(sw)
	f.wait()
	if slices.Contains(f.set.calls, "crashreports") {
		t.Errorf("the switch changed the mode after another setting's work: %v", f.set.calls)
	}
}

func TestSettingsHideTheSwitchWithoutAReportAddress(t *testing.T) {
	f := newFixture(t)
	f.set.noReports = true
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("Settings")
	if f.has("Send crash reports") {
		t.Errorf("switch shown in a build without an address: %v", f.texts())
	}
}

// A --background start never raises the window for a waiting report: the
// dialog comes the first time the window is shown.
func TestAskReportsWithoutRaiseWaitsForTheWindow(t *testing.T) {
	f, r := reportsFixture(t, true)
	f.w.AskReports(false)
	f.wait()
	if f.w.visible || f.w.win.Canvas().Overlays().Top() != nil {
		t.Fatal("the window or the dialog came up on a background start")
	}
	f.w.Show("")
	if !f.has("pcb stopped unexpectedly") || !f.hasButton("Always send") {
		t.Fatalf("no dialog when the window is first shown: %v", f.texts())
	}
	f.tap("Close")
	f.w.Show("")
	if f.w.win.Canvas().Overlays().Top() != nil {
		t.Error("the dialog came back at the next Show")
	}
	if len(r.calls) != 0 {
		t.Errorf("answers %v", r.calls)
	}
}

func TestAskReportsSkipsReportsAnsweredMeanwhile(t *testing.T) {
	f, r := reportsFixture(t, true)
	f.w.AskReports(false)
	r.n = 0
	f.w.Show("")
	if f.w.win.Canvas().Overlays().Top() != nil {
		t.Error("a dialog for no report")
	}
}
