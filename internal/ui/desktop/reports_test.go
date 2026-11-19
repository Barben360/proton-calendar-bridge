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
		f.w.AskReports()
		f.wait()
		if !f.has(tc.title) {
			t.Fatalf("no dialog titled %q: %v", tc.title, f.texts())
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
		f.w.AskReports()
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
	f.w.AskReports()
	f.tap("Close")
	if len(r.calls) != 0 || f.w.win.Canvas().Overlays().Top() != nil {
		t.Errorf("calls %v, overlay %v", r.calls, f.w.win.Canvas().Overlays().Top())
	}
}

func TestReportDialogFailureStaysAndSaysWhy(t *testing.T) {
	f, r := reportsFixture(t, true)
	r.err = errors.New("the report server is down")
	f.w.AskReports()
	f.tap("Send")
	if !f.has("the report server is down") {
		t.Errorf("the failure is not shown: %v", f.texts())
	}
}

func TestShowReportIsTheExactTextReadOnly(t *testing.T) {
	f, r := reportsFixture(t, true)
	f.w.AskReports()
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
