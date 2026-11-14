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
// to Proton did after a login (HUMAN-CHECKS 7.1.5, live 2026-11-13). The
// server still answers that connection's PINGs; once BlackHole is called,
// the first connection carries no byte at all in either direction, PING
// acknowledgements included, as live (nothing was received for minutes).
type StallFront struct {
	URL string

	srv     *httptest.Server
	mu      sync.Mutex
	first   net.Conn // the first connection the server accepted
	conns   int      // connections accepted
	stalled atomic.Bool
	holed   atomic.Bool
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
		if r.ProtoMajor != 2 {
			t.Errorf("StallFront got an HTTP/%d.%d request: the stall tests need HTTP/2", r.ProtoMajor, r.ProtoMinor)
		}
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
	s.srv = srv
	s.URL = "https://" + s.relay(t, srv.Listener.Addr().String())
	return s
}

// relay listens on a loopback port and passes each connection's bytes to
// and from the server at backend, except the first connection's once
// BlackHole is called: those are read and dropped. It returns the address.
func (s *StallFront) relay(t TB, backend string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("StallFront: %v", err)
	}
	var mu sync.Mutex
	var open []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range open {
			_ = c.Close()
		}
	})
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", backend) // in accept order: the server's first is the relay's first
			if err != nil {
				_ = c.Close()
				continue
			}
			mu.Lock()
			open = append(open, c, b)
			mu.Unlock()
			holed := func() bool { return n == 0 && s.holed.Load() }
			go pass(c, b, holed)
			go pass(b, c, holed)
		}
	}()
	return ln.Addr().String()
}

// pass copies src to dst, dropping what it reads while holed, until src
// ends; it then closes dst.
func pass(src, dst net.Conn, holed func() bool) {
	defer func() { _ = dst.Close() }()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !holed() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Front is a StallFront before f: requests are served by the fake as if
// sent to its URL (recorded, failed or stalled as it was told).
func Front(t TB, f *Fake) *StallFront {
	t.Helper()
	return NewStallFront(t, http.HandlerFunc(f.serve))
}

// Stall makes the first connection stop answering requests from now on.
func (s *StallFront) Stall() { s.stalled.Store(true) }

// BlackHole makes the first connection carry nothing from now on, in
// either direction.
func (s *StallFront) BlackHole() { s.holed.Store(true) }

// Trust makes tr trust the server's certificate.
func (s *StallFront) Trust(tr *http.Transport) {
	tr.TLSClientConfig = s.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
}

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
