// Package labtest is the fake Proton the lab's tests share: an httptest proxy
// in front of go-proton-api's in-memory server. Test use only.
package labtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-proton-api/server"
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
}

// NewFront starts the fake with one user (User, Pass).
func NewFront(t *testing.T, f *Front) *Front {
	t.Helper()
	backend := server.New(server.WithTLS(false))
	t.Cleanup(backend.Close)
	if _, _, err := backend.CreateUser(User, []byte(Pass)); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(backend.GetHostURL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
			return nil
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return err
		}
		if _, ok := body["Code"]; !ok { // Proton always sends one; the backend does not
			body["Code"] = 1000
		}
		if f.TwoFA && resp.Request.Method == http.MethodPost && resp.Request.URL.Path == "/auth/v4" {
			body["2FA"] = map[string]any{"Enabled": 1}
		}
		data, _ := json.Marshal(body)
		resp.Body = io.NopCloser(bytes.NewReader(data))
		resp.ContentLength = int64(len(data))
		resp.Header.Del("Content-Length")
		return nil
	}
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
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Code": 9001, "Error": "Human verification required",
					"Details": map[string]any{
						"HumanVerificationMethods": []string{"captcha"},
						"HumanVerificationToken":   challenge,
					},
				})
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
