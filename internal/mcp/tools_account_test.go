package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The login and logout tools (design §5.5): login raises pcb-ui's wizard
// (Options.ShowLogin) or opens a loopback page, never taking a secret;
// logout signs an account out, confirmed when the client can ask.

// settable is a clock a test moves.
type settable struct {
	mu sync.Mutex
	t  time.Time
}

func (c *settable) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *settable) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newRigWith is newRig with options changed by mod.
func newRigWith(t *testing.T, hub *stubHub, mod func(*mcp.Options)) *rig {
	t.Helper()
	r := &rig{hub: hub, log: &bytes.Buffer{}}
	o := mcp.Options{
		Hub:    hub,
		Now:    func() time.Time { return fixedNow },
		Zone:   paris,
		Logger: slog.New(slog.NewJSONHandler(&lockedWriter{w: r.log}, nil)),
	}
	if mod != nil {
		mod(&o)
	}
	r.srv = mcp.NewServer(o)
	return r
}

// jsonOf is v through JSON, as a client reads it.
func jsonOf(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	loginGrant  = grant(mcp.CapFreeBusy, mcp.CapRead, mcp.CapLogin)
	manageGrant = grant(mcp.CapFreeBusy, mcp.CapRead, mcp.CapManage)
)

// shown records the usernames ShowLogin was called with.
type shown struct {
	mu    sync.Mutex
	names []string
	err   error
}

func (s *shown) show(u string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, u)
	return s.err
}

func (s *shown) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names)
}

// connectEliciting connects (entry stdio) a client that declares caps and
// answers every question with answer, recording the questions.
func (r *rig) connectEliciting(t *testing.T, g mcp.Grant, caps *sdkmcp.ElicitationCapabilities, answer string) (*sdkmcp.ClientSession, func() []*sdkmcp.ElicitParams) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked []*sdkmcp.ElicitParams
	)
	st, ct := sdkmcp.NewInMemoryTransports()
	ss, err := r.srv.Session(g, mcp.EntryStdio).Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "0"}, &sdkmcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			mu.Lock()
			asked = append(asked, req.Params)
			mu.Unlock()
			return &sdkmcp.ElicitResult{Action: answer, Content: map[string]any{}}, nil
		},
		Capabilities: &sdkmcp.ClientCapabilities{Elicitation: caps},
	})
	cs, err := client.Connect(t.Context(), ct, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, func() []*sdkmcp.ElicitParams {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// loginText calls login and returns its result and every text of it, its
// error included.
func loginText(t *testing.T, cs *sdkmcp.ClientSession, args map[string]any) (*sdkmcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "login", Arguments: args})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return res, b.String()
}

var loginURL = regexp.MustCompile(`http://127\.0\.0\.1:\d+/login/([0-9a-f]{32})`)

func TestLoginToolRaisesTheWizardInPcbUI(t *testing.T) {
	s := &shown{}
	r := newRigWith(t, newHub(), func(o *mcp.Options) { o.ShowLogin = s.show })
	cs := r.connect(t, loginGrant, mcp.EntryStdio)
	res, text := loginText(t, cs, map[string]any{"username": "carol@example.com"})
	if res.IsError {
		t.Fatalf("login: %s", text)
	}
	if got := s.got(); !slices.Equal(got, []string{"carol@example.com"}) {
		t.Errorf("ShowLogin got %q, want the username once", got)
	}
	out := obj(res.StructuredContent)
	if out["where"] != "pcb-ui" || out["username"] != "carol@example.com" || out["cli_fallback"] != "pcb login --username carol@example.com" {
		t.Errorf("login = %v", out)
	}
	if loginURL.MatchString(text) || strings.Contains(text, "http://") {
		t.Errorf("pcb-ui raised its wizard, yet the result carries a URL: %s", text)
	}
	if !strings.Contains(text, "pcb-ui") || !strings.Contains(text, "pcb login --username carol@example.com") {
		t.Errorf("text %q: want where to finish and the CLI fallback", text)
	}
	// pcb-ui failing to show its window is a retry, not a page.
	s.err = errors.New("no window")
	r2 := newRigWith(t, newHub(), func(o *mcp.Options) { o.ShowLogin = s.show })
	mustFail(t, r2.connect(t, loginGrant, mcp.EntryRelay), "login", map[string]any{"username": "carol@example.com"}, "retry")
}

