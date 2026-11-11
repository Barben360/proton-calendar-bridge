package tlsutil_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

var now = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func clock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

// noInterfaces fails the test if the interfaces are read: only an
// unspecified bind needs them.
func noInterfaces(t *testing.T) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		t.Error("interfaces read for a specific bind")
		return nil, nil
	}
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	slices.Sort(out)
	return out
}

func sorted(s ...string) []string { slices.Sort(s); return s }

func TestSelfSignedCertIsPersistentAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "tls")
	at := now
	o := tlsutil.Options{Dir: dir, Hostnames: []string{"pcb.lan", "10.9.8.7"}, BindIP: net.ParseIP("192.168.1.5"),
		Now: clock(&at), InterfaceAddrs: noInterfaces(t)}
	first, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Generated || first.Reason == "" {
		t.Fatalf("first start: generated=%v reason=%q, want a new certificate and why", first.Generated, first.Reason)
	}
	leaf := first.Leaf
	if got, want := sorted(leaf.DNSNames...), sorted("pcb.lan", "localhost"); !slices.Equal(got, want) {
		t.Errorf("DNS SANs = %v, want %v", got, want)
	}
	if got, want := ipStrings(leaf.IPAddresses), sorted("10.9.8.7", "192.168.1.5"); !slices.Equal(got, want) {
		t.Errorf("IP SANs = %v, want %v", got, want)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("key = %T, want ECDSA P-256", leaf.PublicKey)
	}
	if got := leaf.NotAfter.Sub(now); got < 825*24*time.Hour-time.Minute || got > 825*24*time.Hour+time.Minute {
		t.Errorf("validity from now = %v, want 825 days", got)
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		t.Errorf("ext key usage = %v, want server auth", leaf.ExtKeyUsage)
	}
	sum := sha256.Sum256(leaf.Raw)
	if want := strings.ToUpper(strings.Join(hexPairs(sum[:]), ":")); first.Fingerprint != want {
		t.Errorf("fingerprint = %q, want %q", first.Fingerprint, want)
	}
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(first.Fingerprint) {
		t.Errorf("fingerprint %q is not 32 uppercase hex pairs joined by colons", first.Fingerprint)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]fs.FileMode{dir: 0o700, tlsutil.KeyPath(dir): 0o600, tlsutil.CertPath(dir): 0o600} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != want {
				t.Errorf("%s mode %o, want %o", path, info.Mode().Perm(), want)
			}
		}
	}
	// No temporary file is left next to them (atomic writes).
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("%s holds %d entries, want cert and key only", dir, len(entries))
	}

	// Second start, a day later: the same certificate.
	at = now.Add(24 * time.Hour)
	second, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generated || second.Fingerprint != first.Fingerprint {
		t.Fatalf("second start: generated=%v fingerprint %s, want the first one %s reused", second.Generated, second.Fingerprint, first.Fingerprint)
	}
	if second.Certificate.PrivateKey == nil || len(second.Certificate.Certificate) != 1 {
		t.Fatal("the reused certificate has no key or no DER")
	}
}

func hexPairs(b []byte) []string {
	out := make([]string, len(b))
	for i, c := range b {
		out[i] = strings.ToUpper(string("0123456789abcdef"[c>>4]) + string("0123456789abcdef"[c&15]))
	}
	return out
}

func TestSelfSignedUnspecifiedBindCoversEveryInterface(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "::"} {
		t.Run(bind, func(t *testing.T) {
			addrs := func() ([]net.Addr, error) {
				return []net.Addr{
					&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
					&net.IPNet{IP: net.ParseIP("192.168.1.5"), Mask: net.CIDRMask(24, 32)},
					&net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)},
					&net.IPNet{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)},
				}, nil
			}
			res, err := tlsutil.SelfSigned(tlsutil.Options{Dir: t.TempDir(), BindIP: net.ParseIP(bind), Now: clock(&now), InterfaceAddrs: addrs})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := ipStrings(res.Leaf.IPAddresses), sorted("127.0.0.1", "192.168.1.5", "::1", "fd00::5"); !slices.Equal(got, want) {
				t.Errorf("IP SANs = %v, want %v", got, want)
			}
			if got := res.Leaf.DNSNames; !slices.Equal(got, []string{"localhost"}) {
				t.Errorf("DNS SANs = %v, want localhost", got)
			}
			for _, ip := range res.Leaf.IPAddresses {
				if ip.IsUnspecified() {
					t.Errorf("the unspecified address %s is a SAN", ip)
				}
			}
		})
	}
}

