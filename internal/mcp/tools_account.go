package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/mcp/loginpage"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The account tools (design §5.5), stdio only: a remote session never has
// them, whatever its grant. login (capability login) never takes a secret:
// the process that owns the accounts asks the person for it out of band, in
// pcb-ui's add-account wizard (Options.ShowLogin) or on a single-use
// loopback page (internal/mcp/loginpage) whose URL is offered by URL-mode
// elicitation when the client declares it, else given in the result. It is
// limited to one call a minute per MCP session. logout (manage) signs one
// account out, confirmed by the person through a form elicitation when the
// client declares one.

func accountTools() []toolDef {
	return []toolDef{
		{name: "login", cap: CapLogin, stdioOnly: true, add: addLogin},
		{name: "logout", cap: CapManage, stdioOnly: true, add: addLogout},
	}
}

// loginCooldown is the time between two login calls of one MCP session
// (Global Constraints).
const loginCooldown = 60 * time.Second

// loginKey names the login's URL question in InputRequests;
// confirmLogoutKey the logout's.
const (
	loginKey         = "login_page"
	confirmLogoutKey = "confirm_logout"
)

// usernameRE is what login accepts as a username: a Proton address or
// username, nothing a shell, a page or a log line could read otherwise.
var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._%+@-]{0,253}$`)

// logins is the login tool's state per MCP session: when it last started a
// login, and the page it opened.
type logins struct {
	mu        sync.Mutex
	bySession map[*sdkmcp.ServerSession]*sessionLogin
}

type sessionLogin struct {
	last time.Time // the last call that started a login
	page *openPage // nil when none is open
}

// openPage is a login page a session opened. awaiting: its URL was offered
// by elicitation, and the call that offered it waits for the answer.
type openPage struct {
	username string
	stop     func()
	awaiting bool
}

// start records a login started by session at now and gives the page it
// replaces (to stop), or a wait (> 0) when the session is cooling down.
func (l *logins) start(session *sdkmcp.ServerSession, now time.Time) (wait time.Duration, replaced *openPage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for s, sl := range l.bySession { // forget sessions long quiet
		if now.Sub(sl.last) > loginCooldown+loginpage.DefaultTTL {
			delete(l.bySession, s)
		}
	}
	sl := l.bySession[session]
	if sl == nil {
		sl = &sessionLogin{}
		l.bySession[session] = sl
	} else if wait := sl.last.Add(loginCooldown).Sub(now); wait > 0 {
		return wait, nil
	}
	sl.last, replaced, sl.page = now, sl.page, nil
	return 0, replaced
}

// hold records the page session opened.
func (l *logins) hold(session *sdkmcp.ServerSession, p *openPage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sl := l.bySession[session]; sl != nil {
		sl.page = p
	}
}

// answered settles the page session offered for username: accepted, it
// stays open; else it is stopped. ok is false when no such page awaits.
func (l *logins) answered(session *sdkmcp.ServerSession, username string, accept bool) (ok bool) {
	l.mu.Lock()
	sl := l.bySession[session]
	if sl == nil || sl.page == nil || !sl.page.awaiting || sl.page.username != username {
		l.mu.Unlock()
		return false
	}
	p := sl.page
	p.awaiting = false
	if !accept {
		sl.page = nil
	}
	l.mu.Unlock()
	if !accept {
		p.stop()
	}
	return true
}

type loginIn struct {
	Username          string `json:"username" jsonschema:"the Proton address (or username) of the account to log in"`
	UnattendedRelogin bool   `json:"unattended_relogin,omitempty" jsonschema:"suggest unattended relogin (pcb keeps the password sealed to repair the session alone); the person decides"`
}

type loginOut struct {
	Username    string `json:"username"`
	Where       string `json:"where" jsonschema:"pcb-ui: its add-account wizard is shown; page: a login page on this machine's 127.0.0.1"`
	URL         string `json:"url,omitempty" jsonschema:"the page to give the person, when it was not offered to them already"`
	ExpiresInS  int    `json:"expires_in_s,omitempty" jsonschema:"seconds the page stays open"`
	CLIFallback string `json:"cli_fallback" jsonschema:"what the person runs in a terminal instead"`
}

func addLogin(ss *session) {
	destructive := false
	t := &sdkmcp.Tool{
		Name: "login",
		Description: "Start a Proton login for an account (new, pending or signed out; an account already ready is refused). It never takes a password, code or token: " +
			"the person types them in pcb-ui's window or on a single-use page of this machine, never through you. " +
			"Give the person the URL when the result has one; call status to follow the account. One call a minute.",
		Annotations: &sdkmcp.ToolAnnotations{Title: "Log in to Proton", DestructiveHint: &destructive},
	}
	t.Description += " " + untrustedNote
	sdkmcp.AddTool(ss.sdk, t, func(ctx context.Context, req *sdkmcp.CallToolRequest, in loginIn) (*sdkmcp.CallToolResult, loginOut, error) {
		out, text, ask, err := ss.login(ctx, req, in)
		switch {
		case err != nil:
			return nil, loginOut{}, toToolError(err)
		case ask != nil:
			return &sdkmcp.CallToolResult{InputRequests: sdkmcp.InputRequestMap{loginKey: ask}}, loginOut{}, nil
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}}}, out, nil
	})
}

// login runs once, or twice when the page's URL is offered by elicitation
// (E19): the first run opens the page and asks; the second reads the
// answer and opens nothing.
func (ss *session) login(ctx context.Context, req *sdkmcp.CallToolRequest, in loginIn) (loginOut, string, *sdkmcp.ElicitParams, error) {
	if !usernameRE.MatchString(in.Username) {
		return loginOut{}, "", nil, refusedErr("username is not a Proton address or username: "+quote(in.Username),
			"give the account's address (name@proton.me) or its Proton username")
	}
	cli := "pcb login --username " + in.Username
	out := loginOut{Username: in.Username, CLIFallback: cli}
	lg := &ss.srv.logins
	if answer, asked := req.Params.InputResponses[loginKey]; asked {
		r, ok := answer.(*sdkmcp.ElicitResult)
		accept := ok && r.Action == "accept"
		switch {
		case !lg.answered(req.Session, in.Username, accept):
			return loginOut{}, "", nil, refusedErr("the login page of this call is gone", "call login again, or the person runs: "+cli)
		case !accept:
			return loginOut{}, "", nil, refusedErr("the person declined the login page: it is closed and nothing was started",
				"do not retry unless they ask for it; they can also run: "+cli)
		}
		out.Where, out.ExpiresInS = "page", int(loginpage.DefaultTTL.Seconds())
		return out, "The person was offered the login page for " + in.Username + " (single use, open 5 min): they type the password there, never here. " +
			"Call status to follow the account. If their browser cannot reach this machine's 127.0.0.1, they run: " + cli, nil, nil
	}
	if a, ok := ss.readyAccount(in.Username); ok {
		return loginOut{}, "", nil, refusedErr(a.Address+" is already logged in to Proton: nothing was started",
			"no login is needed; to replace the session, the person runs pcb logout then pcb login")
	}
	if g := ss.srv.o.LoginGuard; g != nil {
		if err := g(ctx); err != nil {
			return loginOut{}, "", nil, err
		}
	}
	wait, replaced := lg.start(req.Session, ss.srv.o.Now())
	if wait > 0 {
		return loginOut{}, "", nil, &toolError{Class: classRetry, Message: "one login call a minute per session",
			Hint: "the person finishes the login already offered, or wait and call login again", RetryAfterS: int(math.Ceil(wait.Seconds()))}
	}
	if replaced != nil {
		replaced.stop()
	}
	if show := ss.srv.o.ShowLogin; show != nil {
		if err := show(in.Username); err != nil {
			return loginOut{}, "", nil, &toolError{Class: classRetry, Message: "pcb-ui could not show its add-account wizard",
				Hint: "the person opens pcb-ui and adds the account, or runs: " + cli}
		}
		out.Where = "pcb-ui"
		return out, "pcb-ui shows its add-account wizard for " + in.Username + ": the person types the password (and any 2FA code or CAPTCHA) there, never here. " +
			"Call status to follow the account. Without pcb-ui at hand, they run: " + cli, nil, nil
	}
	url, stop, err := loginpage.Start(ctx, loginpage.Options{Username: in.Username, Unattended: in.UnattendedRelogin, Hub: ss.srv.o.Hub, Now: ss.srv.o.Now})
	if err != nil {
		return loginOut{}, "", nil, &toolError{Class: classRetry, Message: "the login page could not open", Hint: "the person runs: " + cli}
	}
	page := &openPage{username: in.Username, stop: stop}
	if asksURL(req) {
		id, err := elicitationID()
		if err != nil {
			stop()
			return loginOut{}, "", nil, &toolError{Class: classRetry, Message: "the login page could not open", Hint: "the person runs: " + cli}
		}
		page.awaiting = true
		lg.hold(req.Session, page)
		return loginOut{}, "", &sdkmcp.ElicitParams{
			Mode:          "url",
			Message:       "Log in to Proton as " + in.Username + " for pcb, on a page of this machine (single use, open 5 minutes).",
			URL:           url,
			ElicitationID: id,
		}, nil
	}
	lg.hold(req.Session, page)
	out.Where, out.URL, out.ExpiresInS = "page", url, int(loginpage.DefaultTTL.Seconds())
	return out, "Give the person this link to log in to Proton as " + in.Username + " (this machine only, single use, open 5 min): " + url +
		" They type the password there, never here. Call status to follow the account. If their browser cannot reach this machine's 127.0.0.1, they run: " + cli, nil, nil
}

// readyAccount is the account username names (its local ID, its address,
// or a bare username its address starts with) when that account is ready
// and serving: login refuses it, since a relogin abandoned on its 2FA step
// would only disturb a working session (review C I1).
func (ss *session) readyAccount(username string) (service.AccountInfo, bool) {
	for _, a := range ss.srv.o.Hub.Accounts() {
		local, _, _ := strings.Cut(a.Address, "@")
		named := namesAccount(username, a) || !strings.Contains(username, "@") && strings.EqualFold(username, local)
		if named && a.Phase == string(service.StateReady) && a.Stopped == nil {
			return a, true
		}
	}
	return service.AccountInfo{}, false
}

// asksURL: the client declared URL-mode elicitation on this request.
func asksURL(req *sdkmcp.CallToolRequest) bool {
	caps := req.ClientCapabilities()
	return caps != nil && caps.Elicitation != nil && caps.Elicitation.URL != nil
}

// elicitationID names a URL elicitation: random, unrelated to the page's
// nonce.
func elicitationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type logoutIn struct {
	Account string `json:"account" jsonschema:"the account's address or local ID (status lists them)"`
}

type logoutOut struct {
	Account     string `json:"account"`
	ID          string `json:"id"`
	LoggedOut   bool   `json:"logged_out"`
	WasLoggedIn bool   `json:"was_logged_in"`
	Revoked     bool   `json:"revoked" jsonschema:"Proton revoked the session (else it ends on its own)"`
}

func addLogout(ss *session) {
	t := &sdkmcp.Tool{
		Name: "logout",
		Description: "Sign pcb out of one Proton account, as pcb logout does: its calendars stop syncing until someone logs in again. " +
			"The person may be asked to confirm; declined, nothing changes.",
		Annotations: writing("Sign an account out", true),
	}
	t.Description += " " + untrustedNote
	sdkmcp.AddTool(ss.sdk, t, func(ctx context.Context, req *sdkmcp.CallToolRequest, in logoutIn) (*sdkmcp.CallToolResult, logoutOut, error) {
		out, ask, err := ss.logout(ctx, req, in)
		switch {
		case err != nil:
			return nil, logoutOut{}, toToolError(err)
		case ask != nil:
			return &sdkmcp.CallToolResult{InputRequests: sdkmcp.InputRequestMap{confirmLogoutKey: ask}}, logoutOut{}, nil
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: out.Account + " is signed out"}}}, out, nil
	})
}

// logout runs once, or twice when the person is asked (E19): the run that
// asks changes nothing.
func (ss *session) logout(ctx context.Context, req *sdkmcp.CallToolRequest, in logoutIn) (logoutOut, *sdkmcp.ElicitParams, error) {
	if in.Account == "" {
		return logoutOut{}, nil, refusedErr("account is required", "the status tool lists the accounts this session can see")
	}
	ts, err := ss.accounts(ctx, []string{in.Account})
	if err != nil {
		return logoutOut{}, nil, err
	}
	a := ts[0].info
	auditOf(ctx).account(a.ID)
	if asksForm(req) {
		answer, asked := req.Params.InputResponses[confirmLogoutKey]
		if !asked {
			return logoutOut{}, &sdkmcp.ElicitParams{
				Mode:            "form",
				Message:         "Sign pcb out of the Proton account " + promptText(a.Address) + "? Its calendars stop syncing until someone logs in again.",
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			}, nil
		}
		if r, ok := answer.(*sdkmcp.ElicitResult); !ok || r.Action != "accept" {
			return logoutOut{}, nil, refusedErr("the logout was not confirmed: nothing changed",
				"the person declined it; do not retry unless they ask for it again")
		}
	}
	res, err := ss.srv.o.Hub.SignOut(ctx, a.ID, service.LogoutRequest{})
	if err != nil {
		return logoutOut{}, nil, err
	}
	return logoutOut{Account: a.Address, ID: a.ID, LoggedOut: true, WasLoggedIn: res.WasLoggedIn, Revoked: res.Revoked}, nil, nil
}
