package loginpage_test

import (
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/loginflow"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp/loginpage"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestMain(m *testing.M) {
	// A new state never reaches the developer's desktop keyring.
	_ = os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	os.Exit(m.Run())
}

type lab struct {
	f    *protontest.Fake
	dirs paths.Dirs
	hub  *service.Hub
}

func newLab(t *testing.T) *lab {
	t.Helper()
	f := protontest.New(t, protontest.Options{})
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(dirs)
	cfg.Proton.BaseURL = f.URL
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: dirs, Config: cfg}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return &lab{f: f, dirs: dirs, hub: hub}
}

// forgetLastLogin lifts the one-login-a-minute guard (it has its own tests).
func (l *lab) forgetLastLogin(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(l.dirs.State, "last-login")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// page is a started login page and the browser of the test.
type page struct {
	t      *testing.T
	url    string
	stop   func()
	cookie *http.Cookie // the claim, once a POST set it
	bodies []string     // every body the page answered
}

func start(t *testing.T, o loginpage.Options) *page {
	t.Helper()
	u, stop, err := loginpage.Start(t.Context(), o)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(stop)
	return &page{t: t, url: u, stop: stop}
}

func (l *lab) options(c *clock) loginpage.Options {
	o := loginpage.Options{Username: l.f.Username, Hub: l.hub}
	if c != nil {
		o.Now = c.now
	}
	return o
}

// do sends req to the page and records the answer.
func (p *page) do(req *http.Request) (*http.Response, string) {
	p.t.Helper()
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	for _, c := range res.Cookies() {
		p.cookie = c
	}
	p.bodies = append(p.bodies, string(b))
	return res, string(b)
}

func (p *page) get(withCookie bool) (*http.Response, string) {
	p.t.Helper()
	req, err := http.NewRequest(http.MethodGet, p.url, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	if withCookie && p.cookie != nil {
		req.AddCookie(p.cookie)
	}
	return p.do(req)
}

// post submits the form of step with the fields kv, from the page's own
// origin, with the claim cookie when there is one.
func (p *page) post(step string, kv ...string) (*http.Response, string) {
	p.t.Helper()
	form := url.Values{"step": {step}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	req, err := http.NewRequest(http.MethodPost, p.url, strings.NewReader(form.Encode()))
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	u, _ := url.Parse(p.url)
	req.Header.Set("Origin", "http://"+u.Host)
	if p.cookie != nil {
		req.AddCookie(p.cookie)
	}
	return p.do(req)
}

// closed waits (2 s at most) for the page's port to refuse connections.
func (p *page) closed() bool {
	u, _ := url.Parse(p.url)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", u.Host, 200*time.Millisecond)
		if err != nil {
			return true
		}
		_ = c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func totpCode(t *testing.T, uri string) string {
	t.Helper()
	o, err := otp.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	return o.Code(time.Now())
}

func TestLoginPageBindsLoopbackWithANonce(t *testing.T) {
	l := newLab(t)
	p := start(t, l.options(nil))
	u, err := url.Parse(p.url)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(u.Host)
	if u.Scheme != "http" || host != "127.0.0.1" || port == "0" || port == "" {
		t.Errorf("URL %q: want http://127.0.0.1:<port>", p.url)
	}
	if !regexp.MustCompile(`^/login/[0-9a-f]{32}$`).MatchString(u.Path) {
		t.Errorf("path %q: want /login/<32 hex>", u.Path)
	}
	other := start(t, l.options(nil))
	if other.url == p.url {
		t.Error("two pages share one URL")
	}
	res, body := p.get(false)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, html.EscapeString(l.f.Username)) || !strings.Contains(body, `type="password"`) {
		t.Errorf("GET: %d, the username shown read-only and a password field expected:\n%s", res.StatusCode, body)
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "src=") || strings.Contains(body, "<link") {
		t.Errorf("the page loads something: %s", body)
	}
	wrong := strings.TrimSuffix(p.url, u.Path) + "/login/" + strings.Repeat("0", 32)
	req, _ := http.NewRequest(http.MethodGet, wrong, nil)
	if res, _ := p.do(req); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET of another nonce: %d, want 404", res.StatusCode)
	}
}

func TestLoginPageSingleUse(t *testing.T) {
	l := newLab(t)
	p := start(t, l.options(nil))
	if res, _ := p.get(false); res.StatusCode != http.StatusOK {
		t.Fatalf("first GET: %d", res.StatusCode)
	}
	res, body := p.post("credentials", "password", "wrong password")
	if res.StatusCode != http.StatusOK || p.cookie == nil {
		t.Fatalf("first POST: %d, cookie %v: %s", res.StatusCode, p.cookie, body)
	}
	if res, _ := p.get(false); res.StatusCode != http.StatusGone {
		t.Errorf("a second GET after the first POST: %d, want 410", res.StatusCode)
	}
	// Another browser (no claim cookie) cannot post either.
	claim := p.cookie
	p.cookie = nil
	if res, _ := p.post("credentials", "password", l.f.Password); res.StatusCode != http.StatusGone {
		t.Errorf("a POST without the claim: %d, want 410", res.StatusCode)
	}
	if n := len(l.hub.Accounts()); n != 0 {
		t.Errorf("the unclaimed POST logged in: %d accounts", n)
	}
	// The browser that claimed it goes on: a reload shows its step.
	p.cookie = claim
	if res, _ := p.get(true); res.StatusCode != http.StatusOK {
		t.Errorf("a reload with the claim: %d, want 200", res.StatusCode)
	}
}

func TestLoginPageExpires(t *testing.T) {
	t.Run("clock", func(t *testing.T) {
		l := newLab(t)
		l.f.SetTwoFA(l.f.Username, true)
		c := &clock{t: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}
		p := start(t, l.options(c))
		if res, body := p.post("credentials", "password", l.f.Password); res.StatusCode != http.StatusOK || !strings.Contains(body, `name="code"`) {
			t.Fatalf("POST: %d, want the 2FA step: %s", res.StatusCode, body)
		}
		if len(l.hub.Accounts()) != 1 {
			t.Fatal("control: the pending add is not listed")
		}
		c.add(5*time.Minute + time.Second)
		if res, _ := p.post("totp", "code", "123456"); res.StatusCode != http.StatusGone {
			t.Errorf("a POST past the TTL: %d, want 410", res.StatusCode)
		}
		if !p.closed() {
			t.Error("the page still listens past its TTL")
		}
		if n := len(l.hub.Accounts()); n != 0 {
			t.Errorf("the expired page left the pending add: %d accounts", n)
		}
	})
	t.Run("timer", func(t *testing.T) {
		l := newLab(t)
		o := l.options(nil)
		o.TTL = 100 * time.Millisecond
		p := start(t, o)
		time.Sleep(150 * time.Millisecond)
		if !p.closed() {
			t.Error("nobody came and the page still listens past its TTL")
		}
	})
}

func TestLoginPageRefusesForeignHostAndOrigin(t *testing.T) {
	l := newLab(t)
	p := start(t, l.options(nil))
	u, _ := url.Parse(p.url)
	_, port, _ := net.SplitHostPort(u.Host)
	for _, host := range []string{"localhost:" + port, "evil.example", "127.0.0.1", "[::1]:" + port} {
		req, _ := http.NewRequest(http.MethodGet, p.url, nil)
		req.Host = host
		if res, _ := p.do(req); res.StatusCode != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403", host, res.StatusCode)
		}
	}
	for _, origin := range []string{"http://evil.example", "null", "http://localhost:" + port, "https://127.0.0.1:" + port} {
		form := url.Values{"step": {"credentials"}, "password": {l.f.Password}}
		req, _ := http.NewRequest(http.MethodPost, p.url, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		if res, _ := p.do(req); res.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q: %d, want 403", origin, res.StatusCode)
		}
	}
	if n := len(l.hub.Accounts()); n != 0 {
		t.Errorf("a refused request logged in: %d accounts", n)
	}
	// Not claimed by the refused requests: the page still serves.
	if res, _ := p.get(false); res.StatusCode != http.StatusOK {
		t.Errorf("GET after the refusals: %d, want 200", res.StatusCode)
	}
}

func TestLoginPageNoStore(t *testing.T) {
	l := newLab(t)
	p := start(t, l.options(nil))
	u, _ := url.Parse(p.url)
	check := func(what string, res *http.Response) {
		t.Helper()
		if got := res.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s (%d): Cache-Control %q, want no-store", what, res.StatusCode, got)
		}
		if got := res.Header.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("%s (%d): Referrer-Policy %q, want same-origin (the URL never leaves as a Referer)", what, res.StatusCode, got)
		}
	}
	res, _ := p.get(false)
	check("GET", res)
	req, _ := http.NewRequest(http.MethodGet, p.url, nil)
	req.Host = "evil.example"
	res, _ = p.do(req)
	check("foreign host", res)
	req, _ = http.NewRequest(http.MethodGet, "http://"+u.Host+"/elsewhere", nil)
	res, _ = p.do(req)
	check("404", res)
	res, _ = p.post("credentials", "password", "wrong password")
	check("POST", res)
	res, _ = p.get(false)
	check("410", res)
}

