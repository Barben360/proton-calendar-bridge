// Package protontest is the fake Proton every C1, C3 and CLI test shares: an
// httptest front over go-proton-api's in-memory server that adds what the
// server lacks (Proton's envelope and error codes, CAPTCHA, TOTP, two-password
// and FIDO2 accounts, the locked scope, token expiry, revocation and injected
// failures). It never contacts Proton. Test use only, and the developer-only
// rehearsal fake cmd/pcb-fake (through Process); pcb itself never links it.
//
// Backends and their accounts are pooled per test binary (see pool), so a
// fake is cheap while its crypto stays real.
package protontest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/go-proton-api/server"

	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// Proton codes the fake answers with. CodeTOTPWrong is assumed: callers must
// not depend on it (see auth.ErrTOTPRejected).
const (
	CodePasswordWrong = 8002
	CodeTOTPWrong     = 12060
)

const appVersion = "Other" // for the fake's own calls to the backend

// Options choose the account's shape and the fake's challenges.
type Options struct {
	TwoFA            bool // 2FA.Enabled = 1; /auth/v4/2fa checks codes of Fake.TOTPURI
	FIDO2Only        bool // 2FA.Enabled = 2
	TwoPassword      bool // PasswordMode = 2
	LockedSalts      bool // GET /core/v4/keys/salts answers 403/9101 until unlocked
	BadUnlockProof   bool // PUT /core/v4/users/unlock returns a wrong ServerProof
	EmptyUnlockProof bool // PUT /core/v4/users/unlock returns no ServerProof
	BadLoginProof    bool // POST /auth/v4 returns a wrong ServerProof
	// Captcha, when set, makes POST /auth/v4 answer 422/9001 with this token
	// unless the request carries it (x-pm-human-verification-token, type
	// captcha). SetCaptcha changes it later.
	Captcha        string
	CaptchaMethods []string         // offered with Captcha; default ["captcha"]
	Now            func() time.Time // clock for TOTP; default time.Now
	// Username and Password name the first account; empty: throwaway values.
	Username, Password string
	// BadPassphraseSignature serves our member passphrases signed by an
	// unrelated key, so the signature does not verify with ours (research G).
	BadPassphraseSignature bool
	// EmptyPassphraseSignature serves no signature at all (a stripped one).
	EmptyPassphraseSignature bool
	// ReplayedPassphraseSignature serves a genuine signature of our own
	// address key over another plaintext (a replay from another calendar,
	// research G): it verifies on its own, not on this passphrase.
	ReplayedPassphraseSignature bool
	// Write selects the other reading of each unsettled write behaviour
	// (see WriteOptions).
	Write WriteOptions
	// TOTPSecret is the base32 TOTP secret of the first account (Fake.TOTPURI
	// carries it); empty: a random one.
	TOTPSecret string
	// Listener is where the fake serves; nil: a new loopback port. The fake
	// closes it with the rest.
	Listener net.Listener
	// UIDMatch, when set, decides which rows a listing's ?UID= returns
	// (rowUID, query); nil: equal UIDs only. Whether Proton matches exactly
	// is open (HUMAN-CHECKS W3).
	UIDMatch func(rowUID, query string) bool
}

