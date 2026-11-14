//go:build windows

package ctl_test

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// foreignSID is a user SID that is never this test's.
const foreignSID = "S-1-5-21-1-2-3-1001"

// fileAllAccess is FILE_ALL_ACCESS, what GENERIC_ALL maps to on a pipe.
const fileAllAccess = 0x1F01FF

// errUntrustedMountPoint is ERROR_UNTRUSTED_MOUNT_POINT: a junction made by
// a standard user, which Redirection Guard does not follow.
const errUntrustedMountPoint = syscall.Errno(448)

// servePipe runs srv on the pipe of a fresh state directory until the test
// ends.
func servePipe(t *testing.T, srv *ctl.Server) served {
	t.Helper()
	return serveAt(t, srv, filepath.Join(t.TempDir(), "pcb.sock"))
}

func TestPipeServesTheSameUser(t *testing.T) {
	t.Parallel()
	s := servePipe(t, &ctl.Server{Hub: newHub(service.Status{Phase: "ready", Username: "alice"}), Daemon: daemon()})
	c, err := ctl.Dial(t.Context(), s.path)
	if err != nil {
		t.Fatalf("Dial(%s): %v", s.path, err)
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatalf("Status over the pipe: %v", err)
	}
	if st.Daemon == nil || st.Daemon.PID != daemon().PID || len(st.Accounts) != 1 {
		t.Errorf("Status over the pipe = %+v, want the served daemon and its account", st)
	}
	if logs := s.logs.String(); !strings.Contains(logs, "control request") || strings.Contains(logs, "level=WARN") {
		t.Errorf("logs lack the served request, or warn:\n%s", logs)
	}
}