func TestSelfSignedIsRegeneratedWhenAHostnameIsNotCovered(t *testing.T) {
	dir := t.TempDir()
	o := tlsutil.Options{Dir: dir, Hostnames: []string{"pcb.lan"}, BindIP: net.ParseIP("192.168.1.5"), Now: clock(&now), InterfaceAddrs: noInterfaces(t)}
	first, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	o.Hostnames = []string{"pcb.lan", "calendar.example"}
	second, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Generated || second.Fingerprint == first.Fingerprint || !strings.Contains(second.Reason, "calendar.example") {
		t.Fatalf("generated=%v reason=%q same=%v; want a new certificate naming the uncovered host", second.Generated, second.Reason, second.Fingerprint == first.Fingerprint)
	}
	if !slices.Contains(second.Leaf.DNSNames, "calendar.example") {
		t.Errorf("DNS SANs = %v", second.Leaf.DNSNames)
	}
	// Dropping a hostname keeps the certificate: it still covers the rest.
	o.Hostnames = []string{"calendar.example"}
	third, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	if third.Generated || third.Fingerprint != second.Fingerprint {
		t.Errorf("a covered subset regenerated the certificate (reason %q)", third.Reason)
	}
}

func TestSelfSignedIsRegeneratedWithin30DaysOfExpiry(t *testing.T) {
	dir := t.TempDir()
	at := now
	o := tlsutil.Options{Dir: dir, BindIP: net.ParseIP("192.168.1.5"), Now: clock(&at), InterfaceAddrs: noInterfaces(t)}
	first, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	at = first.Leaf.NotAfter.Add(-31 * 24 * time.Hour)
	kept, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Generated {
		t.Fatalf("31 days before expiry regenerated: %q", kept.Reason)
	}
	at = first.Leaf.NotAfter.Add(-29 * 24 * time.Hour)
	renewed, err := tlsutil.SelfSigned(o)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.Generated || renewed.Fingerprint == first.Fingerprint || !strings.Contains(renewed.Reason, "expires") {
		t.Fatalf("29 days before expiry: generated=%v reason=%q", renewed.Generated, renewed.Reason)
	}
	if renewed.Leaf.NotAfter.Sub(at) < 824*24*time.Hour {
		t.Errorf("the new certificate expires %v, want 825 days after %v", renewed.Leaf.NotAfter, at)
	}
}

func TestSelfSignedReplacesAnUnreadableOrMismatchedPair(t *testing.T) {
	otherDir := t.TempDir()
	if _, err := tlsutil.SelfSigned(tlsutil.Options{Dir: otherDir, BindIP: net.ParseIP("192.168.1.5"), Now: clock(&now), InterfaceAddrs: noInterfaces(t)}); err != nil {
		t.Fatal(err)
	}
	otherKey, err := os.ReadFile(tlsutil.KeyPath(otherDir))
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string][]byte{"garbage": []byte("garbage"), "another pair's key": otherKey} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			o := tlsutil.Options{Dir: dir, BindIP: net.ParseIP("192.168.1.5"), Now: clock(&now), InterfaceAddrs: noInterfaces(t)}
			first, err := tlsutil.SelfSigned(o)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tlsutil.KeyPath(dir), key, 0o600); err != nil {
				t.Fatal(err)
			}
			res, err := tlsutil.SelfSigned(o)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Generated || res.Fingerprint == first.Fingerprint {
				t.Fatalf("an unusable pair was reused (generated=%v)", res.Generated)
			}
		})
	}
}

func TestInspectAndCovers(t *testing.T) {
	dir := t.TempDir()
	if _, err := tlsutil.Inspect(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Inspect(no cert) = %v, want fs.ErrNotExist", err)
	}
	res, err := tlsutil.SelfSigned(tlsutil.Options{Dir: dir, Hostnames: []string{"pcb.lan"}, BindIP: net.ParseIP("192.168.1.5"), Now: clock(&now), InterfaceAddrs: noInterfaces(t)})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tlsutil.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tlsutil.Fingerprint(leaf.Raw) != res.Fingerprint {
		t.Error("Inspect read another certificate")
	}
	if missing := tlsutil.Uncovered(leaf, []string{"pcb.lan", "192.168.1.5"}); missing != "" {
		t.Errorf("Uncovered = %q, want none", missing)
	}
	if missing := tlsutil.Uncovered(leaf, []string{"pcb.lan", "other.lan"}); missing != "other.lan" {
		t.Errorf("Uncovered = %q, want other.lan", missing)
	}
}

func TestLoadPairReadsAUserCertificate(t *testing.T) {
	src := t.TempDir()
	res, err := tlsutil.SelfSigned(tlsutil.Options{Dir: src, BindIP: net.ParseIP("192.168.1.5"), Now: clock(&now), InterfaceAddrs: noInterfaces(t)})
	if err != nil {
		t.Fatal(err)
	}
	cert, leaf, err := tlsutil.LoadPair(tlsutil.CertPath(src), tlsutil.KeyPath(src))
	if err != nil {
		t.Fatal(err)
	}
	if tlsutil.Fingerprint(leaf.Raw) != res.Fingerprint || cert.PrivateKey == nil {
		t.Fatal("LoadPair returned another certificate")
	}
	if _, _, err := tlsutil.LoadPair(filepath.Join(src, "nope.pem"), tlsutil.KeyPath(src)); err == nil {
		t.Fatal("LoadPair of a missing file succeeded")
	}
}
