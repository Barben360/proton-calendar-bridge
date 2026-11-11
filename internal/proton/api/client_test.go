package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
