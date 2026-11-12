package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

var tok = api.Tokens{UID: "uid-1", AccessToken: "access-secret", RefreshToken: "refresh-secret"}

// sleeps records injected sleeps without waiting.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func newClient(t *testing.T, h http.HandlerFunc, o api.Options) (*api.Client, *sleeps) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s := &sleeps{}
	o.BaseURL = srv.URL
	if o.Sleep == nil {
		o.Sleep = s.sleep
	}
	if o.Limiter == nil {
		o.Limiter = rate.NewLimiter(rate.Inf, 1)
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	return api.New(tok, o), s
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestSendsIdentityHeaders(t *testing.T) {
	var got http.Header
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{AppVersion: "Other", UserAgent: "pcb/test"})
	if err := c.Get(t.Context(), "/core/v4/users", nil, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"X-Pm-Uid":        "uid-1",
		"Authorization":   "Bearer access-secret",
		"X-Pm-Appversion": "Other",
		"User-Agent":      "pcb/test",
		"Accept":          "application/vnd.protonmail.v1+json",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
	if _, ok := got["Content-Type"]; ok {
		t.Error("GET carries Content-Type")
	}
}

func TestBodyIsJSONWithContentType(t *testing.T) {
	var ct string
	var body map[string]string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&body)
		reply(w, 200, `{"Code":1000,"Name":"x"}`)
	}, api.Options{})
	var out struct{ Name string }
	if err := c.Put(t.Context(), "/p", map[string]string{"k": "v"}, &out); err != nil {
		t.Fatal(err)
	}
	if ct != "application/json" || body["k"] != "v" || out.Name != "x" {
		t.Fatalf("ct=%q body=%v out=%v", ct, body, out)
	}
}

func TestQueryIsSent(t *testing.T) {
	var q url.Values
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	if err := c.Get(t.Context(), "/p", url.Values{"Page": {"2"}}, nil); err != nil {
		t.Fatal(err)
	}
	if q.Get("Page") != "2" {
		t.Fatalf("query %v", q)
	}
}

func TestDecodesProtonError(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok-status" {
			reply(w, 200, `{"Code":2001,"Error":"Invalid input"}`)
			return
		}
		reply(w, 422, `{"Code":2501,"Error":"Calendar does not exist"}`)
	}, api.Options{})
	err := c.Get(t.Context(), "/calendars/x", nil, nil)
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 422 || e.Code != 2501 || e.Message != "Calendar does not exist" {
		t.Fatalf("got %#v", err)
	}
	if !api.IsCode(err, api.CodeNotFound) || api.IsCode(err, api.CodeMulti) {
		t.Fatal("IsCode wrong")
	}
	if want := "Proton: Calendar does not exist (code 2501, HTTP 422)"; e.Error() != want {
		t.Fatalf("Error() = %q", e.Error())
	}
	err = c.Get(t.Context(), "/ok-status", nil, nil)
	if !api.IsCode(err, 2001) {
		t.Fatalf("2xx with bad code: %v", err)
	}
}

