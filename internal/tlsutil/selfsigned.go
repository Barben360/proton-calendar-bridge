// Package tlsutil holds the certificate of pcb serve's TLS listener: the
// self-signed one pcb generates and keeps in <state>/tls (ruling M3-R29), or
// the user's own pair.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

const (
	// Validity is the life of a generated certificate.
	Validity = 825 * 24 * time.Hour
	// RenewBefore: a generated certificate this close to its expiry is
	// replaced at the next start (and pcb doctor warns about any).
	RenewBefore = 30 * 24 * time.Hour

	certFile = "cert.pem"
	keyFile  = "key.pem"
)

// CertPath is the generated certificate in dir.
func CertPath(dir string) string { return filepath.Join(dir, certFile) }

// KeyPath is the generated certificate's private key in dir.
func KeyPath(dir string) string { return filepath.Join(dir, keyFile) }

// Options say where the generated certificate lives and what it must cover.
type Options struct {
	Dir       string   // <state>/tls, created 0700
	Hostnames []string // the configured hostnames (DNS names or IP addresses)
	// BindIP is the listener's resolved IP. An unspecified one (0.0.0.0,
	// ::) puts every non-loopback interface IP in the certificate, plus
	// 127.0.0.1 and ::1.
	BindIP         net.IP
	Now            func() time.Time           // nil: time.Now
	InterfaceAddrs func() ([]net.Addr, error) // nil: net.InterfaceAddrs
}

// Result is the certificate to serve. Generated says it was made by this
// call, and Reason why ("" when an existing one was reused).
type Result struct {
	Certificate tls.Certificate
	Leaf        *x509.Certificate
	Fingerprint string
	Generated   bool
	Reason      string
	// Unusable: the stored pair could not be read or did not match, and was
	// replaced (pcb serve warns: a new fingerprint to accept).
	Unusable bool
	// Repaired lists the paths whose too-wide modes were narrowed (the
	// directory to 0700, the key to 0600).
	Repaired []string
}

// SelfSigned returns the generated certificate of o.Dir, reusing it unless
// it is missing or unusable, does not cover a configured hostname or a
// specific bind IP, or expires within RenewBefore; then it generates a new
// one (ECDSA P-256, Validity) and writes it atomically, the key 0600.
// On Unix, a directory or a reused key owned by another user is refused,
// and a directory wider than 0700 or a reused key wider than 0600 is
// narrowed (ruling M3-R32).
func SelfSigned(o Options) (Result, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.InterfaceAddrs == nil {
		o.InterfaceAddrs = net.InterfaceAddrs
	}
	now := o.Now()
	repaired, err := secure(o.Dir, 0o700, true)
	if err != nil {
		return Result{}, err
	}
	reason, unusable := "no certificate yet", false
	cert, leaf, err := LoadPair(CertPath(o.Dir), KeyPath(o.Dir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		reason, unusable = "the stored certificate or key is unusable", true
	default:
		reason = staleReason(leaf, o, now)
		if reason == "" {
			fixed, err := secure(KeyPath(o.Dir), 0o600, false)
			if err != nil {
				return Result{}, err
			}
			return Result{Certificate: cert, Leaf: leaf, Fingerprint: Fingerprint(leaf.Raw), Repaired: append(repaired, fixed...)}, nil
		}
	}
	res, err := generate(o, now)
	if err != nil {
		return Result{}, err
	}
	res.Generated, res.Reason, res.Unusable, res.Repaired = true, reason, unusable, repaired
	return res, nil
}

// secure refuses path when another user owns it and narrows its mode to
// want; it returns path when it narrowed it. A missing path is left to be
// created. Nothing is checked on Windows (no Unix modes or owners).
func secure(path string, want fs.FileMode, dir bool) ([]string, error) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case info.IsDir() != dir:
		return nil, fmt.Errorf("%s is not a %s", path, map[bool]string{true: "directory", false: "regular file"}[dir])
	}
	if uid, ok := ownerOf(info); ok && uid != os.Getuid() {
		return nil, fmt.Errorf("%s is owned by another user (uid %d): pcb will not use it", path, uid)
	}
	if info.Mode().Perm()&^want == 0 {
		return nil, nil
	}
	if err := os.Chmod(path, want); err != nil {
		return nil, fmt.Errorf("restrict %s: %w", path, err)
	}
	return []string{path}, nil
}