// Fake is a running fake Proton. URL, Username, Password and TOTPURI are the
// fake's own throwaway values.
type Fake struct {
	URL, Username, Password, TOTPURI string

	t       TB
	o       Options
	backend *server.Server
	pool    *pooled // where backend comes from
	proxy   *httputil.ReverseProxy
	totp    *otp.TOTP
	http    *http.Client

	mu        sync.Mutex
	calls     []call
	failures  map[userRoute][]failure       // user ("" = every user), "METHOD /path" → queued answers
	stalls    map[userRoute][]time.Duration // user ("" = every user), "METHOD /path" → queued stalls, by their bound
	perUser   bool                          // a per-user failure, stall, CAPTCHA or 2FA was set: serve resolves the user
	issued    map[string]bool               // access tokens handed out, not yet expired
	minted    []string                      // every access token handed out, in order
	expired   map[string]bool               // access tokens that answer 401
	unlocked  map[string]bool               // UIDs holding the locked scope
	users     map[string]string             // username → user ID
	passwords map[string]string             // username → password
	captchas  map[string]string             // user ("" = every user) → CAPTCHA token; Options.Captcha, SetCaptcha*
	twoFAs    map[string]userTwoFA          // user → 2FA set by SetTwoFA, overriding Options.TwoFA
	codes     []string                      // 2FA codes received, in order
	sessions  map[string]sessionRec         // access token minted by Session → session
	logins    map[string]loginRec           // UID → session handed out by Login, not spent
	rotated   map[string]string             // refresh token issued by a refresh → its pair's access token

	cal calendars
}

type call struct{ method, path string }

// userRoute keys the queues of injected failures and stalls: user "" applies
// to every user.
type userRoute struct{ user, route string }

type userTwoFA struct {
	on   bool
	totp *otp.TOTP
	uri  string
}

type failure struct {
	status, code int
	retryAfter   string // the Retry-After header, "" for none
}

// New starts a fake with one user (Options.Username and Password, or
// throwaway values). It is closed when the test ends (t's cleanups run).
func New(t TB, o Options) *Fake {
	t.Helper()
	if len(o.CaptchaMethods) == 0 {
		o.CaptchaMethods = []string{"captcha"}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	p := borrow(t)
	target, err := url.Parse(p.srv.GetHostURL())
	if err != nil {
		t.Fatal(err)
	}
	if o.Username == "" {
		o.Username = "pcbfake"
	}
	if o.Password == "" {
		o.Password = defaultPassword()
	}
	if o.TOTPSecret == "" {
		o.TOTPSecret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random(t, 20))
	}
	f := &Fake{
		Username:  o.Username,
		Password:  o.Password,
		TOTPURI:   "otpauth://totp/pcbfake?secret=" + o.TOTPSecret,
		t:         t,
		o:         o,
		backend:   p.srv,
		pool:      p,
		proxy:     httputil.NewSingleHostReverseProxy(target),
		http:      &http.Client{Timeout: 10 * time.Second},
		failures:  map[userRoute][]failure{},
		stalls:    map[userRoute][]time.Duration{},
		captchas:  map[string]string{},
		twoFAs:    map[string]userTwoFA{},
		issued:    map[string]bool{},
		expired:   map[string]bool{},
		unlocked:  map[string]bool{},
		users:     map[string]string{},
		passwords: map[string]string{},
		sessions:  map[string]sessionRec{},
		logins:    map[string]loginRec{},
		rotated:   map[string]string{},
		cal:       calendars{byUser: map[string]*userCalendars{}, byID: map[string]*calendarRec{}},
	}
	if f.totp, err = otp.Parse(f.TOTPURI); err != nil {
		t.Fatal(err)
	}
	f.captchas[""] = o.Captcha
	t.Cleanup(f.giveBackLogins) // before borrow's: the backend comes back with them
	f.proxy.ModifyResponse = f.fixResponse
	f.AddUser(f.Username, f.Password)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	if o.Listener != nil {
		_ = ts.Listener.Close()
		ts.Listener = o.Listener
	}
	ts.Start()
	t.Cleanup(ts.Close)
	f.URL = ts.URL
	return f
}

