package desktop

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// fakeActions records the calls; block, when set, holds every call until it
// is closed.
type fakeActions struct {
	mu    sync.Mutex
	calls []string
	pw    string
	err   error
	block chan struct{}
}

func (f *fakeActions) rec(s string) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}

func (f *fakeActions) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeActions) SignOut(_ context.Context, id string) error { return f.rec("signout " + id) }
func (f *fakeActions) Remove(_ context.Context, id string) error  { return f.rec("remove " + id) }
func (f *fakeActions) SetCalDAV(_ context.Context, id string, on bool) error {
	if on {
		return f.rec("caldav " + id + " on")
	}
	return f.rec("caldav " + id + " off")
}
func (f *fakeActions) SetMCP(_ context.Context, id string, on bool) error {
	if on {
		return f.rec("mcp " + id + " on")
	}
	return f.rec("mcp " + id + " off")
}
func (f *fakeActions) Password(_ context.Context, id string) (string, error) {
	return f.pw, f.rec("password " + id)
}
func (f *fakeActions) NewPassword(_ context.Context, id string) (string, error) {
	return "fresh-" + f.pw, f.rec("newpassword " + id)
}
func (f *fakeActions) Restart(context.Context) error { return f.rec("restart") }
func (f *fakeActions) Doctor(context.Context) ([]doctor.Result, error) {
	return []doctor.Result{{Name: "keyring", Status: doctor.StatusWarn, Message: "no keyring", Fix: "install one"}}, f.rec("doctor")
}

type fakeSettings struct {
	calls     []string
	autostart bool
	autoErr   error
	level     slog.Level
}

func (s *fakeSettings) Autostart() bool { return s.autostart }
func (s *fakeSettings) SetAutostart(on bool) error {
	s.calls = append(s.calls, "autostart")
	if s.autoErr != nil {
		return s.autoErr
	}
	s.autostart = on
	return nil
}
func (s *fakeSettings) Level() slog.Level { return s.level }
func (s *fakeSettings) SetLevel(l slog.Level) {
	s.calls = append(s.calls, "level "+l.String())
	s.level = l
}
func (s *fakeSettings) OpenLogs() error   { s.calls = append(s.calls, "logs"); return nil }
func (s *fakeSettings) OpenConfig() error { s.calls = append(s.calls, "config"); return nil }

type fixture struct {
	t    *testing.T
	app  fyne.App
	w    *Window
	act  *fakeActions
	set  *fakeSettings
	wiz  *fakeWizard
	open []string

	qmu   sync.Mutex
	queue []func()
}

func (f *fixture) enqueue(fn func()) {
	f.qmu.Lock()
	f.queue = append(f.queue, fn)
	f.qmu.Unlock()
}

// wait is the main loop of the tests: it runs, on the test's goroutine, what
// the goroutines in flight asked fyne.Do for, until none is left.
func (f *fixture) wait() {
	for {
		f.w.wg.Wait()
		f.qmu.Lock()
		q := f.queue
		f.queue = nil
		f.qmu.Unlock()
		if len(q) == 0 {
			return
		}
		for _, fn := range q {
			fn()
		}
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, app: test.NewApp(), act: &fakeActions{pw: "s3cret-pw"}, set: &fakeSettings{}, wiz: &fakeWizard{}}
	t.Cleanup(func() { test.NewApp() })
	f.w = NewWindow(f.app, f.act, f.set, func() WizardModel { return f.wiz }, func(u *url.URL) error {
		f.open = append(f.open, u.String())
		return nil
	})
	f.w.dispatch = f.enqueue
	return f
}

