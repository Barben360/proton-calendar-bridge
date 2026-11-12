package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The client's timeouts (ruling M5-Rpre1). A logout through the daemon
// stops its sync loop and revokes the session: it gets the login's.
const (
	dialTimeout   = 2 * time.Second
	statusTimeout = 10 * time.Second
	loginTimeout  = 180 * time.Second
)

// firstConnTTL is how long the connection Dial made stays usable for the
// first request: well within the server's readHeaderTimeout.
const firstConnTTL = 2 * time.Second

// maxResponse bounds what the client reads of an answer.
const maxResponse = 16 << 20

// Client talks to pcb serve over its control socket.
type Client struct {
	hc *http.Client

	mu      sync.Mutex
	first   net.Conn         // the connection Dial made, until a request or Close takes it
	firstAt time.Time        // when Dial made it
	now     func() time.Time // the clock that ages it
}

// Dial connects to the daemon at path: ErrNoDaemon when none answers there
// (no socket, or a stale one, or a path too long to bind), and always off
// Unix. Before it dials, the path must be a socket owned by this process's
// effective UID (ruling M5-R2), and on Linux each connection's server must
// run under that UID too (SO_PEERCRED, re-review N1): anything else is
// refused, blocked (20), before a request is sent.
func Dial(ctx context.Context, path string) (*Client, error) {
	return dial(ctx, path, defaultPeerUID)
}

// dial is Dial with peer reading a connection's server UID.
func dial(ctx context.Context, path string, peer func(net.Conn) (int, bool)) (*Client, error) {
	first, err := dialSocket(ctx, path, peer)
	if err != nil {
		var ee *exitcode.Error
		if errors.Is(err, ErrNoDaemon) || errors.As(err, &ee) {
			return nil, err
		}
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("cannot reach the pcb daemon on %s: %w", path, err),
			Hint: "check that you run pcb as the user that runs pcb serve"}
	}
	c := &Client{first: first, now: time.Now}
	c.firstAt = c.now()
	tr := &http.Transport{
		// The first request reuses the connection Dial made.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if conn := c.takeFirst(); conn != nil {
				return conn, nil
			}
			return dialSocket(ctx, path, peer)
		},
		MaxIdleConns:       1,
		DisableCompression: true,
	}
	c.hc = &http.Client{Transport: tr}
	return c, nil
}

// takeFirst returns the connection Dial made, once, while it is fresh: a
// caller may prompt between Dial and its first request (pcb login), and the
// server drops a connection that sent no request within readHeaderTimeout.
// A stale one is closed, and the request dials anew.
func (c *Client) takeFirst() net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn := c.first
	c.first = nil
	if conn != nil && c.now().Sub(c.firstAt) > firstConnTTL {
		_ = conn.Close()
		return nil
	}
	return conn
}

// Close releases the client's connections, the one Dial made included when
// no request used it.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.first
	c.first = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	c.hc.CloseIdleConnections()
	return nil
}

// Status is the daemon's live status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	var st Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// Login asks the daemon to log in (service.API.Login). The credentials
// travel in the request body only.
func (c *Client) Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error) {
	return c.login(ctx, "/v1/login", req)
}

// SubmitTOTP sends the 2FA code of the login the daemon holds
// (service.API.SubmitTOTP).
func (c *Client) SubmitTOTP(ctx context.Context, code string) (service.LoginResult, error) {
	return c.login(ctx, "/v1/login/totp", TOTPRequest{Code: code})
}

// Logout asks the daemon to log out (service.API.Logout).
func (c *Client) Logout(ctx context.Context) (service.LogoutResult, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	var res service.LogoutResult
	err := c.do(ctx, http.MethodPost, "/v1/logout", nil, &res)
	return res, err
}

func (c *Client) login(ctx context.Context, target string, body any) (service.LoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	b, err := json.Marshal(body)
	if err != nil {
		return service.LoginResult{}, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("cannot encode the login request"), Hint: "use the pcb of the same version as the daemon"}
	}
	defer clear(b)
	var res service.LoginResult
	err = c.do(ctx, http.MethodPost, target, b, &res)
	return res, err
}

type envelopeIn struct {
	OK    bool              `json:"ok"`
	Data  json.RawMessage   `json:"data"`
	Error *output.ErrorBody `json:"error"`
}

// do sends one request with body (nil: none) and decodes its envelope into
// out: a command error (422) comes back as the daemon's *exitcode.Error, a
// transport error as a blocked one naming the HTTP status.
func (c *Client) do(ctx context.Context, method, target string, body []byte, out any) error {
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://pcb"+target, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("the pcb daemon did not answer: %w", err),
			Hint: "see the daemon's logs (pcb service logs)"}
	}
	defer func() { _ = resp.Body.Close() }()
	var env envelopeIn
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&env); err != nil {
		return &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("the pcb daemon answered HTTP %d without an envelope", resp.StatusCode),
			Hint: "use the pcb of the same version as the daemon"}
	}
	switch {
	case resp.StatusCode == http.StatusOK && env.OK:
		if err := json.Unmarshal(env.Data, out); err != nil {
			return &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("cannot read the pcb daemon's answer: %w", err),
				Hint: "use the pcb of the same version as the daemon"}
		}
		return nil
	case resp.StatusCode == http.StatusUnprocessableEntity && env.Error != nil && env.Error.Code != 0:
		return &exitcode.Error{Code: exitcode.Code(env.Error.Code), Err: errors.New(env.Error.Message), Hint: env.Error.Hint}
	}
	msg, hint := http.StatusText(resp.StatusCode), ""
	if env.Error != nil {
		msg, hint = env.Error.Message, env.Error.Hint
	}
	return &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("the pcb daemon refused the request (HTTP %d): %s", resp.StatusCode, msg), Hint: hint}
}
