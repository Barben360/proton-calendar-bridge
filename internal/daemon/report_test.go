package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	index := func(sub string) int {
		for i, f := range frames {
			if f.Function == sub || strings.Contains(f.Function, sub) && sub != "panic" {
				return i
			}
		}
		return -1
	}
	// Outermost first (Sentry order): the connection's serve loop, the
	// middleware's ServeHTTP, the panicking Route, the runtime's panic, the
	// deferred recovery, and debug.Stack last.
	serve, route, rtpanic, stack := index("net/http.(*conn).serve"), index("panicHub.Route"), index("panic"), index("runtime/debug.Stack")
	if serve < 0 || route < 0 || rtpanic < 0 || stack < 0 || serve >= route || route >= rtpanic || rtpanic >= stack {
		t.Errorf("frame order wrong (serve %d, Route %d, rtpanic %d, Stack %d of %d):\n%v", serve, route, rtpanic, stack, len(frames), frames)
	}
	if stack != len(frames)-1 {
		t.Errorf("the innermost frame is at %d of %d, want the last", stack, len(frames))
	}
	if route >= 0 && !frames[route].InApp {
		t.Errorf("the panicking handler's frame is not in_app: %+v", frames[route])
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

func TestSenderStopsWithRun(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	store := reportStore(e)
	k := newReportSink(t, nil)
	if err := store.SetMode(t.Context(), report.Always); err != nil {
		t.Fatal(err)
	}
	poke := make(chan struct{}, 1)
	r := e.start(t, e.hub, func(o *daemon.Options) {
		o.Reports = &report.Sender{DSN: k.dsn(), Store: store}
		o.ReportPoke = poke
	})
	r.cancel()
	select {
	case <-r.fin:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	if err := store.Save(t.Context(), report.PanicEvent("x", []report.Frame{{Function: "p.f"}}, report.Meta{}, time.Now())); err != nil {
		t.Fatal(err)
	}
	poke <- struct{}{}
	time.Sleep(300 * time.Millisecond)
	if n := k.count(); n != 0 {
		t.Errorf("the sender sent %d requests after Run returned", n)
	}
}

func asError(v any) error {
	err, _ := v.(error)
	return err
}
