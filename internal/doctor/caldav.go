package doctor

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

// listenIP resolves the host of a valid listen address (an IP literal
// needs no lookup). pcb serve decides on the address it actually binds;
// this is the doctor's best guess of it.
func listenIP(listen string) (net.IP, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %s", host)
	}
	return ips[0], nil
}

// usesTLS says whether pcb serve serves TLS: always with a configured
// certificate, else on any bind but loopback (an unresolved one included).
func usesTLS(cc config.CalDAV, ip net.IP) bool {
	return cc.TLSCert != "" || ip == nil || !ip.IsLoopback()
}

// ListenerCheck reports what pcb serve will listen on, and how.
type ListenerCheck struct {
	CalDAV config.CalDAV
}

// Name implements Check.
func (ListenerCheck) Name() string { return "caldav listener" }

// Run implements Check.
func (c ListenerCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	if err := config.ValidateListen(c.CalDAV.Listen); err != nil {
		r.Status, r.Message = StatusError, "caldav.listen "+err.Error()
		r.Fix = "set [caldav] listen in config.toml to HOST:PORT, such as " + config.DefaultListen
		return r
	}
	ip, err := listenIP(c.CalDAV.Listen)
	r.Status = StatusOK
	switch {
	case c.CalDAV.TLSCert != "":
		r.Message = c.CalDAV.Listen + ", TLS with the certificate of caldav.tls_cert"
	case err != nil:
		r.Status, r.Message = StatusWarn, c.CalDAV.Listen+": "+err.Error()+"; pcb serve will serve TLS unless it binds a loopback address"
	case usesTLS(c.CalDAV, ip):
		r.Message = c.CalDAV.Listen + ", TLS with a self-signed certificate (not loopback)"
	default:
		r.Message = c.CalDAV.Listen + ", plain HTTP (loopback only)"
	}
	return r
}

// CertificateCheck reports the certificate pcb serve presents: the user's
// (caldav.tls_cert) or the self-signed one of TLSDir. It warns when fewer
// than 30 days are left or a configured hostname is not covered, and shows
// the SHA-256 fingerprint to compare with Thunderbird's dialog.
type CertificateCheck struct {
	CalDAV config.CalDAV
	TLSDir string           // <state>/tls
	Now    func() time.Time // nil: time.Now
}

// Name implements Check.
func (CertificateCheck) Name() string { return "tls certificate" }

// Run implements Check.
func (c CertificateCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	ip, ipErr := listenIP(c.CalDAV.Listen)
	if ipErr == nil && !usesTLS(c.CalDAV, ip) {
		r.Status, r.Message = StatusSkip, "plain HTTP on loopback: no certificate needed"
		return r
	}
	if c.CalDAV.TLSCert != "" {
		_, leaf, err := tlsutil.LoadPair(c.CalDAV.TLSCert, c.CalDAV.TLSKey)
		if err != nil {
			r.Status, r.Message = StatusError, "caldav.tls_cert and caldav.tls_key: "+err.Error()
			r.Fix = "fix the two files or their paths in config.toml, or remove both keys to use a self-signed certificate"
			return r
		}
		return c.judge(r, leaf, now(), "certificate of caldav.tls_cert", "replace it")
	}
	if runtime.GOOS != "windows" {
		key := tlsutil.KeyPath(c.TLSDir)
		if info, err := os.Stat(key); err == nil && info.Mode().Perm()&^0o600 != 0 {
			r.Status = StatusError
			r.Message = key + " has mode " + modeString(info.Mode().Perm()) + ", want 600"
			r.Fix = "chmod 600 " + shellquote.Quote(key)
			return r
		}
	}
	leaf, err := tlsutil.Inspect(c.TLSDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Status, r.Message = StatusSkip, "no self-signed certificate yet: pcb serve generates it"
		return r
	case err != nil:
		r.Status, r.Message = StatusWarn, err.Error()+": pcb serve replaces it at its next start"
		r.Fix = "restart pcb serve, then accept the new certificate in Thunderbird"
		return r
	}
	return c.judge(r, leaf, now(), "self-signed certificate", "restart pcb serve (it replaces the certificate), then accept the new one in Thunderbird")
}

// judge rates a certificate at t: expiry, hostnames, and its fingerprint
// in every message.
func (c CertificateCheck) judge(r Result, leaf *x509.Certificate, t time.Time, what, fix string) Result {
	fp := "SHA-256 fingerprint " + tlsutil.Fingerprint(leaf.Raw)
	expires := leaf.NotAfter.UTC().Format(time.DateOnly)
	switch {
	case !t.Before(leaf.NotAfter):
		r.Status, r.Message, r.Fix = StatusError, what+" expired on "+expires+", "+fp, fix
		if c.CalDAV.TLSCert == "" {
			r.Status = StatusWarn // pcb serve replaces it
		}
	case !t.Add(tlsutil.RenewBefore).Before(leaf.NotAfter):
		r.Status, r.Message, r.Fix = StatusWarn, what+" expires on "+expires+", in less than 30 days, "+fp, fix
	case tlsutil.Uncovered(leaf, c.CalDAV.Hostnames) != "":
		r.Status, r.Fix = StatusWarn, fix
		r.Message = what + " does not cover the hostname " + tlsutil.Uncovered(leaf, c.CalDAV.Hostnames) + ", " + fp
	default:
		r.Status, r.Message = StatusOK, what+", expires on "+expires+", "+fp
	}
	return r
}

// ClientsCheck warns when no CalDAV client can sign in.
type ClientsCheck struct {
	StateDir string
}

// Name implements Check.
func (ClientsCheck) Name() string { return "caldav clients" }

// Run implements Check.
func (c ClientsCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	list, err := access.NewStore(c.StateDir, nil).List()
	switch {
	case err != nil:
		r.Status, r.Message = StatusError, err.Error()
		r.Fix = "check clients.json in " + shellquote.Quote(c.StateDir) + " (valid JSON in the format pcb wrote, mode 0600), or remove it and add the clients again"
	case len(list) == 0:
		r.Status, r.Message, r.Fix = StatusWarn, "no CalDAV client: nothing can sign in to pcb serve", "pcb client add NAME"
	case len(list) == 1:
		r.Status, r.Message = StatusOK, "1 client"
	default:
		r.Status, r.Message = StatusOK, fmt.Sprintf("%d clients", len(list))
	}
	return r
}
