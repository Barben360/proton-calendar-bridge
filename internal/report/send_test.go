package report_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// sink is a loopback Sentry endpoint recording what it is sent.
type sink struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []sunk
}

type sunk struct {
	Path   string
	Header http.Header
	Body   []byte
}

func newSink(t *testing.T, handler func(n int, w http.ResponseWriter)) *sink {
	t.Helper()
	k := &sink{}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		k.mu.Lock()
		k.reqs = append(k.reqs, sunk{r.URL.Path, r.Header.Clone(), buf.Bytes()})
		n := len(k.reqs)
		k.mu.Unlock()
		if handler != nil {
			handler(n, w)
		}
	}))
	t.Cleanup(k.Close)
	return k
}

func (k *sink) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.reqs)
}

func (k *sink) dsn() string { return "http://pubkey@" + strings.TrimPrefix(k.URL, "http://") + "/7" }

// saveN saves n reports of distinct fingerprints and returns the store.
func saveN(t *testing.T, s report.Store, n int) {
	t.Helper()
	for i := range n {
		save(t, s, crashIn(fmt.Sprintf("fn%d", i), t0))
	}
}

func TestSendEnvelopeShape(t *testing.T) {
	k := newSink(t, nil)
	s := newStore(t)
	save(t, s, crashIn("serveLoop", t0))
	pend, _ := s.Pending()
	res, err := report.Sender{DSN: k.dsn(), Store: s}.SendPending(ctx)
	if err != nil || len(res) != 1 || !res[0].Sent {
		t.Fatalf("res %+v err %v", res, err)
	}
	r := k.reqs[0]
	if r.Path != "/api/7/envelope/" {
		t.Errorf("path %q", r.Path)
	}
	if got, want := r.Header.Get("X-Sentry-Auth"), "Sentry sentry_version=7, sentry_key=pubkey, sentry_client=pcb"; got != want {
		t.Errorf("auth %q", got)
	}
	if r.Header.Get("User-Agent") != "pcb" {
		t.Errorf("user agent %q", r.Header.Get("User-Agent"))
	}
	if r.Header.Get("Cookie") != "" {
		t.Error("cookie sent")
	}
	lines := bytes.SplitN(r.Body, []byte("\n"), 3)
	var head struct {
		EventID string `json:"event_id"`
		SentAt  string `json:"sent_at"`
		DSN     string `json:"dsn"`
	}
	var item struct {
		Type   string
		Length int
	}
	if len(lines) != 3 || json.Unmarshal(lines[0], &head) != nil || json.Unmarshal(lines[1], &item) != nil {
		t.Fatalf("envelope %q", r.Body)
	}
	if head.EventID != pend[0].ID || head.DSN != k.dsn() || head.SentAt != "2026-11-19T10:00:00Z" {
		t.Errorf("header %+v", head)
	}
	if item.Type != "event" || item.Length != len(pend[0].JSON) {
		t.Errorf("item header %+v", item)
	}
	if !bytes.Equal(lines[2], append(append([]byte{}, pend[0].JSON...), '\n')) {
		t.Errorf("payload differs from the pending file:\n%s", lines[2])
	}
}

func TestSentIsDeleted(t *testing.T) {
	k := newSink(t, nil)
	s := newStore(t)
	saveN(t, s, 2)
	res, err := report.Sender{DSN: k.dsn(), Store: s}.SendPending(ctx)
	if err != nil || len(res) != 2 || k.count() != 2 {
		t.Fatalf("res %+v err %v reqs %d", res, err, k.count())
	}
	if ids := pendingIDs(t, s); len(ids) != 0 {
		t.Errorf("still pending: %v", ids)
	}
}