func twoAccounts() ui.View {
	return ui.View{Tray: ui.TrayAttention, Accounts: []ui.AccountView{
		{ID: "a1", Address: "alice@example.org", Short: "synced 1 min ago", Dot: ui.TrayOK, SignedIn: true, CalDAV: true, MCP: true,
			URL: "http://127.0.0.1:8080/", Username: "alice@example.org",
			Calendars: []ui.CalendarView{{Name: "Personal"}, {Name: "Shared", ReadOnly: true, Badge: "read-only: ambiguous"}},
			Alerts:    []ctl.Alert{{Code: "sync-stalled", Severity: "error", Message: "sync is stalled", Fix: "restart the bridge"}}},
		{ID: "b2", Address: "bob@example.org", Short: "needs login", Dot: ui.TrayAttention, SignedIn: false, CalDAV: false, MCP: false},
	}}
}

// walk visits the visible objects under root, widgets' parts included.
func walk(root fyne.CanvasObject, fn func(fyne.CanvasObject)) {
	if root == nil || !root.Visible() {
		return
	}
	fn(root)
	switch o := root.(type) {
	case *fyne.Container:
		for _, c := range o.Objects {
			walk(c, fn)
		}
	case fyne.Widget:
		for _, c := range test.WidgetRenderer(o).Objects() {
			walk(c, fn)
		}
	}
}

// roots are what the person sees: the top overlay when there is one, else
// the window's content.
func (f *fixture) roots() fyne.CanvasObject {
	c := f.w.win.Canvas()
	if top := c.Overlays().Top(); top != nil {
		return top
	}
	return c.Content()
}

func (f *fixture) button(text string) *widget.Button {
	f.t.Helper()
	var found *widget.Button
	walk(f.roots(), func(o fyne.CanvasObject) {
		if b, ok := o.(*widget.Button); ok && b.Text == text && found == nil {
			found = b
		}
	})
	if found == nil {
		f.t.Fatalf("no visible button %q; visible texts: %v", text, f.texts())
	}
	return found
}

func (f *fixture) hasButton(text string) bool {
	found := false
	walk(f.roots(), func(o fyne.CanvasObject) {
		if b, ok := o.(*widget.Button); ok && b.Text == text {
			found = true
		}
	})
	return found
}

func (f *fixture) texts() []string {
	var out []string
	walk(f.roots(), func(o fyne.CanvasObject) {
		switch v := o.(type) {
		case *widget.Label:
			out = append(out, v.Text)
		case *widget.Button:
			out = append(out, "["+v.Text+"]")
		case *widget.Check:
			out = append(out, "(check "+v.Text+")")
		case *toggle:
			out = append(out, "(toggle "+v.Label+")")
		case *textLine:
			out = append(out, v.Text)
		}
	})
	return out
}

// visible tells whether o is drawn (it and every parent shown).
func (f *fixture) visible(o fyne.CanvasObject) bool {
	found := false
	walk(f.roots(), func(c fyne.CanvasObject) {
		if c == o {
			found = true
		}
	})
	return found
}

func (f *fixture) toggle(label string) *toggle {
	f.t.Helper()
	var found *toggle
	walk(f.roots(), func(o fyne.CanvasObject) {
		if c, ok := o.(*toggle); ok && c.Label == label && found == nil {
			found = c
		}
	})
	if found == nil {
		f.t.Fatalf("no visible toggle %q; visible: %v", label, f.texts())
	}
	return found
}

// line is the visible text line whose text is exactly text.
func (f *fixture) line(text string) *textLine {
	f.t.Helper()
	var found *textLine
	walk(f.roots(), func(o fyne.CanvasObject) {
		if l, ok := o.(*textLine); ok && l.Text == text && found == nil {
			found = l
		}
	})
	if found == nil {
		f.t.Fatalf("no visible line %q; visible: %v", text, f.texts())
	}
	return found
}

// tapAdd taps the sidebar's + (add an account).
func (f *fixture) tapAdd() {
	f.t.Helper()
	if !f.visible(f.w.addBtn) {
		f.t.Fatalf("the add-account button is not visible: %v", f.texts())
	}
	test.Tap(f.w.addBtn)
	f.wait()
}

