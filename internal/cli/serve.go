package cli

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

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
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
	// drainTimeout bounds the wait for requests in flight when pcb serve
	// stops; what still runs then is cut.
	drainTimeout = 5 * time.Second
)

func (a *app) newServe() *cobra.Command {
	var listen, logLevel string
	var waitForLogin bool
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the calendars over CalDAV and keep them in sync",
		Long: "Run the sync loop and the CalDAV listener in one process until Ctrl-C (SIGINT or SIGTERM: exit 0; a " +
			"second one while it stops: exit 20) or until you must log in again. With --wait-for-login it keeps " +
			"running instead, logged out or after the session ends: CalDAV answers 503 until a login completes " +
			"(pcb login, from any process), then serves again. The first lines name the URL to enter in Thunderbird " +
			"and, over TLS, the certificate fingerprint to check in its exception dialog. A loopback address serves " +
			"plain HTTP; any other serves TLS, with [caldav] tls_cert and tls_key or a self-signed certificate kept " +
			"in the state directory. Logs go to stderr.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if JSONMode(cmd) {
				return &exitcode.Error{Code: exitcode.Usage,
					Err:  errors.New("--json cannot be combined with serve: it runs until stopped and prints logs"),
					Hint: "drop --json"}
			}
			if cmd.Flags().Changed("listen") {
				if err := config.ValidateListen(listen); err != nil {
					return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("--listen %w", err),
						Hint: "use HOST:PORT, such as 127.0.0.1:1080 or 0.0.0.0:8443"}
				}
			}
			if cmd.Flags().Changed("log-level") {
				if _, err := config.ParseLevel(logLevel); err != nil {
					return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("--log-level %w", err),
						Hint: "use debug, info, warn or error"}
				}
			}
			dirs, cfg, err := loadConfig(a.Env)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("log-level") {
				cfg.Log.Level = logLevel
			}
			if cmd.Flags().Changed("listen") {
				cfg.CalDAV.Listen = listen
			}
			svc, err := a.service(cmd)
			if err != nil {
				return err
			}
			err = a.serve(cmd, svc, dirs, cfg.CalDAV, cfg.Log.Level, waitForLogin)
			if !errors.Is(err, errStopCut) { // cut: the process ends with the loop still running
				a.closeService(svc)
			}
			if a.daemonSyncs(cmd, err) {
				// Exit 20 as any refused start (M5 plan: sync.lock refused).
				return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("pcb serve is already running"), Hint: "run pcb status to see it"}
			}
			return err
		},
	}
	c.Flags().StringVar(&listen, "listen", "", "HOST:PORT to listen on, over [caldav] listen (default "+config.DefaultListen+")")
	c.Flags().StringVar(&logLevel, "log-level", "", "debug, info, warn or error, over [log] level (default info)")
	c.Flags().BoolVar(&waitForLogin, "wait-for-login", false, "keep serving (CalDAV answers 503) while logged out or after the session ends, until a login")
	return NoJSON(c)
}

// waitingLine is what pcb serve --wait-for-login prints while it waits.
const waitingLine = "Waiting for a login: run pcb login"

// errStopCut is the cause of the error pcb serve ends with when a second
// signal cuts its stop (ruling M5-P22).
var errStopCut = errors.New("a second signal cut the stop of pcb serve")

func stopCut() error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: errStopCut, Hint: "the sync loop was cut; run pcb serve again"}
}

// stopSignals watches sigs: stop is closed on the first signal or the end
// of ctx (Execute turns the first signal into that end too), cut on the
// second signal. end stops the watch.
func stopSignals(ctx context.Context, sigs <-chan os.Signal) (stop, cut <-chan struct{}, end func()) {
	stopC, cutC, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		var once sync.Once
		stopping := func() { once.Do(func() { close(stopC) }) }
		ctxDone := ctx.Done()
		for n := 0; ; {
			select {
			case <-done:
				return
			case <-ctxDone:
				stopping()
				ctxDone = nil
			case <-sigs:
				stopping()
				if n++; n == 2 {
					close(cutC)
					return
				}
			}
		}
	}()
	var once sync.Once
	return stopC, cutC, func() { once.Do(func() { close(done) }) }
}

