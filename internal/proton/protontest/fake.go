// Package protontest is the fake Proton every C1, C3 and CLI test shares: an
// httptest front over go-proton-api's in-memory server that adds what the
// server lacks (Proton's envelope and error codes, CAPTCHA, TOTP, two-password
// and FIDO2 accounts, the locked scope, token expiry, revocation and injected
// failures). It never contacts Proton. Test use only.
//
// Backends and their accounts are pooled per test binary (see pool), so a
// fake is cheap while its crypto stays real.
package protontest

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	// Write selects the other reading of each unsettled write behaviour
	// (see WriteOptions).
	Write WriteOptions
}

// Fake is a running fake Proton. URL, Username, Password and TOTPURI are the
// fake's own throwaway values.
type Fake struct {
	URL, Username, Password, TOTPURI string

	t       testing.TB
	o       Options
	backend *server.Server
	pool    *pooled // where backend comes from
	proxy   *httputil.ReverseProxy
	totp    *otp.TOTP
	http    *http.Client

	mu        sync.Mutex
	calls     []call
	failures  map[string][]failure  // "METHOD /path" → queued answers
	issued    map[string]bool       // access tokens handed out, not yet expired
	minted    []string              // every access token handed out, in order
	expired   map[string]bool       // access tokens that answer 401
	unlocked  map[string]bool       // UIDs holding the locked scope
	users     map[string]string     // username → user ID
	passwords map[string]string     // username → password
	captcha   string                // Options.Captcha, changed by SetCaptcha
	codes     []string              // 2FA codes received, in order
	sessions  map[string]sessionRec // access token minted by Session → session

	cal calendars
}

type call struct{ method, path string }

type failure struct{ status, code, retryAfter int }

// New starts a fake with one user (Options.Username and Password, or
// throwaway values). It is closed when the test ends.
func New(t testing.TB, o Options) *Fake {
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
	f := &Fake{
		Username:  o.Username,
		Password:  o.Password,
		TOTPURI:   "otpauth://totp/pcbfake?secret=" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random(t, 20)),
		t:         t,
		o:         o,
		backend:   p.srv,
		pool:      p,
		proxy:     httputil.NewSingleHostReverseProxy(target),
		http:      &http.Client{Timeout: 10 * time.Second},
		failures:  map[string][]failure{},
		issued:    map[string]bool{},
		expired:   map[string]bool{},
		unlocked:  map[string]bool{},
		users:     map[string]string{},
		passwords: map[string]string{},
		sessions:  map[string]sessionRec{},
		cal:       calendars{byUser: map[string]*userCalendars{}, byID: map[string]*calendarRec{}},
	}
	if f.totp, err = otp.Parse(f.TOTPURI); err != nil {
		t.Fatal(err)
	}
	f.captcha = o.Captcha
	f.proxy.ModifyResponse = f.fixResponse
	f.AddUser(f.Username, f.Password)
	ts := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)
	f.URL = ts.URL
	return f
}

func random(t testing.TB, n int) []byte {
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
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + path
	f.failures[key] = append(f.failures[key], failure{status, code, retryAfter})
}

// SetCaptcha makes later logins answer the CAPTCHA challenge token, like
// Options.Captcha; "" stops challenging.
func (f *Fake) SetCaptcha(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captcha = token
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
	for _, id := range f.users {
		if err := f.backend.RevokeUser(id); err != nil {
			f.t.Errorf("protontest: revoke sessions: %v", err)
		}
	}
	clear(f.unlocked)
	clear(f.sessions)
}

// Session returns live tokens for username's account without an SRP login:
// the fake mints them itself and accepts them on the calendar events and
// change-feed routes (/calendar/v1/{cal}/events, /events/{id},
// /modelevents/...), where it owns the answer. The backend does not know them,
// so every other route (calendar list and bootstrap, keys, addresses) answers
// 401 and a refresh fails: a test needing those logs in. The tokens expire
// (ExpireAccessTokens) and die (RevokeSessions) like issued ones.
func (f *Fake) Session(t testing.TB, username string) api.Tokens {
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
	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path})
	fail, injected := f.popFailure(route)
	expired := f.expired[bearer(r)] && r.URL.Path != "/auth/v4/refresh"
	f.mu.Unlock()
	switch {
	case injected:
		if fail.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(fail.retryAfter))
		}
		reply(w, fail.status, fail.code, "injected failure")
	case expired:
		reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
	case route == "POST /auth/v4" && f.challenged(r):
		WriteChallenge(w, f.captchaToken(), f.o.CaptchaMethods)
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

func (f *Fake) popFailure(route string) (failure, bool) {
	q := f.failures[route]
	if len(q) == 0 {
		return failure{}, false
	}
	f.failures[route] = q[1:]
	return q[0], true
}

func (f *Fake) captchaToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captcha
}

func (f *Fake) challenged(r *http.Request) bool {
	token := f.captchaToken()
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
	if _, ok := f.whoami(r); !ok {
		reply(w, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid access token")
		return
	}
	var req struct{ TwoFactorCode string }
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.codes = append(f.codes, req.TwoFactorCode)
	f.mu.Unlock()
	now := f.o.Now()
	if !f.o.TwoFA || (req.TwoFactorCode != f.totp.Code(now) && req.TwoFactorCode != f.totp.Code(now.Add(-30*time.Second))) {
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
	case f.o.TwoFA:
		body["2FA"] = map[string]any{"Enabled": 1}
	}
	if f.o.TwoPassword {
		body["PasswordMode"] = 2
	}
	if proof, ok := body["ServerProof"].(string); ok && f.o.BadLoginProof {
		body["ServerProof"] = corrupt(proof)
	}
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
