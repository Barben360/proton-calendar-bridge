package ctl

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// lossyListener plays go-winio v0.6.2's pipe listener: Close sends on
// closeCh and then waits for doneCh; the listener's routine closes doneCh
// on a close signal, but the first lose signals are received where they
// are lost (by the connect of a pipe instance a client dropped) and it goes
// on waiting. A pending Accept returns only once doneCh closes.
type lossyListener struct {
	closeCh chan int
	doneCh  chan struct{}
	lost    atomic.Int32
}

func newLossyListener(lose int32) *lossyListener {
	l := &lossyListener{closeCh: make(chan int), doneCh: make(chan struct{})}
	go func() {
		for range l.closeCh {
			if l.lost.Add(1) > lose {
				close(l.doneCh)
				return
			}
		}
	}()
	return l
}

func (l *lossyListener) Close() error {
	select {
	case l.closeCh <- 1:
		<-l.doneCh
	case <-l.doneCh:
	}
	return nil
}

func (l *lossyListener) Accept() (net.Conn, error) {
	<-l.doneCh
	return nil, errors.New("pipe listener closed")
}

func (*lossyListener) Addr() net.Addr { return &net.UnixAddr{Name: "lossy", Net: "pipe"} }

// closeWithin runs Close and fails t unless it returns within d.
func closeWithin(t *testing.T, ln net.Listener, d time.Duration) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- ln.Close() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(d):
		t.Fatalf("Close still waits after %v: the lost close signal hangs it", d)
		return nil
	}
}

// A close signal the listener loses (go-winio's lost close) is sent again:
// Close completes, cleanly, and a pending Accept ends with the listener.
func TestBoundCloseResendsALostSignal(t *testing.T) {
	t.Parallel()
	inner := newLossyListener(1)
	ln := boundClose(inner, 10*time.Millisecond, 2*time.Second)
	accepted := make(chan error, 1)
	go func() { _, err := ln.Accept(); accepted <- err }()
	if err := closeWithin(t, ln, time.Second); err != nil {
		t.Errorf("Close = %v, want nil once the signal got through", err)
	}
	select {
	case err := <-accepted:
		if err == nil {
			t.Error("Accept succeeded on a closed listener")
		}
	case <-time.After(time.Second):
		t.Error("Accept still waits after Close")
	}
	if err := ln.Close(); err != nil { // once: a second Close is a no-op
		t.Errorf("second Close = %v", err)
	}
}

// A listener that loses every signal is given up after the bound:
// Close returns ErrCloseStuck, and Accept, pending or new, returns
// net.ErrClosed instead of waiting on it, so the server's Serve ends.
func TestBoundCloseGivesUpOnAListenerThatNeverCloses(t *testing.T) {
	t.Parallel()
	inner := newLossyListener(1 << 30)
	ln := boundClose(inner, 10*time.Millisecond, 200*time.Millisecond)
	accepted := make(chan error, 1)
	go func() { _, err := ln.Accept(); accepted <- err }()
	start := time.Now()
	if err := closeWithin(t, ln, 2*time.Second); !errors.Is(err, ErrCloseStuck) {
		t.Errorf("Close = %v, want ErrCloseStuck", err)
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("Close gave up after %v, before its bound", took)
	}
	select {
	case err := <-accepted:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("pending Accept = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Error("the pending Accept still waits on the stuck listener")
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after the give-up = %v, want net.ErrClosed", err)
	}
	if inner.lost.Load() < 3 {
		t.Errorf("%d close signals sent in 200 ms at a 10 ms step, want it resent", inner.lost.Load())
	}
}

// Listen's listener on Windows is bounded at closeBound: about 2 s.
func TestCloseBoundIsAboutTwoSeconds(t *testing.T) {
	t.Parallel()
	if closeBound != 2*time.Second || closeRetry <= 0 || closeRetry > 100*time.Millisecond {
		t.Errorf("closeBound %v, closeRetry %v; want 2 s and a step of at most 100 ms", closeBound, closeRetry)
	}
}
