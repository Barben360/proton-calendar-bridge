package report

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DailyCap is how many reports one install sends in a UTC day.
	DailyCap = 10
	// MaxAttempts is how many failed sends a report gets before it is deleted.
	MaxAttempts = 3

	sendTimeout     = 10 * time.Second
	defaultWait     = time.Hour
	maxWait         = 24 * time.Hour
	envelopeContent = "application/x-sentry-envelope"
)

// ParseDSN reads https://<key>@<host>/<project> and gives the envelope
// endpoint https://<host>/api/<project>/envelope/ and the public key. Only
// https is accepted, and http for a loopback host (tests).
func ParseDSN(dsn string) (endpoint *url.URL, key string, err error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, "", errors.New("crash reports: malformed DSN")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback(u.Hostname()):
	default:
		return nil, "", errors.New("crash reports: DSN must be https")
	}
	key = u.User.Username()
	project := strings.Trim(u.Path, "/")
	switch {
	case u.Host == "" || u.Hostname() == "":
		return nil, "", errors.New("crash reports: DSN has no host")
	case key == "":
		return nil, "", errors.New("crash reports: DSN has no key")
	case project == "" || strings.Contains(project, "/"):
		return nil, "", errors.New("crash reports: DSN has no project")
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/api/" + project + "/envelope/"}, key, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewClient is the sender's own client: it follows no redirect, keeps no
// cookie and gives up after d.
func NewClient(d time.Duration) *http.Client {
	return &http.Client{
		Timeout:       d,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Sender posts the waiting reports to the DSN's Sentry endpoint. A nil
// Client is NewClient(10 s).
type Sender struct {
	DSN    string
	Client *http.Client
	Store  Store
	// Every is how often Run also wakes, for the reports other processes
	// save; zero is runEvery.
	Every time.Duration
}

// runEvery is how often Run wakes by default.
const runEvery = time.Hour

// SendResult is the outcome of one report that was posted: Sent when the
// server took it, else Err says why (the report is kept, or deleted after
// its last attempt).
type SendResult struct {
	ID   string
	Sent bool
	Err  error
}

func (s Sender) now() time.Time {
	if s.Store.Now != nil {
		return s.Store.Now()
	}
	return time.Now()
}

// SendPending posts the waiting reports, oldest first, one request each:
// 2xx deletes the report; 429 keeps it, stores Retry-After and stops; any
// other answer, or none, counts a failed attempt and deletes the report at
// the third. Nothing is sent before a stored Retry-After, nor past
// DailyCap requests in a UTC day. The mode is not read: the caller decides.
// Reports left alone (cap, wait) have no result.
func (s Sender) SendPending(ctx context.Context) ([]SendResult, error) {
	endpoint, key, err := ParseDSN(s.DSN)
	if err != nil {
		return nil, err
	}
	client := s.Client
	if client == nil {
		client = NewClient(sendTimeout)
	}
	pend, err := s.Store.Pending()
	if err != nil {
		return nil, err
	}
	var results []SendResult
	for _, p := range pend {
		ok, err := s.reserve(ctx)
		if err != nil {
			return results, err
		}
		if !ok {
			break
		}
		wait, sendErr := s.post(ctx, client, endpoint, key, p)
		if ctx.Err() != nil && sendErr != nil {
			// Cancelled by the caller, not refused: no attempt is counted.
			return results, ctx.Err()
		}
		results = append(results, SendResult{ID: p.ID, Sent: sendErr == nil, Err: sendErr})
		if err := s.settle(ctx, p.ID, sendErr, wait); err != nil {
			return results, err
		}
		if wait > 0 {
			break
		}
	}
	return results, nil
}

// reserve counts one send in today's total, unless a Retry-After is still
// running or the cap is reached.
func (s Sender) reserve(ctx context.Context) (ok bool, err error) {
	err = s.Store.Edit(ctx, func(st *State) {
		now := s.now()
		if now.Before(st.RetryAfter) {
			return
		}
		if day := now.UTC().Format(time.DateOnly); st.SentDay != day {
			st.SentDay, st.SentCount = day, 0
		}
		if st.SentCount >= DailyCap {
			return
		}
		st.SentCount++
		ok = true
	})
	return ok, err
}

// settle records the outcome of one request: sent deletes the report, a
// wait (429) stores Retry-After, a failure counts an attempt.
func (s Sender) settle(ctx context.Context, id string, sendErr error, wait time.Duration) error {
	var rmErr error
	err := s.Store.Edit(ctx, func(st *State) {
		switch {
		case sendErr == nil:
			rmErr = s.Store.remove(st, id)
		case wait > 0:
			st.RetryAfter = s.now().Add(wait)
		default:
			if st.Attempts == nil {
				st.Attempts = map[string]int{}
			}
			st.Attempts[id]++
			if st.Attempts[id] >= MaxAttempts {
				rmErr = s.Store.remove(st, id)
			}
		}
	})
	return errors.Join(err, rmErr)
}

// post sends one report. wait is the server's Retry-After when it said 429.
func (s Sender) post(ctx context.Context, client *http.Client, endpoint *url.URL, key string, p Pending) (wait time.Duration, err error) {
	body, err := envelope(s.DSN, p, s.now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", envelopeContent)
	req.Header.Set("User-Agent", "pcb")
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_key="+key+", sentry_client=pcb")
	resp, err := client.Do(req)
	if err != nil {
		return 0, errors.New("crash reports: send failed") // the URL error would name the host
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode/100 == 2:
		return 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return retryAfter(resp.Header.Get("Retry-After"), s.now()), fmt.Errorf("crash reports: rate limited (HTTP 429)")
	default:
		return 0, fmt.Errorf("crash reports: HTTP %d", resp.StatusCode)
	}
}

// retryAfter reads Retry-After (seconds or an HTTP date) as a wait: at
// least one second, at most a day, an hour when absent or unreadable.
func retryAfter(v string, now time.Time) time.Duration {
	d := defaultWait
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, time.Second), maxWait)
}

// envelope is the Sentry envelope of one report: a header line, an item
// header with the payload's length, and the pending file's bytes unchanged.
func envelope(dsn string, p Pending, now time.Time) ([]byte, error) {
	head, err := marshal(struct {
		EventID string `json:"event_id"`
		SentAt  string `json:"sent_at"`
		DSN     string `json:"dsn"`
	}{p.ID, now.UTC().Format(time.RFC3339), dsn})
	if err != nil {
		return nil, err
	}
	item, err := marshal(struct {
		Type   string `json:"type"`
		Length int    `json:"length"`
	}{"event", len(p.JSON)})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(head)
	b.WriteByte('\n')
	b.Write(item)
	b.WriteByte('\n')
	b.Write(p.JSON)
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// Run sends the waiting reports whenever the mode in force is Always: at
// start, on each poke (a report this process saved) and every Every (one
// another process saved: a cron pcb sync, a collected crash), until ctx
// ends. It never logs nor prints; what fails stays for the next wake.
func (s Sender) Run(ctx context.Context, poke <-chan struct{}) {
	every := s.Every
	if every <= 0 {
		every = runEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if s.Store.Mode() == Always {
			_, _ = s.SendPending(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-poke:
		case <-tick.C:
		}
	}
}
