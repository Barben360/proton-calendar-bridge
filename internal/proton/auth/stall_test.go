package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
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

// A refresh on a connection that carries nothing at all (live 2026-11-13)
// fails once the HTTP/2 health check drops it, well before its deadline,
// and the next refresh is answered on a new connection.
func TestRefreshOnAConnectionThatAnswersNothing(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	front := protontest.Front(t, f)
	tr := api.NewTransport(300 * time.Millisecond)
	front.Trust(tr)
	t.Cleanup(tr.CloseIdleConnections)
	m := auth.NewManagerOver(front.URL, tr)
	tok, err := auth.Refresh(t.Context(), m, f.Login(t, f.Username))
	if err != nil {
		t.Fatalf("Refresh before the black hole: %v", err)
	}
	front.BlackHole()
	const deadline = 10 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	start := time.Now()
	_, err = auth.Refresh(ctx, m, tok)
	if took := time.Since(start); err == nil || took > deadline/2 {
		t.Fatalf("Refresh on the black-holed connection = %v after %v; want a failure once the health check drops it", err, took)
	}
	if _, err := auth.Refresh(ctx, m, tok); err != nil || front.Conns() != 2 {
		t.Fatalf("the next refresh = %v on %d connections, want it answered on a second connection", err, front.Conns())
	}
}
