// Package loginpage is the temporary loopback page where a person finishes
// a login an agent started with the MCP login tool (design §5.5), when the
// process that owns the accounts is not pcb-ui: the password, then the 2FA
// code or a CAPTCHA, typed in the person's browser, never through the
// agent. The steps are loginflow's, run against the Hub in process; the
// page holds no login state of its own.
//
// One page serves one login of one username: 127.0.0.1 on an ephemeral
// port only, a path with a 128-bit nonce, single use (the first POST claims
// it for that browser with a cookie; any other request is 410 then), Host
// and Origin checked, Cache-Control: no-store, no external asset, no secret
// ever echoed. It closes when the login ends, is cancelled, or its TTL
// passes, cancelling a login left half-way.
package loginpage

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/loginflow"
)

// DefaultTTL is how long a page waits for its login (Global Constraints).
const DefaultTTL = 5 * time.Minute

const (
	nonceBytes  = 16 // 128 bits
	maxBody     = 16 << 10
	claimCookie = "pcb_login"
	// callTimeout bounds one step's call to the Hub: a CAPTCHA retry may
	// wait for the one-login-a-minute guard.
	callTimeout = 2 * time.Minute
	// shutdownGrace lets an answer in flight finish when the page closes.
	shutdownGrace = 2 * time.Second
)

// Options configure a page. Username and Hub are required; TTL defaults to
// DefaultTTL, Now to time.Now. Unattended is the unattended-relogin box's
// first state (the person decides).
type Options struct {
	Username   string
	Unattended bool
	Hub        loginflow.Hub
	TTL        time.Duration
	Now        func() time.Time
}

type page struct {
	o        Options
	flow     *loginflow.Flow
	host     string // 127.0.0.1:<port>, the only Host accepted
	origin   string // http://127.0.0.1:<port>, the only Origin accepted
	path     string // /login/<nonce>
	deadline time.Time
	srv      *http.Server
	once     sync.Once
	// base is the context of the steps' calls: end cancels it, so a call
	// in flight does not hold the page open.
	base   context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	claim string // the claim cookie's value, once a POST claimed the page
	ended bool
}

// Start opens a page for o.Username and returns its URL and a stop that
// closes it (cancelling a login left half-way; safe to call twice). ctx
// bounds the bind only: the page lives until its login ends, its TTL
// passes, or stop.
func Start(ctx context.Context, o Options) (url string, stop func(), err error) {
	if o.Username == "" || o.Hub == nil {
		return "", nil, errors.New("loginpage: a username and the accounts are required")
	}
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	nonce, err := random()
	if err != nil {
		return "", nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	hostPort := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	hub := o.Hub
	p := &page{
		o:        o,
		flow:     loginflow.New(func() (loginflow.Hub, error) { return hub, nil }),
		host:     hostPort,
		origin:   "http://" + hostPort,
		path:     "/login/" + nonce,
		deadline: o.Now().Add(o.TTL),
	}
	p.base, p.cancel = context.WithCancel(context.Background())
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = p.srv.Serve(ln) }()
	timer := time.AfterFunc(o.TTL, p.end)
	return p.origin + p.path, func() { timer.Stop(); p.end() }, nil
}

