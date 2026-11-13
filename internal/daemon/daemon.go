// Package daemon is the wiring of a pcb process that serves its accounts:
// the CalDAV listener (TLS included), the control socket and the drain at
// the end. pcb serve runs it; the desktop app reuses it. It owns none of
// the accounts: the caller opens the Hub and closes it after Run returns.
package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

// The listener's limits. A calendar-multiget of 1 000 objects (the
// adapter's cap) decrypts and renders up to 1 000 events and sends the
// result; 60 s for reading a request (8 MiB at most) and 60 s for writing a
// response leave it room on a slow machine or link, while a stalled client
// still cannot hold a connection open forever. A PUT or DELETE may run
// longer when Proton is slow: it extends its own write deadline once its
// body is read (caldav, ruling M4-R42), so a write that landed delivers its
// answer; the read timeout never cuts a request once its body is read.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	// drainTimeout bounds the wait for requests in flight when the daemon
	// stops; what still runs then is cut.
	drainTimeout = 5 * time.Second
)

// Hub is what the daemon needs of the accounts: *service.Hub.
type Hub interface {
	ctl.Hub
	service.Router
	Serve(ctx context.Context, o service.HubServeOptions) error
	OnChange(f func())
}

// ErrCut is the cause of the error Run ends with when Options.Cut closed
// while it stopped (ruling M5-P22): the sync loops may still run, the
// process is to end at once.
var ErrCut = errors.New("a second signal cut the stop of the daemon")

func cutError() error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: ErrCut, Hint: "the sync loop was cut; run pcb serve again"}
}

// Options configure Run.
//
// OnState and OnPass run on an account's loop goroutine: they must never
// wait for a Hub call that changes the accounts (Login, SubmitTOTP,
// Remove, SetCalDAV, Restart, SignOut), which may wait for that loop.
type Options struct {
	Dirs   paths.Dirs
	Config config.Config
	// Hub is the accounts, opened by the caller, who closes it after Run
	// returns (Run closes nothing it did not open).
	Hub Hub
	// Kind is what the control socket reports as the daemon's kind
	// ("serve", "ui", "mcp"), and what serve.lock records.
	Kind     string
	LogLevel string
	Logger   *slog.Logger // nil: no logs
	// Warn gets the "Warning:" lines a person should read (nil: dropped).
	Warn io.Writer
	// Listen opens the CalDAV listener (nil: net.Listen).
	Listen func(network, addr string) (net.Listener, error)
	// WaitForLogin is Hub.Serve's process exit rule.
	WaitForLogin bool
	// NoListener runs without the CalDAV listener, and so without the
	// remote /mcp whatever [mcp] remote says: the session daemon of pcb
	// mcp opens no network port (design §5.3). The control socket, its
	// /mcp included, still opens; Ready gets a zero Info.
	NoListener bool
	// Cut closing while Run stops ends the stop at once with ErrCut.
	Cut     <-chan struct{}
	OnState func(accountID string, s service.ServeState)
	OnPass  func(accountID string, p service.WatchPass)
	// Ready is called once the control socket and the CalDAV listener are
	// bound, before the listener accepts (NoListener: once the control
	// socket is, with a zero Info).
	Ready func(Info)
	// MCP is the MCP adapter. The control socket serves it on /mcp to the
	// relays of pcb mcp (mcp.RelayAuth: the grant they send) and reports
	// its sessions. With [mcp] remote on, the listener also serves it on
	// /mcp to the holders of a token (<state>/mcp-tokens.json). nil: no MCP
	// in this process.
	MCP *mcp.Server
	// ShowUI raises the desktop app's window for POST /v2/ui/show (nil: the
	// route does not exist, as in pcb serve). It runs on the control
	// socket's handler goroutine and must return quickly, for example by
	// posting to the UI loop.
	ShowUI func(account string) error
}

