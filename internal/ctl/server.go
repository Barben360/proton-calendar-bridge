package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The socket server's limits. A login through the daemon (Task 5) may take
// up to the client's 180 s, so writes get a little more.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 190 * time.Second
	idleTimeout       = 60 * time.Second
)

// Hub is what the control socket needs of the accounts: *service.Hub. A
// selector is an account's local ID or address; an account-changing call
// goes through the Hub, never the per-account API (which Account returns
// for reads such as Status).
type Hub interface {
	Accounts() []service.AccountInfo
	Account(selector string) (service.API, service.AccountInfo, error)
	Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error)
	SubmitTOTP(ctx context.Context, selector, code string) (service.LoginResult, error)
	SignOut(ctx context.Context, selector string, req service.LogoutRequest) (service.LogoutResult, error)
	Remove(ctx context.Context, selector string, req service.LogoutRequest) (service.AccountInfo, error)
	SetCalDAV(ctx context.Context, selector string, on bool) (service.AccountInfo, error)
	SetMCP(ctx context.Context, selector string, on bool) (service.AccountInfo, error)
	Restart(ctx context.Context, selector string) (service.AccountInfo, error)
	Health(ctx context.Context, selector string) (service.Health, error)
}

// Server answers the control socket of a pcb process. Listener returns the
// CalDAV listener once bound (nil before). PeerUID reads a peer's UID (ok
// false: unchecked); nil is SO_PEERCRED on Linux and unchecked elsewhere
// (ruling M5-P3). A peer of another UID gets 403 and a warning naming its UID.
type Server struct {
	Hub      Hub
	Daemon   Daemon
	Listener func() *Listener
	Now      func() time.Time
	Logger   *slog.Logger
	PeerUID  func(net.Conn) (uid int, ok bool)
	// ShowUI raises the desktop app's window (on the account's pane when
	// named). nil: this process has no window, and POST /v2/ui/show is an
	// unknown endpoint. It runs on the HTTP handler goroutine and must
	// return quickly, for example by posting to the UI loop.
	ShowUI func(account string) error

	once, initOnce sync.Once
	srv            *http.Server
	mux            *http.ServeMux

	// logins is held while a login or a 2FA code runs: another one is
	// refused at once, not queued behind it.
	logins sync.Mutex
}

// route is one endpoint: its method and command (the envelope's), and what
// it answers. id is the {id} of its path (an account selector), body the
// request's, at most maxBody bytes.
type route struct {
	pattern, method, command string
	run                      func(s *Server, ctx context.Context, id string, body []byte) (any, error)
}

var routes = []route{
	{"/v2/accounts", http.MethodGet, "accounts", (*Server).accounts},
	{"/v2/status", http.MethodGet, "status", (*Server).status},
	{"/v2/login", http.MethodPost, "login", (*Server).login},
	{"/v2/accounts/{id}/login/totp", http.MethodPost, "login resume", (*Server).submitTOTP},
	{"/v2/accounts/{id}/logout", http.MethodPost, "logout", (*Server).logout},
	{"/v2/accounts/{id}/remove", http.MethodPost, "account remove", (*Server).remove},
	{"/v2/accounts/{id}/caldav", http.MethodPost, "account caldav", (*Server).setCalDAV},
	{"/v2/accounts/{id}/mcp", http.MethodPost, "account mcp", (*Server).setMCP},
	{"/v2/accounts/{id}/restart", http.MethodPost, "account restart", (*Server).restart},
}

// errBadBody is a request body that is not the endpoint's JSON: 400.
var errBadBody = errors.New("the request body is not valid JSON for this endpoint")

type connKey struct{}

// ConnContext records the connection a request came on, for the peer
// check; Serve sets it, and an http.Server serving Handler must too.
func (*Server) ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// Serve answers on ln until Shutdown (then http.ErrServerClosed).
func (s *Server) Serve(ln net.Listener) error { return s.http().Serve(ln) }

// Shutdown stops accepting and waits for the requests in flight until ctx
// ends; closing the listener removes the socket.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.http().Shutdown(ctx); err != nil {
		_ = s.http().Close()
		return err
	}
	return nil
}

func (s *Server) http() *http.Server {
	s.once.Do(func() {
		s.srv = &http.Server{
			Handler:           s.Handler(),
			ConnContext:       s.ConnContext,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			ErrorLog:          slog.NewLogLogger(s.logger().Handler(), slog.LevelWarn),
		}
	})
	return s.srv
}

func (s *Server) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

// outcome is what a request answered, for its log line.
type outcome struct {
	status int
	code   exitcode.Code
}

type outcomeKey struct{}

