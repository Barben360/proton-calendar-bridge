package ctl

import (
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
)

// The client's timeouts (ruling M5-Rpre1).
const (
	dialTimeout   = 2 * time.Second
	statusTimeout = 10 * time.Second
)

// maxResponse bounds what the client reads of an answer.
const maxResponse = 16 << 20

// Client talks to pcb serve over its control socket.
type Client struct {
	hc *http.Client
}

// Dial connects to the daemon at path: ErrNoDaemon when none answers there
// (no socket, or a stale one), and always off Unix.
func Dial(ctx context.Context, path string) (*Client, error) {
	first, err := dialSocket(ctx, path)
	if err != nil {
		if errors.Is(err, ErrNoDaemon) {
			return nil, err
		}
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("cannot reach the pcb daemon on %s: %w", path, err),
			Hint: "check that you run pcb as the user that runs pcb serve"}
	}
	var mu sync.Mutex
	tr := &http.Transport{
		// The first request reuses the connection Dial made.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			mu.Lock()
			c := first
			first = nil
			mu.Unlock()
			if c != nil {
				return c, nil
			}
			return dialSocket(ctx, path)
		},
		MaxIdleConns:       1,
		DisableCompression: true,
	}
	return &Client{hc: &http.Client{Transport: tr}}, nil
}

// Close releases the client's connections.
func (c *Client) Close() error {
	c.hc.CloseIdleConnections()
	return nil
}

// Status is the daemon's live status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	var st Status
	err := c.do(ctx, http.MethodGet, "/v1/status", &st)
	return st, err
}

type envelopeIn struct {
	OK    bool              `json:"ok"`
	Data  json.RawMessage   `json:"data"`
	Error *output.ErrorBody `json:"error"`
}

// do sends one request and decodes its envelope into out: a command error
// (422) comes back as the daemon's *exitcode.Error, a transport error as a
// blocked one naming the HTTP status.
func (c *Client) do(ctx context.Context, method, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://pcb"+target, http.NoBody)
	if err != nil {
		return err
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
