// Package ctl is the control socket (CTL) of pcb serve: JSON over HTTP/1.1
// on <state>/pcb.sock (on Windows, a named pipe private to the user, named
// after the state directory), internal and versioned under /v2/, and its
// client.
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

// Status is what GET /v2/status answers: the process (Daemon, Listener,
// MCP), each account and the process-level Alerts (the TLS
// certificate's); the alerts of an account are its own.
type Status struct {
	Daemon   *Daemon         `json:"daemon"`
	Listener *Listener       `json:"listener,omitempty"`
	MCP      *MCP            `json:"mcp,omitempty"`
	Accounts []AccountStatus `json:"accounts"`
	Alerts   []Alert         `json:"alerts"`
}

// MCP is the process's MCP adapter (design §5.8): whether /mcp is served
// on the listener ([mcp] remote), and the live sessions per entry
// ("remote", "relay", "stdio").
type MCP struct {
	Remote   bool           `json:"remote"`
	Sessions map[string]int `json:"sessions"`
}

// AccountStatus is one account of Status: its AccountInfo flat, its login,
// Serve's health for it, the CalDAV URL to enter in a client (empty while
// no listener is bound) and its alerts.
type AccountStatus struct {
	service.AccountInfo
	Login  service.Status  `json:"login"`
	Health *service.Health `json:"health,omitempty"`
	URL    string          `json:"url,omitempty"`
	Alerts []Alert         `json:"alerts"`
}

// Accounts is the data of GET /v2/accounts.
type Accounts struct {
	Accounts []service.AccountInfo `json:"accounts"`
}

// CalDAVRequest is the body of POST /v2/accounts/{id}/caldav: the
// account's CalDAV switch.
type CalDAVRequest struct {
	On bool `json:"on"`
}

// MCPRequest is the body of POST /v2/accounts/{id}/mcp: the account's MCP
// mode. On is the body before the read-only mode, still taken: false is
// off, true read-write; a body gives one of them.
type MCPRequest struct {
	Mode service.MCPMode `json:"mode,omitempty"`
	On   *bool           `json:"on,omitempty"`
}

// TOTPRequest is the body of POST /v2/accounts/{id}/login/totp: the 2FA
// code (or the otpauth:// URI) of the login the daemon holds for the account.
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

// MailboxPasswordRequest is the body of
// POST /v2/accounts/{id}/login/mailbox-password: the mailbox password of the
// login the daemon holds for the account.
type MailboxPasswordRequest struct {
	Password string `json:"password"`
}

const redactedMailbox = "ctl.MailboxPasswordRequest(redacted)"

// String hides the password from fmt.
func (MailboxPasswordRequest) String() string { return redactedMailbox }

// GoString hides the password from %#v.
func (MailboxPasswordRequest) GoString() string { return redactedMailbox }

// LogValue hides the password from slog.
func (MailboxPasswordRequest) LogValue() slog.Value { return slog.StringValue(redactedMailbox) }

// Daemon is the pcb serve process that answers on the socket.
type Daemon struct {
	// Kind is "serve" (pcb serve), "ui" (the desktop app pcb-ui) or "mcp"
	// (the session daemon of pcb mcp, an agent session).
	Kind      string    `json:"kind"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	LogLevel  string    `json:"log_level"`
	Socket    string    `json:"socket"`
}

// NewDaemon describes this process, of kind "serve", "ui" or "mcp": its PID and
// build version.
func NewDaemon(kind, socket, logLevel string, startedAt time.Time) Daemon {
	return Daemon{Kind: kind, PID: os.Getpid(), Version: buildinfo.Version, StartedAt: startedAt, LogLevel: logLevel, Socket: socket}
}

// Listener is the CalDAV listener, once bound: its address, its base URL
// (scheme://host:port; an account's URL adds service.AccountPath) and, over TLS, the certificate's fingerprint and end.
type Listener struct {
	Addr         string     `json:"addr"`
	URL          string     `json:"url"`
	TLS          bool       `json:"tls"`
	Fingerprint  string     `json:"fingerprint,omitempty"`
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`
}

// Alert is a problem a person should look at (ruling M5-P7). Codes are
// stable; Account is the local ID of the account an alert is about (none
// for a process alert), Calendar the calendar's ID for a per-calendar alert.
type Alert struct {
	Account  string `json:"account,omitempty"`
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
	// ErrUnsupported: no control socket off Unix and Windows (ruling M5-P3).
	ErrUnsupported = errors.New("no control socket on this system")
	// ErrPipeTaken: the control pipe's name is already served on Windows,
	// by another pcb of the same state or another user's process.
	ErrPipeTaken = errors.New("the control pipe is already served")
	// ErrPipeHeld, an ErrPipeTaken: nothing answers on the control pipe's
	// name, and it stayed held while Listen waited: the previous pcb of
	// this state still closing it, or a process that holds it without
	// serving.
	ErrPipeHeld error = pipeHeld{}
	// ErrCloseStuck: the control pipe's listener did not close within its
	// bound (Windows, boundClose): the daemon logs it and stops anyway.
	ErrCloseStuck = errors.New("the control pipe's listener did not close in time")
	// ErrNoDaemon: no pcb serve answers on the socket.
	ErrNoDaemon = errors.New("no pcb daemon answers")
	// ErrRefused: Dial refuses the control socket: not a socket, another
	// user's, or answered by another user's process (Windows: the pipe,
	// another user's). A person must act; no retry helps (the doctor's exit
	// 10). The error's text says which.
	ErrRefused = errors.New("pcb refuses this " + Channel)
)

// refusal is a refused control socket: its text is msg, and
// errors.Is(err, ErrRefused) holds.
type refusal struct{ msg string }

func refused(msg string) error { return &refusal{msg: msg} }

func (r *refusal) Error() string        { return r.msg }
func (r *refusal) Is(target error) bool { return target == ErrRefused }

// maxPath is the longest socket path: sun_path with its NUL. Listen and
// Dial share it, so pcb serve's hint and pcb status's offline fallback agree.
const maxPath = sunPathLen - 1

// maxBody is the largest request body.
const maxBody = 64 << 10

// ShowUIRequest is the body of POST /v2/ui/show: the account whose pane to
// show (a local ID; none: wherever the window was).
type ShowUIRequest struct {
	Account string `json:"account,omitempty"`
}

// ShowUI is the answer of POST /v2/ui/show.
type ShowUI struct {
	Shown bool `json:"shown"`
}

// pipeHeld is ErrPipeHeld.
type pipeHeld struct{}

func (pipeHeld) Error() string {
	return "the control pipe's name is still held, and nothing answers on it"
}

// Is makes ErrPipeHeld an ErrPipeTaken.
func (pipeHeld) Is(target error) bool { return target == ErrPipeTaken }
