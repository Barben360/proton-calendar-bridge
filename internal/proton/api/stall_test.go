package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// stallClient is a client of a StallFront answering {"Code":1000}, with a
// short request timeout and real short back-off sleeps; one GET has
// opened the first connection before it stalls.
func stallClient(t *testing.T) (*api.Client, *protontest.StallFront) {
	t.Helper()
	front := protontest.NewStallFront(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, `{"Code":1000}`)
	}))
	tr := front.Transport()
	t.Cleanup(tr.CloseIdleConnections)
	c := api.New(tok, api.Options{
		BaseURL: front.URL,
		HTTP:    &http.Client{Timeout: 300 * time.Millisecond, Transport: tr},
		Limiter: rate.NewLimiter(rate.Inf, 1),
		Sleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-time.After(20 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatalf("GET before the stall: %v", err)
	}
	if n := front.Conns(); n != 1 {
		t.Fatalf("%d connections before the stall, want 1", n)
	}
	front.Stall()
	return c, front
}

// Live 2026-11-13: an HTTP/2 connection to Proton stopped answering, and
// every retry and every later pass went to it again until a restart. A GET
// that times out is retried on a new connection.
func TestTimedOutGETIsRetriedOnANewConnection(t *testing.T) {
	t.Parallel()
	c, front := stallClient(t)
	if err := c.Get(t.Context(), "/p", nil, nil); err != nil {
		t.Fatalf("GET after the connection stalled = %v, want its retry answered on a new connection", err)
	}
	if front.Hung() != 1 || front.Conns() != 2 {
		t.Errorf("%d requests hung on %d connections; want 1 hung, then a second connection", front.Hung(), front.Conns())
	}
}

// A write is never retried, but the request after its timeout does not go
// to the connection that stopped answering either.
func TestRequestAfterATimeoutUsesANewConnection(t *testing.T) {
	t.Parallel()
	c, front := stallClient(t)
	if err := c.PutOnce(t.Context(), "/w", struct{}{}, nil); err == nil {
		t.Fatal("PutOnce on the stalled connection succeeded")
	}
	time.Sleep(50 * time.Millisecond) // the reset stream is forgotten
	before := front.Hung()
	if err := c.PutOnce(t.Context(), "/w", struct{}{}, nil); err != nil {
		t.Fatalf("the write after the timeout = %v, want it answered on a new connection", err)
	}
	if front.Hung() != before || front.Conns() != 2 {
		t.Errorf("%d requests hung (%d before), %d connections; want none more and a second connection", front.Hung(), before, front.Conns())
	}
}