func random(t TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// AddUser creates another account and returns its user ID. An account an
// earlier fake left on the pooled backend under that username is reused if
// its password matches, replaced otherwise.
func (f *Fake) AddUser(username, password string) (userID string) {
	f.t.Helper()
	id, err := f.pool.user(username, password)
	if err != nil {
		f.t.Fatalf("protontest: create user: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[username] = id
	f.passwords[username] = password
	return id
}

// UserID returns the user ID of username, "" if unknown.
func (f *Fake) UserID(username string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[username]
}

// Calls counts the requests received for method ("" for any) whose path is
// pathPrefix, or starts with it when pathPrefix ends with "/".
func (f *Fake) Calls(method, pathPrefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if method != "" && c.method != method {
			continue
		}
		if c.path == pathPrefix || (strings.HasSuffix(pathPrefix, "/") && strings.HasPrefix(c.path, pathPrefix)) {
			n++
		}
	}
	return n
}

// FailNext makes the next request to method and path (exact) answer status
// with Proton code code, and a Retry-After header when retryAfter > 0. Calls
// queue up: each answers one request.
func (f *Fake) FailNext(method, path string, status, code, retryAfter int) {
	f.FailNextFor("", method, path, status, code, retryAfter)
}

// FailNextFor is FailNext for the requests of username only ("" for every
// user). The user of a request is its session's, or on POST /auth/v4 and
// /auth/v4/info the Username it names.
func (f *Fake) FailNextFor(username, method, path string, status, code, retryAfter int) {
	ra := ""
	if retryAfter > 0 {
		ra = strconv.Itoa(retryAfter)
	}
	f.failNext(username, method, path, failure{status, code, ra})
}

// FailNextRetryAt is FailNext with Retry-After in its HTTP-date form, at.
func (f *Fake) FailNextRetryAt(method, path string, status, code int, at time.Time) {
	f.failNext("", method, path, failure{status, code, at.UTC().Format(http.TimeFormat)})
}

func (f *Fake) failNext(user, method, path string, fl failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.perUser = f.perUser || user != ""
	key := userRoute{user, method + " " + path}
	f.failures[key] = append(f.failures[key], fl)
}

// StallNext makes the next request to method and path (exact) wait without
// an answer until the client gives up, or at most bound, after which it is
// answered as usual. Calls queue up: each stalls one request.
func (f *Fake) StallNext(method, path string, bound time.Duration) {
	f.StallNextFor("", method, path, bound)
}

// StallNextFor is StallNext for the requests of username only ("" for every
// user), as FailNextFor names them.
func (f *Fake) StallNextFor(username, method, path string, bound time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.perUser = f.perUser || username != ""
	key := userRoute{username, method + " " + path}
	f.stalls[key] = append(f.stalls[key], bound)
}

// SetCaptcha makes later logins answer the CAPTCHA challenge token, like
// Options.Captcha; "" stops challenging.
func (f *Fake) SetCaptcha(token string) {
	f.SetCaptchaFor("", token)
}

// SetCaptchaFor is SetCaptcha for the logins of username only ("" for every
// user). A token set for a user, "" included, wins over the fake-wide one.
func (f *Fake) SetCaptchaFor(username, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.perUser = f.perUser || username != ""
	f.captchas[username] = token
}

// SetTwoFA turns 2FA on or off for username, overriding Options.TwoFA, and
// returns the otpauth URI whose codes /auth/v4/2fa then accepts for that user.
func (f *Fake) SetTwoFA(username string, on bool) (totpURI string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.twoFAs[username]
	if !ok {
		rec.uri = "otpauth://totp/pcbfake?secret=" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random(f.t, 20))
		var err error
		if rec.totp, err = otp.Parse(rec.uri); err != nil {
			f.t.Fatal(err)
		}
	}
	rec.on = on
	f.twoFAs[username] = rec
	return rec.uri
}

// TOTPCodes returns the 2FA codes received so far, in order.
func (f *Fake) TOTPCodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.codes...)
}

// AccessTokens returns every access token the fake has handed out, expired or
// not, so a test can check that none leaks into a message.
func (f *Fake) AccessTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.minted...)
}

// ExpireAccessTokens makes every access token handed out so far answer 401
// until it is refreshed.
func (f *Fake) ExpireAccessTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for tok := range f.issued {
		f.expired[tok] = true
	}
	clear(f.issued)
}