func TestLoginToolCooldown(t *testing.T) {
	c := &settable{t: fixedNow}
	s := &shown{}
	r := newRigWith(t, newHub(), func(o *mcp.Options) { o.Now = c.now; o.ShowLogin = s.show })
	cs := r.connect(t, loginGrant, mcp.EntryStdio)
	args := map[string]any{"username": "carol@example.com"}
	mustCall(t, cs, "login", args)
	c.add(20 * time.Second)
	terr := mustFail(t, cs, "login", with(args, "username", "dave@example.com"), "retry")
	if terr.RetryAfterS != 40 {
		t.Errorf("retry_after_s = %d, want 40 (60 s after the first call)", terr.RetryAfterS)
	}
	if got := s.got(); len(got) != 1 {
		t.Errorf("ShowLogin called %d times, want once: the cooldown refuses before anything", len(got))
	}
	// Per session: another session may log in at once.
	mustCall(t, r.connect(t, loginGrant, mcp.EntryStdio), "login", args)
	c.add(41 * time.Second)
	mustCall(t, cs, "login", args)
	if got := s.got(); len(got) != 3 {
		t.Errorf("ShowLogin called %d times, want 3", len(got))
	}
	// A refused call (bad username) does not start the clock.
	cs2 := r.connect(t, loginGrant, mcp.EntryStdio)
	mustFail(t, cs2, "login", map[string]any{"username": "carol example"}, "refused")
	mustCall(t, cs2, "login", args)
}

func TestLoginToolRefusesWhatIsNoUsername(t *testing.T) {
	s := &shown{}
	r := newRigWith(t, newHub(), func(o *mcp.Options) { o.ShowLogin = s.show })
	cs := r.connect(t, loginGrant, mcp.EntryStdio)
	for _, u := range []string{"", "a b@example.com", "-rf", "a;rm@x", "a'b@x", strings.Repeat("a", 300)} {
		mustFail(t, cs, "login", map[string]any{"username": u}, "refused")
	}
	if got := s.got(); len(got) != 0 {
		t.Errorf("ShowLogin got %q", got)
	}
	// The tool takes no secret: a password argument is refused by its schema.
	res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "login", Arguments: map[string]any{"username": "carol@example.com", "password": "x"}})
	if err == nil && !res.IsError {
		t.Error("login accepted a password argument")
	}
	_, tools := toolNames(t, cs)
	for _, tl := range tools {
		if tl.Name != "login" {
			continue
		}
		props := obj(obj(jsonOf(t, tl.InputSchema))["properties"])
		var names []string
		for k := range props {
			names = append(names, k)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"unattended_relogin", "username"}) {
			t.Errorf("login's arguments %v, want username and unattended_relogin only", names)
		}
	}
}

// Review C I1: login refuses an account that is already ready, before it
// raises pcb-ui's wizard or opens a page (an abandoned relogin must not be
// able to sign a working account out); one that needs a login is served.
func TestLoginToolRefusesAReadyAccount(t *testing.T) {
	for _, viaUI := range []bool{true, false} {
		s := &shown{}
		hub := newHub()
		r := newRigWith(t, hub, func(o *mcp.Options) {
			if viaUI {
				o.ShowLogin = s.show
			}
		})
		cs := r.connect(t, loginGrant, mcp.EntryStdio)
		for _, u := range []string{"alice@example.com", "Alice@Example.com", "a1", "alice"} {
			res, text := loginText(t, cs, map[string]any{"username": u})
			if !res.IsError || !strings.Contains(text, `"class":"refused"`) || !strings.Contains(text, "already logged in") ||
				!strings.Contains(text, "pcb logout") {
				t.Errorf("pcb-ui %t, login %s: %s, want refused: already logged in, naming pcb logout", viaUI, u, text)
			}
			if loginURL.MatchString(text) {
				t.Errorf("login %s opened a page: %s", u, text)
			}
		}
		if got := s.got(); len(got) != 0 {
			t.Errorf("ShowLogin got %q for a ready account", got)
		}
		if got := hub.signedOut(); len(got) != 0 {
			t.Errorf("signed out %q", got)
		}
		// The refusal does not start the cooldown, and an account that needs
		// a login (or a stopped one) is served.
		hub.set("a1", func(a *service.AccountInfo) { a.Phase = "needs-login" })
		res, text := loginText(t, cs, map[string]any{"username": "alice@example.com"})
		if res.IsError {
			t.Errorf("pcb-ui %t: login of an account that needs one: %s", viaUI, text)
		}
		hub.set("b2", func(a *service.AccountInfo) { a.Stopped = &service.StopInfo{Code: 10, Message: "the session ended"} })
		res, text = loginText(t, r.connect(t, loginGrant, mcp.EntryStdio), map[string]any{"username": "bob@example.com"})
		if res.IsError {
			t.Errorf("pcb-ui %t: login of a stopped account: %s", viaUI, text)
		}
	}
}

