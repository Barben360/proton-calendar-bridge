package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
)

// The streamable HTTP endpoint /mcp (design §5.2): stateful sessions (an
// elicitation needs one, E19), each bound to the caller that opened it.
// The daemon mounts it on its listener behind TokenAuth; the control
// socket mounts it for the relays of pcb mcp.

const (
	// sessionIdle closes a session nobody used for that long: the idle
	// timeout that replaces the listener's write timeout on /mcp (design
	// §5.2). A client that comes back later gets 404 and starts a session
	// again.
	sessionIdle = time.Hour
	// boundBearer replaces the caller's Authorization header before the
	// SDK reads the request: the secret never reaches a tool's
	// RequestExtra, and the SDK's bearer middleware, which alone can bind
	// a session to its caller, gets a token to pass to our verifier.
	boundBearer = "Bearer pcb-authorised"
)

// Auth decides a request's grant: (grant, entry, token name) or an error
// answered 401/503 (an *AuthError says which, and may say 403).
type Auth func(r *http.Request) (Grant, Entry, string, error)

// AuthError is an Auth refusal: the HTTP status it is answered with, the
// Retry-After of a 503, and a message for the client that never quotes a
// secret. Err is the cause, for the log.
type AuthError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
	Err        error
}

func (e *AuthError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *AuthError) Unwrap() error { return e.Err }

// caller is what Auth said of a request, carried to getServer.
type caller struct {
	grant Grant
	entry Entry
	// user binds a session: the entry, the token's name and the grant.
	// A request with another identity cannot use the session (the SDK
	// answers 403).
	user string
}

type callerKey struct{}

// HTTPHandler serves MCP over streamable HTTP for the callers auth admits.
// A refused request gets auth's status and never reaches the SDK. An
// admitted one has the listener's write deadline lifted (a tool call or an
// SSE stream may outlive it; the session's idle timeout bounds a session
// instead; net/http already lifts the read deadline once the body is
// read) and is served by the SDK server of its grant (Session).
func (s *Server) HTTPHandler(a Auth) http.Handler {
	sdk := sdkmcp.NewStreamableHTTPHandler(func(r *http.Request) *sdkmcp.Server {
		c, ok := r.Context().Value(callerKey{}).(caller)
		if !ok {
			return nil
		}
		return s.Session(c.grant, c.entry)
	}, &sdkmcp.StreamableHTTPOptions{
		// The SDK refuses a configured hostname on a loopback bind and
		// never checks Origin (E19): TokenAuth guards Host and Origin.
		DisableLocalhostProtection: true,
		SessionTimeout:             sessionIdle,
	})
	// The SDK binds a session to TokenInfo.UserID only when this
	// middleware set it (E19).
	bound := auth.RequireBearerToken(func(_ context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
		c, ok := r.Context().Value(callerKey{}).(caller)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: c.user}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(sdk)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, e, name, err := a(r)
		if err != nil {
			s.refuse(w, r, err)
			return
		}
		// Not supported (the control socket's server may have none):
		// nothing to lift.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		r = r.Clone(context.WithValue(r.Context(), callerKey{}, caller{grant: g, entry: e,
			user: string(e) + "\x00" + name + "\x00" + g.key()}))
		r.Header.Set("Authorization", boundBearer)
		bound.ServeHTTP(w, r)
	})
}

