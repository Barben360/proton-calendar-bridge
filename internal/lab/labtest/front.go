// Package labtest is the fake Proton the lab's tests share: an httptest proxy
// in front of protontest's fake. Test use only.
package labtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

const (
	User = "lab"
	Pass = "lab-pass-not-a-secret"
)

// Reply is a canned answer for one "METHOD /path".
type Reply struct {
	Status int
	Body   string // JSON
	Once   bool   // answer only the next matching request
}

// Seen is one request that reached the fake.
type Seen struct {
	Method, Path, AppVersion, Authorization, UserAgent string
}

// Front can answer /auth/v4 with a CAPTCHA challenge, fake a TOTP account and
// answer chosen paths with canned replies. Set the exported fields before the
// first request; read them under Mu.
type Front struct {
	Mu        sync.Mutex
	AuthCalls int      // POST /auth/v4
	HVHeaders []string // human-verification token header per POST /auth/v4
	Codes     []string // 2FA codes received
	Requests  int      // every request
	Challenge string   // non-empty: answer POST /auth/v4 with 9001 and this token
	TwoFA     bool
	Replies   map[string]Reply // key "METHOD /path": answered instead of the backend
	Log       []Seen
	URL       string
	// Options shape the fake behind the front (its Username, Password and
	// TwoFA are the front's own); Backend is that fake, for calendars and
	// its records.
	Options protontest.Options
	Backend *protontest.Fake
}

// NewFront starts the fake with one user (User, Pass). It is a lab-specific
// layer (request log, canned replies, a changeable challenge, any 2FA code
// accepted) over protontest.Fake, which supplies the backend and Proton's
// response shape.
func NewFront(t *testing.T, f *Front) *Front {
	t.Helper()
	o := f.Options
	o.TwoFA, o.Username, o.Password = f.TwoFA, User, Pass
	fake := protontest.New(t, o)
	f.Backend = fake
	target, err := url.Parse(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Mu.Lock()
		f.Requests++
		f.Log = append(f.Log, Seen{r.Method, r.URL.Path, r.Header.Get("x-pm-appversion"), r.Header.Get("Authorization"), r.Header.Get("User-Agent")})
		reply, canned := f.Replies[r.Method+" "+r.URL.Path]
		if canned && reply.Once {
			delete(f.Replies, r.Method+" "+r.URL.Path)
		}
		f.Mu.Unlock()
		if canned {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.Status)
			_, _ = io.WriteString(w, reply.Body)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/auth/v4":
			f.Mu.Lock()
			f.AuthCalls++
			f.HVHeaders = append(f.HVHeaders, r.Header.Get("x-pm-human-verification-token"))
			challenge := f.Challenge
			f.Mu.Unlock()
			if challenge != "" {
				protontest.WriteChallenge(w, challenge, []string{"captcha"})
				return
			}
		case r.Method == http.MethodPost && r.URL.Path == "/auth/v4/2fa":
			var req struct{ TwoFactorCode string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.Mu.Lock()
			f.Codes = append(f.Codes, req.TwoFactorCode)
			f.Mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": 1000, "Scopes": []string{"full"}})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	f.URL = ts.URL
	return f
}

// R builds a Reply.
func R(status int, body string) Reply { return Reply{Status: status, Body: body} }
