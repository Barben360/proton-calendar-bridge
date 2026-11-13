package auth

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// The manager's transport runs the HTTP/2 health check
// (TestRefreshOnAConnectionThatAnswersNothing shows what it does).
func TestManagerTransportRunsTheHealthCheck(t *testing.T) {
	t.Parallel()
	tr := newTransport()
	if h := tr.HTTP2; h == nil || h.SendPingTimeout != api.DefaultPing || h.PingTimeout != api.DefaultPing || !tr.ForceAttemptHTTP2 {
		t.Errorf("the manager's HTTP/2 config is %+v, want the health check at %v", h, api.DefaultPing)
	}
}
