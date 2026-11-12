package probe

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// Answer is what Proton answered to one request: the HTTP status and
// Proton's code (a per-entry code for a sync entry), or a Note when no
// usable answer came back. Message is Proton's own error text, when any.
type Answer struct {
	Status  int    `json:"http_status"`
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
	Note    string `json:"note,omitempty"`
}

// String is "HTTP 200, code 1000", with Proton's message quoted after it.
func (a Answer) String() string {
	if a.Status == 0 {
		return "no answer (" + a.Note + ")"
	}
	s := fmt.Sprintf("HTTP %d, code %d", a.Status, a.Code)
	if a.Message != "" {
		s += fmt.Sprintf(" %q", a.Message)
	}
	return s
}

// OK reports Proton's success code.
func (a Answer) OK() bool { return a.Code == api.CodeOK }

// maxMessage bounds the Proton message kept in an Answer.
const maxMessage = 160

func message(s string) string {
	if len(s) > maxMessage {
		return s[:maxMessage] + "…"
	}
	return s
}

// answerOf is the answer behind err, a failed api.Client call. A call
// that never got an HTTP answer gives a Note (our errors carry no secret).
func answerOf(err error) Answer {
	var ae *api.Error
	if errors.As(err, &ae) {
		return Answer{Status: ae.Status, Code: ae.Code, Message: message(ae.Message)}
	}
	return Answer{Note: err.Error()}
}

// statusRecorder is a transport that keeps the HTTP status of the last
// answer, so a success through api.Client is printed with its real status.
type statusRecorder struct {
	mu   sync.Mutex
	last int
	rt   http.RoundTripper
}

func (s *statusRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.rt.RoundTrip(r)
	if err == nil {
		s.mu.Lock()
		s.last = resp.StatusCode
		s.mu.Unlock()
	}
	return resp, err
}

func (s *statusRecorder) status() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// newRecorder is an HTTP client recording its last status.
func newRecorder() (*http.Client, *statusRecorder) {
	rec := &statusRecorder{rt: http.DefaultTransport}
	return &http.Client{Timeout: callTimeout, Transport: rec}, rec
}
