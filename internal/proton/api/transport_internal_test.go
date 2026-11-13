package api

import (
	"testing"
	"time"
)

// The clients built without Options.HTTP run the HTTP/2 health check at
// DefaultPing (TestHealthCheckClosesAConnectionThatAnswersNothing shows
// what it does, on NewTransport).
func TestDefaultTransportRunsTheHealthCheck(t *testing.T) {
	t.Parallel()
	c := New(Tokens{}, Options{BaseURL: "https://example.invalid"})
	if c.o.HTTP.Timeout != 30*time.Second {
		t.Errorf("default timeout %v, want 30s", c.o.HTTP.Timeout)
	}
	h := defaultTransport.HTTP2
	if c.o.HTTP.Transport != defaultTransport || h == nil || h.SendPingTimeout != DefaultPing || h.PingTimeout != DefaultPing || !defaultTransport.ForceAttemptHTTP2 {
		t.Errorf("the default client's transport is %p (default %p), HTTP/2 config %+v; want the default transport with the health check at %v",
			c.o.HTTP.Transport, defaultTransport, h, DefaultPing)
	}
}
