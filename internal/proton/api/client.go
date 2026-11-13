package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
)

const (
	defaultTimeout = 30 * time.Second
	maxAttempts    = 3
	maxRetryAfter  = 60 * time.Second
	maxResponse    = 32 << 20
	// A Retry-After above this is clamped; it is far over maxRetryAfter, so
	// the request is not retried and the duration cannot overflow.
	maxRetryAfterHeader = time.Hour
	refreshTimeout      = 30 * time.Second
	defaultAppVersion   = "Other"
	// pingAfter and pingTimeout are the HTTP/2 health check of the default
	// transport: after pingAfter without a frame from Proton a PING is
	// sent, and a connection whose PING is unanswered for pingTimeout is
	// closed (its requests fail and are retried on a new one). A dead
	// connection is so found within 30 s, the request timeout; a live one
	// costs one PING per 15 s of silence, and a slow answer on it is not
	// cut, since Proton answers the PING meanwhile.
	pingAfter   = 15 * time.Second
	pingTimeout = 15 * time.Second
)

// defaultTransport is the transport of the clients built without Options.HTTP:
// http.DefaultTransport's settings with the HTTP/2 health check, one pool
// for all of them and nothing else in the process.
var defaultTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	t.HTTP2 = HTTP2Health()
	return t
}()

// HTTP2Health is the HTTP/2 health check pcb's transports to Proton use
// (pingAfter, pingTimeout).
func HTTP2Health() *http.HTTP2Config {
	return &http.HTTP2Config{SendPingTimeout: pingAfter, PingTimeout: pingTimeout}
}

var errTooLarge = errors.New("response too large")

// Options configures a Client. BaseURL is required (Do refuses to send
// without it); the other zero fields take the documented defaults.
type Options struct {
	BaseURL    string
	AppVersion string       // x-pm-appversion; default "Other"
	UserAgent  string       // default "pcb/<buildinfo.Version>"
	HTTP       *http.Client // default: 30 s timeout, an HTTP/2 health check; redirects are never followed
	// Refresh exchanges stale tokens for new ones. It should wrap
	// ErrSessionEnded when the refresh token is no longer usable. Without it,
	// a 401 ends the session. It runs under the client's lock and detached
	// from the requester's cancellation (30 s timeout, since refresh tokens
	// are single-use), so it must not call the Client.
	Refresh func(ctx context.Context, stale Tokens) (Tokens, error)
	Limiter *rate.Limiter // default: one request per 250 ms, burst 8
	Sleep   func(ctx context.Context, d time.Duration) error
}

// Client sends authenticated requests. It is safe for concurrent use.
type Client struct {
	o  Options
	mu sync.Mutex // guards tok and serialises refreshes
	tk Tokens
	// failed is set when a send failed: its connection may be one that
	// stopped answering, kept in the pool once its stream is reset.
	failed atomic.Bool
}

// New returns a Client holding t.
func New(t Tokens, o Options) *Client {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: defaultTimeout, Transport: defaultTransport}
	}
	// A followed redirect would send the session headers to another URL; the
	// 3xx is answered as a failure instead. Set on a copy: the caller's client
	// is left as it was.
	hc := *o.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTP = &hc
	if o.Limiter == nil {
		o.Limiter = rate.NewLimiter(rate.Every(250*time.Millisecond), 8)
	}
	if o.AppVersion == "" {
		o.AppVersion = defaultAppVersion
	}
	if o.UserAgent == "" {
		o.UserAgent = "pcb/" + buildinfo.Version
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	return &Client{o: o, tk: t}
}

// Tokens returns the current tokens (they change after a refresh).
func (c *Client) Tokens() Tokens {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tk
}

// SetTokens replaces the tokens: the caller learned of newer ones (another
// process rotated and stored them). Requests already in flight finish with
// the tokens they started with.
func (c *Client) SetTokens(t Tokens) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tk = t
}

// Get sends a GET, decoding the JSON answer into out (may be nil).
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.Do(ctx, http.MethodGet, path, query, nil, out)
}

