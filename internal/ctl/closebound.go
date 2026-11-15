package ctl

import (
	"net"
	"sync"
	"time"
)

// The control pipe's listener on Windows (Listen) is go-winio's, whose
// Close can lose its signal (v0.6.2): while an Accept is pending, the
// close may be received by the connect of the next pipe instance, which
// closes that instance and then, unless the connect ends with nil or
// ErrFileClosed, goes on (a client that dropped the instance first, an
// ERROR_NO_DATA, makes a fresh instance that waits for a close forever;
// another error goes back to waiting for a close). Close then waits forever
// for a close that is never seen, and http.Server.Shutdown, which closes
// its listeners before it looks at its context, with it: pcb-ui's Quit and
// a session end hang. Plain logic, tested on every OS with a listener that
// loses signals; only Windows uses it.

// Close's steps: closeRetry between two signals, closeBound in all.
const (
	closeRetry = 75 * time.Millisecond
	closeBound = 2 * time.Second
)

// boundClose wraps ln so Close always returns: it signals ln's Close again
// every retry until one completes (each extra Close reaches whichever wait
// the lost signal left; they all return once ln is closed), and after bound
// it gives up with ErrCloseStuck. Accept then returns net.ErrClosed rather
// than wait on ln for good, so the server's Serve ends too. Accept, Addr and
// the connections pass through otherwise.
func boundClose(ln net.Listener, retry, bound time.Duration) net.Listener {
	return &boundedListener{Listener: ln, retry: retry, bound: bound, stuck: make(chan struct{})}
}

type boundedListener struct {
	net.Listener
	retry, bound time.Duration

	once  sync.Once
	err   error
	stuck chan struct{} // closed when Close gave up
}

// Close closes the listener once, within bound (see boundClose).
func (l *boundedListener) Close() error {
	l.once.Do(func() {
		done := make(chan struct{})
		var closed sync.Once
		signal := func() { // one Close of ln; done once any has returned
			_ = l.Listener.Close()
			closed.Do(func() { close(done) })
		}
		go signal()
		retry := time.NewTicker(l.retry)
		defer retry.Stop()
		giveUp := time.NewTimer(l.bound)
		defer giveUp.Stop()
		for {
			select {
			case <-done:
				return
			case <-retry.C:
				go signal()
			case <-giveUp.C:
				l.err = ErrCloseStuck
				close(l.stuck)
				return
			}
		}
	})
	return l.err
}

// Accept is the listener's, unless Close gave up on it: then it returns
// net.ErrClosed at once (a connection the abandoned Accept still yields is
// closed), so no Serve waits on a listener that will not close.
func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case <-l.stuck:
		return nil, net.ErrClosed
	default:
	}
	type accepted struct {
		c   net.Conn
		err error
	}
	got := make(chan accepted, 1)
	go func() {
		c, err := l.Listener.Accept()
		got <- accepted{c, err}
	}()
	select {
	case a := <-got:
		return a.c, a.err
	case <-l.stuck:
		go func() {
			if a := <-got; a.c != nil {
				_ = a.c.Close()
			}
		}()
		return nil, net.ErrClosed
	}
}