// random is 128 random bits in hex.
func random() (string, error) {
	b := make([]byte, nonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// end closes the page, once: a call in flight is cancelled, the login is
// cancelled if it was left half-way (loginflow's Cancel removes an add
// that never finished), then the listener closes. A handler calls it on a
// goroutine of its own: Shutdown waits for handlers.
func (p *page) end() {
	p.once.Do(func() {
		p.mu.Lock()
		p.ended = true
		p.mu.Unlock()
		p.cancel()
		p.flow.Cancel()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := p.srv.Shutdown(ctx); err != nil {
			_ = p.srv.Close()
		}
	})
}

func (p *page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "same-origin") // the URL never leaves as a Referer
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	switch {
	case r.Host != p.host:
		plain(w, http.StatusForbidden, "this page answers on "+p.host+" only")
		return
	case r.Header.Get("Origin") != "" && r.Header.Get("Origin") != p.origin:
		plain(w, http.StatusForbidden, "this page accepts forms from itself only")
		return
	case r.URL.Path != p.path:
		plain(w, http.StatusNotFound, "not found")
		return
	}
	if p.o.Now().After(p.deadline) {
		p.gone(w)
		go p.end()
		return
	}
	switch r.Method {
	case http.MethodGet:
		p.get(w, r)
	case http.MethodPost:
		p.post(w, r)
	default:
		h.Set("Allow", "GET, POST")
		plain(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func plain(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

func (p *page) gone(w http.ResponseWriter) {
	plain(w, http.StatusGone, "this login page was used or has expired: ask the agent to call login again, or run: pcb login --username "+p.o.Username)
}

// claimed reports whether the page was claimed, and whether r carries the
// claim.
func (p *page) claimed(r *http.Request) (claimed, ours bool) {
	p.mu.Lock()
	claim, ended := p.claim, p.ended
	p.mu.Unlock()
	if ended {
		return true, false
	}
	if claim == "" {
		return false, false
	}
	c, err := r.Cookie(claimCookie)
	return true, err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(claim)) == 1
}

func (p *page) get(w http.ResponseWriter, r *http.Request) {
	if claimed, ours := p.claimed(r); claimed && !ours {
		p.gone(w)
		return
	}
	p.render(w, p.flow.Step())
}

func (p *page) post(w http.ResponseWriter, r *http.Request) {
	claimed, ours := p.claimed(r)
	if claimed && !ours {
		p.gone(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "the form could not be read")
		return
	}
	if !claimed && !p.claimNow(w) {
		p.gone(w)
		return
	}
	form := r.PostForm
	if form.Get("cancel") != "" {
		p.flow.Cancel()
		p.render(w, stepCancelled)
		go p.end()
		return
	}
	ctx, cancel := context.WithTimeout(p.base, callTimeout) // not the request's: a closed tab does not abort a login
	defer cancel()
	if loginflow.Step(form.Get("step")) == p.flow.Step() { // a stale form only shows the current step
		switch p.flow.Step() {
		case loginflow.StepCredentials:
			pw := []byte(form.Get("password"))
			delete(form, "password") // best effort: the request's strings stay until collected
			delete(r.Form, "password")
			_ = p.flow.Submit(ctx, p.o.Username, pw, form.Get("unattended") != "")
			clear(pw)
		case loginflow.StepTOTP:
			_ = p.flow.SubmitTOTP(ctx, strings.TrimSpace(form.Get("code")))
		case loginflow.StepCaptcha:
			_ = p.flow.CaptchaSolved(ctx)
		}
	}
	step := p.flow.Step()
	p.render(w, step)
	if step == loginflow.StepDone {
		go p.end()
	}
}

// claimNow claims the page for this browser (false: another request did
// first, or the page ended).
func (p *page) claimNow(w http.ResponseWriter) bool {
	v, err := random()
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.claim != "" || p.ended {
		return false
	}
	p.claim = v
	http.SetCookie(w, &http.Cookie{Name: claimCookie, Value: v, Path: p.path, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return true
}

// stepCancelled is the screen after Cancel.
const stepCancelled loginflow.Step = "cancelled"

type view struct {
	Username   string
	Step       string
	Err        string
	Hint       string
	CaptchaURL string
	Unattended bool
}

func (p *page) render(w http.ResponseWriter, step loginflow.Step) {
	v := view{Username: p.o.Username, Step: string(step), Unattended: p.o.Unattended}
	if step != loginflow.StepDone && step != stepCancelled {
		if e := p.flow.Err(); e != nil {
			v.Err, v.Hint = e.Error(), e.Hint
		}
	}
	if step == loginflow.StepCaptcha {
		v.CaptchaURL = p.flow.CaptchaURL()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, v)
}

var tmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>pcb: log in to Proton</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem;line-height:1.4}
label{display:block;margin:.8rem 0}input[type=password],input[type=text]{width:100%;padding:.4rem;box-sizing:border-box}
.error{color:#b00020}.note{color:#555;font-size:.9rem}button{margin-right:.5rem;padding:.4rem .9rem}</style></head>
<body><main>
<h1>Log in to Proton</h1>
<p>Account: <strong>{{.Username}}</strong></p>
{{if .Err}}<p class="error">{{.Err}}{{if .Hint}}<br>{{.Hint}}{{end}}</p>{{end}}
{{if eq .Step "credentials"}}<form method="post">
<input type="hidden" name="step" value="credentials">
<label>Proton password <input type="password" name="password" autocomplete="current-password" required autofocus></label>
<label><input type="checkbox" name="unattended" value="1"{{if .Unattended}} checked{{end}}> Repair the session without me (unattended relogin: pcb keeps the password, sealed)</label>
<button type="submit">Sign in</button><button type="submit" name="cancel" value="1" formnovalidate>Cancel</button>
</form>
{{else if eq .Step "totp"}}<form method="post">
<input type="hidden" name="step" value="totp">
<label>Two-factor code <input type="text" name="code" inputmode="numeric" autocomplete="one-time-code" required autofocus></label>
<p class="note">With unattended relogin, paste the otpauth:// URI instead of a code.</p>
<button type="submit">Verify</button><button type="submit" name="cancel" value="1" formnovalidate>Cancel</button>
</form>
{{else if eq .Step "captcha"}}<p>Proton asks for a human verification. <a href="{{.CaptchaURL}}" target="_blank" rel="noopener noreferrer">Open the verification</a>, solve it, then come back here and press I solved it.</p>
<form method="post">
<input type="hidden" name="step" value="captcha">
<button type="submit">I solved it</button><button type="submit" name="cancel" value="1">Cancel</button>
</form>
<p class="note">After I solved it, Proton may take up to a minute to answer.</p>
{{else if eq .Step "done"}}<p>The account is added: pcb syncs it now. You can close this page.</p>
{{else}}<p>The login is cancelled. You can close this page.</p>
{{end}}</main></body></html>
`))