func (f *fixture) has(sub string) bool {
	for _, s := range f.texts() {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (f *fixture) check(text string) *widget.Check {
	f.t.Helper()
	var found *widget.Check
	walk(f.roots(), func(o fyne.CanvasObject) {
		if c, ok := o.(*widget.Check); ok && c.Text == text && found == nil {
			found = c
		}
	})
	if found == nil {
		f.t.Fatalf("no visible check %q; visible: %v", text, f.texts())
	}
	return found
}

func (f *fixture) tap(text string) {
	f.t.Helper()
	test.Tap(f.button(text))
	f.wait()
}

func (f *fixture) entry(placeholder string) *widget.Entry {
	f.t.Helper()
	var found *widget.Entry
	walk(f.roots(), func(o fyne.CanvasObject) {
		if e, ok := o.(*widget.Entry); ok && e.PlaceHolder == placeholder && found == nil {
			found = e
		}
	})
	if found == nil {
		f.t.Fatalf("no visible entry %q", placeholder)
	}
	return found
}

func TestAccountListRendersView(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	if len(f.w.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(f.w.rows))
	}
	for i, want := range []struct {
		addr string
		dot  ui.TrayState
	}{{"alice@example.org", ui.TrayOK}, {"bob@example.org", ui.TrayAttention}} {
		r := f.w.rows[i]
		if r.addr.Text != want.addr {
			t.Errorf("row %d text = %q, want %q", i, r.addr.Text, want.addr)
		}
		if r.dot.FillColor != dotColor(want.dot) {
			t.Errorf("row %d dot colour = %v, want %v", i, r.dot.FillColor, dotColor(want.dot))
		}
	}
	if dotColor(ui.TrayOK) == dotColor(ui.TrayAttention) {
		t.Error("ok and attention dots share a colour")
	}
	f.w.Show("")
	for _, s := range []string{"alice@example.org", "synced 1 min ago", "Personal", "Shared", "read-only: ambiguous", "sync is stalled", "restart the bridge", "http://127.0.0.1:8080/"} {
		if !f.has(s) {
			t.Errorf("selected account pane lacks %q; visible: %v", s, f.texts())
		}
	}
	if !f.visible(f.w.addBtn) || f.w.addBtn.Icon == nil {
		t.Error("no + (add account) in the sidebar")
	}
	test.Tap(f.w.rows[1].btn)
	if !f.has("needs login") || f.has("Personal") {
		t.Errorf("tapping the second row did not show bob's pane: %v", f.texts())
	}
}

// Paper design: each row has an avatar with the address's initial and a
// status line; only the selected row is highlighted.
func TestAccountRowsAvatarAndSelection(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	for i, want := range []struct{ initial, short string }{{"A", "synced 1 min ago"}, {"B", "needs login"}} {
		r := f.w.rows[i]
		if r.initial.Text != want.initial || r.short.Text != want.short {
			t.Errorf("row %d initial=%q short=%q, want %q %q", i, r.initial.Text, r.short.Text, want.initial, want.short)
		}
		if !f.visible(r.initial) || !f.visible(r.short) {
			t.Errorf("row %d avatar or status line not drawn", i)
		}
	}
	if !f.visible(f.w.rows[0].sel) || f.visible(f.w.rows[1].sel) {
		t.Error("the highlight is not on the selected row only")
	}
	test.Tap(f.w.rows[1].btn)
	if f.visible(f.w.rows[0].sel) || !f.visible(f.w.rows[1].sel) {
		t.Error("the highlight did not follow the selection")
	}
	f.tap("Settings")
	if f.visible(f.w.rows[0].sel) || f.visible(f.w.rows[1].sel) {
		t.Error("a row stays highlighted while Settings shows")
	}
}

// Paper design: the account pane opens with the alerts, then the grouped
// boxes in order; a calendar's badge is a warning-coloured badge.
func TestAccountPaneOrderAndBadge(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	at := func(sub string) int {
		for i, s := range f.texts() {
			if strings.Contains(s, sub) {
				return i
			}
		}
		t.Fatalf("%q not visible: %v", sub, f.texts())
		return -1
	}
	order := []string{"alice@example.org", "sync is stalled", "ADAPTERS", "(toggle CalDAV)", "CALDAV CONNECTION", "http://127.0.0.1:8080/", "CALENDARS", "Personal"}
	for i := 1; i < len(order); i++ {
		if at(order[i-1]) >= at(order[i]) {
			t.Errorf("%q is not above %q: %v", order[i-1], order[i], f.texts())
		}
	}
	if l := f.line("read-only: ambiguous"); l.ColorName != theme.ColorNameWarning {
		t.Errorf("badge colour = %v, want warning", l.ColorName)
	}
}

// Paper design: the pane's main action stands out: Sign in for a signed-out
// account; Remove is drawn as a destructive action.
func TestAccountActionsEmphasis(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("b2")
	if f.button("Sign in").Importance != widget.HighImportance {
		t.Error("Sign in is not the highlighted action of a signed-out account")
	}
	if f.button("Remove").Importance != widget.DangerImportance {
		t.Error("Remove is not drawn as destructive")
	}
	f.w.Show("a1")
	if f.button("Sign out").Importance == widget.HighImportance {
		t.Error("Sign out is highlighted")
	}
}

// With CalDAV off the connection box and its title are gone, and so is the
// calendars box of an account without calendars: no empty boxes.
func TestConnectionGroupHiddenWithCalDAVOff(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("b2")
	if f.has("CALDAV CONNECTION") || f.hasButton("New password") {
		t.Errorf("connection group shown for an account with CalDAV off: %v", f.texts())
	}
	if f.has("CALENDARS") {
		t.Errorf("an empty calendars box is shown: %v", f.texts())
	}
}

func TestCalDAVSwitchCallsSetCalDAV(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	sw := f.toggle("CalDAV")
	if !sw.On {
		t.Fatal("CalDAV switch is off for an account with CalDAV on")
	}
	// a failure puts the switch back and says why.
	f.act.err = &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("bridge busy"), Hint: "try later"}
	test.Tap(sw)
	f.wait()
	if !f.w.pane.caldav.On {
		t.Error("the switch stayed off after the call failed")
	}
	if !f.has("bridge busy") || !f.has("try later") {
		t.Errorf("error dialog lacks the message and the hint: %v", f.texts())
	}
	f.tap("OK")
	f.act.err = nil
	test.Tap(f.toggle("CalDAV"))
	f.wait()
	if got := f.act.got(); !slices.Equal(got, []string{"caldav a1 off", "caldav a1 off"}) {
		t.Fatalf("calls = %v", got)
	}
}