// Handler answers the requests: the peer check first, so a refused peer
// learns nothing, not even which paths exist. Bodies are never logged: a
// request's log line carries its method, path, HTTP status and exit code.
func (s *Server) Handler() http.Handler {
	s.initOnce.Do(s.init)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out outcome
		r = r.WithContext(context.WithValue(r.Context(), outcomeKey{}, &out))
		if uid, refused := s.refusedPeer(r); refused {
			s.logger().Warn("control socket: refused a peer of another user", "peer_uid", uid)
			out = transport(w, "ctl", http.StatusForbidden, "the control socket answers only its own user", "run pcb as the user that runs pcb serve")
		} else if path.Clean(r.URL.Path) != r.URL.Path {
			// ServeMux would redirect (301, plain text); no path is unclean here.
			out = transport(w, "ctl", http.StatusNotFound, "no such control endpoint: "+r.URL.Path, "use the pcb of the same version as the daemon")
		} else {
			s.mux.ServeHTTP(w, r)
		}
		s.logger().Debug("control request", "method", r.Method, "path", r.URL.Path, "status", out.status, "exit", int(out.code))
	})
}

// init builds the routing: one pattern per route (the method is checked
// by answer, for the envelope's 405), and a 404 for the rest.
func (s *Server) init() {
	s.mux = http.NewServeMux()
	for _, rt := range routes {
		s.mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { s.answer(w, r, rt) })
	}
	if s.ShowUI != nil {
		rt := route{"/v2/ui/show", http.MethodPost, "ui show", (*Server).showUI}
		s.mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { s.answer(w, r, rt) })
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		record(r, transport(w, "ctl", http.StatusNotFound, "no such control endpoint: "+r.URL.Path, "use the pcb of the same version as the daemon"))
	})
}

func record(r *http.Request, o outcome) {
	if out, ok := r.Context().Value(outcomeKey{}).(*outcome); ok {
		*out = o
	}
}

// answer serves r on rt, and records the HTTP status and the exit code it answered.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, rt route) {
	command := rt.command
	if r.Method != rt.method {
		w.Header().Set("Allow", rt.method)
		record(r, transport(w, command, http.StatusMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path, "use the pcb of the same version as the daemon"))
		return
	}
	tooLarge := func() {
		record(r, transport(w, command, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request body exceeds %d bytes", maxBody), ""))
	}
	if r.ContentLength > maxBody {
		tooLarge()
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	defer clear(body) // a login's body holds its secrets
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		tooLarge()
		return
	case err != nil:
		record(r, transport(w, command, http.StatusBadRequest, "cannot read the request body", ""))
		return
	}
	data, err := rt.run(s, r.Context(), r.PathValue("id"), body)
	switch {
	case errors.Is(err, errBadBody):
		record(r, transport(w, command, http.StatusBadRequest, errBadBody.Error(), "use the pcb of the same version as the daemon"))
	case err != nil:
		// A refusal to choose among several accounts lists them (data.accounts).
		var several *service.SeveralAccountsError
		var details any
		if errors.As(err, &several) {
			details = Accounts{Accounts: several.Accounts}
		}
		record(r, write(w, command, http.StatusUnprocessableEntity, details, err))
	default:
		record(r, write(w, command, http.StatusOK, data, nil))
	}
}

// refusedPeer reports whether r's peer is of another UID than this
// process's effective one (SO_PEERCRED reports the peer's effective UID);
// a request whose connection is unknown is refused.
func (s *Server) refusedPeer(r *http.Request) (int, bool) {
	c, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok {
		return -1, true
	}
	peer := s.PeerUID
	if peer == nil {
		peer = defaultPeerUID
	}
	uid, checked := peer(c)
	return uid, checked && uid != os.Geteuid()
}

// transport answers a transport error: blocked (20), with a hint.
func transport(w http.ResponseWriter, command string, code int, msg, hint string) outcome {
	return write(w, command, code, nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New(msg), Hint: hint})
}

// write answers one envelope with HTTP status code, and returns it with
// the exit code the envelope carries.
func write(w http.ResponseWriter, command string, code int, data any, err error) outcome {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = output.Write(w, command, data, err) // a peer gone has nowhere to be told
	return outcome{code, exitcode.From(err)}
}

// accounts is GET /v2/accounts: the Hub's accounts, pending ones included.
func (s *Server) accounts(context.Context, string, []byte) (any, error) {
	return Accounts{Accounts: s.Hub.Accounts()}, nil
}