// staleReason says why leaf must be replaced, "" if it can be kept.
func staleReason(leaf *x509.Certificate, o Options, now time.Time) string {
	if !now.Add(RenewBefore).Before(leaf.NotAfter) {
		return fmt.Sprintf("the certificate expires on %s, in less than %d days", leaf.NotAfter.UTC().Format(time.DateOnly), int(RenewBefore.Hours()/24))
	}
	if h := Uncovered(leaf, o.Hostnames); h != "" {
		return "the certificate does not cover the hostname " + h
	}
	if o.BindIP != nil && !o.BindIP.IsUnspecified() {
		if h := Uncovered(leaf, []string{o.BindIP.String()}); h != "" {
			return "the certificate does not cover the bind address " + h
		}
	}
	return ""
}

// Uncovered returns the first of hostnames leaf is not valid for, "" if it
// covers them all.
func Uncovered(leaf *x509.Certificate, hostnames []string) string {
	for _, h := range hostnames {
		if leaf.VerifyHostname(h) != nil {
			return h
		}
	}
	return ""
}

// sans lists the names and addresses a new certificate covers: the
// hostnames, localhost and the bind IP, or for an unspecified bind every
// non-loopback interface IP plus 127.0.0.1 and ::1.
func sans(o Options) (dns []string, ips []net.IP, err error) {
	seenDNS, seenIP := map[string]bool{}, map[string]bool{}
	addIP := func(ip net.IP) {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if !seenIP[ip.String()] {
			seenIP[ip.String()] = true
			ips = append(ips, ip)
		}
	}
	addDNS := func(name string) {
		if key := strings.ToLower(name); !seenDNS[key] {
			seenDNS[key] = true
			dns = append(dns, name)
		}
	}
	for _, h := range o.Hostnames {
		if ip := net.ParseIP(h); ip != nil {
			addIP(ip)
		} else {
			addDNS(h)
		}
	}
	addDNS("localhost")
	switch {
	case o.BindIP == nil:
	case o.BindIP.IsUnspecified():
		addrs, err := o.InterfaceAddrs()
		if err != nil {
			return nil, nil, fmt.Errorf("list the network interfaces: %w", err)
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
				addIP(ip)
			}
		}
		addIP(net.IPv4(127, 0, 0, 1))
		addIP(net.IPv6loopback)
	default:
		addIP(o.BindIP)
	}
	return dns, ips, nil
}

func generate(o Options, now time.Time) (Result, error) {
	dns, ips, err := sans(o)
	if err != nil {
		return Result{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Result{}, fmt.Errorf("generate the TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return Result{}, fmt.Errorf("generate the TLS certificate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pcb self-signed", Organization: []string{"proton-calendar-bridge"}},
		NotBefore:             now.Add(-time.Hour), // a client clock slightly behind still accepts it
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Result{}, fmt.Errorf("create the TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Result{}, fmt.Errorf("encode the TLS key: %w", err)
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create %s: %w", o.Dir, err)
	}
	if err := os.Chmod(o.Dir, 0o700); err != nil { // an older directory, or the umask
		return Result{}, fmt.Errorf("restrict %s: %w", o.Dir, err)
	}
	// The key first: a certificate never sits next to a key it does not match
	// for longer than a failed run, and the next start replaces such a pair.
	if err := atomicfile.Write(KeyPath(o.Dir), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return Result{}, fmt.Errorf("write the TLS key: %w", err)
	}
	if err := atomicfile.Write(CertPath(o.Dir), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return Result{}, fmt.Errorf("write the TLS certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return Result{}, fmt.Errorf("read the new TLS certificate: %w", err)
	}
	return Result{Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		Leaf: leaf, Fingerprint: Fingerprint(der)}, nil
}

// LoadPair reads a PEM certificate (chain) and its key. A missing file is
// an error wrapping fs.ErrNotExist.
func LoadPair(certPath, keyPath string) (tls.Certificate, *x509.Certificate, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, errors.New("the certificate and key do not form a valid PEM pair")
	}
	if cert.Leaf == nil { // GODEBUG x509keypairleaf=0
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return tls.Certificate{}, nil, errors.New("the certificate is unreadable")
		}
	}
	return cert, cert.Leaf, nil
}

// Inspect reads the generated certificate of dir without its key (pcb
// doctor). No certificate is an error wrapping fs.ErrNotExist.
func Inspect(dir string) (*x509.Certificate, error) {
	return ReadCertificate(CertPath(dir))
}

// ReadCertificate reads the first certificate of a PEM file.
func ReadCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New(path + " holds no PEM certificate")
		}
		if block.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, errors.New(path + " holds an unreadable certificate")
			}
			return c, nil
		}
	}
}

// Fingerprint is the SHA-256 of a certificate's DER as uppercase hex pairs
// joined by colons, the way Thunderbird's exception dialog shows it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(pairs, ":")
}