func TestRetryAfterKeepsAndWaits(t *testing.T) {
	k := newSink(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	s := newStore(t)
	now := t0
	s.Now = func() time.Time { return now }
	saveN(t, s, 2)
	snd := report.Sender{DSN: k.dsn(), Store: s}
	res, err := snd.SendPending(ctx)
	if err != nil || len(res) != 1 || res[0].Sent || k.count() != 1 {
		t.Fatalf("res %+v err %v reqs %d (a 429 must stop the batch)", res, err, k.count())
	}
	if len(pendingIDs(t, s)) != 2 {
		t.Error("a 429 must keep the report")
	}
	now = t0.Add(119 * time.Second)
	if _, err := snd.SendPending(ctx); err != nil || k.count() != 1 {
		t.Fatalf("sent before Retry-After: reqs %d err %v", k.count(), err)
	}
	now = t0.Add(121 * time.Second)
	if res, err := snd.SendPending(ctx); err != nil || len(res) != 2 || k.count() != 3 {
		t.Fatalf("after Retry-After: res %+v err %v reqs %d", res, err, k.count())
	}
	if len(pendingIDs(t, s)) != 0 {
		t.Error("sent reports remain")
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	k := newSink(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", t0.Add(10*time.Minute).Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	s := newStore(t)
	saveN(t, s, 1)
	if _, err := (report.Sender{DSN: k.dsn(), Store: s}).SendPending(ctx); err != nil {
		t.Fatal(err)
	}
	var got time.Time
	_ = s.Edit(ctx, func(st *report.State) { got = st.RetryAfter })
	if want := t0.Add(10 * time.Minute); !got.Equal(want) {
		t.Errorf("retry_after %v, want %v", got, want)
	}
}

func TestThreeFailuresDelete(t *testing.T) {
	k := newSink(t, func(_ int, w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })
	s := newStore(t)
	saveN(t, s, 1)
	snd := report.Sender{DSN: k.dsn(), Store: s}
	for i := 1; i <= 3; i++ {
		res, err := snd.SendPending(ctx)
		if err != nil || len(res) != 1 || res[0].Sent || res[0].Err == nil {
			t.Fatalf("round %d: res %+v err %v", i, res, err)
		}
		if got := len(pendingIDs(t, s)); (i < 3) != (got == 1) {
			t.Fatalf("after failure %d: %d pending", i, got)
		}
	}
	if res, _ := snd.SendPending(ctx); len(res) != 0 || k.count() != 3 {
		t.Errorf("a deleted report was sent again: %d requests", k.count())
	}
}

func TestDailyCap(t *testing.T) {
	k := newSink(t, nil)
	s := newStore(t)
	saveN(t, s, 12)
	snd := report.Sender{DSN: k.dsn(), Store: s}
	if _, err := snd.SendPending(ctx); err != nil || k.count() != 10 {
		t.Fatalf("reqs %d err %v, want 10", k.count(), err)
	}
	if len(pendingIDs(t, s)) != 2 {
		t.Error("the two over the cap must be kept")
	}
	if _, err := snd.SendPending(ctx); err != nil || k.count() != 10 {
		t.Fatalf("sent past the cap: %d", k.count())
	}
	s.Now = func() time.Time { return t0.Add(24 * time.Hour) }
	snd.Store = s
	if _, err := snd.SendPending(ctx); err != nil || k.count() != 12 {
		t.Fatalf("next day: reqs %d err %v, want 12", k.count(), err)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	target := newSink(t, nil)
	k := newSink(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Location", target.URL+"/elsewhere")
		w.WriteHeader(http.StatusFound)
	})
	s := newStore(t)
	saveN(t, s, 1)
	res, err := report.Sender{DSN: k.dsn(), Store: s}.SendPending(ctx)
	if err != nil || len(res) != 1 || res[0].Sent {
		t.Fatalf("res %+v err %v", res, err)
	}
	if target.count() != 0 {
		t.Error("the redirect was followed")
	}
	if len(pendingIDs(t, s)) != 1 {
		t.Error("the report must be kept")
	}
	var attempts int
	_ = s.Edit(ctx, func(st *report.State) {
		for _, n := range st.Attempts {
			attempts += n
		}
	})
	if attempts != 1 {
		t.Errorf("attempts %d, want 1", attempts)
	}
}

func TestUnreachableCountsAttemptWithoutNamingTheHost(t *testing.T) {
	k := newSink(t, nil)
	dsn := k.dsn()
	k.Close()
	s := newStore(t)
	saveN(t, s, 1)
	res, err := report.Sender{DSN: dsn, Store: s}.SendPending(ctx)
	if err != nil || len(res) != 1 || res[0].Err == nil {
		t.Fatalf("res %+v err %v", res, err)
	}
	if strings.Contains(res[0].Err.Error(), k.Listener.Addr().String()) {
		t.Errorf("error names the host: %v", res[0].Err)
	}
}

func TestCancelledSendCountsNoAttempt(t *testing.T) {
	k := newSink(t, nil)
	s := newStore(t)
	saveN(t, s, 1)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (report.Sender{DSN: k.dsn(), Store: s}).SendPending(c); err == nil {
		t.Fatal("no error for a cancelled context")
	}
	var attempts int
	_ = s.Edit(ctx, func(st *report.State) { attempts = len(st.Attempts) })
	if attempts != 0 || len(pendingIDs(t, s)) != 1 {
		t.Errorf("attempts %d, pending %d", attempts, len(pendingIDs(t, s)))
	}
}

func TestRunSendsOnlyWhenAlways(t *testing.T) {
	k := newSink(t, nil)
	s := newStore(t)
	saveN(t, s, 1)
	poke := make(chan struct{}, 1)
	run := func(c context.Context) <-chan struct{} {
		done := make(chan struct{})
		go func() { report.Sender{DSN: k.dsn(), Store: s}.Run(c, poke); close(done) }()
		return done
	}

	c, cancel := context.WithCancel(ctx)
	done := run(c)
	poke <- struct{}{}
	for len(poke) > 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done // Run is one goroutine: once it has returned, the poke was handled
	if k.count() != 0 {
		t.Fatalf("Ask sent %d requests", k.count())
	}

	if err := s.SetMode(ctx, report.Always); err != nil {
		t.Fatal(err)
	}
	c, cancel = context.WithCancel(ctx)
	done = run(c)
	poke <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for k.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if k.count() != 1 {
		t.Errorf("Always sent %d requests, want 1", k.count())
	}
}

func TestParseDSN(t *testing.T) {
	ep, key, err := report.ParseDSN("https://abc123@errors.example.test/42")
	if err != nil || ep.String() != "https://errors.example.test/api/42/envelope/" || key != "abc123" {
		t.Errorf("good: %v %q %v", ep, key, err)
	}
	if ep, _, err := report.ParseDSN("http://k@127.0.0.1:8000/1"); err != nil || ep.String() != "http://127.0.0.1:8000/api/1/envelope/" {
		t.Errorf("loopback http: %v %v", ep, err)
	}
	for name, dsn := range map[string]string{
		"http, not loopback": "http://k@errors.example.test/1",
		"no key":             "https://errors.example.test/1",
		"no project":         "https://k@errors.example.test",
		"empty":              "",
		"no host":            "https://k@/1",
	} {
		if _, _, err := report.ParseDSN(dsn); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRunSendsOnPoke(t *testing.T) {
	k := newSink(t, nil)
	s := newStore(t)
	if err := s.SetMode(ctx, report.Always); err != nil {
		t.Fatal(err)
	}
	poke := make(chan struct{}, 1)
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { report.Sender{DSN: k.dsn(), Store: s}.Run(c, poke); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// Run started with nothing waiting: it is idle. A report saved later
	// goes out on the poke, once.
	time.Sleep(50 * time.Millisecond)
	saveN(t, s, 1)
	poke <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for k.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if k.count() != 1 {
		t.Errorf("the poke sent %d requests, want exactly 1", k.count())
	}
	if ids := pendingIDs(t, s); len(ids) != 0 {
		t.Errorf("%v still waits after the send", ids)
	}
}

// A report another process saves (no poke here) goes out at Run's next
// periodic wake.
func TestRunWakesPeriodically(t *testing.T) {
	k := newSink(t, nil)
	s := newStore(t)
	if err := s.SetMode(ctx, report.Always); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		report.Sender{DSN: k.dsn(), Store: s, Every: 20 * time.Millisecond}.Run(c, make(chan struct{}))
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(50 * time.Millisecond) // Run's start found nothing to send
	saveN(t, s, 1)
	deadline := time.Now().Add(5 * time.Second)
	for k.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if k.count() != 1 {
		t.Errorf("sent %d requests without a poke, want 1 at a periodic wake", k.count())
	}
}