// Design §5.5b: the MCP switch beside the CalDAV one, with the same
// behaviour: it shows the account's switch, a failure puts it back and
// says why, a tap calls SetMCP and leaves CalDAV alone.
func TestMCPSwitchCallsSetMCP(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	sw := f.toggle("MCP")
	if !sw.On {
		t.Fatal("MCP switch is off for an account with MCP on")
	}
	f.act.err = &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("bridge busy"), Hint: "try later"}
	test.Tap(sw)
	f.wait()
	if !f.w.pane.mcp.On {
		t.Error("the switch stayed off after the call failed")
	}
	if !f.has("bridge busy") || !f.has("try later") {
		t.Errorf("error dialog lacks the message and the hint: %v", f.texts())
	}
	f.tap("OK")
	f.act.err = nil
	test.Tap(f.toggle("MCP"))
	f.wait()
	if got := f.act.got(); !slices.Equal(got, []string{"mcp a1 off", "mcp a1 off"}) {
		t.Fatalf("calls = %v", got)
	}
	if !f.w.pane.caldav.On {
		t.Error("the MCP switch turned the CalDAV switch off")
	}
	test.Tap(f.w.rows[1].btn)
	if f.w.pane.mcp.On {
		t.Error("b2's MCP switch is on for an account with MCP off")
	}
}

