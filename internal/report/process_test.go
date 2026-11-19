package report_test

import (
	"bytes"
	"errors"
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