// The sealer guard of pcb mcp refuses a login that would choose how a new
// state is sealed: must-act, nothing opened.
func TestLoginToolAsksTheGuard(t *testing.T) {
	s := &shown{}
	guard := &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("pcb mcp will not choose how secrets are sealed"), Hint: "run the first pcb login in a terminal"}
	r := newRigWith(t, newHub(), func(o *mcp.Options) {
		o.ShowLogin = s.show
		o.LoginGuard = func(context.Context) error { return guard }
	})
	terr := mustFail(t, r.connect(t, loginGrant, mcp.EntryStdio), "login", map[string]any{"username": "carol@example.com"}, "must-act")
	if !strings.Contains(terr.Message, "will not choose how secrets are sealed") {
		t.Errorf("error %+v", *terr)
	}
	if len(s.got()) != 0 {
		t.Error("the wizard was raised past the guard")
	}
}

func TestLoginToolResultHasNoSecret(t *testing.T) {
	t.Run("url in the text", func(t *testing.T) {
		r := newRigWith(t, newHub(), nil)
		cs := r.connect(t, loginGrant, mcp.EntryStdio)
		res, text := loginText(t, cs, map[string]any{"username": "carol@example.com"})
		if res.IsError {
			t.Fatalf("login: %s", text)
		}
		m := loginURL.FindStringSubmatch(text)
		if m == nil || !strings.Contains(text, "pcb login --username carol@example.com") {
			t.Fatalf("text %q: want the page's URL and the CLI fallback", text)
		}
		out := obj(res.StructuredContent)
		if out["where"] != "page" || out["url"] != m[0] || out["expires_in_s"] != float64(300) {
			t.Errorf("login = %v", out)
		}
		page := getPage(t, m[0])
		if !strings.Contains(page, "carol@example.com") {
			t.Errorf("the page does not show the username:\n%s", page)
		}
		if strings.Contains(r.log.String(), m[1]) {
			t.Errorf("the nonce is in the audit:\n%s", r.log.String())
		}
	})
	t.Run("url elicited", func(t *testing.T) {
		r := newRigWith(t, newHub(), nil)
		cs, asked := r.connectEliciting(t, loginGrant, &sdkmcp.ElicitationCapabilities{URL: &sdkmcp.URLElicitationCapabilities{}}, "accept")
		res, text := loginText(t, cs, map[string]any{"username": "carol@example.com"})
		if res.IsError {
			t.Fatalf("login: %s", text)
		}
		q := asked()
		if len(q) != 1 || q[0].Mode != "url" || !loginURL.MatchString(q[0].URL) || q[0].ElicitationID == "" || !strings.Contains(q[0].Message, "carol@example.com") {
			t.Fatalf("asked %+v, want one URL elicitation of the page", q)
		}
		nonce := loginURL.FindStringSubmatch(q[0].URL)[1]
		if strings.Contains(q[0].ElicitationID, nonce) {
			t.Error("the elicitation ID carries the nonce")
		}
		if loginURL.MatchString(text) || strings.Contains(jsonText(t, res.StructuredContent), nonce) {
			t.Errorf("the URL was elicited, yet the result carries it: %s", text)
		}
		if !strings.Contains(text, "pcb login --username carol@example.com") {
			t.Errorf("text %q: want the CLI fallback", text)
		}
		getPage(t, q[0].URL) // the page the person was sent to serves
		if strings.Contains(r.log.String(), nonce) {
			t.Errorf("the nonce is in the audit:\n%s", r.log.String())
		}
		if n := len(toolLines(t, r, "login")); n != 1 {
			t.Errorf("%d audit lines for one login", n)
		}
	})
	t.Run("url declined", func(t *testing.T) {
		r := newRigWith(t, newHub(), nil)
		cs, asked := r.connectEliciting(t, loginGrant, &sdkmcp.ElicitationCapabilities{URL: &sdkmcp.URLElicitationCapabilities{}}, "decline")
		terr := mustFail(t, cs, "login", map[string]any{"username": "carol@example.com"}, "refused")
		if !strings.Contains(terr.Hint, "pcb login --username carol@example.com") {
			t.Errorf("error %+v: want the CLI fallback in the hint", *terr)
		}
		q := asked()
		if len(q) != 1 {
			t.Fatalf("asked %d", len(q))
		}
		if _, err := http.Get(q[0].URL); err == nil { //nolint:noctx // a probe
			t.Error("the declined page still serves")
		}
	})
	t.Run("form only", func(t *testing.T) {
		// A client that declares form elicitation only gets the URL in the
		// text: a form cannot carry a link to open.
		r := newRigWith(t, newHub(), nil)
		cs, asked := r.connectEliciting(t, loginGrant, &sdkmcp.ElicitationCapabilities{Form: &sdkmcp.FormElicitationCapabilities{}}, "accept")
		_, text := loginText(t, cs, map[string]any{"username": "carol@example.com"})
		if !loginURL.MatchString(text) || len(asked()) != 0 {
			t.Errorf("text %q, asked %d: want the URL in the text and no question", text, len(asked()))
		}
	})
}