// RevokeSessions ends every existing session: its requests answer 401 and its
// refresh 422/10013.
func (f *Fake) RevokeSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, id := range f.users {
		if err := f.backend.RevokeUser(id); err != nil {
			f.t.Errorf("protontest: revoke sessions: %v", err)
		}
		f.pool.dropSessions(name) // the idle ones died too
	}
	clear(f.unlocked)
	clear(f.sessions)
	clear(f.logins)
}

// Session returns live tokens for username's account without an SRP login:
// the fake mints them itself and accepts them on the calendar events and
// change-feed routes (/calendar/v1/{cal}/events, /events/{id},
// /modelevents/...), where it owns the answer. The backend does not know them,
// so every other route (calendar list and bootstrap, keys, addresses) answers
// 401 and a refresh fails: a test needing those logs in. The tokens expire
// (ExpireAccessTokens) and die (RevokeSessions) like issued ones.
func (f *Fake) Session(t TB, username string) api.Tokens {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[username]; !ok {
		t.Fatalf("protontest: unknown user %q", username)
	}
	tk := api.Tokens{
		UID:          "fakeuid" + hex.EncodeToString(random(t, 8)),
		AccessToken:  "fakeacc" + hex.EncodeToString(random(t, 16)),
		RefreshToken: "fakeref" + hex.EncodeToString(random(t, 16)),
	}
	f.sessions[tk.AccessToken] = sessionRec{username, tk.UID}
	f.issued[tk.AccessToken] = true
	f.minted = append(f.minted, tk.AccessToken)
	return tk
}

type loginRec struct {
	user string
	tok  api.Tokens
}

// Login returns live tokens of username's account, as a successful SRP login
// would, without its cost (about 1.5 s of CPU under -race) on every test: the
// session is opened by SRP against the backend directly, so this fake's
// Calls do not count it, and kept with the pooled backend once the test ends,
// for a later test to take. A session the test spent (any /auth/ call on it:
// refresh, logout, 2FA; RevokeSessions) is not kept. The tokens expire
// (ExpireAccessTokens) like issued ones. A test of the login itself logs in.
func (f *Fake) Login(t TB, username string) api.Tokens {
	t.Helper()
	f.mu.Lock()
	password, ok := f.passwords[username]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("protontest: unknown user %q", username)
	}
	tok, ok := f.pool.takeSession(username)
	if !ok {
		m := gpa.New(gpa.WithHostURL(f.backend.GetHostURL()), gpa.WithAppVersion(appVersion), gpa.WithRetryCount(0))
		defer m.Close()
		// TB has no Context (a Process holds the fake too): the login is bounded instead.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, a, err := m.NewClientWithLogin(ctx, username, []byte(password))
		if err != nil {
			t.Fatalf("protontest: login %q: %v", username, err)
		}
		c.Close()
		tok = api.Tokens{UID: a.UID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins[tok.UID] = loginRec{username, tok}
	f.issued[tok.AccessToken] = true
	f.minted = append(f.minted, tok.AccessToken)
	return tok
}

// spend forgets the Login session an /auth/ call names (refreshed, revoked
// or 2FA'd: not to be handed out again): in x-pm-uid, or in the body's UID
// as go-proton-api's refresh sends it. The body is put back for the route.
func (f *Fake) spend(r *http.Request) {
	uids := []string{r.Header.Get("x-pm-uid")}
	if body, err := io.ReadAll(r.Body); err == nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		var named struct{ UID string }
		if json.Unmarshal(body, &named) == nil {
			uids = append(uids, named.UID)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, uid := range uids {
		delete(f.logins, uid)
	}
}

// giveBackLogins keeps the sessions Login handed out and the test did not
// spend with the pooled backend.
func (f *Fake) giveBackLogins() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.logins {
		f.pool.putSession(l.user, l.tok)
	}
	clear(f.logins)
}

// sessionUser returns the user of a token minted by Session. The token must
// not have expired (serve answers 401 before a route sees an expired one).
func (f *Fake) sessionUser(r *http.Request) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[bearer(r)]
	return s.user, ok && s.uid == r.Header.Get("x-pm-uid")
}

