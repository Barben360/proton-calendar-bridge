package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const marker = "MARKER-7f3a"

// panicHub panics with marker in every CalDAV request's Route.
type panicHub struct{ *service.Hub }

func (h panicHub) Route(context.Context, string, string, string) (service.API, string, time.Duration, error) {
	panic(marker)
}

// reportSink is a loopback Sentry endpoint: hold, when set, blocks each
// request until it closes (or the request is cancelled).
type reportSink struct {
	*httptest.Server
	hold chan struct{}
	mu   sync.Mutex
	n    int
}

func newReportSink(t *testing.T, hold chan struct{}) *reportSink {
	t.Helper()
	k := &reportSink{hold: hold}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		k.mu.Lock()
		k.n++
		k.mu.Unlock()
		if k.hold != nil {
			select {
			case <-k.hold:
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(k.Close)
	return k
}

func (k *reportSink) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n
}

func (k *reportSink) dsn() string {
	return "http://pubkey@" + strings.TrimPrefix(k.URL, "http://") + "/7"
}

func reportStore(e *env) report.Store {
	return report.Store{StateDir: e.dirs.State, Env: func(string) string { return "" }}
}

// install makes the process record into store for the test (not parallel:
// the recorder is process-wide).
func install(t *testing.T, store report.Store, dsn string) {
	t.Helper()
	report.Install(&report.Process{Store: store, Meta: report.Meta{Binary: "pcb", Command: "pcb serve", Install: "test", Version: "0"}, DSN: dsn})
	t.Cleanup(report.Uninstall)
}

func davURL(r *running, id string) string {
	_, port, _ := net.SplitHostPort(r.info.Addr)
	return "http://127.0.0.1:" + port + "/dav/" + id + "/"
}

func TestHandlerPanicAbortsAndRecords(t *testing.T) {
	e := newEnv(t)
	id, password := e.login(t)
	store := reportStore(e)
	install(t, store, "http://pubkey@127.0.0.1:1/7")
	r := e.start(t, panicHub{e.hub}, nil)

	resp, err := r.propfind(t.Context(), davURL(r, id), password)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the client got %s, want the aborted connection", resp.Status)
	}
	if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") {
		t.Errorf("error %v, want an aborted connection", err)
	}

	pend, err := store.Pending()
	if err != nil || len(pend) != 1 {
		t.Fatalf("pending = %d, %v; want exactly one report", len(pend), err)
	}
	if strings.Contains(string(pend[0].JSON), marker) {
		t.Errorf("the report holds the panic's text:\n%s", pend[0].JSON)
	}
	var ev struct {
		Exception struct {
			Values []struct {
				Stacktrace struct {
					Frames []report.Frame `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		} `json:"exception"`
	}
	if err := json.Unmarshal(pend[0].JSON, &ev); err != nil || len(ev.Exception.Values) != 1 {
		t.Fatalf("report JSON: %v\n%s", err, pend[0].JSON)
	}
	frames := ev.Exception.Values[0].Stacktrace.Frames
	// Outermost first (Sentry order), ending at the panicking handler: the
	// recovery's own frames (runtime's panic, the middleware's deferred
	// func, debug.Stack) are dropped.
	if len(frames) == 0 || !strings.Contains(frames[len(frames)-1].Function, "panicHub.Route") || !frames[len(frames)-1].InApp {
		t.Errorf("the last frame is not the in_app panicking handler:\n%v", frames)
	}
	for _, f := range frames {
		if strings.HasSuffix(f.Function, "recordPanics.func1.1") || strings.Contains(f.Function, "runtime/debug") || f.Function == "panic" {
			t.Errorf("recovery frame %q in the report", f.Function)
		}
	}
	if len(frames) < 2 || !strings.Contains(frames[0].Function, "net/http") {
		t.Errorf("frames do not start at the outermost call: %v", frames)
	}
}

func TestHandlerAbortIsNotRecorded(t *testing.T) {
	e := newEnv(t)
	store := reportStore(e)
	install(t, store, "http://pubkey@127.0.0.1:1/7")
	h := daemon.RecordPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if v := recover(); !errors.Is(asError(v), http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
		if pend, _ := store.Pending(); len(pend) != 0 {
			t.Errorf("%d reports for an abort", len(pend))
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestServeReadyWhileSenderHangs(t *testing.T) {
	e := newEnv(t)
	id, password := e.login(t)
	store := reportStore(e)
	hold := make(chan struct{})
	k := newReportSink(t, hold)
	t.Cleanup(func() { close(hold) }) // after the daemon's own cleanup: Run ends first
	if err := store.SetMode(t.Context(), report.Always); err != nil {
		t.Fatal(err)
	}
	install(t, store, k.dsn())
	if err := store.Save(t.Context(), report.PanicEvent("x", []report.Frame{{Function: "p.f"}}, report.Meta{}, time.Now())); err != nil {
		t.Fatal(err)
	}
	snd := &report.Sender{DSN: k.dsn(), Store: store}
	begun := time.Now()
	r := e.start(t, e.hub, func(o *daemon.Options) { o.Reports = snd })
	if d := time.Since(begun); d > 2*time.Second {
		t.Errorf("Ready took %v with the sender hanging", d)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	resp, err := r.propfind(ctx, davURL(r, id), password)
	if err != nil {
		t.Fatalf("CalDAV did not answer within 2 s while the sender hangs: %v", err)
	}
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for k.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if k.count() == 0 {
		t.Error("the sender never reached its endpoint: the test proves nothing")
	}
}

// stuckTransport holds each request until release closes, ignoring its
// context: a sender that cannot be interrupted.
type stuckTransport struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stuckTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil, errors.New("released")
}

// Run ends the sender with itself: it returns only once the sender
// goroutine has, and the sender sends nothing after that.
func TestSenderStopsWithRun(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	store := reportStore(e)
	tr := &stuckTransport{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-tr.release:
		default:
			close(tr.release)
		}
	})
	if err := store.SetMode(t.Context(), report.Always); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), report.PanicEvent("x", []report.Frame{{Function: "p.f"}}, report.Meta{}, time.Now())); err != nil {
		t.Fatal(err)
	}
	r := e.start(t, e.hub, func(o *daemon.Options) {
		o.Reports = &report.Sender{DSN: "http://pubkey@127.0.0.1:1/7", Store: store, Client: &http.Client{Transport: tr}}
	})
	select {
	case <-tr.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the sender never sent")
	}
	r.cancel()
	select {
	case <-r.fin:
		t.Fatalf("Run returned while its sender goroutine was still running (%v)", r.err)
	case <-time.After(500 * time.Millisecond):
	}
	close(tr.release)
	select {
	case <-r.fin:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return once the sender ended")
	}
	if n := senderGoroutines(); n != 0 {
		t.Errorf("%d sender goroutines still run when Run returned", n)
	}
}

// senderGoroutines counts the goroutines running report.Sender.Run.
func senderGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "report.Sender.Run")
}

func asError(v any) error {
	err, _ := v.(error)
	return err
}

// startReports runs StartReports for dir with the given environment and
// returns whether it started.
func startReports(t *testing.T, dir string, env map[string]string) bool {
	t.Helper()
	_, stop, ok := daemon.StartReports(daemon.ReportStart{
		StateDir: dir, DSN: "https://k@127.0.0.1/1",
		Meta: report.Meta{Binary: "pcb", Command: "pcb status", Install: "other", Version: "v0.0.0"},
		Env:  func(k string) string { return env[k] },
		Now:  time.Now,
	})
	t.Cleanup(stop)
	return ok
}

func crashFiles(t *testing.T, dir string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(report.Dir(dir), "crash-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

func TestStartReportsWritesNoCrashFileWhenNever(t *testing.T) {
	setFile := func(t *testing.T, dir string) {
		t.Helper()
		st := report.Store{StateDir: dir}
		if err := st.SetMode(context.Background(), report.Never); err != nil {
			t.Fatal(err)
		}
	}
	for name, prep := range map[string]func(*testing.T, string) map[string]string{
		"environment": func(*testing.T, string) map[string]string { return map[string]string{report.EnvSwitch: "never"} },
		"reports.json": func(t *testing.T, dir string) map[string]string {
			setFile(t, dir)
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			env := prep(t, dir)
			if startReports(t, dir, env) {
				t.Fatal("StartReports started with reports turned off")
			}
			if n := crashFiles(t, dir); n != 0 {
				t.Fatalf("%d crash files written with reports turned off", n)
			}
			// A later process in ask mode finds nothing to collect.
			n, err := report.Store{StateDir: dir, Env: func(string) string { return "" }, Now: time.Now}.Collect(context.Background())
			if err != nil || n != 0 {
				t.Fatalf("Collect = %d, %v; want nothing", n, err)
			}
		})
	}
}

func TestStartReportsWritesACrashFileWhenAsking(t *testing.T) {
	dir := t.TempDir()
	if !startReports(t, dir, nil) {
		t.Fatal("StartReports did not start in ask mode")
	}
	if crashFiles(t, dir) == 0 {
		t.Fatal("no crash file in ask mode")
	}
}

// writeDeadCrash writes a crash file with a traceback that no process
// holds, named for this process's pid: the previous PID 1 of a container.
func writeDeadCrash(t *testing.T, dir string) string {
	t.Helper()
	rdir := report.Dir(dir)
	if err := os.MkdirAll(rdir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rdir, fmt.Sprintf("crash-pcb-%d-1.txt", os.Getpid()))
	body := "# pcb crash file v1 binary=pcb command=serve version=1 install=docker\npanic: x\n\ngoroutine 1 [running]:\nmain.main()\n\tmain.go:1 +0x1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Start-up collects a dead process's crash, even one whose file carries
// this process's pid (pcb serve is PID 1 at every container start).
func TestStartReportsCollectsTheCrashOfAPreviousSamePid(t *testing.T) {
	dir := t.TempDir()
	dead := writeDeadCrash(t, dir)
	if !startReports(t, dir, nil) {
		t.Fatal("StartReports did not start in ask mode")
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("the dead crash file is still there: %v", err)
	}
	pend, err := report.Store{StateDir: dir, Env: func(string) string { return "" }}.Pending()
	if err != nil || len(pend) != 1 || pend[0].Source != "crash" {
		t.Errorf("pending %v, %v; want the one crash", pend, err)
	}
}