// Info describes the CalDAV listener.
type Info struct {
	Addr string
	// Base is <scheme>://<host>:<port>, no trailing slash.
	Base         string
	TLS          bool
	Fingerprint  string
	CertNotAfter *time.Time
}

// CTL is the listener as the control socket publishes it.
func (i Info) CTL() *ctl.Listener {
	return &ctl.Listener{Addr: i.Addr, URL: i.Base, TLS: i.TLS, Fingerprint: i.Fingerprint, CertNotAfter: i.CertNotAfter}
}

// AccountURL is the URL to enter in a CalDAV client for the account localID.
func (i Info) AccountURL(localID string) string { return i.Base + service.AccountPath(localID) }

// Run serves the accounts of o.Hub until ctx ends: Hub.Serve, then, once it
// holds its locks and found a login to serve (with WaitForLogin: its locks
// only), the control socket then the CalDAV listener (a refused start binds
// neither; NoListener: the control socket only). The end of ctx drains the
// listener for up to 5 s, stops the control socket then Hub.Serve and
// returns nil. Hub.Serve ending on its
// own ends Run the same way, with its error; so does a failed listener
// (Blocked), unless Hub.Serve's error is one a person must act on. Closing
// o.Cut meanwhile makes Run return at once with ErrCut. The control socket
// closes, and is removed, before Hub.Serve releases the sync lock.
func Run(ctx context.Context, o Options) error {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	warn := o.Warn
	if warn == nil {
		warn = io.Discard
	}
	listen := o.Listen
	if listen == nil {
		listen = net.Listen
	}
	cc := o.Config.CalDAV
	started := time.Now()
	// The sync loops outlive ctx until the listener is drained, so the
	// requests in flight still read their cache handles.
	syncCtx, stopSync := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSync()
	ready, synced := make(chan struct{}), make(chan error, 1)
	go func() {
		synced <- o.Hub.Serve(syncCtx, service.HubServeOptions{
			ServeOptions: service.ServeOptions{
				Ready:        func() { close(ready) },
				Logger:       log,
				WaitForLogin: o.WaitForLogin,
			},
			OnState: o.OnState,
			OnPass:  o.OnPass,
			Kind:    o.Kind,
		})
	}()
	// finish stops Hub.Serve and returns its error over err: a person is
	// needed, which matters more than a listener that failed meanwhile.
	finish := func(err error) error {
		return unlessCut(log, o.Cut, func() error {
			stopSync()
			if serr := <-synced; serr != nil {
				return serr
			}
			return err
		})
	}
	select {
	case <-ready:
	case err := <-synced:
		return err
	case <-ctx.Done():
		return finish(nil)
	}

	remoteMCP := o.Config.MCP.Remote && o.MCP != nil && !o.NoListener
	switch {
	case o.Config.MCP.Remote && o.NoListener:
		log.Info("[mcp] remote is on, but this process opens no listener: /mcp is served on the control socket only")
	case o.Config.MCP.Remote && o.MCP == nil:
		log.Warn("[mcp] remote is on, but this process has no MCP server: /mcp is not served")
	}
	var relay http.Handler
	if o.MCP != nil {
		relay = o.MCP.HTTPHandler(mcp.RelayAuth)
	}
	var published atomic.Pointer[ctl.Listener]
	stopControl, err := startControl(log, o.Hub, ctl.NewDaemon(o.Kind, o.Dirs.Socket(), o.LogLevel, started), published.Load, o.ShowUI,
		mcpStatus(o.MCP, remoteMCP), relay)
	if err != nil {
		return finish(err)
	}
	syncFinish := finish
	finish = func(err error) error {
		if err := unlessCut(log, o.Cut, func() error { stopControl(); return nil }); err != nil {
			return err
		}
		return syncFinish(err)
	}

	if o.NoListener {
		if o.Ready != nil {
			o.Ready(Info{})
		}
		log.Info("serving without a listener: the control socket only")
		var result error
		select {
		case <-ctx.Done():
		case result = <-synced:
			synced <- result // finish waits on a Serve that already returned
		}
		return finish(result)
	}

	ln, err := listen(listenNetwork(cc.Listen), cc.Listen)
	if err != nil {
		return finish(&exitcode.Error{Code: exitcode.Blocked, Err: causeError("cannot listen on "+cc.Listen, err),
			Hint: "stop what uses that address, or choose another with --listen or [caldav] listen"})
	}
	ip := bindIP(ln.Addr())
	tlsConf, fingerprint, err := listenerTLS(log, warn, ip, filepath.Join(o.Dirs.State, "tls"), cc)
	if err != nil {
		_ = ln.Close()
		return finish(err)
	}
	base, err := thunderbirdBase(cc, ip, ln.Addr(), tlsConf != nil)
	if err != nil {
		log.Warn("cannot list the network interfaces: the printed URL says localhost", "error", err.Error())
	}
	info := Info{Addr: ln.Addr().String(), Base: base, TLS: tlsConf != nil, Fingerprint: fingerprint, CertNotAfter: certNotAfter(tlsConf)}
	published.Store(info.CTL())
	if o.Ready != nil {
		o.Ready(info)
	}

	handler := caldav.Handler(o.Hub, caldav.Options{Logger: log})
	if remoteMCP {
		// Its own failure limiter: guesses at tokens and at CalDAV
		// passwords are counted apart (design §5.2).
		auth := mcp.TokenAuth(access.NewTokenStore(o.Dirs.State, nil), access.NewLimiter(nil), cc.Hostnames)
		handler = withMCP(handler, o.MCP.HTTPHandler(auth))
	}
	srv := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          serverErrorLog(log),
	}
	served := make(chan error, 1)
	go func() {
		if tlsConf != nil {
			served <- srv.ServeTLS(ln, "", "")
			return
		}
		served <- srv.Serve(ln)
	}()
	log.Info("listener started", "addr", info.Addr, "tls", info.TLS, "mcp", remoteMCP)

	var result error
	select {
	case <-ctx.Done():
	case result = <-synced:
		// Hub.Serve ended on its own: a person is needed. The listener is
		// drained below, then finish waits on a Serve that already returned.
		synced <- result
	case err := <-served:
		result = &exitcode.Error{Code: exitcode.Blocked, Err: causeError("the CalDAV listener failed", err),
			Hint: "run pcb serve again"}
	}
	if err := unlessCut(log, o.Cut, func() error { stopListener(srv, log, drainTimeout); return nil }); err != nil {
		return err
	}
	return finish(result)
}

