package ctl

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sockDir is a private directory for a socket. Not t.TempDir: its path can
// exceed the 108 bytes of sun_path.
func sockDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	dir, err := os.MkdirTemp("", "pcb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func listen(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestSocketIsPrivate(t *testing.T) {
	t.Parallel()
	dir := sockDir(t)
	path := filepath.Join(dir, "pcb.sock")
	listen(t, path)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", fi.Mode())
	}
	for _, mode := range []os.FileMode{0o755, 0o710, 0o701} {
		open := filepath.Join(dir, "open")
		if err := os.Mkdir(open, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, mode); err != nil { // past the umask
			t.Fatal(err)
		}
		ln, err := Listen(filepath.Join(open, "pcb.sock"))
		if !errors.Is(err, ErrUnsafeDir) {
			if ln != nil {
				_ = ln.Close()
			}
			t.Errorf("Listen in a %o directory: %v, want ErrUnsafeDir", mode, err)
		}
		if _, err := os.Lstat(filepath.Join(open, "pcb.sock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a socket was created in the %o directory", mode)
		}
		if err := os.Remove(open); err != nil {
			t.Fatal(err)
		}
	}
}

// A socket left by a daemon that died is replaced without a probe (M5-P2).
func TestStaleSocketIsReplaced(t *testing.T) {
	t.Parallel()
	path := filepath.Join(sockDir(t), "pcb.sock")
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the stale socket is not there: %v", err)
	}
	if _, err := Dial(t.Context(), path); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("Dial on a stale socket: %v, want ErrNoDaemon", err)
	}
	listen(t, path)
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the new socket refuses: %v", err)
	}
	_ = c.Close()
}

func TestNonSocketFileIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(sockDir(t), "pcb.sock")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if !errors.Is(err, ErrNotASocket) {
		if ln != nil {
			_ = ln.Close()
		}
		t.Fatalf("Listen over a file: %v, want ErrNotASocket", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "keep me" {
		t.Errorf("the file was touched: %q, %v", b, err)
	}
}

// sun_path holds 108 bytes with its NUL: 107 is the longest path.
func TestLongSocketPathIsRefused(t *testing.T) {
	t.Parallel()
	dir := sockDir(t)
	pad := func(n int) string { // a path of n bytes in dir
		name := strings.Repeat("s", n-len(dir)-1)
		return filepath.Join(dir, name)
	}
	if p := pad(108); len(p) != 108 {
		t.Fatalf("len = %d", len(p))
	}
	if _, err := Listen(pad(108)); !errors.Is(err, ErrPathTooLong) {
		t.Errorf("Listen(108 bytes): %v, want ErrPathTooLong", err)
	}
	if _, err := Dial(t.Context(), pad(108)); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("Dial(108 bytes): %v, want ErrNoDaemon", err)
	}
	listen(t, pad(107))
}

func TestDialWithoutADaemon(t *testing.T) {
	t.Parallel()
	if _, err := Dial(t.Context(), filepath.Join(sockDir(t), "pcb.sock")); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("Dial on a missing socket: %v, want ErrNoDaemon", err)
	}
}

// The default peer check reads the peer's UID with SO_PEERCRED on Linux and
// leaves it unchecked elsewhere (the socket's mode guards it there).
func TestDefaultPeerUID(t *testing.T) {
	t.Parallel()
	path := filepath.Join(sockDir(t), "pcb.sock")
	ln := listen(t, path)
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer func() { _ = server.Close() }()
	uid, checked := defaultPeerUID(server)
	if runtime.GOOS != "linux" {
		if checked {
			t.Errorf("defaultPeerUID = %d, checked off Linux", uid)
		}
		return
	}
	if !checked || uid != os.Getuid() {
		t.Errorf("defaultPeerUID = %d, %v; want %d, true", uid, checked, os.Getuid())
	}
	// A connection that is not a socket is checked and refused (-1), never
	// let through unchecked.
	a, b := net.Pipe()
	defer func() { _, _ = a.Close(), b.Close() }()
	if uid, checked := defaultPeerUID(a); !checked || uid != -1 {
		t.Errorf("defaultPeerUID(pipe) = %d, %v; want -1, true", uid, checked)
	}
}