type sessionRec struct{ user, uid string }

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	route := r.Method + " " + r.URL.Path
	if strings.HasPrefix(r.URL.Path, "/auth/") {
		f.spend(r)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path})
	perUser := f.perUser
	f.mu.Unlock()
	user := ""
	if perUser {
		user = f.requestUser(r)
	}
	f.mu.Lock()
	stall := f.popStall(user, route)
	f.mu.Unlock()
	if stall > 0 {
		// The server notices a client that gave up only once the body is
		// read: read it now, and serve it again after the stall.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		t := time.NewTimer(stall)
		select {
		case <-r.Context().Done(): // the client gave up: nothing to answer
			t.Stop()
			return
		case <-t.C:
		}
	}
	f.mu.Lock()
	fail, injected := f.popFailure(user, route)
	expired := f.expired[bearer(r)] && r.URL.Path != "/auth/v4/refresh"
	f.mu.Unlock()
	switch {
	case injected:
		if fail.retryAfter != "" {
			w.Header().Set("Retry-After", fail.retryAfter)
		}
		reply(w, fail.status, fail.code, "injected failure")
	case expired:
		reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
	case route == "POST /auth/v4/refresh" && f.refusesRefresh(r):
		reply(w, http.StatusBadRequest, api.CodeRefreshInvalid, "Invalid refresh token")
	case route == "POST /auth/v4" && f.challenged(r, user):
		WriteChallenge(w, f.captchaToken(user), f.o.CaptchaMethods)
	case strings.HasPrefix(r.URL.Path, "/calendar/") || route == "GET /settings/calendar":
		f.serveCalendar(w, r)
	case route == "POST /auth/v4/2fa":
		f.twoFA(w, r)
	case route == "PUT /core/v4/users/unlock":
		f.unlock(w, r)
	case route == "PUT /core/v4/users/lock":
		f.lock(w, r)
	case route == "GET /core/v4/keys/salts" && f.o.LockedSalts && !f.isUnlocked(r):
		if _, ok := f.whoami(r); !ok {
			reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
			return
		}
		reply(w, http.StatusForbidden, 9101, "Access token does not have sufficient scope")
	default:
		f.proxy.ServeHTTP(w, r)
	}
}

// refusesRefresh reports a refresh Proton refuses before looking further
// (measured live 2026-11-12, spec/experiments/E01): the refresh token was
// issued by a refresh and the body does not carry that pair's access token.
// A login's refresh token needs none. The refusal does not spend the token.
// The body is put back for the route.
func (f *Fake) refusesRefresh(r *http.Request) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct{ RefreshToken, AccessToken string }
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	acc, rotated := f.rotated[req.RefreshToken]
	return rotated && req.AccessToken != acc
}

// requestUser returns the user of r: the session's, or on POST /auth/v4 and
// /auth/v4/info the Username of its body; "" when unknown.
func (f *Fake) requestUser(r *http.Request) string {
	if r.Method == http.MethodPost && (r.URL.Path == "/auth/v4" || r.URL.Path == "/auth/v4/info") {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return ""
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var named struct{ Username string }
		_ = json.Unmarshal(body, &named)
		return named.Username
	}
	if name, ok := f.sessionUser(r); ok {
		return name
	}
	name, _ := f.whoami(r)
	return name
}

// popFailure takes the next failure queued for user, else the next one queued
// for every user.
func (f *Fake) popFailure(user, route string) (failure, bool) {
	for _, u := range []string{user, ""} {
		key := userRoute{u, route}
		if q := f.failures[key]; len(q) > 0 {
			f.failures[key] = q[1:]
			return q[0], true
		}
	}
	return failure{}, false
}

func (f *Fake) popStall(user, route string) time.Duration {
	for _, u := range []string{user, ""} {
		key := userRoute{u, route}
		if q := f.stalls[key]; len(q) > 0 {
			f.stalls[key] = q[1:]
			return q[0]
		}
	}
	return 0
}

