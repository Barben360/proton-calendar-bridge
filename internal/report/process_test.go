package report_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

func install(t *testing.T, dsn string) (*report.Process, report.Store) {
	t.Helper()
	s := newStore(t)
	p := &report.Process{Store: s, Meta: meta, DSN: dsn}
	report.Install(p)
	t.Cleanup(report.Uninstall)
	return p, s
}

func TestRecordErrorWithoutProcessIsNoop(t *testing.T) {
	report.Uninstall()
	report.RecordError(errors.New("x"))
	report.RecordPanic("x", nil)
	if report.Current() != nil {
		t.Error("Current is not nil")
	}
	_, s := install(t, "") // a Process without DSN records nothing either
	report.RecordError(errors.New("x"))
	report.RecordPanic("x", nil)
	if ids := pendingIDs(t, s); len(ids) != 0 {
		t.Errorf("saved without a DSN: %v", ids)
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no signal")
	}
}

func TestRecordErrorSavesAndPokes(t *testing.T) {
	p, s := install(t, "https://k@errors.example.test/1")
	a, stopA := p.Subscribe()
	b, stopB := p.Subscribe()
	defer stopB()
	if report.Current() != p {
		t.Error("Current is not the installed Process")
	}
	report.RecordError(errors.New("boom"))
	waitSignal(t, a)
	waitSignal(t, b)
	pend, err := s.Pending()
	if err != nil || len(pend) != 1 || pend[0].Source != "error" {
		t.Fatalf("pending %+v err %v", pend, err)
	}
	if !bytes.Contains(pend[0].JSON, []byte("TestRecordErrorSavesAndPokes")) {
		t.Errorf("the caller is not in the frames: %s", pend[0].JSON)
	}
	if bytes.Contains(pend[0].JSON, []byte("boom")) {
		t.Error("the error text is in the report")
	}

	stopA()
	report.RecordPanic("secret panic text", []byte("goroutine 1 [running]:\nmain.f()\n\t/x/y/z.go:1 +0x1\n"))
	waitSignal(t, b)
	select {
	case <-a:
		t.Error("an unsubscribed channel was signalled")
	default:
	}
	if len(pendingIDs(t, s)) != 2 {
		t.Errorf("pending %v", pendingIDs(t, s))
	}
}

func TestRecordDoesNotBlockOnASlowSubscriber(t *testing.T) {
	p, _ := install(t, "https://k@errors.example.test/1")
	_, stop := p.Subscribe() // never read
	defer stop()
	done := make(chan struct{})
	go func() {
		report.RecordError(errors.New("one"))
		report.RecordPanic("two", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RecordError blocked on a subscriber")
	}
}

// recovered panics, records the panic through RecordPanic from its deferred
// call, as an HTTP middleware does.
//
//go:noinline
func recovered() {
	defer func() {
		if v := recover(); v != nil {
			report.RecordPanic(v, debug.Stack())
		}
	}()
	panicHere()
}

//go:noinline
func panicHere() { panic("MARKER-7f3a") }

func TestRecordPanicEndsAtThePanickingFrame(t *testing.T) {
	s := newStore(t)
	report.Install(&report.Process{Store: s, Meta: meta, DSN: "http://k@127.0.0.1:1/1"})
	defer report.Uninstall()
	recovered()
	pend, err := s.Pending()
	if err != nil || len(pend) != 1 {
		t.Fatalf("pending %d, %v", len(pend), err)
	}
	var ev struct {
		Exception struct {
			Values []struct {
				Stacktrace struct{ Frames []report.Frame }
			}
		}
	}
	if err := json.Unmarshal(pend[0].JSON, &ev); err != nil {
		t.Fatal(err)
	}
	frames := ev.Exception.Values[0].Stacktrace.Frames
	if len(frames) == 0 || !strings.HasSuffix(frames[len(frames)-1].Function, "report_test.panicHere") {
		t.Fatalf("the last frame is not the panicking function: %v", frames)
	}
	for _, f := range frames {
		if f.Function == "panic" || strings.Contains(f.Function, "runtime/debug") || strings.HasSuffix(f.Function, "report_test.recovered.func1") {
			t.Errorf("recovery frame %q kept", f.Function)
		}
	}
	if !strings.HasSuffix(frames[len(frames)-2].Function, "report_test.recovered") {
		t.Errorf("the caller of the panicking function is missing: %v", frames)
	}
}