// refuse answers a request Auth refused.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, err error) {
	var ae *AuthError
	if !errors.As(err, &ae) {
		ae = &AuthError{Status: http.StatusServiceUnavailable, Message: "MCP is not available", Err: err}
	}
	switch ae.Status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer realm="pcb"`)
	case http.StatusServiceUnavailable:
		if ae.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((ae.RetryAfter+time.Second-1)/time.Second)))
		}
	}
	if ae.Err != nil { // a fault of this side (the token file): a person should look
		s.o.Logger.Warn("mcp request refused", "status", ae.Status, "error", ae.Err.Error())
	} else {
		s.o.Logger.Debug("mcp request refused", "status", ae.Status, "remote", r.RemoteAddr)
	}
	http.Error(w, ae.Message, ae.Status)
}

// TokenAuth admits the holders of a token of store (design §5.2), as entry
// remote. In order:
//   - a Host that is neither loopback, nor one of hostnames, nor the
//     address the request came in on, is refused (403), as is a present
//     Origin whose host:port is not the Host (the DNS-rebinding guard);
//   - an address lim blocks gets 503 and Retry-After;
//   - no token, or one that is unknown, expired or removed, gets 401 and
//     counts as a failure in lim;
//   - a token is the grant of its capabilities, accounts and calendars,
//     and its use is recorded (Touch).
//
// A token file that cannot be read is 503 and counts nothing.
func TokenAuth(store *access.TokenStore, lim *access.Limiter, hostnames []string) Auth {
	hostnames = slices.Clone(hostnames)
	return func(r *http.Request) (Grant, Entry, string, error) {
		if !hostAllowed(r, hostnames) {
			return Grant{}, "", "", &AuthError{Status: http.StatusForbidden, Message: "this host name is not served"}
		}
		if !originMatches(r) {
			return Grant{}, "", "", &AuthError{Status: http.StatusForbidden, Message: "cross-origin requests are refused"}
		}
		if ok, wait := lim.Allow(r.RemoteAddr); !ok {
			return Grant{}, "", "", &AuthError{Status: http.StatusServiceUnavailable, RetryAfter: wait,
				Message: "too many failed attempts: try again later"}
		}
		unauthorised := &AuthError{Status: http.StatusUnauthorized, Message: "an MCP token is required (Authorization: Bearer pcbm_...)"}
		secret, ok := bearerToken(r)
		if !ok {
			lim.Fail(r.RemoteAddr)
			return Grant{}, "", "", unauthorised
		}
		tok, ok, err := store.Verify(secret)
		switch {
		case err != nil:
			return Grant{}, "", "", &AuthError{Status: http.StatusServiceUnavailable, Message: "the MCP token list cannot be read", Err: err}
		case !ok:
			lim.Fail(r.RemoteAddr)
			return Grant{}, "", "", unauthorised
		}
		caps, err := ParseCaps(strings.Join(tok.Caps, ","))
		if err != nil {
			return Grant{}, "", "", &AuthError{Status: http.StatusForbidden,
				Message: "this token's capabilities cannot be served: remove it and add it again"}
		}
		_ = store.Touch(r.Context(), tok.Name) // a use that could not be recorded still is one
		return Grant{Name: tok.Name, Caps: caps, Accounts: tok.Accounts, Calendars: tok.Calendars}, EntryRemote, tok.Name, nil
	}
}

// bearerToken is the token of an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
		return "", false
	}
	return token, true
}

// hostOnly is the host of a Host value (host, host:port, [v6]:port),
// lower case, without the brackets of an IPv6 literal.
func hostOnly(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
}

// hostAllowed: the request's Host is a loopback name or address, one of
// hostnames, or the IP address the request came in on (a client that
// typed the bind address; an IP literal is not a rebound name).
func hostAllowed(r *http.Request, hostnames []string) bool {
	host := hostOnly(r.Host)
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	if slices.ContainsFunc(hostnames, func(h string) bool { return strings.EqualFold(h, host) }) {
		return true
	}
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && ip != nil {
		if t, ok := local.(*net.TCPAddr); ok && t.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// originMatches: no Origin, or one whose host:port is the request's Host.
func originMatches(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// LiveSessions counts the open MCP sessions per entry, over the SDK
// servers Session keeps (a server dropped from that cache past 256
// grants keeps serving its sessions uncounted).
func (s *Server) LiveSessions() map[Entry]int {
	s.mu.Lock()
	servers := make(map[string]*sdkmcp.Server, len(s.servers))
	for k, v := range s.servers {
		servers[k] = v
	}
	s.mu.Unlock()
	out := map[Entry]int{EntryRemote: 0, EntryRelay: 0, EntryStdio: 0}
	for key, srv := range servers {
		entry, _, _ := strings.Cut(key, "\x02")
		for range srv.Sessions() {
			out[Entry(entry)]++
		}
	}
	return out
}
