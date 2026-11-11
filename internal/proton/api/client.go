package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultTimeout = 30 * time.Second
	maxAttempts    = 3
	maxRetryAfter  = 60 * time.Second
	maxResponse    = 32 << 20
)

// Options configures a Client. Zero fields take the documented defaults.
type Options struct {
	BaseURL    string
	AppVersion string // x-pm-appversion
	UserAgent  string
	HTTP       *http.Client // default: 30 s timeout
	// Refresh exchanges stale tokens for new ones. It should wrap
	// ErrSessionEnded when the refresh token is no longer usable. Without it,
	// a 401 ends the session.
	Refresh func(ctx context.Context, stale Tokens) (Tokens, error)
	Limiter *rate.Limiter // default: one request per 250 ms, burst 8
	Sleep   func(ctx context.Context, d time.Duration) error
}

// Client sends authenticated requests. It is safe for concurrent use.
type Client struct {
	o  Options
	mu sync.Mutex // guards tok and serialises refreshes
	tk Tokens
}

// New returns a Client holding t.
func New(t Tokens, o Options) *Client {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: defaultTimeout}
	}
	if o.Limiter == nil {
		o.Limiter = rate.NewLimiter(rate.Every(250*time.Millisecond), 8)
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

// Get sends a GET, decoding the JSON answer into out (may be nil).
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.Do(ctx, http.MethodGet, path, query, nil, out)
}

// Put sends a PUT with a JSON body.
func (c *Client) Put(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPut, path, nil, body, out)
}

// Delete sends a DELETE.
func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodDelete, path, nil, nil, out)
}

// Do sends one request and decodes the JSON answer into out (may be nil).
//
// Policy: a 401 triggers one refresh and one retry; a 429 waits Retry-After
// (at most 60 s, at most 3 attempts, any method since Proton did not process
// the request); 5xx and network errors are retried for GET only, 3 attempts
// with 1 s then 2 s backoff and ±20 % jitter. Errors never contain the body,
// the query or any header value.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("%s %s: encode body: %w", method, path, err)
		}
	}
	target := c.o.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	where := method + " " + path
	var refreshed bool
	var limited, failed int
	for {
		sent := c.Tokens()
		status, hdr, data, err := c.send(ctx, method, target, payload, sent)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%s: %w", where, ctx.Err())
			}
			failed++
			if method == http.MethodGet && failed < maxAttempts {
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
			if refreshed {
				return fmt.Errorf("%s: %w: %w", where, ErrSessionEnded, perr)
			}
			refreshed = true
			if err := c.refresh(ctx, sent); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		case status == http.StatusTooManyRequests:
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
	fresh, err := c.o.Refresh(ctx, c.tk)
	if err != nil {
		return fmt.Errorf("refresh tokens: %w", err)
	}
	c.tk = fresh
	return nil
}

// send makes one HTTP round trip. Transport errors are reduced to their
// cause: *url.Error would echo the full URL, query included.
func (c *Client) send(ctx context.Context, method, target string, payload []byte, t Tokens) (int, http.Header, []byte, error) {
	if err := c.o.Limiter.Wait(ctx); err != nil {
		return 0, nil, nil, err
	}
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
		return 0, nil, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read response: %w", err)
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
	if secs, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Retry-After"))); err == nil && secs > 0 {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	return e
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
