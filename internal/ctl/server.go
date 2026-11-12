package ctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
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

// Server answers the control socket for pcb serve. Listener returns the
// CalDAV listener once bound (nil before). PeerUID reads a peer's UID (ok
// false: unchecked); nil is SO_PEERCRED on Linux and unchecked elsewhere
// (ruling M5-P3). A peer of another UID gets 403 and a warning naming its UID.
type Server struct {
	API      service.API
	Daemon   Daemon
	Listener func() *Listener
	Now      func() time.Time
	Logger   *slog.Logger
	PeerUID  func(net.Conn) (uid int, ok bool)

	once sync.Once
	srv  *http.Server
}

// route is one endpoint: its method and command (the envelope's), and what
// it answers. body is the request's, at most maxBody bytes.
type route struct {
	method, command string
	run             func(s *Server, ctx context.Context, body []byte) (any, error)
}

var routes = map[string]route{
	"/v1/status": {http.MethodGet, "status", (*Server).status},
}

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

// Handler answers the requests: the peer check first, so a refused peer
// learns nothing, not even which paths exist. Bodies are never logged.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := s.logger()
		rt, known := routes[r.URL.Path]
		command := "ctl"
		if known {
			command = rt.command
		}
		status := s.answer(w, r, rt, known, command)
		log.Debug("control request", "method", r.Method, "path", r.URL.Path, "status", status)
	})
}

// answer serves r and returns the HTTP status it answered.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, rt route, known bool, command string) int {
	if uid, refused := s.refusedPeer(r); refused {
		s.logger().Warn("control socket: refused a peer of another user", "peer_uid", uid)
		return transport(w, command, http.StatusForbidden, "the control socket answers only its own user", "run pcb as the user that runs pcb serve")
	}
	switch {
	case !known:
		return transport(w, command, http.StatusNotFound, "no such control endpoint: "+r.URL.Path, "use the pcb of the same version as the daemon")
	case r.Method != rt.method:
		w.Header().Set("Allow", rt.method)
		return transport(w, command, http.StatusMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path, "use the pcb of the same version as the daemon")
	}
	tooLarge := func() int {
		return transport(w, command, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request body exceeds %d bytes", maxBody), "")
	}
	if r.ContentLength > maxBody {
		return tooLarge()
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return tooLarge()
	case err != nil:
		return transport(w, command, http.StatusBadRequest, "cannot read the request body", "")
	}
	data, err := rt.run(s, r.Context(), body)
	if err != nil {
		return write(w, command, http.StatusUnprocessableEntity, nil, err)
	}
	return write(w, command, http.StatusOK, data, nil)
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
func transport(w http.ResponseWriter, command string, code int, msg, hint string) int {
	return write(w, command, code, nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New(msg), Hint: hint})
}

func write(w http.ResponseWriter, command string, code int, data any, err error) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = output.Write(w, command, data, err) // a peer gone has nowhere to be told
	return code
}

// status is GET /v1/status: the login, Serve's health, the listener and
// the alerts.
func (s *Server) status(ctx context.Context, _ []byte) (any, error) {
	st, err := s.API.Status(ctx)
	if err != nil {
		return nil, err
	}
	h, err := s.API.Health(ctx)
	if err != nil {
		return nil, err
	}
	var ln *Listener
	if s.Listener != nil {
		ln = s.Listener()
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	d := s.Daemon
	return Status{Status: st, Daemon: &d, Health: &h, Listener: ln,
		Alerts: Alerts(AlertInput{Login: st, Health: &h, Listener: ln, Now: now()})}, nil
}