func TestMultiCodeIsSuccess(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"Code":1001,"Responses":[]}`)
	}, api.Options{})
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// refresher returns a Refresh func that rotates to "access-2" and counts calls.
func refresher(calls *atomic.Int32, stale *atomic.Value) func(context.Context, api.Tokens) (api.Tokens, error) {
	return func(_ context.Context, s api.Tokens) (api.Tokens, error) {
		calls.Add(1)
		if stale != nil {
			stale.Store(s)
		}
		time.Sleep(20 * time.Millisecond) // let concurrent callers pile up
		return api.Tokens{UID: s.UID, AccessToken: "access-2", RefreshToken: "refresh-2"}, nil
	}
}

func bearerHandler(seen *[]string, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		mu.Lock()
		*seen = append(*seen, a)
		mu.Unlock()
		if a != "Bearer access-2" {
			reply(w, 401, `{"Code":401,"Error":"Invalid access token"}`)
			return
		}
		reply(w, 200, `{"Code":1000}`)
	}
}

func TestRefreshOn401ThenRetryOnce(t *testing.T) {
	var calls atomic.Int32
	var stale atomic.Value
	var seen []string
	var mu sync.Mutex
	c, _ := newClient(t, bearerHandler(&seen, &mu), api.Options{Refresh: refresher(&calls, &stale)})
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatal(err)
	}
	if s := stale.Load().(api.Tokens); s != tok {
		t.Fatalf("Refresh got %v", s)
	}
	if got := strings.Join(seen, "|"); got != "Bearer access-secret|Bearer access-2" {
		t.Fatalf("requests %q", got)
	}
	if c.Tokens().AccessToken != "access-2" || c.Tokens().RefreshToken != "refresh-2" {
		t.Fatalf("tokens %v", c.Tokens())
	}
}

func TestSecond401IsSessionEnded(t *testing.T) {
	var calls atomic.Int32
	var n atomic.Int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		reply(w, 401, `{"Code":401,"Error":"Invalid access token"}`)
	}, api.Options{Refresh: refresher(&calls, nil)})
	err := c.Get(t.Context(), "/p", nil, nil)
	if !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("got %v", err)
	}
	if n.Load() != 2 || calls.Load() != 1 {
		t.Fatalf("requests=%d refreshes=%d", n.Load(), calls.Load())
	}
}

func TestRefreshFailureEndsSession(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 401, `{"Code":401,"Error":"x"}`)
	}, api.Options{Refresh: func(context.Context, api.Tokens) (api.Tokens, error) {
		return api.Tokens{}, fmt.Errorf("refresh token used: %w", api.ErrSessionEnded)
	}})
	if err := c.Get(t.Context(), "/p", nil, nil); !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("got %v", err)
	}
}

func TestNoRefreshFuncEndsSession(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 401, `{"Code":401,"Error":"x"}`)
	}, api.Options{})
	if err := c.Get(t.Context(), "/p", nil, nil); !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrent401sRefreshOnce(t *testing.T) {
	var calls atomic.Int32
	var seen []string
	var mu sync.Mutex
	c, _ := newClient(t, bearerHandler(&seen, &mu), api.Options{Refresh: refresher(&calls, nil)})
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Get(t.Context(), "/p", nil, nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("Refresh called %d times", calls.Load())
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	var n atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			reply(w, 429, `{"Code":85131,"Error":"Too many requests"}`)
			return
		}
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 || len(s.d) != 1 || s.d[0] != 3*time.Second {
		t.Fatalf("requests=%d sleeps=%v", n.Load(), s.d)
	}
}

func TestRetryAfterGivesUpAfterThreeAttempts(t *testing.T) {
	var n atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "1")
		reply(w, 429, `{"Code":85131,"Error":"Too many requests"}`)
	}, api.Options{})
	err := c.Get(t.Context(), "/p", nil, nil)
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 429 || n.Load() != 3 || len(s.d) != 2 {
		t.Fatalf("err=%v requests=%d sleeps=%v", err, n.Load(), s.d)
	}
}

func TestRetryAfterTooLongFails(t *testing.T) {
	var n atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "600")
		reply(w, 429, `{"Code":85131,"Error":"Too many requests"}`)
	}, api.Options{})
	err := c.Get(t.Context(), "/p", nil, nil)
	var e *api.Error
	if !errors.As(err, &e) || e.RetryAfter != 600*time.Second {
		t.Fatalf("got %#v", err)
	}
	if n.Load() != 1 || len(s.d) != 0 {
		t.Fatalf("requests=%d sleeps=%v", n.Load(), s.d)
	}
}

func TestGET5xxBacksOff(t *testing.T) {
	var n atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			reply(w, 503, `{"Code":0,"Error":"down"}`)
			return
		}
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 || len(s.d) != 2 {
		t.Fatalf("requests=%d sleeps=%v", n.Load(), s.d)
	}
	if s.d[0] < 800*time.Millisecond || s.d[0] > 1200*time.Millisecond ||
		s.d[1] < 1600*time.Millisecond || s.d[1] > 2400*time.Millisecond {
		t.Fatalf("sleeps out of range: %v", s.d)
	}
}

func TestGET5xxGivesUpAfterThree(t *testing.T) {
	var n atomic.Int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		reply(w, 502, `bad gateway`)
	}, api.Options{})
	err := c.Get(t.Context(), "/p", nil, nil)
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 502 || n.Load() != 3 {
		t.Fatalf("err=%v requests=%d", err, n.Load())
	}
}

func TestWritesNeverRetriedOn5xx(t *testing.T) {
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPost} {
		var n atomic.Int32
		c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			reply(w, 503, `{"Code":0,"Error":"down"}`)
		}, api.Options{})
		if err := c.Do(t.Context(), m, "/p", nil, map[string]int{"a": 1}, nil); err == nil {
			t.Fatalf("%s: no error", m)
		}
		if n.Load() != 1 || len(s.d) != 0 {
			t.Fatalf("%s: requests=%d sleeps=%v", m, n.Load(), s.d)
		}
	}
}

func TestPUTNeverRetried(t *testing.T) {
	var n atomic.Int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		reply(w, 503, `{"Code":0,"Error":"down"}`)
	}, api.Options{})
	if err := c.Put(t.Context(), "/p", map[string]int{"a": 1}, nil); err == nil || n.Load() != 1 {
		t.Fatalf("err=%v requests=%d", err, n.Load())
	}
}

func TestWriteNetworkErrorNotRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	c := api.New(tok, api.Options{BaseURL: srv.URL, Limiter: rate.NewLimiter(rate.Inf, 1),
		Sleep: func(context.Context, time.Duration) error { return nil }})
	if err := c.Delete(t.Context(), "/p", nil); err == nil || n.Load() != 1 {
		t.Fatalf("err=%v requests=%d", err, n.Load())
	}
}

func TestErrorsCarryNoSecrets(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	c := api.New(tok, api.Options{BaseURL: base, Limiter: rate.NewLimiter(rate.Inf, 1),
		Sleep: func(context.Context, time.Duration) error { return nil }})
	for _, call := range []func() error{
		func() error { return c.Put(t.Context(), "/core/v4/x", map[string]string{"Password": "hunter2"}, nil) },
		func() error {
			return c.Get(t.Context(), "/core/v4/x", url.Values{"Email": {"hunter2@example.com"}}, nil)
		},
	} {
		err := call()
		if err == nil {
			t.Fatal("expected error")
		}
		for _, bad := range []string{"access-secret", "refresh-secret", "hunter2", "Bearer"} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("error leaks %q: %v", bad, err)
			}
		}
		if !strings.Contains(err.Error(), "/core/v4/x") {
			t.Errorf("error lacks the path: %v", err)
		}
	}
}

func TestProtonErrorCarriesNoRequestData(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 400, `{"Code":2000,"Error":"bad"}`)
	}, api.Options{})
	err := c.Put(t.Context(), "/p", map[string]string{"Password": "hunter2"}, nil)
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "access-secret") {
		t.Fatalf("got %v", err)
	}
	var e *api.Error
	if !errors.As(err, &e) || e.Method != "PUT" || e.Path != "/p" {
		t.Fatalf("no method/path: %#v", err)
	}
}

func TestLimiterSpacesRequests(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{Limiter: rate.NewLimiter(rate.Every(20*time.Millisecond), 1)})
	start := time.Now()
	for range 3 {
		if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 40*time.Millisecond {
		t.Fatalf("3 requests took %v", el)
	}
}

func TestCancelledContextStops(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Get(ctx, "/p", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestTokensRedact(t *testing.T) {
	for _, s := range []string{tok.String(), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok), tok.LogValue().String()} {
		if strings.Contains(s, "access-secret") || strings.Contains(s, "refresh-secret") {
			t.Fatalf("leak: %s", s)
		}
	}
}

func TestRefreshSurvivesRequesterCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var refreshCtxErr atomic.Value
	refresh := func(ctx context.Context, s api.Tokens) (api.Tokens, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return api.Tokens{}, errors.New("test timeout")
		}
		refreshCtxErr.Store(fmt.Sprint(ctx.Err()))
		return api.Tokens{UID: s.UID, AccessToken: "access-2", RefreshToken: "refresh-2"}, nil
	}
	var seen []string
	var mu sync.Mutex
	c, _ := newClient(t, bearerHandler(&seen, &mu), api.Options{Refresh: refresh})

	ctxA, cancelA := context.WithCancel(t.Context())
	errA := make(chan error, 1)
	go func() { errA <- c.Get(ctxA, "/p", nil, nil) }()
	<-started
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- c.Get(t.Context(), "/p", nil, nil) }()
	}
	time.Sleep(50 * time.Millisecond) // let the waiters reach the lock
	cancelA()
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("waiter: %v", err)
		}
	}
	if err := <-errA; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: %v", err)
	}
	if v := refreshCtxErr.Load(); v != "<nil>" {
		t.Fatalf("Refresh ctx was cancelled: %v", v)
	}
	if calls.Load() != 1 || c.Tokens().AccessToken != "access-2" {
		t.Fatalf("calls=%d tokens=%v", calls.Load(), c.Tokens())
	}
}

func TestPathValidation(t *testing.T) {
	var n atomic.Int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	for _, p := range []string{"core", "", "/a?x=1", "/a#f", "/a/../b", "/..", `/a\b`, "/a\nb", "/a\x00b", "/a\x7fb"} {
		if err := c.Get(t.Context(), p, nil, nil); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("%d requests sent for refused paths", n.Load())
	}
}

func TestPathWithEscapedIDIsSentAsIs(t *testing.T) {
	var raw string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.EscapedPath()
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	id := "ab/c==" // escaped by the caller
	if err := c.Get(t.Context(), "/calendar/v1/"+url.PathEscape(id)+"/events", nil, nil); err != nil {
		t.Fatal(err)
	}
	if raw != "/calendar/v1/ab%2Fc==/events" {
		t.Fatalf("path %q", raw)
	}
}

func TestBaseURLWithTrailingSlashHasNoDoubleSlash(t *testing.T) {
	var p string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = r.URL.Path
		reply(w, 200, `{"Code":1000}`)
	}))
	t.Cleanup(srv.Close)
	c := api.New(tok, api.Options{BaseURL: srv.URL + "/", Limiter: rate.NewLimiter(rate.Inf, 1)})
	if err := c.Get(t.Context(), "/a/b", nil, nil); err != nil || p != "/a/b" {
		t.Fatalf("err=%v path=%q", err, p)
	}
}

func TestLimiterErrorIsTerminal(t *testing.T) {
	var n atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{Limiter: rate.NewLimiter(1, 0)}) // burst 0: Wait always errors
	if err := c.Get(t.Context(), "/p", nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if n.Load() != 0 || len(s.d) != 0 {
		t.Fatalf("requests=%d sleeps=%v", n.Load(), s.d)
	}
}

func TestDefaultsAndMissingBaseURL(t *testing.T) {
	var h http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		reply(w, 200, `{"Code":1000}`)
	}))
	t.Cleanup(srv.Close)
	c := api.New(tok, api.Options{BaseURL: srv.URL, Limiter: rate.NewLimiter(rate.Inf, 1)})
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatal(err)
	}
	if h.Get("x-pm-appversion") != "Other" || !strings.HasPrefix(h.Get("User-Agent"), "pcb/") {
		t.Fatalf("headers %v", h)
	}
	err := api.New(tok, api.Options{}).Get(t.Context(), "/p", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "BaseURL") {
		t.Fatalf("got %v", err)
	}
}

func TestWrite429RetriedAfterRetryAfter(t *testing.T) {
	var n atomic.Int32
	var bodies []string
	var mu sync.Mutex
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			reply(w, 429, `{"Code":85131,"Error":"slow down"}`)
			return
		}
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	if err := c.Put(t.Context(), "/p", map[string]int{"a": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.d) != 1 || s.d[0] != 2*time.Second || len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] == "" {
		t.Fatalf("sleeps=%v bodies=%q", s.d, bodies)
	}
}

func TestHugeRetryAfterIsClampedAndNotRetried(t *testing.T) {
	for _, v := range []string{"99999999999", "99999999999999999999999"} {
		var n atomic.Int32
		c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.Header().Set("Retry-After", v)
			reply(w, 429, `{"Code":85131,"Error":"x"}`)
		}, api.Options{})
		err := c.Get(t.Context(), "/p", nil, nil)
		var e *api.Error
		if !errors.As(err, &e) || e.RetryAfter != time.Hour || n.Load() != 1 || len(s.d) != 0 {
			t.Fatalf("%s: err=%#v requests=%d sleeps=%v", v, err, n.Load(), s.d)
		}
	}
}

func TestResponseTooLarge(t *testing.T) {
	var n atomic.Int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(200)
		chunk := []byte(strings.Repeat("a", 1<<20))
		for range 33 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}, api.Options{})
	err := c.Get(t.Context(), "/p", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "response too large") || n.Load() != 1 {
		t.Fatalf("got %v", err)
	}
}

// A redirect is never followed: it would carry the session headers to
// another URL. It is answered as a failure.
func TestRedirectIsNeverFollowed(t *testing.T) {
	for name, hc := range map[string]*http.Client{
		"default client":  nil,
		"caller's client": {Timeout: 5 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			var reached atomic.Int32
			elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				reply(w, 200, `{"Code":1000}`)
			}))
			t.Cleanup(elsewhere.Close)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhere.URL+"/stolen", http.StatusFound)
			}))
			t.Cleanup(srv.Close)
			c := api.New(tok, api.Options{BaseURL: srv.URL, HTTP: hc, Limiter: rate.NewLimiter(rate.Inf, 1),
				Sleep: func(context.Context, time.Duration) error { return nil }})
			err := c.Get(t.Context(), "/core/v4/users", nil, nil)
			var e *api.Error
			if !errors.As(err, &e) || e.Status != http.StatusFound {
				t.Errorf("err = %v, want the 302 as an *api.Error", err)
			}
			if n := reached.Load(); n != 0 {
				t.Errorf("the redirect was followed (%d requests elsewhere)", n)
			}
		})
	}
}

func TestSetTokensReplacesWhatIsSent(t *testing.T) {
	t.Parallel()
	var seen []string
	var mu sync.Mutex
	c, _ := newClient(t, bearerHandler(&seen, &mu), api.Options{})
	_ = c.Get(t.Context(), "/x", nil, nil)
	next := api.Tokens{UID: "uid-1", AccessToken: "access-other", RefreshToken: "refresh-other"}
	c.SetTokens(next)
	if c.Tokens() != next {
		t.Fatalf("Tokens() = %+v", c.Tokens())
	}
	_ = c.Get(t.Context(), "/x", nil, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[1] != "Bearer access-other" {
		t.Errorf("bearers sent = %v", seen)
	}
}

func TestPutOnceDoesNotRetryA429(t *testing.T) {
	var n atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "2")
		reply(w, 429, `{"Code":0,"Error":"slow"}`)
	}, api.Options{})
	err := c.PutOnce(t.Context(), "/p", map[string]int{"a": 1}, nil)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter != 2*time.Second {
		t.Fatalf("err = %v, want the 429 with its Retry-After", err)
	}
	if n.Load() != 1 || len(s.d) != 0 || errors.Is(err, api.ErrNotSent) {
		t.Errorf("requests=%d sleeps=%v notSent=%v, want one request, no wait", n.Load(), s.d, errors.Is(err, api.ErrNotSent))
	}
	// Put keeps waiting out a 429 (it is rejected before processing).
	n.Store(0)
	if err := c.Put(t.Context(), "/p", map[string]int{"a": 1}, nil); err == nil || n.Load() < 2 {
		t.Errorf("Put: err=%v requests=%d, want it retried", err, n.Load())
	}
}

func TestNotSentOnlyBeforeTheRequestCanLeave(t *testing.T) {
	// A refused connection: never left.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + l.Addr().String()
	_ = l.Close()
	c := api.New(api.Tokens{}, api.Options{BaseURL: base, Sleep: func(context.Context, time.Duration) error { return nil }})
	if err := c.PutOnce(t.Context(), "/p", map[string]int{"a": 1}, nil); !errors.Is(err, api.ErrNotSent) {
		t.Errorf("refused connection: %v, want ErrNotSent", err)
	}
	// A body that cannot encode: never left.
	if err := c.PutOnce(t.Context(), "/p", make(chan int), nil); !errors.Is(err, api.ErrNotSent) {
		t.Errorf("unencodable body: %v, want ErrNotSent", err)
	}
	// An answer cut after the request arrived: may have been processed.
	c2, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}, api.Options{})
	if err := c2.PutOnce(t.Context(), "/p", map[string]int{"a": 1}, nil); err == nil || errors.Is(err, api.ErrNotSent) {
		t.Errorf("reset after send: %v, want an error that is not ErrNotSent", err)
	}
}

// TestParseRetryAfter: both forms of the header (RFC 9110 §10.2.3), clamped
// to an hour; a past date, garbage or nothing is 0.
func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	date := func(d time.Duration) string { return now.Add(d).Format(http.TimeFormat) }
	for _, c := range []struct {
		value string
		want  time.Duration
	}{
		{"120", 120 * time.Second},
		{" 7 ", 7 * time.Second},
		{date(90 * time.Second), 90 * time.Second},
		{date(48 * time.Hour), time.Hour},
		{"99999999999999999999999", time.Hour},
		{"-99999999999999999999999", 0},
		{date(-time.Minute), 0},
		{"-5", 0},
		{"soon", 0},
		{"", 0},
	} {
		h := http.Header{}
		if c.value != "" {
			h.Set("Retry-After", c.value)
		}
		if got := api.ParseRetryAfter(h, now); got != c.want {
			t.Errorf("Retry-After %q: %v, want %v", c.value, got, c.want)
		}
	}
}

// TestRetryAfterDateHonoured: a 429 whose Retry-After is an HTTP date waits
// until that date, not the 1 s back-off.
func TestRetryAfterDateHonoured(t *testing.T) {
	var n atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
			reply(w, 429, `{"Code":85131,"Error":"Too many requests"}`)
			return
		}
		reply(w, 200, `{"Code":1000}`)
	}, api.Options{})
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 || len(s.d) != 1 || s.d[0] < 25*time.Second || s.d[0] > 30*time.Second {
		t.Fatalf("requests=%d sleeps=%v; want one wait of about 30 s", n.Load(), s.d)
	}
}