func TestRemoveAsksThenCalls(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.tap("Remove")
	for _, s := range []string{"alice@example.org", "cache", "CalDAV passwords"} {
		if !f.has(s) {
			t.Errorf("confirmation lacks %q: %v", s, f.texts())
		}
	}
	f.tap("Cancel")
	if got := f.act.got(); len(got) != 0 {
		t.Fatalf("cancel still called: %v", got)
	}
	f.tap("Remove")
	f.tap("Remove account")
	if got := f.act.got(); !slices.Equal(got, []string{"remove a1"}) {
		t.Fatalf("calls = %v, want [remove a1]", got)
	}
}

func TestShowRevealsAndCopyCopies(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	if got := f.w.pane.pw.label.Text; got != mask {
		t.Fatalf("password shown before Show: %q", got)
	}
	f.tap("Show")
	if got := f.w.pane.pw.label.Text; got != "s3cret-pw" {
		t.Fatalf("after Show the password label = %q", got)
	}
	f.w.pane.pw.hide()
	f.tap("Copy")
	if got := f.app.Clipboard().Content(); got != "s3cret-pw" {
		t.Fatalf("clipboard = %q, want the password", got)
	}
	if got := f.w.pane.pw.label.Text; got != mask {
		t.Errorf("Copy revealed the password: %q", got)
	}
}

func TestShownPasswordHidesOnAccountChange(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.tap("Show")
	if f.w.pane.pw.label.Text == mask {
		t.Fatal("setup: password not shown")
	}
	test.Tap(f.w.rows[1].btn)
	if got := f.w.pane.pw.label.Text; got != mask {
		t.Fatalf("password still displayed after changing account: %q", got)
	}
	test.Tap(f.w.rows[0].btn)
	if got := f.w.pane.pw.label.Text; got != mask {
		t.Fatalf("password back after returning to the account: %q", got)
	}
}

func TestShownPasswordHidesOnHide(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.tap("Show")
	f.w.Hide()
	if got := f.w.pane.pw.label.Text; got != mask {
		t.Fatalf("password still displayed after Hide: %q", got)
	}
	if f.w.visible {
		t.Error("window still visible after Hide")
	}
}

func TestPasswordArrivingAfterPaneChangeStaysHidden(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.act.block = make(chan struct{})
	test.Tap(f.button("Show"))
	test.Tap(f.w.rows[1].btn) // the pane changes while the call is out
	close(f.act.block)
	f.wait()
	if got := f.w.pane.pw.label.Text; got != mask {
		t.Fatalf("a late password was displayed: %q", got)
	}
}

func TestNewPasswordAsks(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.tap("New password")
	if !f.has("the old password stops working at once") {
		t.Errorf("confirmation lacks the warning: %v", f.texts())
	}
	f.tap("Cancel")
	if got := f.act.got(); len(got) != 0 {
		t.Fatalf("cancel still called: %v", got)
	}
	f.tap("New password")
	f.tap("Replace password")
	if got := f.act.got(); !slices.Equal(got, []string{"newpassword a1"}) {
		t.Fatalf("calls = %v", got)
	}
	if got := f.w.pane.pw.label.Text; got != "fresh-s3cret-pw" {
		t.Errorf("the new password is not shown: %q", got)
	}
}