// unlessCut runs stop and returns its error, unless cut closes first: then
// it returns ErrCut at once (stop goes on in the background, the process
// ends). A nil cut never closes.
func unlessCut(log *slog.Logger, cut <-chan struct{}, stop func() error) error {
	done := make(chan error, 1)
	go func() { done <- stop() }()
	select {
	case err := <-done:
		return err
	case <-cut:
		log.Warn("a second signal cut the stop: the sync loop did not end")
		return cutError()
	}
}

// causeError is msg, then the operating system's reason for err (such as
// "address already in use"), without the Go operation and address it
// repeats ("listen tcp 127.0.0.1:1080: bind: ..."; ux Minor). It unwraps to
// err.
func causeError(msg string, err error) error {
	reason := err.Error()
	var (
		se *os.SyscallError
		oe *net.OpError
	)
	switch {
	case errors.As(err, &se):
		reason = se.Err.Error()
	case errors.As(err, &oe) && oe.Err != nil:
		reason = oe.Err.Error()
	}
	return &wrapped{msg: msg + ": " + reason, err: err}
}

// wrapped is an error whose text is msg and whose cause is err.
type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.err }

// startControl serves the control socket of d (d.Socket) on hub and
// returns what stops it: it closes the socket, which removes it. Off Unix
// there is no control socket (ruling M5-P3); any other failure to open it
// is exit 20 with a hint for its cause.
func startControl(log *slog.Logger, hub ctl.Hub, d ctl.Daemon, listener func() *ctl.Listener, showUI func(account string) error,
	mcpStatus func() *ctl.MCP, relay http.Handler) (stop func(), err error) {
	ln, err := ctl.Listen(d.Socket)
	switch {
	case errors.Is(err, ctl.ErrUnsupported):
		log.Debug("no control socket on this system")
		return func() {}, nil
	case err != nil:
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("cannot open the control socket %s: %w", d.Socket, err),
			Hint: controlHint(err, d.Socket)}
	}
	srv := &ctl.Server{Hub: hub, Daemon: d, Listener: listener, Now: time.Now, Logger: log, ShowUI: showUI, MCPStatus: mcpStatus, MCP: relay}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Warn("the control socket failed: pcb status shows the offline view", "error", err.Error())
		}
	}()
	log.Info("control socket started", "path", d.Socket)
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
			_ = srv.Shutdown(ctx)
			<-done
			log.Info("control socket stopped")
		})
	}, nil
}

