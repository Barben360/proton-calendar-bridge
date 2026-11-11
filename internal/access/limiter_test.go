package access_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
)

func fail(l *access.Limiter, remote string, n int) {
	for range n {
		l.Fail(remote)
	}
}

func TestLimiterBlocksAfterFiveFailuresUntilTheMinuteEnds(t *testing.T) {
	t.Parallel()
	c := newClock()
	l := access.NewLimiter(c.now)
	fail(l, "10.0.0.1:1000", 4)
	if ok, _ := l.Allow("10.0.0.1:1000"); !ok {
		t.Fatal("blocked after 4 failures")
	}
	c.add(20 * time.Second)
	l.Fail("10.0.0.1:1000")
	ok, retry := l.Allow("10.0.0.1:1000")
	if ok || retry != 40*time.Second {
		t.Fatalf("after 5 failures: ok=%v retry=%v, want blocked for the 40s left of the window", ok, retry)
	}
	if ok, _ := l.Allow("10.0.0.2:1000"); !ok {
		t.Error("another IP is blocked")
	}
	c.add(39 * time.Second)
	if ok, _ := l.Allow("10.0.0.1:1"); ok {
		t.Error("unblocked before the minute ended")
	}
	c.add(time.Second)
	if ok, _ := l.Allow("10.0.0.1:1"); !ok {
		t.Error("still blocked after the minute ended")
	}
}

func TestLimiterKeysByIPWithoutPort(t *testing.T) {
	t.Parallel()
	l := access.NewLimiter(newClock().now)
	for i := range 5 {
		l.Fail(fmt.Sprintf("192.0.2.7:%d", 40000+i)) // a new source port each time
	}
	if ok, _ := l.Allow("192.0.2.7:9"); ok {
		t.Error("source ports gave an attacker a counter each")
	}
	for i := range 5 {
		l.Fail(fmt.Sprintf("[2001:db8::1]:%d", 40000+i))
	}
	if ok, _ := l.Allow("[2001:db8::1]:1"); ok {
		t.Error("IPv6 remotes are not keyed by IP")
	}
	if ok, _ := l.Allow("192.0.2.8:1"); !ok {
		t.Error("a neighbour IP is blocked")
	}
}

func TestLimiterSharesOneKeyForUnparsableRemotes(t *testing.T) {
	t.Parallel()
	l := access.NewLimiter(newClock().now)
	for i := range 5 {
		l.Fail(fmt.Sprintf("weird-%d", i))
	}
	if ok, _ := l.Allow("another-weird"); ok {
		t.Error("unparsable remotes do not share a key: rotating the garbage resets the count")
	}
	if ok, _ := l.Allow(""); ok {
		t.Error("an empty remote is not counted with the unparsable ones")
	}
	if ok, _ := l.Allow("192.0.2.1:1"); !ok {
		t.Error("a real IP is blocked by the unparsable key")
	}
}

func TestLimiterPrunesExpiredAndStaysBounded(t *testing.T) {
	t.Parallel()
	c := newClock()
	l := access.NewLimiter(c.now)
	for i := range 3 * access.MaxTracked {
		l.Fail(fmt.Sprintf("10.%d.%d.%d:1", i>>16&255, i>>8&255, i&255))
	}
	if n := l.Tracked(); n > access.MaxTracked+1 {
		t.Fatalf("%d entries tracked, want at most %d", n, access.MaxTracked+1)
	}
	c.add(time.Minute)
	l.Fail("203.0.113.1:1")
	if n := l.Tracked(); n != 1 {
		t.Errorf("%d entries after the window passed, want 1: expired entries must be pruned", n)
	}
}

func TestLimiterFullTableStillBlocksNewAttackers(t *testing.T) {
	t.Parallel()
	l := access.NewLimiter(newClock().now)
	for i := range access.MaxTracked {
		l.Fail(fmt.Sprintf("10.%d.%d.%d:1", i>>16&255, i>>8&255, i&255))
	}
	fail(l, "198.51.100.9:1", 5) // a new IP when the table is full
	if ok, _ := l.Allow("198.51.100.9:1"); ok {
		t.Error("a full table let a new IP guess without limit")
	}
}