// unlessCut runs stop and returns its error, unless cut closes first: then
// it returns at once (stop goes on in the background, the process ends).
func unlessCut(log *slog.Logger, cut <-chan struct{}, stop func() error) error {
	done := make(chan error, 1)
	go func() { done <- stop() }()
	select {
	case err := <-done:
		return err
	case <-cut:
		log.Warn("a second signal cut the stop: the sync loop did not end")
		return stopCut()
	}
}

// waitNotice prints waitingLine each time Serve begins waiting for a
// login, never before the intro: a wait reported earlier is printed right
// after it.
type waitNotice struct {
	mu        sync.Mutex
	w         io.Writer
	introDone bool
	waiting   bool
}

func (n *waitNotice) state(s service.ServeState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.waiting = s == service.ServeWaitingForLogin
	if n.waiting && n.introDone {
		fmt.Fprintln(n.w, waitingLine)
	}
}

func (n *waitNotice) intro() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.introDone = true
	if n.waiting {
		fmt.Fprintln(n.w, waitingLine)
	}
}

// serve runs the sync loop and, once it holds the sync lock and a ready
// login (with waitForLogin: the sync lock only), the control socket then
// the listener (a refused start binds neither). A signal (the end of cmd's
// context, or the first of a.Signals) drains the listener and ends with
// exit 0; a second signal meanwhile ends at once with exit 20 (ruling
// M5-P22). The sync loop stopping on an error drains it and ends with that
// error. The control socket closes, and is removed, before the sync loop
// releases the sync lock.
func (a *app) serve(cmd *cobra.Command, svc service.API, dirs paths.Dirs, cc config.CalDAV, level string, waitForLogin bool) error {
	ctx := cmd.Context()
	sigs, unsubscribe := a.Signals()
	defer unsubscribe()
	stopping, cut, endSignals := stopSignals(ctx, sigs)
	defer endSignals()
	started := time.Now()
	tlsDir := filepath.Join(dirs.State, "tls")
	lvl, err := config.ParseLevel(level)
	if err != nil { // Load validated it; this is for a caller that did not
		return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("log level %w", err), Hint: "use debug, info, warn or error"}
	}
	log := slog.New(slog.NewTextHandler(a.Stderr, &slog.HandlerOptions{Level: lvl}))
	// The sync loop outlives a signal until the listener is drained, so the
	// requests in flight still read its cache handle.
	syncCtx, stopSync := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSync()
	ready, synced := make(chan struct{}), make(chan error, 1)
	var counters renderLog
	notice := &waitNotice{w: cmd.OutOrStdout()}
	go func() {
		synced <- svc.Serve(syncCtx, service.ServeOptions{
			Ready:        func() { close(ready) },
			Logger:       log,
			WaitForLogin: waitForLogin,
			OnState:      notice.state,
			OnPass: func(p service.WatchPass) {
				logPass(log, p)
				counters.log(log, p.Render)
			},
		})
	}()
	// finish stops the sync loop and returns its error over err: a person
	// is needed, which matters more than a listener that failed meanwhile.
	// A second signal cuts the wait for the loop.
	finish := func(err error) error {
		return unlessCut(log, cut, func() error {
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
	case <-stopping:
		return finish(nil)
	}

	var published atomic.Pointer[ctl.Listener]
	stopControl, err := startControl(log, svc, ctl.NewDaemon(dirs.Socket(), level, started), published.Load)
	if err != nil {
		return finish(err)
	}
	syncFinish := finish
	finish = func(err error) error {
		if err := unlessCut(log, cut, func() error { stopControl(); return nil }); err != nil {
			return err
		}
		return syncFinish(err)
	}

	ln, err := a.Listen(listenNetwork(cc.Listen), cc.Listen)
	if err != nil {
		return finish(&exitcode.Error{Code: exitcode.Blocked, Err: causeError("cannot listen on "+cc.Listen, err),
			Hint: "stop what uses that address, or choose another with --listen or [caldav] listen"})
	}
	ip := bindIP(ln.Addr())
	tlsConf, fingerprint, err := listenerTLS(log, a.Stderr, ip, tlsDir, cc)
	if err != nil {
		_ = ln.Close()
		return finish(err)
	}
	url, err := thunderbirdURL(cc, ip, ln.Addr(), tlsConf != nil)
	if err != nil {
		log.Warn("cannot list the network interfaces: the printed URL says localhost", "error", err.Error())
	}
	a.printIntro(cmd, svc, log, url, tlsConf != nil, fingerprint)
	notice.intro()
	published.Store(&ctl.Listener{Addr: ln.Addr().String(), URL: url, TLS: tlsConf != nil, Fingerprint: fingerprint,
		CertNotAfter: certNotAfter(tlsConf)})

	srv := &http.Server{
		Handler:           caldav.Handler(oneAccount{svc}, caldav.Options{Logger: log}),
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
	log.Info("listener started", "addr", ln.Addr().String(), "tls", tlsConf != nil)

	var result error
	select {
	case <-stopping: // SIGINT or SIGTERM
	case result = <-synced:
		// The sync loop ended on its own: a person is needed. It is drained
		// below, then finish waits on a loop that has already returned.
		synced <- result
	case err := <-served:
		result = &exitcode.Error{Code: exitcode.Blocked, Err: causeError("the CalDAV listener failed", err),
			Hint: "run pcb serve again"}
	}
	if err := unlessCut(log, cut, func() error { stopListener(srv, log, drainTimeout); return nil }); err != nil {
		return err
	}
	return finish(result)
}

// oneAccount routes every CalDAV request on the compatibility URLs to api,
// the one account pcb serve serves until it serves a Hub (M6 Task 8); an
// account URL names none of its accounts (401).
type oneAccount struct{ api service.API }

func (o oneAccount) Route(ctx context.Context, accountID, password, remote string) (service.API, string, time.Duration, error) {
	if accountID != "" {
		return nil, "", 0, nil
	}
	if ok, wait := o.api.CheckClient(ctx, password, remote); !ok {
		return nil, "", wait, nil
	}
	return o.api, "", 0, nil
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

// startControl serves the control socket of d (d.Socket) on svc and
// returns what stops it: it closes the socket, which removes it. Off Unix
// there is no control socket (ruling M5-P3); any other failure to open it
// is exit 20 with a hint for its cause.
func startControl(log *slog.Logger, svc service.API, d ctl.Daemon, listener func() *ctl.Listener) (stop func(), err error) {
	ln, err := ctl.Listen(d.Socket)
	switch {
	case errors.Is(err, ctl.ErrUnsupported):
		log.Debug("no control socket on this system")
		return func() {}, nil
	case err != nil:
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("cannot open the control socket %s: %w", d.Socket, err),
			Hint: controlHint(err, d.Socket)}
	}
	srv := &ctl.Server{API: svc, Daemon: d, Listener: listener, Now: time.Now, Logger: log}
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

// offlineListener is what pcb serve would print for cc with no daemon
// running: the URL for the configured listen address (its host resolved
// as pcb doctor predicts the bind) and, over TLS, the fingerprint of the
// certificate on disk: caldav.tls_cert, else the self-signed one of tlsDir
// ("" while there is none or it is unreadable). It creates nothing.
func offlineListener(ctx context.Context, cc config.CalDAV, tlsDir string) (url, fingerprint string) {
	host, portText, err := net.SplitHostPort(cc.Listen)
	if err != nil {
		return "", ""
	}
	port, _ := strconv.Atoi(portText)
	ip, _ := (&doctor.Bind{Listen: cc.Listen}).IP(ctx)
	useTLS := servesTLS(cc, ip)
	addr := &net.TCPAddr{IP: ip, Port: port}
	// A prediction: localhost when the interfaces cannot be listed.
	url, _ = thunderbirdURL(cc, ip, addr, useTLS)
	if ip == nil && len(cc.Hostnames) == 0 { // unresolved: the name itself
		url, _ = thunderbirdURL(config.CalDAV{Hostnames: []string{host}}, nil, addr, useTLS)
	}
	if !useTLS {
		return url, ""
	}
	var leaf *x509.Certificate
	if cc.TLSCert != "" {
		leaf, err = tlsutil.ReadCertificate(cc.TLSCert)
	} else {
		leaf, err = tlsutil.Inspect(tlsDir)
	}
	if err != nil {
		return url, ""
	}
	return url, tlsutil.Fingerprint(leaf.Raw)
}

// thunderbirdURL is the URL to enter in Thunderbird for the listener at
// addr, bound to ip; the error is urlHost's.
func thunderbirdURL(cc config.CalDAV, ip net.IP, addr net.Addr, useTLS bool) (string, error) {
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	port := "0"
	if t, ok := addr.(*net.TCPAddr); ok {
		port = fmt.Sprint(t.Port)
	}
	host, err := urlHost(cc, ip, net.InterfaceAddrs)
	return scheme + "://" + net.JoinHostPort(host, port) + "/", err
}

// printIntro writes what to enter in Thunderbird: the URL, the
// fingerprint over TLS, and how to get a password when no client exists.
func (a *app) printIntro(cmd *cobra.Command, svc service.API, log *slog.Logger, url string, useTLS bool, fingerprint string) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Thunderbird URL: %s\n", url)
	if useTLS {
		fmt.Fprintf(w, "TLS certificate SHA-256 fingerprint: %s (check it in Thunderbird's certificate exception dialog)\n", fingerprint)
	}
	clients, err := svc.Clients(cmd.Context())
	switch {
	case err != nil:
		log.Warn("cannot list the CalDAV clients", "error", err.Error())
	case len(clients) == 0:
		// Decision 0013: a login creates the client default; once logged
		// in, only client add makes one.
		if st, err := svc.Status(cmd.Context()); err == nil && st.Phase == string(service.StateReady) {
			fmt.Fprintln(w, "No CalDAV client: run pcb client add NAME, then use its password in Thunderbird.")
		} else {
			fmt.Fprintln(w, "No CalDAV client yet: run pcb login (it creates the client default), then pcb client show for its password.")
		}
	}
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

// renderLog logs the render counters of each pass (ruling M3-R34) while one
// is non-zero, and once when they change (so a return to zero is logged);
// counts and calendar IDs only. Serve calls OnPass from one goroutine.
type renderLog struct {
	last service.RenderCounts
}

func (rl *renderLog) log(log *slog.Logger, c *service.RenderCounts) {
	if c == nil || c.Zero() && rl.last.Zero() {
		return
	}
	rl.last = *c
	attrs := []any{"undecryptable", c.Undecryptable, "unverified", c.Unverified, "duplicate_masters", c.DuplicateMasters, "dropped_alarms", c.DroppedAlarms}
	if len(c.KeysErrors) > 0 {
		attrs = append(attrs, "keys_error_ids", c.KeysErrors)
	}
	if c.Zero() {
		log.Info("render counters", attrs...)
		return
	}
	log.Warn("render counters", attrs...)
}

// logPass logs one sync pass: counts and calendar IDs, never names or
// content; a failed pass at warning level with its (secret-free) error.
func logPass(log *slog.Logger, p service.WatchPass) {
	var added, updated, deleted int
	var failed []string
	for _, c := range p.Result.Calendars {
		added, updated, deleted = added+c.Added, updated+c.Updated, deleted+c.Deleted
		if c.Error != "" {
			failed = append(failed, c.ID)
		}
	}
	attrs := []any{"calendars", len(p.Result.Calendars), "added", added, "updated", updated, "deleted", deleted}
	if len(failed) > 0 {
		attrs = append(attrs, "failed_ids", failed)
	}
	if p.Next > 0 {
		attrs = append(attrs, "next", p.Next.Round(time.Second).String())
	}
	if p.Err != nil {
		attrs = append(attrs, "error", p.Err.Error())
		if h := exitcode.HintOf(p.Err); h != "" {
			attrs = append(attrs, "hint", h)
		}
		log.Warn("sync pass failed", attrs...)
		return
	}
	log.Info("sync pass", attrs...)
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

// listenNetwork is the network pcb serve listens on: "tcp4" for an IPv4
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