// mcpPath is where the listener serves MCP when [mcp] remote is on.
const mcpPath = "/mcp"

// withMCP sends the path /mcp to m and every other one to dav. Not a
// ServeMux: its path cleaning and redirects would change what CalDAV
// clients get.
func withMCP(dav, m http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mcpPath {
			m.ServeHTTP(w, r)
			return
		}
		dav.ServeHTTP(w, r)
	})
}

// mcpStatus is the control socket's view of the MCP adapter: remote /mcp
// on or off, and the live sessions per entry (all 0 without one).
func mcpStatus(srv *mcp.Server, remote bool) func() *ctl.MCP {
	return func() *ctl.MCP {
		m := &ctl.MCP{Remote: remote, Sessions: map[string]int{}}
		if srv != nil {
			for e, n := range srv.LiveSessions() {
				m.Sessions[string(e)] = n
			}
		}
		return m
	}
}

// controlHint is what to do about a control socket that cannot be opened.
func controlHint(err error, socket string) string {
	dir := filepath.Dir(socket)
	switch {
	case errors.Is(err, ctl.ErrPathTooLong):
		return "set " + paths.EnvStateDir + " to a shorter path"
	case errors.Is(err, ctl.ErrUnsafeDir):
		return "chmod 700 " + dir
	case errors.Is(err, ctl.ErrNotASocket):
		return "move " + socket + " away: pcb serve keeps its control socket there"
	}
	return "make sure " + dir + " is writable by this user"
}

// certNotAfter is the end of the listener's certificate, nil without TLS.
func certNotAfter(conf *tls.Config) *time.Time {
	if conf == nil || len(conf.Certificates) == 0 || len(conf.Certificates[0].Certificate) == 0 {
		return nil
	}
	leaf := conf.Certificates[0].Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(conf.Certificates[0].Certificate[0]); err != nil {
			return nil
		}
	}
	t := leaf.NotAfter.UTC()
	return &t
}

// stopListener stops accepting, waits up to timeout for the requests in
// flight, then cuts what is left.
func stopListener(srv *http.Server, log *slog.Logger, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		log.Warn("listener stopped, requests still running after the drain were cut", "drain", timeout)
		return
	}
	log.Info("listener stopped")
}

// bindIP is the IP the listener is bound to, nil if it is not a TCP one.
func bindIP(a net.Addr) net.IP {
	if t, ok := a.(*net.TCPAddr); ok {
		return t.IP
	}
	return nil
}