// Put sends a PUT with a JSON body.
func (c *Client) Put(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPut, path, nil, body, out)
}

// PutOnce sends a PUT with a JSON body that must never be sent twice: a write
// without an idempotency key. It behaves as Put except that a 429 is
// returned at once, not waited out and retried; the caller decides whether
// and when to send again. A 401 still refreshes the tokens and re-sends the
// body exactly once (ruling M4-R23): Proton answers 401 at authentication,
// before processing, so the first send cannot have been applied. 5xx and network
// errors are never retried for a PUT. Errors that happened before anything
// could have reached Proton satisfy errors.Is(err, ErrNotSent).
func (c *Client) PutOnce(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPut, path, nil, body, out, true)
}

// Delete sends a DELETE.
func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodDelete, path, nil, nil, out)
}

// Do sends one request and decodes the JSON answer into out (may be nil).
//
// Policy: a 401 triggers one refresh and one retry; a 429 waits Retry-After
// (at most 60 s, at most 3 attempts; any method, because Proton rejects a
// rate-limited request before processing it); 5xx and network errors are retried for GET only, 3 attempts
// with 1 s then 2 s backoff and ±20 % jitter. Errors never contain the body,
// the query or any header value.
//
// path must start with "/" and be already escaped: callers url.PathEscape
// every ID they put in it. Paths with "?", "#", "..", a backslash or control
// characters are refused without sending anything.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.do(ctx, method, path, query, body, out, false)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any, once bool) error {
	target, err := c.target(path, query)
	if err != nil {
		return notSent(fmt.Errorf("%s %s: %w", method, path, err))
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return notSent(fmt.Errorf("%s %s: encode body: %w", method, path, err))
		}
	}
	where := method + " " + path
	var refreshed bool
	var limited, failed int
	for {
		if err := c.o.Limiter.Wait(ctx); err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return notSent(fmt.Errorf("%s: %w", where, err))
		}
		sent := c.Tokens()
		if c.failed.Swap(false) {
			c.dropIdle()
		}
		status, hdr, data, err := c.send(ctx, method, target, payload, sent)
		if err != nil {
			c.failed.Store(true) // the next send drops the idle connections
			if ctx.Err() != nil {
				return fmt.Errorf("%s: %w", where, ctx.Err())
			}
			failed++
			if method == http.MethodGet && failed < maxAttempts && !errors.Is(err, errTooLarge) {
				if err := c.o.Sleep(ctx, backoff(failed)); err != nil {
					return fmt.Errorf("%s: %w", where, err)
				}
				continue
			}
			return fmt.Errorf("%s: %w", where, err)
		}
		if status >= 200 && status < 300 {
			perr := parse(method, path, status, hdr, data)
			if perr == nil {
				return decode(where, data, out)
			}
			return perr
		}
		perr := parse(method, path, status, hdr, data)
		switch {
		case status == http.StatusUnauthorized:
			// Ruling M4-R23: even for PutOnce, one resend after a refresh is
			// allowed: a 401 is answered before the request is processed.
			if refreshed {
				return fmt.Errorf("%s: %w: %w", where, ErrSessionEnded, perr)
			}
			refreshed = true
			if err := c.refresh(ctx, sent); err != nil {
				return notSent(fmt.Errorf("%s: %w", where, err))
			}
		case status == http.StatusTooManyRequests:
			if once {
				return perr
			}
			limited++
			wait := perr.RetryAfter
			if wait == 0 {
				wait = backoff(limited)
			}
			if wait > maxRetryAfter || limited >= maxAttempts {
				return perr
			}
			if err := c.o.Sleep(ctx, wait); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		case status >= 500 && method == http.MethodGet:
			failed++
			if failed >= maxAttempts {
				return perr
			}
			if err := c.o.Sleep(ctx, backoff(failed)); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		default:
			return perr
		}
	}
}

