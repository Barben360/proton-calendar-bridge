// Package ctl is the control socket (CTL) of pcb serve: JSON over HTTP/1.1
// on <state>/pcb.sock, internal and versioned under /v1/, and its client.
// Every answer is one output.Envelope: 200 when the command succeeds, 422
// when it fails (its exit code, message and hint), 400, 403, 404, 405 or
// 413 for a transport error (ruling M5-Rpre1).
package ctl

import (
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Status is what pcb status shows: the login's fields flat at the top level
// as M1–M4 printed them (ruling M5-P4), then the daemon's live state. Daemon
// is nil in the offline view, with Health and Listener left out.
type Status struct {
	service.Status
	Daemon   *Daemon         `json:"daemon"`
	Health   *service.Health `json:"health,omitempty"`
	Listener *Listener       `json:"listener,omitempty"`
	Alerts   []Alert         `json:"alerts"`
}

// TOTPRequest is the body of POST /v1/login/totp: the 2FA code (or the
// otpauth:// URI) of the login the daemon holds.
type TOTPRequest struct {
	Code string `json:"code"`
}

const redactedTOTP = "ctl.TOTPRequest(redacted)"

// String hides the code from fmt.
func (TOTPRequest) String() string { return redactedTOTP }

// GoString hides the code from %#v.
func (TOTPRequest) GoString() string { return redactedTOTP }

// LogValue hides the code from slog.
func (TOTPRequest) LogValue() slog.Value { return slog.StringValue(redactedTOTP) }

// Daemon is the pcb serve process that answers on the socket.
type Daemon struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	LogLevel  string    `json:"log_level"`
	Socket    string    `json:"socket"`
}

// NewDaemon describes this process: its PID and build version.
func NewDaemon(socket, logLevel string, startedAt time.Time) Daemon {
	return Daemon{PID: os.Getpid(), Version: buildinfo.Version, StartedAt: startedAt, LogLevel: logLevel, Socket: socket}
}

// Listener is the CalDAV listener, once bound: its address, the URL to
// enter in Thunderbird and, over TLS, the certificate's fingerprint and end.
type Listener struct {
	Addr         string     `json:"addr"`
	URL          string     `json:"url"`
	TLS          bool       `json:"tls"`
	Fingerprint  string     `json:"fingerprint,omitempty"`
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`
}

// Alert is a problem a person should look at (ruling M5-P7). Codes are
// stable; Calendar is the calendar's ID for a per-calendar alert.
type Alert struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix"`
	Calendar string `json:"calendar,omitempty"`
}

// The socket's errors.
var (
	// ErrPathTooLong: the path does not fit sun_path with its NUL (107
	// bytes on Linux, 103 on macOS and the BSDs).
	ErrPathTooLong = errors.New("the control socket path is longer than " + strconv.Itoa(maxPath) + " bytes")
	// ErrUnsafeDir: the socket's directory grants its group or others access.
	ErrUnsafeDir = errors.New("the control socket's directory is open to other users")
	// ErrNotASocket: a file that is not a socket holds the path.
	ErrNotASocket = errors.New("a file that is not a socket holds the control socket's path")
	// ErrUnsupported: no control socket off Unix (ruling M5-P3).
	ErrUnsupported = errors.New("no control socket on this system")
	// ErrNoDaemon: no pcb serve answers on the socket.
	ErrNoDaemon = errors.New("no pcb daemon answers")
)

// maxPath is the longest socket path: sun_path with its NUL. Listen and
// Dial share it, so pcb serve's hint and pcb status's offline fallback agree.
const maxPath = sunPathLen - 1

// maxBody is the largest request body.
const maxBody = 64 << 10
