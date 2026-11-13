package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// A refresh that times out on an HTTP/2 connection that stopped answering
// (live 2026-11-13) does not leave the next refresh on that connection.
func TestRefreshAfterATimeoutUsesANewConnection(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	front := protontest.Front(t, f)
	tr := front.Transport()
	t.Cleanup(tr.CloseIdleConnections)
	m := auth.NewManagerOver(front.URL, tr)
	tok, err := auth.Refresh(t.Context(), m, f.Login(t, f.Username))
	if err != nil {
		t.Fatalf("Refresh before the stall: %v", err)
	}
	front.Stall()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	_, err = auth.Refresh(ctx, m, tok)
	cancel()
	if err == nil || front.Hung() != 1 {
		t.Fatalf("Refresh on the stalled connection = %v with %d requests hung, want a timeout", err, front.Hung())
	}
	time.Sleep(50 * time.Millisecond) // the reset stream is forgotten
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := auth.Refresh(ctx, m, tok); err != nil {
		t.Fatalf("the refresh after the timeout = %v (%d requests hung, %d connections), want it answered on a new connection",
			err, front.Hung(), front.Conns())
	}
}