// The name comes from the final path: case, the 8.3 short form and a
// junction give one pipe; another directory gets another.
func TestPipeNameIsCanonical(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want, err := ctl.PipeName(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(want, `\\.\pipe\pcb-`) || len(want) != len(`\\.\pipe\pcb-`)+16 {
		t.Fatalf("pipeName(%s) = %s, want \\\\.\\pipe\\pcb-<16 hex>", dir, want)
	}
	same := func(what, p string) {
		t.Helper()
		got, err := ctl.PipeName(p)
		if err != nil {
			t.Fatalf("pipeName(%s), %s: %v", p, what, err)
		}
		if got != want {
			t.Errorf("pipeName(%s), %s = %s, want %s", p, what, got, want)
		}
	}
	same("upper case", strings.ToUpper(dir))

	long, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatal(err)
	}
	if short := windows.UTF16ToString(buf[:n]); strings.EqualFold(short, dir) {
		t.Logf("short form skipped: 8.3 names are disabled on the volume of %s", dir)
	} else {
		same("8.3 short form", short)
	}

	link := filepath.Join(t.TempDir(), "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput(); err != nil {
		t.Logf("junction skipped: mklink /J failed: %v: %s", err, out)
	} else if _, err := ctl.PipeName(link); errors.Is(err, errUntrustedMountPoint) {
		// Redirection Guard: Windows follows no junction a standard user made.
		t.Logf("junction skipped: Windows refuses to traverse it: %v", err)
	} else {
		same("junction", link)
	}

	other, err := ctl.PipeName(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other == want {
		t.Errorf("two directories share the pipe %s", want)
	}
	if _, err := ctl.PipeName(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pipeName of a missing directory: %v, want ErrNotExist", err)
	}
}

// The pipe's security descriptor: owned by the user, a protected DACL of a
// single ACE giving the user full access, so no other user (Administrators
// included) can open it or create an instance of it.
func TestPipeDACLIsTheUserOnly(t *testing.T) {
	t.Parallel()
	s := servePipe(t, &ctl.Server{Hub: newHub(service.Status{}), Daemon: daemon()})
	name, err := ctl.PipeName(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := winio.DialPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	h := windows.Handle(conn.(interface{ Fd() uintptr }).Fd())
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := ctl.CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner.String() != user {
		t.Errorf("owner = %v (%v), want %s", owner, err, user)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("DACL not protected: control %#x (%v)", control, err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		t.Fatalf("no DACL: %v", err)
	}
	if acl.AceCount != 1 {
		t.Fatalf("%d ACEs, want 1 (%s)", acl.AceCount, sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
	// GENERIC_ALL is stored mapped to the pipe's specific rights.
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || sid != user ||
		(ace.Mask != windows.GENERIC_ALL && ace.Mask != fileAllAccess) {
		t.Errorf("ACE = type %d, mask %#x, SID %s; want allow, full access, %s (%s)", ace.Header.AceType, ace.Mask, sid, user, sd)
	}
}

// A pipe name already served (a second pcb-ui, or another user's process
// squatting it) fails Listen with ErrPipeTaken; never shared. A server
// that answers on it fails Listen at once.
func TestSecondListenIsTaken(t *testing.T) {
	t.Parallel()
	s := servePipe(t, &ctl.Server{Hub: newHub(service.Status{}), Daemon: daemon()})
	start := time.Now()
	ln, err := ctl.Listen(s.path)
	took := time.Since(start)
	if ln != nil {
		_ = ln.Close()
	}
	if !errors.Is(err, ctl.ErrPipeTaken) || errors.Is(err, ctl.ErrPipeHeld) {
		t.Fatalf("Listen while a server answers: %v, want ErrPipeTaken, not ErrPipeHeld", err)
	}
	if took > 250*time.Millisecond {
		t.Errorf("Listen while a server answers took %v, want under 250 ms", took)
	}

	dir := t.TempDir()
	first, err := ctl.Listen(filepath.Join(dir, "pcb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	name, err := ctl.PipeName(dir)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	ln, err = ctl.Listen(filepath.Join(dir, "pcb.sock"))
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the second Listen took %v, want under 2 s", took)
	}
	if ln != nil {
		_ = ln.Close()
	}
	if !errors.Is(err, ctl.ErrPipeTaken) || !strings.Contains(err.Error(), name) {
		t.Fatalf("the second Listen: %v, want ErrPipeTaken naming %s", err, name)
	}
	t.Logf("the second Listen: %v", err)
	// Closing the first releases the name.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := ctl.Listen(filepath.Join(dir, "pcb.sock"))
	if err != nil {
		t.Fatalf("Listen after the first closed: %v", err)
	}
	_ = again.Close()
}

// openEnds connects a client to a listener on dir's pipe and closes the
// listener: the pipe's name lives on in the server's end of that
// connection, which the caller closes (and the client's, which never holds
// the name).
func openEnds(t *testing.T, dir string) (server, client net.Conn) {
	t.Helper()
	ln, err := ctl.Listen(filepath.Join(dir, "pcb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	name, err := ctl.PipeName(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err = winio.DialPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if server = <-accepted; server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return server, client
}

// A restart on the same state: the previous server's end of a connection
// may still be closing when Listen runs again (go-winio's Close returns at
// once to a second caller while the first is still closing the handle, as
// net/http's Close and a connection's own goroutine do). Listen waits for
// the name, within its bound, instead of calling it served.
func TestListenWaitsForThePreviousServerEnd(t *testing.T) {
	t.Parallel()
	// The end must close while Listen runs: a Listen scheduled after the
	// close would pass without waiting, so such a run is set up again.
	for attempt := 1; attempt <= 3; attempt++ {
		dir := t.TempDir()
		server, _ := openEnds(t, dir)
		var closing atomic.Int64 // when the close began, in Unix nanoseconds
		time.AfterFunc(100*time.Millisecond, func() {
			closing.Store(time.Now().UnixNano())
			_ = server.Close()
		})
		start := time.Now()
		ln, err := ctl.Listen(filepath.Join(dir, "pcb.sock"))
		took := time.Since(start)
		if err != nil {
			t.Fatalf("Listen while the previous server's end closes: %v, want the pipe once it has closed", err)
		}
		_ = ln.Close()
		if began := closing.Load(); began == 0 || began < start.UnixNano() {
			t.Logf("attempt %d: Listen started after the close: set up again", attempt)
			continue
		}
		if took > time.Second {
			t.Errorf("Listen took %v, want under 1 s", took)
		}
		return
	}
	t.Skip("Listen started after the close in 3 attempts (a loaded host): the wait was not exercised")
}

// A name nothing answers on that stays held past Listen's bound is
// ErrPipeHeld, an ErrPipeTaken, under 1 s: the restart may be retried in a
// moment.
func TestListenGivesUpOnAHeldName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	openEnds(t, dir)
	name, err := ctl.PipeName(dir)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ln, err := ctl.Listen(filepath.Join(dir, "pcb.sock"))
	took := time.Since(start)
	if ln != nil {
		_ = ln.Close()
	}
	if !errors.Is(err, ctl.ErrPipeHeld) || !errors.Is(err, ctl.ErrPipeTaken) || !strings.Contains(err.Error(), name) {
		t.Fatalf("Listen on a held name: %v, want ErrPipeHeld (an ErrPipeTaken) naming %s", err, name)
	}
	if took < 200*time.Millisecond || took > time.Second {
		t.Errorf("Listen on a held name gave up after %v, want a wait under 1 s", took)
	}
}

// A name ListenPipe is denied while the probe keeps finding it gone (a
// token that may not create it): Listen steps between tries rather than
// spinning, and still gives up within its bound. Not parallel: it replaces
// the package's ListenPipe and probe.
func TestListenStepsOnANameThatKeepsVanishing(t *testing.T) {
	tries := ctl.SetDeniedAndGone(t)
	start := time.Now()
	_, err := ctl.Listen(filepath.Join(t.TempDir(), "pcb.sock"))
	took := time.Since(start)
	if !errors.Is(err, ctl.ErrPipeHeld) {
		t.Fatalf("Listen: %v, want ErrPipeHeld", err)
	}
	if took > time.Second {
		t.Errorf("Listen gave up after %v, want under 1 s", took)
	}
	// 0.5 s in steps of 10 ms is about 50 tries; a busy loop makes
	// thousands.
	if *tries > 100 {
		t.Errorf("Listen tried %d times in %v: a busy loop", *tries, took)
	}
	t.Logf("%d tries in %v", *tries, took)
}

// The server reads its client's SID (the pipe client's process token) and
// refuses any other than its own, as SO_PEERCRED does on Linux; one it
// cannot read is refused too.
func TestServerRefusesAClientOfAnotherSID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		peer func(net.Conn) (string, error)
		log  string
	}{
		{"another SID", func(net.Conn) (string, error) { return foreignSID, nil }, "peer_sid=" + foreignSID},
		{"an unreadable SID", func(net.Conn) (string, error) { return "", errors.New("no token") }, "no token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hub := newHub(service.Status{Phase: "ready", Username: "alice"})
			s := servePipe(t, &ctl.Server{Hub: hub, Daemon: daemon(), PeerSID: tc.peer})
			c, err := ctl.Dial(t.Context(), s.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			st, err := c.Status(t.Context())
			if exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "the control socket answers only its own user") {
				t.Fatalf("Status from %s: %+v, %v (exit %d); want blocked", tc.name, st, err, exitcode.From(err))
			}
			if logs := s.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, tc.log) {
				t.Errorf("logs lack the warning naming %q:\n%s", tc.log, logs)
			}
			var ee *exitcode.Error
			if !errors.As(err, &ee) || !strings.Contains(ee.Hint, "elevated pcb of another login session") {
				t.Errorf("hint = %q, want one naming an elevated pcb of another login session", ee.Hint)
			}
		})
	}
	// The user's own SID is served: the check compares, it does not refuse all.
	user, err := ctl.CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	s := servePipe(t, &ctl.Server{Hub: newHub(service.Status{}), Daemon: daemon(), PeerSID: func(net.Conn) (string, error) { return user, nil }})
	c, err := ctl.Dial(t.Context(), s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Status(t.Context()); err != nil {
		t.Errorf("Status from the user's own SID: %v", err)
	}
}

// The client reads the SID owning the pipe before it sends anything: a
// squatter of another user never gets a request, on the first dial or on a
// redial. Not parallel: it replaces the package's lookup.
func TestDialRefusesAServerOfAnotherSID(t *testing.T) {
	api := newHub(service.Status{})
	s := servePipe(t, &ctl.Server{Hub: api, Daemon: daemon()})
	ctl.SetServerSIDOf(t, func(net.Conn) (string, error) { return foreignSID, nil })
	c, err := ctl.Dial(t.Context(), s.path)
	if err == nil {
		_, err = c.Login(t.Context(), service.LoginRequest{Username: "alice", Password: "pw"})
		_ = c.Close()
	}
	if exitcode.From(err) != exitcode.Blocked || !errors.Is(err, ctl.ErrRefused) ||
		!strings.Contains(err.Error(), "is owned by another user ("+foreignSID+")") {
		t.Fatalf("a server of another SID: %v (exit %d); want blocked and refused, naming its SID", err, exitcode.From(err))
	}
	var ee *exitcode.Error
	if !errors.As(err, &ee) || !strings.Contains(ee.Hint, "pcb-ui") {
		t.Errorf("hint = %q, want one naming pcb-ui", ee.Hint)
	}

	ctl.SetServerSIDOf(t, func(net.Conn) (string, error) { return "", errors.New("no token") })
	if _, err := ctl.Dial(t.Context(), s.path); exitcode.From(err) != exitcode.Blocked || !errors.Is(err, ctl.ErrRefused) {
		t.Errorf("a server of an unreadable SID: %v (exit %d); want blocked and refused", err, exitcode.From(err))
	}

	// The redial that replaces a stale first connection checks too.
	user, err := ctl.CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	ctl.SetServerSIDOf(t, func(net.Conn) (string, error) { return user, nil })
	clock := time.Now()
	c, err = ctl.DialWithClock(t.Context(), s.path, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctl.SetServerSIDOf(t, func(net.Conn) (string, error) { return foreignSID, nil })
	clock = clock.Add(3 * time.Second)
	if _, err := c.Login(t.Context(), service.LoginRequest{Username: "alice", Password: "pw"}); exitcode.From(err) != exitcode.Blocked ||
		!strings.Contains(err.Error(), foreignSID) {
		t.Errorf("Login over a redial to another SID: %v (exit %d); want blocked naming it", err, exitcode.From(err))
	}

	if l, _ := api.calls(); l != 0 {
		t.Errorf("the service got %d logins", l)
	}
	if logs := s.logs.String(); strings.Contains(logs, "control request") {
		t.Errorf("a request reached the server:\n%s", logs)
	}
}

// Without this user's SID the client cannot tell its pipe from a
// squatter's: refused, and the message says why, never "another user".
// Not parallel: it replaces the package's lookup.
func TestDialWithoutTheUsersSIDIsRefused(t *testing.T) {
	api := newHub(service.Status{})
	s := servePipe(t, &ctl.Server{Hub: api, Daemon: daemon()})
	ctl.SetCurrentSID(t, func() (string, error) { return "", errors.New("no token") })
	_, err := ctl.Dial(t.Context(), s.path)
	if exitcode.From(err) != exitcode.Blocked || !errors.Is(err, ctl.ErrRefused) ||
		!strings.Contains(err.Error(), "cannot read this user's SID (no token)") || strings.Contains(err.Error(), "another user") {
		t.Fatalf("Dial without the user's SID: %v (exit %d); want blocked, refused, naming the lookup", err, exitcode.From(err))
	}
	if logs := s.logs.String(); strings.Contains(logs, "control request") {
		t.Errorf("a request reached the server:\n%s", logs)
	}
}

func TestDialWithoutDaemonIsNoDaemon(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "pcb.sock"), filepath.Join(dir, "missing", "pcb.sock")} {
		start := time.Now()
		if _, err := ctl.Dial(t.Context(), path); !errors.Is(err, ctl.ErrNoDaemon) {
			t.Errorf("Dial(%s) with no daemon: %v, want ErrNoDaemon", path, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("Dial(%s) with no daemon took %v", path, took)
		}
	}
	// A pipe that was served and closed is gone: no daemon either.
	ln, err := ctl.Listen(filepath.Join(dir, "pcb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if _, err := ctl.Dial(t.Context(), filepath.Join(dir, "pcb.sock")); !errors.Is(err, ctl.ErrNoDaemon) {
		t.Errorf("Dial after the listener closed: %v, want ErrNoDaemon", err)
	}
}

// The real lookups read the client process's token (server side) and the
// pipe's owner (client side, the SID Listen set) on a real pipe connection,
// and refuse a connection that is not a pipe.
func TestPipePeerSIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ln, err := ctl.Listen(filepath.Join(dir, "pcb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	name, err := ctl.PipeName(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := winio.DialPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer func() { _ = server.Close() }()
	user, err := ctl.CurrentSID()
	if err != nil || !strings.HasPrefix(user, "S-1-5-") {
		t.Fatalf("CurrentSID = %q, %v", user, err)
	}
	if sid, err := ctl.ClientSIDOf(server); err != nil || sid != user {
		t.Errorf("ClientSIDOf = %q, %v; want %s", sid, err, user)
	}
	if sid, err := ctl.ServerSIDOf(client); err != nil || sid != user {
		t.Errorf("ServerSIDOf = %q, %v; want %s", sid, err, user)
	}
	a, b := net.Pipe()
	defer func() { _, _ = a.Close(), b.Close() }()
	if sid, err := ctl.ClientSIDOf(a); err == nil {
		t.Errorf("ClientSIDOf(net.Pipe) = %q, want an error", sid)
	}
	if sid, err := ctl.ServerSIDOf(a); err == nil {
		t.Errorf("ServerSIDOf(net.Pipe) = %q, want an error", sid)
	}
}