func (f *Fake) captchaToken(user string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if token, ok := f.captchas[user]; ok {
		return token
	}
	return f.captchas[""]
}

func (f *Fake) challenged(r *http.Request, user string) bool {
	token := f.captchaToken(user)
	return token != "" &&
		(r.Header.Get("x-pm-human-verification-token") != token ||
			r.Header.Get("x-pm-human-verification-token-type") != "captcha")
}

func (f *Fake) isUnlocked(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unlocked[r.Header.Get("x-pm-uid")]
}

// twoFA accepts the codes of the current and the previous TOTP period.
func (f *Fake) twoFA(w http.ResponseWriter, r *http.Request) {
	name, ok := f.whoami(r)
	if !ok {
		reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
		return
	}
	var req struct{ TwoFactorCode string }
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.codes = append(f.codes, req.TwoFactorCode)
	on, totp := f.o.TwoFA, f.totp
	if rec, ok := f.twoFAs[name]; ok {
		on, totp = rec.on, rec.totp
	}
	f.mu.Unlock()
	now := f.o.Now()
	if !on || (req.TwoFactorCode != totp.Code(now) && req.TwoFactorCode != totp.Code(now.Add(-30*time.Second))) {
		reply(w, http.StatusUnprocessableEntity, CodeTOTPWrong, "Incorrect code")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Code": 1000, "Scopes": []string{"full", "self", "locked"}})
}

// unlock checks the SRP proof by replaying it to the backend's POST /auth/v4
// for the session's user, then drops the session that replay opened.
func (f *Fake) unlock(w http.ResponseWriter, r *http.Request) {
	name, ok := f.whoami(r)
	if !ok {
		reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
		return
	}
	var req struct{ ClientEphemeral, ClientProof, SRPSession string }
	_ = json.NewDecoder(r.Body).Decode(&req)
	var auth struct{ UID, AccessToken, ServerProof string }
	status, err := f.backendCall(http.MethodPost, "/auth/v4", nil, map[string]string{
		"Username": name, "ClientEphemeral": req.ClientEphemeral, "ClientProof": req.ClientProof, "SRPSession": req.SRPSession,
	}, &auth)
	if err != nil || status != http.StatusOK {
		reply(w, http.StatusUnprocessableEntity, CodePasswordWrong, "Incorrect login credentials. Please try again.")
		return
	}
	hdr := http.Header{"x-pm-uid": {auth.UID}, "Authorization": {"Bearer " + auth.AccessToken}}
	if _, err := f.backendCall(http.MethodDelete, "/auth/v4", hdr, nil, nil); err != nil {
		f.t.Errorf("protontest: drop the unlock replay session: %v", err)
	}
	f.mu.Lock()
	f.unlocked[r.Header.Get("x-pm-uid")] = true
	f.mu.Unlock()
	answer := map[string]any{"Code": 1000, "ServerProof": auth.ServerProof}
	switch {
	case f.o.EmptyUnlockProof:
		delete(answer, "ServerProof")
	case f.o.BadUnlockProof:
		answer["ServerProof"] = corrupt(auth.ServerProof)
	}
	writeJSON(w, http.StatusOK, answer)
}

func (f *Fake) lock(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.whoami(r); !ok {
		reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
		return
	}
	f.mu.Lock()
	delete(f.unlocked, r.Header.Get("x-pm-uid"))
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"Code": 1000})
}

// whoami authenticates r's session against the backend and returns its
// username.
func (f *Fake) whoami(r *http.Request) (string, bool) {
	hdr := http.Header{"x-pm-uid": {r.Header.Get("x-pm-uid")}, "Authorization": {r.Header.Get("Authorization")}}
	var out struct{ User struct{ Name string } }
	status, err := f.backendCall(http.MethodGet, "/core/v4/users", hdr, nil, &out)
	return out.User.Name, err == nil && status == http.StatusOK && out.User.Name != ""
}

