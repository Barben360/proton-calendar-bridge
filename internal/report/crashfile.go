package report

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// crashHeader opens a crash file: the process's Meta, as query-escaped
// key=value words, so that whoever collects the file at a later start can
// build the report the dead process would have.
const crashHeader = "# pcb crash file v1"

// maxCrashFile bounds what is read of a crash file.
const maxCrashFile = 1 << 20

var (
	crashName   = regexp.MustCompile(`^(crash-.*-\d+-\d+)\.txt$`)
	binaryChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)
)

// ownFiles holds the names of this process's own crash files, which
// Collect never touches. Names, not the pid: a pid is reused (in a
// container pcb serve is PID 1 at every start), so a dead process's file
// can carry this process's pid.
var (
	ownFiles   = map[string]struct{}{}
	ownFilesMu sync.Mutex
)

func setOwn(name string, own bool) {
	ownFilesMu.Lock()
	defer ownFilesMu.Unlock()
	if own {
		ownFiles[name] = struct{}{}
	} else {
		delete(ownFiles, name)
	}
}

func isOwn(name string) bool {
	ownFilesMu.Lock()
	defer ownFilesMu.Unlock()
	_, ok := ownFiles[name]
	return ok
}

func collectLockPath(stateDir string) string {
	return filepath.Join(Dir(stateDir), "collect.lock")
}

// StartCrashFile makes the Go runtime write a fatal crash's traceback to
// <state>/reports/crash-<binary>-<pid>-<nanos>.txt, whose first line holds
// m. Without a state directory it does nothing. stop ends it and removes
// the file, for a clean exit: a file left behind is a crash, offered by
// Store.Collect at the next start. The process holds a lock on
// crash-….lock for its whole life (the system drops it when the process
// dies), which tells Collect the file is no one's any more.
func StartCrashFile(stateDir string, m Meta) (stop func(), err error) {
	noop := func() {}
	if info, err := os.Stat(stateDir); err != nil || !info.IsDir() {
		return noop, nil
	}
	if err := acl.MkdirAll(Dir(stateDir)); err != nil {
		return noop, err
	}
	name := fmt.Sprintf("crash-%s-%d-%d.txt", binaryChars.ReplaceAllString(m.Binary, "_"), os.Getpid(), time.Now().UnixNano())
	path := filepath.Join(Dir(stateDir), name)
	lock := strings.TrimSuffix(path, ".txt") + ".lock"
	unlock, _, err := filelock.TryLock(lock)
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		unlock = func() {}
	case err != nil:
		return noop, err
	}
	release := func() {
		unlock()
		_ = os.Remove(lock)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		release()
		return noop, err
	}
	if _, err := io.WriteString(f, header(m)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		release()
		return noop, err
	}
	// The runtime keeps its own duplicate of the descriptor: closing f
	// lets stop remove the file on Windows too.
	setOwn(name, true)
	err = debug.SetCrashOutput(f, debug.CrashOptions{})
	_ = f.Close()
	if err != nil {
		_ = os.Remove(path)
		setOwn(name, false)
		release()
		return noop, err
	}
	return func() {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		_ = os.Remove(path)
		setOwn(name, false)
		release()
	}, nil
}

func header(m Meta) string {
	var b strings.Builder
	b.WriteString(crashHeader)
	for _, kv := range [][2]string{{"binary", m.Binary}, {"command", m.Command}, {"version", m.Version}, {"install", m.Install}} {
		b.WriteString(" " + kv[0] + "=" + url.QueryEscape(kv[1]))
	}
	return b.String() + "\n"
}

// parseHeader reads a crash file's first line; ok is false when it is not
// one.
func parseHeader(line string) (Meta, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), crashHeader)
	if !ok {
		return Meta{}, false
	}
	var m Meta
	for _, w := range strings.Fields(rest) {
		k, v, _ := strings.Cut(w, "=")
		v, err := url.QueryUnescape(v)
		if err != nil {
			continue
		}
		switch k {
		case "binary":
			m.Binary = v
		case "command":
			m.Command = v
		case "version":
			m.Version = v
		case "install":
			m.Install = v
		}
	}
	return m, true
}

// Collect turns the crash files of dead processes into pending reports and
// deletes them, and returns how many reports it saved. A crash file whose
// lock a running process holds, or that this process made, is left alone; one with
// nothing past its header and no holder is a clean death and is deleted
// (where locks are unsupported it is kept, and only files with a
// traceback are collected). A file that does
// not read as a traceback becomes a report of kind panic with no frames,
// rather than being lost. Calls serialise on <state>/reports/collect.lock
// (Save has its own lock), so two processes never report one crash twice.
func (s Store) Collect(ctx context.Context) (int, error) {
	if s.StateDir == "" {
		return 0, nil
	}
	dir := Dir(s.StateDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	unlock, err := filelock.Lock(ctx, collectLockPath(s.StateDir))
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
	case err != nil:
		return 0, err
	default:
		defer unlock()
	}
	saved := 0
	for _, ent := range entries {
		m := crashName.FindStringSubmatch(ent.Name())
		if m == nil || !ent.Type().IsRegular() || isOwn(ent.Name()) {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		lock := filepath.Join(dir, m[1]+".lock")
		o, locked, err := tryOwn(lock)
		if err != nil {
			return saved, err
		}
		if !locked {
			continue // its process is running
		}
		meta, body, at, err := readCrashFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			o.keep() // collected by another process meanwhile
			continue
		}
		if err != nil {
			o.keep()
			return saved, err
		}
		if strings.TrimSpace(body) == "" {
			if o.held {
				_ = os.Remove(path)
			}
			o.done()
			continue
		}
		c, err := ParseCrash([]byte(body))
		if err != nil {
			c = Crash{Kind: "panic", Message: removed}
		}
		if err := s.Save(ctx, CrashEvent(c, meta, at)); err != nil {
			o.keep()
			return saved, err
		}
		_ = os.Remove(path) // the report is safe: a file that stays is only collected again
		o.done()
		if s.Mode() != Never {
			saved++
		}
	}
	return saved, nil
}

// readCrashFile returns a crash file's Meta, what follows its header and
// its modification time (when the crash was written). A file without a
// header is all body, with no Meta.
func readCrashFile(path string) (Meta, string, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}, "", time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Meta{}, "", time.Time{}, err
	}
	r := bufio.NewReader(io.LimitReader(f, maxCrashFile))
	first, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Meta{}, "", time.Time{}, err
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		return Meta{}, "", time.Time{}, err
	}
	meta, ok := parseHeader(first)
	if !ok {
		return Meta{}, first + string(rest), info.ModTime(), nil
	}
	return meta, string(rest), info.ModTime(), nil
}

// owned is a crash file's lock, taken by Collect: held is false where
// locks are unsupported (nothing says the file's process is dead).
type owned struct {
	unlock func()
	lock   string
	held   bool
}

// tryOwn takes the lock without waiting; ok is false when a running
// process holds it.
func tryOwn(lock string) (*owned, bool, error) {
	unlock, ok, err := filelock.TryLock(lock)
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
		return &owned{unlock: func() {}}, true, nil
	case err != nil:
		return nil, false, err
	case !ok:
		return nil, false, nil
	}
	return &owned{unlock: unlock, lock: lock, held: true}, true, nil
}

// keep lets go of the lock, the file staying for another try.
func (o *owned) keep() { o.unlock() }

// done lets go of the lock and removes its file: the crash file is gone.
func (o *owned) done() {
	o.unlock() // before the removal: Windows refuses to remove a locked file
	if o.held {
		_ = os.Remove(o.lock)
	}
}