// dropIdle closes the idle connections of the client's transport after a
// send failed (live 2026-11-13): an HTTP/2 connection that stops answering
// stays in the pool once the timed-out stream is reset, and every later
// request would go to it again. A connection still carrying a request is
// left alone. It runs before the send that follows the failure: at the
// failure itself the reset stream still holds the connection.
func (c *Client) dropIdle() { c.o.HTTP.CloseIdleConnections() }

// refresh rotates the tokens unless another goroutine already did.
func (c *Client) refresh(ctx context.Context, stale Tokens) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tk.AccessToken != stale.AccessToken {
		return nil
	}
	if c.o.Refresh == nil {
		return fmt.Errorf("%w: no refresh available", ErrSessionEnded)
	}
	// Refresh tokens are single use: once started, a refresh must finish and
	// be recorded even if the requester gives up.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	fresh, err := c.o.Refresh(rctx, c.tk)
	if err != nil {
		return fmt.Errorf("refresh tokens: %w", err)
	}
	c.tk = fresh
	return nil
}

// send makes one HTTP round trip. Transport errors are reduced to their
// cause: *url.Error would echo the full URL, query included.
func (c *Client) send(ctx context.Context, method, target string, payload []byte, t Tokens) (int, http.Header, []byte, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, nil, nil, errors.New("build request")
	}
	req.Header.Set("x-pm-uid", t.UID)
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	req.Header.Set("x-pm-appversion", c.o.AppVersion)
	req.Header.Set("User-Agent", c.o.UserAgent)
	req.Header.Set("Accept", "application/vnd.protonmail.v1+json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" {
			err = notSent(err) // no connection: the request never left
		}
		return 0, nil, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponse {
		return 0, nil, nil, errTooLarge
	}
	return resp.StatusCode, resp.Header, data, nil
}

// parse returns an *Error for a non-2xx answer or a 2xx whose Code is not
// 1000/1001, nil otherwise.
func parse(method, path string, status int, hdr http.Header, data []byte) *Error {
	var env struct {
		Code  int
		Error string
	}
	_ = json.Unmarshal(data, &env) // a non-JSON body leaves the zero value
	ok := status >= 200 && status < 300
	if ok && (env.Code == CodeOK || env.Code == CodeMulti) {
		return nil
	}
	e := &Error{Method: method, Path: path, Status: status, Code: env.Code, Message: env.Error}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	e.RetryAfter = ParseRetryAfter(hdr, time.Now())
	return e
}

// ParseRetryAfter is the wait h's Retry-After asks for at now, in either
// form (delay-seconds or an HTTP date), clamped to an hour; 0 when there is
// none, it is malformed or its date is past.
func ParseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	var ne *strconv.NumError
	switch {
	case errors.As(err, &ne) && errors.Is(ne.Err, strconv.ErrRange) && !strings.HasPrefix(v, "-"):
		return maxRetryAfterHeader
	case err == nil && secs > 0:
		return min(time.Duration(min(secs, 3600))*time.Second, maxRetryAfterHeader)
	case err == nil:
		return 0
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	return min(max(at.Sub(now), 0), maxRetryAfterHeader)
}

func decode(where string, data []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", where, err)
	}
	return nil
}

// backoff is 1 s, 2 s, 4 s… for attempt 1, 2, 3… with ±20 % jitter.
func backoff(attempt int) time.Duration {
	base := time.Second << (attempt - 1)
	return time.Duration(float64(base) * (0.8 + 0.4*rand.Float64()))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// target builds the request URL from the base, a validated path and the query.
func (c *Client) target(path string, query url.Values) (string, error) {
	if c.o.BaseURL == "" {
		return "", errors.New("api: no BaseURL configured")
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\") || strings.Contains(path, "..") {
		return "", errors.New("invalid request path")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("invalid request path")
		}
	}
	u, err := url.Parse(c.o.BaseURL)
	if err != nil {
		return "", errors.New("api: invalid BaseURL")
	}
	u = u.JoinPath(path)
	u.RawQuery = query.Encode()
	return u.String(), nil
}