func (f *Fake) backendCall(method, path string, hdr http.Header, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, f.backend.GetHostURL()+path, rd)
	if err != nil {
		return 0, err
	}
	for k, v := range hdr {
		req.Header[http.CanonicalHeaderKey(k)] = v
	}
	req.Header.Set("x-pm-appversion", appVersion)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// fixResponse gives the backend's answers Proton's shape: a Code on every
// success, the account options on POST /auth/v4 and Proton's error codes
// where the backend answers a bare status.
func (f *Fake) fixResponse(resp *http.Response) error {
	route := resp.Request.Method + " " + resp.Request.URL.Path
	switch {
	case resp.StatusCode == http.StatusOK:
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		body := map[string]any{}
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &body); err != nil { // not JSON: leave it
				resp.Body = io.NopCloser(bytes.NewReader(data))
				return nil
			}
		}
		if _, ok := body["Code"]; !ok { // Proton always sends one; the backend does not
			body["Code"] = 1000
		}
		if route == "POST /auth/v4" {
			f.shapeAuth(body)
		}
		if tok, ok := body["AccessToken"].(string); ok && (route == "POST /auth/v4" || route == "POST /auth/v4/refresh") {
			f.mu.Lock()
			f.issued[tok] = true
			f.minted = append(f.minted, tok)
			if ref, ok := body["RefreshToken"].(string); ok && route == "POST /auth/v4/refresh" {
				f.rotated[ref] = tok
			}
			f.mu.Unlock()
		}
		return setBody(resp, http.StatusOK, body)
	case resp.StatusCode == http.StatusUnauthorized && (route == "POST /auth/v4" || route == "POST /auth/v4/info"):
		// The backend answers 401 to an unknown user or a wrong proof.
		return setBody(resp, http.StatusUnprocessableEntity, errBody(CodePasswordWrong, "Incorrect login credentials. Please try again."))
	case route == "POST /auth/v4/refresh" && resp.StatusCode >= 400 && resp.StatusCode < 500:
		return setBody(resp, http.StatusUnprocessableEntity, errBody(10013, "Invalid refresh token"))
	case resp.StatusCode == http.StatusUnauthorized:
		return setBody(resp, http.StatusUnauthorized, errBody(http.StatusUnauthorized, "Invalid access token"))
	}
	return nil
}

func (f *Fake) shapeAuth(body map[string]any) {
	switch {
	case f.o.FIDO2Only:
		body["2FA"] = map[string]any{"Enabled": 2}
	case f.twoFAOn(body["UserID"]):
		body["2FA"] = map[string]any{"Enabled": 1}
	}
	if f.o.TwoPassword {
		body["PasswordMode"] = 2
	}
	if proof, ok := body["ServerProof"].(string); ok && f.o.BadLoginProof {
		body["ServerProof"] = corrupt(proof)
	}
}

// twoFAOn tells whether 2FA is on for the user whose ID is userID.
func (f *Fake) twoFAOn(userID any) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, id := range f.users {
		if id == userID {
			if rec, ok := f.twoFAs[name]; ok {
				return rec.on
			}
		}
	}
	return f.o.TwoFA
}

// corrupt flips the first byte of a base64 proof.
func corrupt(proof string) string {
	b, _ := base64.StdEncoding.DecodeString(proof)
	if len(b) == 0 {
		b = []byte{0}
	}
	b[0] ^= 0xff
	return base64.StdEncoding.EncodeToString(b)
}

// WriteChallenge answers a login with Proton's human-verification demand
// (422, code 9001) for token and methods.
func WriteChallenge(w http.ResponseWriter, token string, methods []string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"Code": 9001, "Error": "Human verification required",
		"Details": map[string]any{
			"HumanVerificationMethods": methods,
			"HumanVerificationToken":   token,
		},
	})
}

func setBody(resp *http.Response, status int, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
	resp.Header.Set("Content-Type", "application/json")
	return nil
}

func errBody(code int, msg string) map[string]any {
	return map[string]any{"Code": code, "Error": msg}
}

func reply(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, errBody(code, msg))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