func TestSignedOutShowsSignIn(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("b2")
	if f.hasButton("Sign out") || !f.hasButton("Sign in") {
		t.Fatalf("signed-out pane buttons: %v", f.texts())
	}
	f.tap("Sign in")
	if got := f.entry("name@example.org").Text; got != "bob@example.org" {
		t.Errorf("wizard address = %q, want the account's", got)
	}
	f.w.Show("a1")
	if !f.hasButton("Sign out") || f.hasButton("Sign in") {
		t.Fatalf("signed-in pane buttons: %v", f.texts())
	}
	f.tap("Sign out")
	if got := f.act.got(); !slices.Equal(got, []string{"signout a1"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestBusyDisablesButtonsAndShowsWorking(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.act.block = make(chan struct{})
	test.Tap(f.button("Sign out"))
	if !f.has("Working…") {
		t.Errorf("no Working… while a call runs: %v", f.texts())
	}
	if !f.button("Remove").Disabled() || !f.button("Sign out").Disabled() {
		t.Error("buttons stay enabled while a call runs")
	}
	close(f.act.block)
	f.wait()
	if f.has("Working…") || f.button("Remove").Disabled() {
		t.Errorf("still busy after the call: %v", f.texts())
	}
}

func TestSettingsCallsAndListenerShown(t *testing.T) {
	f := newFixture(t)
	v := twoAccounts()
	v.Listener = &ctl.Listener{Addr: "127.0.0.1:8443", URL: "https://127.0.0.1:8443/", TLS: true}
	f.w.Update(v)
	f.w.Show("")
	f.tap("Settings")
	if !f.has("127.0.0.1:8443") || f.w.settings.tls.Text != "on" || !f.visible(f.w.settings.tls) {
		t.Errorf("listener not shown: %v", f.texts())
	}
	if !f.has("set [log] level in config.toml to keep it") {
		t.Errorf("log level hint missing: %v", f.texts())
	}
	test.Tap(f.toggle("Start at login"))
	f.wait()
	var sel *widget.Select
	walk(f.roots(), func(o fyne.CanvasObject) {
		if s, ok := o.(*widget.Select); ok {
			sel = s
		}
	})
	if sel == nil {
		t.Fatal("no log level select")
	}
	sel.SetSelected("debug")
	f.tap("Open logs folder")
	f.tap("Open config.toml")
	f.tap("Restart bridge")
	f.tap("Doctor report")
	if want := []string{"autostart", "level DEBUG", "logs", "config"}; !slices.Equal(f.set.calls, want) {
		t.Errorf("settings calls = %v, want %v", f.set.calls, want)
	}
	if !f.set.autostart {
		t.Error("autostart not switched on")
	}
	if got := f.act.got(); !slices.Equal(got, []string{"restart", "doctor"}) {
		t.Errorf("actions calls = %v", got)
	}
	if !f.has("keyring") || !f.has("install one") {
		t.Errorf("doctor report not shown: %v", f.texts())
	}
	f.tap("Close")
	v.Listener = &ctl.Listener{Addr: "127.0.0.1:8080", URL: "http://127.0.0.1:8080/"}
	f.w.Update(v)
	if f.w.settings.tls.Text != "off" || !f.visible(f.w.settings.tls) {
		t.Errorf("TLS off not shown: %v", f.texts())
	}
}

func TestCloseHides(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	if !f.w.visible {
		t.Fatal("setup: not visible")
	}
	// Fyne's test window has no public way to trigger or read the intercept:
	// reflection only checks that one is installed (no unsafe), and onClose
	// is what it runs.
	rv := reflect.ValueOf(f.w.win).Elem().FieldByName("onCloseIntercepted")
	if !rv.IsValid() || rv.IsNil() {
		t.Fatal("no close intercept registered on the window")
	}
	f.w.onClose()
	if f.w.visible {
		t.Error("closing left the window visible")
	}
	if !slices.Contains(f.app.Driver().AllWindows(), f.w.win) {
		t.Error("closing destroyed the window instead of hiding it")
	}
}

func TestWindowSizeAndNoBranding(t *testing.T) {
	f := newFixture(t)
	if got := f.w.win.Canvas().Size(); got.Width != 880 || got.Height != 620 {
		t.Errorf("size = %v, want 880x620", got)
	}
	if strings.Contains(strings.ToLower(f.w.win.Title()), "proton") {
		t.Errorf("title %q carries the Proton name", f.w.win.Title())
	}
}

func TestBridgeStoppedBanner(t *testing.T) {
	f := newFixture(t)
	f.w.Update(ui.View{Tray: ui.TrayAttention, Bridge: "bridge stopped: port in use", Accounts: []ui.AccountView{}})
	if !f.has("bridge stopped: port in use") {
		t.Errorf("banner missing: %v", f.texts())
	}
}

// A stopped bridge (a failed Restart) has no accounts to show: the pane
// says the bridge is stopped and where to restart it, never "Add an
// account" (review I1).
func TestBridgeStoppedPaneDoesNotOfferAnAdd(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.w.Update(ui.View{Tray: ui.TrayAttention, Bridge: "bridge stopped: log.level must be one of debug, info, warn, error", Accounts: []ui.AccountView{}})
	if f.has("Add an account to start.") {
		t.Errorf("a stopped bridge's pane offers an add: %v", f.texts())
	}
	if !f.has("Settings → Restart bridge") {
		t.Errorf("the pane does not say where to restart the bridge: %v", f.texts())
	}
	f.w.Update(ui.View{Accounts: []ui.AccountView{}})
	if !f.has("Add an account to start.") {
		t.Errorf("a running bridge with no account does not offer an add: %v", f.texts())
	}
}

func TestCalDAVFailureRevertsOnlyTheSameAccount(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts()) // a1 has CalDAV on, b2 off
	f.w.Show("a1")
	f.act.err = errors.New("bridge busy")
	f.act.block = make(chan struct{})
	test.Tap(f.w.pane.caldav)
	test.Tap(f.w.rows[1].btn) // the pane shows b2 while the call is out
	close(f.act.block)
	f.wait()
	if f.w.pane.caldav.On {
		t.Error("the revert of a1's switch landed on b2's pane")
	}
	test.Tap(f.w.rows[0].btn)
	if !f.w.pane.caldav.On {
		t.Error("a1's switch does not show its CalDAV state")
	}
}

func TestMCPFailureRevertsOnlyTheSameAccount(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts()) // a1 has MCP on, b2 off
	f.w.Show("a1")
	f.act.err = errors.New("bridge busy")
	f.act.block = make(chan struct{})
	test.Tap(f.w.pane.mcp)
	test.Tap(f.w.rows[1].btn) // the pane shows b2 while the call is out
	close(f.act.block)
	f.wait()
	if f.w.pane.mcp.On {
		t.Error("the revert of a1's switch landed on b2's pane")
	}
	test.Tap(f.w.rows[0].btn)
	if !f.w.pane.mcp.On {
		t.Error("a1's switch does not show its MCP state")
	}
}

func TestAutostartFailureRevertsSwitch(t *testing.T) {
	f := newFixture(t)
	f.set.autoErr = errors.New("cannot write the autostart file")
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("Settings")
	test.Tap(f.toggle("Start at login"))
	f.wait()
	if f.w.settings.autostart.On {
		t.Error("the switch stayed on although SetAutostart failed")
	}
	if !f.has("cannot write the autostart file") {
		t.Errorf("no error shown: %v", f.texts())
	}
}

// With no account, the empty pane offers the add as its main action; a
// stopped bridge's pane does not (it has nothing to add to).
func TestEmptyPaneOffersAdd(t *testing.T) {
	f := newFixture(t)
	f.w.Update(ui.View{Accounts: []ui.AccountView{}})
	f.w.Show("")
	if b := f.button("Add account"); b.Importance != widget.HighImportance {
		t.Errorf("the empty pane's Add account is not the main action")
	}
	f.tap("Add account")
	if f.w.mode != paneWizard {
		t.Fatalf("Add account did not open the wizard: %v", f.texts())
	}
	f.w.Update(ui.View{Bridge: "bridge stopped", Accounts: []ui.AccountView{}})
	f.tap("Cancel")
	if f.hasButton("Add account") {
		t.Errorf("a stopped bridge's pane offers an add: %v", f.texts())
	}
}