// status is GET /v2/status: BuildStatus of this server's own state.
func (s *Server) status(ctx context.Context, _ string, _ []byte) (any, error) {
	var ln *Listener
	if s.Listener != nil {
		ln = s.Listener()
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return BuildStatus(ctx, s.Hub, s.Daemon, ln, now()), nil
}

// BuildStatus is what GET /v2/status answers, built in-process (the desktop
// app reads its own status through it, with no socket): the process, and
// per account its login, Serve's health, its CalDAV URL and its alerts. ln
// is the CalDAV listener (nil before it is bound).
func BuildStatus(ctx context.Context, h Hub, d Daemon, ln *Listener, now time.Time) Status {
	base := ""
	if ln != nil {
		base = strings.TrimRight(ln.URL, "/")
	}
	infos := h.Accounts()
	accounts := make([]AccountStatus, 0, len(infos))
	for _, info := range infos {
		as, err := accountStatus(ctx, h, info, now)
		if errors.Is(err, service.ErrNoAccount) {
			continue // removed since Accounts listed it
		}
		if base != "" {
			as.URL = base + service.AccountPath(info.ID)
		}
		accounts = append(accounts, as)
	}
	return Status{Daemon: &d, Listener: ln, Accounts: accounts, Alerts: ProcessAlerts(ln, accounts, now)}
}

// accountStatus is info's entry of Status. An account that cannot be read
// still has its entry, with an account-unreadable alert saying why (err
// is then set, for the caller to tell a vanished account), so one account
// never takes the others' status down.
func accountStatus(ctx context.Context, hub Hub, info service.AccountInfo, now time.Time) (AccountStatus, error) {
	as := AccountStatus{AccountInfo: info}
	api, _, err := hub.Account(info.ID)
	if err == nil {
		as.Login, err = api.Status(ctx)
	}
	if err == nil {
		var h service.Health
		if h, err = hub.Health(ctx, info.ID); err == nil {
			as.Health = &h
		}
	}
	as.Alerts = Alerts(AlertInput{Account: info.ID, Login: as.Login, Stopped: info.Stopped, Health: as.Health, Now: now})
	if err != nil && !errors.Is(err, service.ErrNoAccount) {
		as.Alerts = append(as.Alerts, Alert{Account: info.ID, Code: CodeAccountUnreadable, Severity: SeverityError,
			Message: err.Error(), Fix: exitcode.HintOf(err)})
		SortAlerts(as.Alerts)
	}
	return as, err
}

// showUI is POST /v2/ui/show: ShowUI on the body's account (none: the window).
func (s *Server) showUI(_ context.Context, _ string, body []byte) (any, error) {
	var req ShowUIRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, errBadBody
		}
	}
	if err := s.ShowUI(req.Account); err != nil {
		return nil, err
	}
	return ShowUI{Shown: true}, nil
}

// login is POST /v2/login: Hub.Login on the body, a service.LoginRequest.
// A login that completes here wakes a Serve waiting for one, as an
// in-process login does (the same Hub call).
func (s *Server) login(ctx context.Context, _ string, body []byte) (any, error) {
	var req service.LoginRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errBadBody
	}
	unlock, err := s.oneLogin()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.Hub.Login(ctx, req)
}

// submitTOTP is POST /v2/accounts/{id}/login/totp: Hub.SubmitTOTP on the
// body's code, for the login the Hub holds for that account.
func (s *Server) submitTOTP(ctx context.Context, id string, body []byte) (any, error) {
	var req TOTPRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errBadBody
	}
	unlock, err := s.oneLogin()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.Hub.SubmitTOTP(ctx, id, req.Code)
}

// logout is POST /v2/accounts/{id}/logout: Hub.SignOut on the body, a
// service.LogoutRequest (none: the zero request), which also sends a Serve
// with --wait-for-login back to waiting (ruling M5-R8).
func (s *Server) logout(ctx context.Context, id string, body []byte) (any, error) {
	req, err := logoutRequest(body)
	if err != nil {
		return nil, err
	}
	return s.Hub.SignOut(ctx, id, req)
}

// remove is POST /v2/accounts/{id}/remove: Hub.Remove on the same body.
func (s *Server) remove(ctx context.Context, id string, body []byte) (any, error) {
	req, err := logoutRequest(body)
	if err != nil {
		return nil, err
	}
	return s.Hub.Remove(ctx, id, req)
}

func logoutRequest(body []byte) (req service.LogoutRequest, err error) {
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return service.LogoutRequest{}, errBadBody
		}
	}
	return req, nil
}

// setCalDAV is POST /v2/accounts/{id}/caldav: Hub.SetCalDAV on the body's
// "on", which must be given.
func (s *Server) setCalDAV(ctx context.Context, id string, body []byte) (any, error) {
	on, err := switchBody(body)
	if err != nil {
		return nil, err
	}
	return s.Hub.SetCalDAV(ctx, id, on)
}

// setMCP is POST /v2/accounts/{id}/mcp: Hub.SetMCP on the body's "on",
// which must be given.
func (s *Server) setMCP(ctx context.Context, id string, body []byte) (any, error) {
	on, err := switchBody(body)
	if err != nil {
		return nil, err
	}
	return s.Hub.SetMCP(ctx, id, on)
}

// switchBody is the "on" of an adapter switch's body (CalDAVRequest,
// MCPRequest); a body without it is errBadBody.
func switchBody(body []byte) (bool, error) {
	var req struct {
		On *bool `json:"on"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.On == nil {
		return false, errBadBody
	}
	return *req.On, nil
}

// restart is POST /v2/accounts/{id}/restart: Hub.Restart.
func (s *Server) restart(ctx context.Context, id string, _ []byte) (any, error) {
	return s.Hub.Restart(ctx, id)
}

// oneLogin takes the login slot, or refuses at once while another login
// or 2FA code runs.
func (s *Server) oneLogin() (unlock func(), err error) {
	if !s.logins.TryLock() {
		return nil, &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("a login is already in progress"),
			Hint: "wait for it to end, then see pcb status"}
	}
	return s.logins.Unlock, nil
}