// listenerTLS is the TLS of the listener, nil for plain HTTP: the user's
// certificate when [caldav] names one (on any bind: the user asked for
// TLS), else plain HTTP on a loopback bind, else the self-signed certificate
// of tlsDir. An address that is not a TCP one counts as non-loopback.
// Certificates a person should look at get a "Warning:" line on warn (the
// human-readable stderr): a user certificate expired or within 30 days of
// expiry (ruling M3-R32), a stored self-signed pair that was unusable and
// replaced (ruling M3-R31: a new fingerprint to accept).
func listenerTLS(log *slog.Logger, warn io.Writer, ip net.IP, tlsDir string, cc config.CalDAV) (*tls.Config, string, error) {
	var cert tls.Certificate
	var fingerprint string
	switch {
	case cc.TLSCert != "":
		c, leaf, err := tlsutil.LoadPair(cc.TLSCert, cc.TLSKey)
		if err != nil {
			return nil, "", &exitcode.Error{Code: exitcode.MustAct,
				Err:  fmt.Errorf("cannot use caldav.tls_cert and caldav.tls_key: %w", err),
				Hint: "fix the two files or their paths in config.toml, or remove both keys to use a self-signed certificate"}
		}
		cert, fingerprint = c, tlsutil.Fingerprint(leaf.Raw)
		log.Info("tls certificate from the configuration", "fingerprint", fingerprint, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339))
		expires, now := leaf.NotAfter.UTC().Format(time.DateOnly), time.Now()
		switch {
		case !now.Before(leaf.NotAfter):
			fmt.Fprintf(warn, "Warning: the certificate of caldav.tls_cert expired on %s: clients will refuse it; replace it\n", expires)
		case !now.Add(tlsutil.RenewBefore).Before(leaf.NotAfter):
			fmt.Fprintf(warn, "Warning: the certificate of caldav.tls_cert expires on %s, in less than 30 days: renew it\n", expires)
		}
	case !servesTLS(cc, ip):
		return nil, "", nil
	default:
		res, err := tlsutil.SelfSigned(tlsutil.Options{Dir: tlsDir, Hostnames: cc.Hostnames, BindIP: ip})
		if err != nil {
			return nil, "", &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("cannot prepare the TLS certificate: %w", err),
				Hint: "make sure " + tlsDir + " is writable by this user"}
		}
		cert, fingerprint = res.Certificate, res.Fingerprint
		notAfter := res.Leaf.NotAfter.UTC().Format(time.RFC3339)
		for _, p := range res.Repaired {
			log.Warn("tls permissions narrowed", "path", p)
		}
		if res.Unusable {
			fmt.Fprintf(warn, "Warning: the stored self-signed certificate was unusable and was replaced: accept the new one in Thunderbird after checking its fingerprint %s\n", fingerprint)
		}
		if res.Replaced {
			if res.PreviousFingerprint == "" {
				res.PreviousFingerprint = "unknown"
			}
			log.Warn("the TLS certificate changed: Thunderbird will ask to confirm the new fingerprint",
				"old_fingerprint", res.PreviousFingerprint, "new_fingerprint", fingerprint, "reason", res.Reason)
		}
		if res.Generated {
			log.Info("tls certificate generated", "fingerprint", fingerprint, "reason", res.Reason, "not_after", notAfter)
		} else {
			log.Info("tls certificate reused", "fingerprint", fingerprint, "not_after", notAfter)
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, fingerprint, nil
}

// servesTLS reports whether pcb serve, bound to ip, serves TLS: always
// with a configured certificate, else on any bind but a loopback one (nil:
// unknown, counted as non-loopback).
func servesTLS(cc config.CalDAV, ip net.IP) bool {
	return cc.TLSCert != "" || ip == nil || !ip.IsLoopback()
}

