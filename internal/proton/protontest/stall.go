package protontest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
)

// StallFront is a TLS server speaking HTTP/2 in front of a handler (a
// Fake's URL through Front, or any handler). Once Stall is called, every
// request on the connection that was the first one it accepted gets no
// answer at all until the client gives up, while later connections are
// answered as usual: an HTTP/2 connection that stopped answering, as one
// to Proton did after a login (HUMAN-CHECKS 7.1.5, live 2026-11-13).
type StallFront struct {
	URL string

	srv     *httptest.Server
	mu      sync.Mutex
	first   net.Conn // the first connection accepted
	conns   int      // connections accepted
	stalled atomic.Bool
	hung    atomic.Int64 // requests left unanswered
}

type connKey struct{}

// NewStallFront serves h over TLS with HTTP/2 on a loopback port.
func NewStallFront(t TB, h http.Handler) *StallFront {
	t.Helper()
	s := &StallFront{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Context().Value(connKey{}).(net.Conn)
		s.mu.Lock()
		onFirst := c != nil && c == s.first
		s.mu.Unlock()
		if onFirst && s.stalled.Load() {
			s.hung.Add(1)
			<-r.Context().Done() // the client's reset, or the server's close
			return
		}
		h.ServeHTTP(w, r)
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		s.mu.Lock()
		if s.first == nil {
			s.first = c
		}
		s.conns++
		s.mu.Unlock()
		return context.WithValue(ctx, connKey{}, c)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	s.srv, s.URL = srv, srv.URL
	return s
}

// Front is a StallFront before f: requests are served by the fake as if
// sent to its URL (recorded, failed or stalled as it was told).
func Front(t TB, f *Fake) *StallFront {
	t.Helper()
	return NewStallFront(t, http.HandlerFunc(f.serve))
}

// Stall makes the first connection stop answering from now on.
func (s *StallFront) Stall() { s.stalled.Store(true) }

// Transport is a transport that trusts the server's certificate and speaks
// HTTP/2 to it; it shares nothing with another call's.
func (s *StallFront) Transport() *http.Transport {
	tr := s.srv.Client().Transport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	return tr
}

// Conns is the number of connections the server accepted.
func (s *StallFront) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// Hung is the number of requests the stalled connection left unanswered.
func (s *StallFront) Hung() int { return int(s.hung.Load()) }