// A later login of the same session replaces its page: one page per
// session at a time.
func TestLoginToolReplacesTheSessionsPage(t *testing.T) {
	c := &settable{t: fixedNow}
	r := newRigWith(t, newHub(), func(o *mcp.Options) { o.Now = c.now })
	cs := r.connect(t, loginGrant, mcp.EntryStdio)
	_, first := loginText(t, cs, map[string]any{"username": "carol@example.com"})
	c.add(61 * time.Second)
	_, second := loginText(t, cs, map[string]any{"username": "carol@example.com"})
	u1, u2 := loginURL.FindString(first), loginURL.FindString(second)
	if u1 == "" || u2 == "" || u1 == u2 {
		t.Fatalf("URLs %q, %q", u1, u2)
	}
	if _, err := http.Get(u1); err == nil { //nolint:noctx // a probe
		t.Error("the first page still serves")
	}
	getPage(t, u2)
}

func TestLoginAndLogoutNeverRemote(t *testing.T) {
	r := newRigWith(t, newHub(), func(o *mcp.Options) { o.ShowLogin = (&shown{}).show })
	full, _ := mcp.Profile("full")
	cs := r.connect(t, full, mcp.EntryRemote)
	for _, name := range []string{"login", "logout"} {
		res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{"username": "carol@example.com", "account": "a1"}})
		if err == nil && !res.IsError {
			t.Errorf("%s answered a remote session: %v", name, res.StructuredContent)
		}
	}
	got, _ := toolNames(t, r.connect(t, full, mcp.EntryRelay))
	if !slices.Contains(got, "login") || !slices.Contains(got, "logout") {
		t.Errorf("relay full grant lists %v, want login and logout", got)
	}
	got, _ = toolNames(t, r.connect(t, readGrant, mcp.EntryStdio))
	if slices.Contains(got, "login") || slices.Contains(got, "logout") {
		t.Errorf("a grant without login nor manage lists %v", got)
	}
}

func TestLogoutConfirms(t *testing.T) {
	t.Run("asked", func(t *testing.T) {
		hub := newHub()
		r := newRigWith(t, hub, nil)
		form := &sdkmcp.ElicitationCapabilities{Form: &sdkmcp.FormElicitationCapabilities{}}
		for _, answer := range []string{"decline", "cancel"} {
			cs, asked := r.connectEliciting(t, manageGrant, form, answer)
			terr := mustFail(t, cs, "logout", map[string]any{"account": "alice@example.com"}, "refused")
			if !strings.Contains(terr.Message, "not confirmed") {
				t.Errorf("%s: %+v", answer, *terr)
			}
			if q := asked(); len(q) != 1 || q[0].Mode != "form" || !strings.Contains(q[0].Message, "alice@example.com") {
				t.Errorf("%s: asked %+v", answer, q)
			}
			if got := hub.signedOut(); len(got) != 0 {
				t.Fatalf("%s: signed out %v", answer, got)
			}
		}
		cs, asked := r.connectEliciting(t, manageGrant, form, "accept")
		out := mustCall(t, cs, "logout", map[string]any{"account": "alice@example.com"})
		if out["account"] != "alice@example.com" || out["id"] != "a1" || out["logged_out"] != true {
			t.Errorf("logout = %v", out)
		}
		if got := hub.signedOut(); !slices.Equal(got, []string{"a1"}) || len(asked()) != 1 {
			t.Errorf("signed out %v after %d questions, want a1 after one", got, len(asked()))
		}
		lines := toolLines(t, r, "logout")
		if len(lines) != 3 || lines[2]["outcome"] != "ok" || lines[2]["account"] != "a1" {
			t.Errorf("audit lines %v", lines)
		}
	})
	t.Run("not asked", func(t *testing.T) {
		hub := newHub()
		r := newRigWith(t, hub, nil)
		out := mustCall(t, r.connect(t, manageGrant, mcp.EntryStdio), "logout", map[string]any{"account": "b2"})
		if out["account"] != "bob@example.com" || hub.signedOut()[0] != "b2" {
			t.Errorf("logout = %v, signed out %v", out, hub.signedOut())
		}
	})
	t.Run("outside the grant", func(t *testing.T) {
		hub := newHub()
		r := newRigWith(t, hub, nil)
		g := manageGrant
		g.Accounts = []string{"a1"}
		cs := r.connect(t, g, mcp.EntryStdio)
		mustFail(t, cs, "logout", map[string]any{"account": "bob@example.com"}, "not-found")
		mustFail(t, cs, "logout", map[string]any{"account": "nobody@example.com"}, "not-found")
		if got := hub.signedOut(); len(got) != 0 {
			t.Errorf("signed out %v", got)
		}
	})
}

// getPage GETs a login page and returns its body (200 required).
func getPage(t *testing.T, u string) string {
	t.Helper()
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, pu.String(), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", u, res.StatusCode)
	}
	return string(b)
}