// OfflineInfo is what Run would report for cc with no daemon running: the
// base for the configured listen address (its host resolved as pcb doctor
// predicts the bind) and, over TLS, the fingerprint of the certificate on
// disk: caldav.tls_cert, else the self-signed one of dirs.State/tls ("" while
// there is none or it is unreadable). Base is "" when cc.Listen is not
// HOST:PORT. It creates nothing.
func OfflineInfo(ctx context.Context, dirs paths.Dirs, cc config.CalDAV) Info {
	host, portText, err := net.SplitHostPort(cc.Listen)
	if err != nil {
		return Info{}
	}
	port, _ := strconv.Atoi(portText)
	ip, _ := (&doctor.Bind{Listen: cc.Listen}).IP(ctx)
	useTLS := servesTLS(cc, ip)
	addr := &net.TCPAddr{IP: ip, Port: port}
	// A prediction: localhost when the interfaces cannot be listed.
	base, _ := thunderbirdBase(cc, ip, addr, useTLS)
	if ip == nil && len(cc.Hostnames) == 0 { // unresolved: the name itself
		base, _ = thunderbirdBase(config.CalDAV{Hostnames: []string{host}}, nil, addr, useTLS)
	}
	info := Info{Addr: cc.Listen, Base: base, TLS: useTLS}
	if !useTLS {
		return info
	}
	var leaf *x509.Certificate
	if cc.TLSCert != "" {
		leaf, err = tlsutil.ReadCertificate(cc.TLSCert)
	} else {
		leaf, err = tlsutil.Inspect(filepath.Join(dirs.State, "tls"))
	}
	if err != nil {
		return info
	}
	info.Fingerprint = tlsutil.Fingerprint(leaf.Raw)
	return info
}

// thunderbirdBase is <scheme>://<host>:<port> of the listener at addr,
// bound to ip; the error is urlHost's.
func thunderbirdBase(cc config.CalDAV, ip net.IP, addr net.Addr, useTLS bool) (string, error) {
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	port := "0"
	if t, ok := addr.(*net.TCPAddr); ok {
		port = fmt.Sprint(t.Port)
	}
	host, err := urlHost(cc, ip, net.InterfaceAddrs)
	return scheme + "://" + net.JoinHostPort(host, port), err
}

// urlHost is the host of the printed URL: the first configured hostname,
// else localhost for 127.0.0.1 and ::1 (what localhost resolves to), else
// the bind IP; for an unspecified bind, the first non-loopback interface
// address (IPv4 first), else localhost. The error says why interfaces
// could not be listed (the host is then localhost).
func urlHost(cc config.CalDAV, ip net.IP, interfaces func() ([]net.Addr, error)) (string, error) {
	switch {
	case len(cc.Hostnames) > 0:
		return cc.Hostnames[0], nil
	case ip == nil || ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback):
		return "localhost", nil
	case !ip.IsUnspecified():
		return ip.String(), nil
	}
	addrs, err := interfaces()
	if err != nil {
		return "localhost", err
	}
	var v6 string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
			continue
		}
		if n.IP.To4() != nil {
			return n.IP.String(), nil
		}
		if v6 == "" {
			v6 = n.IP.String()
		}
	}
	if v6 != "" {
		return v6, nil
	}
	return "localhost", nil
}

// serverErrorLog is the http.Server's ErrorLog: a failed TLS handshake is a
// port scan or a client that does not trust the certificate, unauthenticated
// and unlimited, so it is Debug; anything else the server reports is Warn
// (audit surface-M7).
func serverErrorLog(log *slog.Logger) *stdlog.Logger {
	return stdlog.New(errorLogWriter{log}, "", 0)
}

type errorLogWriter struct{ log *slog.Logger }

func (w errorLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSuffix(string(p), "\n")
	level := slog.LevelWarn
	if strings.HasPrefix(msg, "http: TLS handshake error") {
		level = slog.LevelDebug
	}
	w.log.Log(context.Background(), level, msg)
	return len(p), nil
}

// listenNetwork is the network the daemon listens on: "tcp4" for an IPv4
// literal, because Go's "tcp" on 0.0.0.0 also binds [::] and would expose
// the port on every IPv6 address of a user who chose IPv4 (audit
// surface-M1); "tcp" for [::], an empty host and names, which keep the dual
// stack.
func listenNetwork(listen string) string {
	host, _, err := net.SplitHostPort(listen)
	if ip := net.ParseIP(host); err == nil && ip != nil && ip.To4() != nil && !strings.Contains(host, ":") {
		return "tcp4"
	}
	return "tcp"
}