func TestLoginPageNeverEchoesThePassword(t *testing.T) {
	l := newLab(t)
	uri := l.f.SetTwoFA(l.f.Username, true)
	l.f.SetCaptchaFor(l.f.Username, "tok")
	p := start(t, l.options(nil))
	p.get(false)
	p.post("credentials", "password", "wrong password") // the fake challenges before it checks
	l.forgetLastLogin(t)
	p.post("captcha") // and refuses the wrong password then
	l.forgetLastLogin(t)
	p.post("credentials", "password", l.f.Password)
	l.forgetLastLogin(t)
	p.post("captcha")
	code := totpCode(t, uri)
	_, last := p.post("totp", "code", code)
	if !strings.Contains(last, "added") {
		t.Fatalf("the login did not end: %s", last)
	}
	secrets := map[string]string{"the password": l.f.Password, "the wrong password": "wrong password", "the 2FA code": code}
	for i, body := range p.bodies {
		for what, s := range secrets {
			if strings.Contains(body, s) || strings.Contains(body, html.EscapeString(s)) {
				t.Errorf("answer %d contains %s:\n%s", i, what, body)
			}
		}
	}
}

func TestLoginPageRunsTOTPAndCaptchaSteps(t *testing.T) {
	t.Run("totp", func(t *testing.T) {
		l := newLab(t)
		uri := l.f.SetTwoFA(l.f.Username, true)
		p := start(t, l.options(nil))
		_, body := p.post("credentials", "password", l.f.Password)
		if !strings.Contains(body, `name="code"`) {
			t.Fatalf("after the password: want the 2FA step:\n%s", body)
		}
		_, body = p.post("totp", "code", "000000")
		if !strings.Contains(body, `name="code"`) || !strings.Contains(body, "class=\"error\"") {
			t.Errorf("a wrong code: want the 2FA step again with the error:\n%s", body)
		}
		_, body = p.post("totp", "code", totpCode(t, uri))
		if !strings.Contains(body, "added") {
			t.Fatalf("after the code: want the end:\n%s", body)
		}
		if a := l.hub.Accounts(); len(a) != 1 || a[0].Phase != "ready" {
			t.Errorf("accounts %+v, want one ready", a)
		}
		if !p.closed() {
			t.Error("the page still listens after the login ended")
		}
	})
	t.Run("captcha", func(t *testing.T) {
		l := newLab(t)
		l.f.SetCaptchaFor(l.f.Username, "tok")
		p := start(t, l.options(nil))
		_, body := p.post("credentials", "password", l.f.Password)
		m := regexp.MustCompile(`<a href="([^"]+)"[^>]*rel="noopener noreferrer"`).FindStringSubmatch(body)
		if m == nil || !strings.HasPrefix(html.UnescapeString(m[1]), "http") {
			t.Fatalf("after the password: want the CAPTCHA step with a link to the verification:\n%s", body)
		}
		if strings.Contains(body, "<iframe") || strings.Contains(body, "<img") {
			t.Errorf("the verification is embedded, not linked:\n%s", body)
		}
		l.forgetLastLogin(t)
		_, body = p.post("captcha")
		if !strings.Contains(body, "added") {
			t.Fatalf("after the CAPTCHA: want the end:\n%s", body)
		}
		if a := l.hub.Accounts(); len(a) != 1 || a[0].Phase != "ready" {
			t.Errorf("accounts %+v, want one ready", a)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		l := newLab(t)
		l.f.SetTwoFA(l.f.Username, true)
		p := start(t, l.options(nil))
		p.post("credentials", "password", l.f.Password)
		if len(l.hub.Accounts()) != 1 {
			t.Fatal("control: the pending add is not listed")
		}
		p.post("totp", "cancel", "1")
		if n := len(l.hub.Accounts()); n != 0 {
			t.Errorf("Cancel left %d accounts", n)
		}
		if !p.closed() {
			t.Error("the page still listens after Cancel")
		}
	})
}

// stop closes the page and cancels a login it left half-way.
func TestLoginPageStopCancels(t *testing.T) {
	l := newLab(t)
	l.f.SetTwoFA(l.f.Username, true)
	p := start(t, l.options(nil))
	p.post("credentials", "password", l.f.Password)
	p.stop()
	if !p.closed() {
		t.Error("the page still listens after stop")
	}
	if n := len(l.hub.Accounts()); n != 0 {
		t.Errorf("stop left %d accounts", n)
	}
	p.stop() // twice is fine
}

const module = "github.com/Barben360/proton-calendar-bridge"

// The page is part of the MCP adapter: never internal/ui (ruling R18), the
// Proton adapters, the store or the CLI.
func TestLoginPageArch(t *testing.T) {
	allowed := map[string]bool{
		module + "/internal/service":   true,
		module + "/internal/exitcode":  true,
		module + "/internal/loginflow": true,
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		ctx := build.Default
		ctx.GOOS = goos
		pkg, err := ctx.ImportDir(".", 0)
		if err != nil {
			t.Fatal(err)
		}
		ours := 0
		for _, imp := range pkg.Imports {
			first, _, _ := strings.Cut(imp, "/")
			switch {
			case allowed[imp]:
				ours++
			case !strings.Contains(first, "."):
			default:
				t.Errorf("GOOS=%s: internal/mcp/loginpage imports %s", goos, imp)
			}
		}
		if ours == 0 {
			t.Errorf("GOOS=%s: no import of this module: the check would pass vacuously", goos)
		}
	}
}

// Review C M1: the unattended-relogin box is never ticked for the person,
// as in pcb login and pcb-ui; an agent's suggestion is a note, and the box
// carries pcb-ui's keyring warning. A person with 2FA who leaves it
// unticked finishes with a 6-digit code.
func TestLoginPageNeverTicksUnattended(t *testing.T) {
	box := regexp.MustCompile(`<input type="checkbox" name="unattended"[^>]*>`)
	for _, suggested := range []bool{true, false} {
		l := newLab(t)
		uri := l.f.SetTwoFA(l.f.Username, true)
		o := l.options(nil)
		o.Unattended = suggested
		p := start(t, o)
		_, body := p.get(false)
		m := box.FindString(body)
		if m == "" || strings.Contains(m, "checked") {
			t.Errorf("suggested %t: box %q, want it present and unticked", suggested, m)
		}
		if got := strings.Contains(body, "The agent suggests unattended relogin"); got != suggested {
			t.Errorf("suggested %t: the page shows the agent's suggestion: %t\n%s", suggested, got, body)
		}
		if !strings.Contains(body, html.EscapeString(loginflow.UnattendedWarning)) {
			t.Errorf("suggested %t: no keyring warning next to the box:\n%s", suggested, body)
		}
		p.post("credentials", "password", l.f.Password)
		_, body = p.post("totp", "code", totpCode(t, uri))
		if !strings.Contains(body, "added") {
			t.Fatalf("suggested %t: a 6-digit code did not finish the login:\n%s", suggested, body)
		}
	}
}

// Review C M4: the nonce in the path is a secret, compared in constant
// time (subtle.ConstantTimeCompare), never with == or !=. The timing is
// not observable in a test, so this reads the code.
func TestLoginPageComparesTheNonceInConstantTime(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "page.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	isPath := func(e ast.Expr) bool {
		s, ok := e.(*ast.SelectorExpr)
		return ok && s.Sel.Name == "path"
	}
	constant := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if (n.Op == token.EQL || n.Op == token.NEQ) && (isPath(n.X) || isPath(n.Y)) {
				t.Errorf("page.go compares a path with %s", n.Op)
			}
		case *ast.CallExpr:
			if s, ok := n.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "ConstantTimeCompare" {
				for _, a := range n.Args {
					if c, ok := a.(*ast.CallExpr); ok && len(c.Args) == 1 && isPath(c.Args[0]) {
						constant++
					}
				}
			}
		}
		return true
	})
	if constant == 0 {
		t.Error("page.go never compares the nonce's path with subtle.ConstantTimeCompare")
	}
}
