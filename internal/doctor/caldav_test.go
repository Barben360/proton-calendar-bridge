package doctor_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

var day0 = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func noIfaces() ([]net.Addr, error) { return nil, nil }

// selfSigned generates the certificate pcb serve would in dir, at day0.
func selfSigned(t *testing.T, dir string, hostnames ...string) tlsutil.Result {
	t.Helper()
	res, err := tlsutil.SelfSigned(tlsutil.Options{Dir: dir, Hostnames: hostnames, BindIP: net.ParseIP("192.168.1.5"),
		Now: func() time.Time { return day0 }, InterfaceAddrs: noIfaces})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestListenerCheck(t *testing.T) {
	ctx := context.Background()
	got := doctor.ListenerCheck{CalDAV: config.CalDAV{Listen: "127.0.0.1:1080"}}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, "127.0.0.1:1080") || !strings.Contains(got.Message, "plain HTTP") {
		t.Errorf("loopback: %+v", got)
	}
	got = doctor.ListenerCheck{CalDAV: config.CalDAV{Listen: "0.0.0.0:8443"}}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, "TLS") || !strings.Contains(got.Message, "self-signed") {
		t.Errorf("every interface: %+v", got)
	}
	got = doctor.ListenerCheck{CalDAV: config.CalDAV{Listen: "1080"}}.Run(ctx)
	if got.Status != doctor.StatusError || got.Fix == "" {
		t.Errorf("invalid listen: %+v", got)
	}
	dir := t.TempDir()
	selfSigned(t, dir)
	got = doctor.ListenerCheck{CalDAV: config.CalDAV{Listen: "127.0.0.1:1080", TLSCert: tlsutil.CertPath(dir), TLSKey: tlsutil.KeyPath(dir)}}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, "TLS") || !strings.Contains(got.Message, "tls_cert") {
		t.Errorf("user certificate on loopback: %+v", got)
	}
}

// M5-R15 C4: with a daemon answering, the CalDAV checks report its
// listener (a --listen override, a config edited since it started), not
// what config.toml predicts.
func TestCalDAVChecksPreferTheDaemonsListener(t *testing.T) {
	ctx := context.Background()
	tlsDir := filepath.Join(t.TempDir(), "tls")
	res := selfSigned(t, tlsDir)
	loopback := config.CalDAV{Listen: "127.0.0.1:1080"}
	at := func() time.Time { return day0 }
	live := &doctor.Live{Addr: "0.0.0.0:1080", URL: "https://192.168.1.5:1080/", TLS: true, Fingerprint: res.Fingerprint, NotAfter: &res.Leaf.NotAfter}

	got := doctor.ListenerCheck{CalDAV: loopback, Live: live}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, "0.0.0.0:1080") || !strings.Contains(got.Message, "TLS") ||
		!strings.Contains(got.Message, "running daemon") || strings.Contains(got.Message, "plain HTTP") {
		t.Errorf("listener: %+v", got)
	}
	got = doctor.CertificateCheck{CalDAV: loopback, TLSDir: tlsDir, Now: at, Live: live}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, res.Fingerprint) {
		t.Errorf("certificate the daemon serves: %+v", got)
	}

	// The daemon serves another certificate than the one on disk.
	other := *live
	other.Fingerprint = "AB:CD"
	got = doctor.CertificateCheck{CalDAV: loopback, TLSDir: tlsDir, Now: at, Live: &other}.Run(ctx)
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, "AB:CD") || !strings.Contains(got.Fix, "restart") {
		t.Errorf("another certificate: %+v", got)
	}
	// None on disk (another state dir): the daemon's own facts.
	got = doctor.CertificateCheck{CalDAV: loopback, TLSDir: filepath.Join(t.TempDir(), "tls"), Now: at, Live: &other}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, "AB:CD") || !strings.Contains(got.Message, "running daemon") {
		t.Errorf("no certificate on disk: %+v", got)
	}

	// A daemon on plain HTTP while the config says TLS.
	plain := &doctor.Live{Addr: "127.0.0.1:1080", URL: "http://localhost:1080/"}
	remote := config.CalDAV{Listen: "0.0.0.0:8443"}
	got = doctor.ListenerCheck{CalDAV: remote, Live: plain}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, "127.0.0.1:1080, plain HTTP") {
		t.Errorf("plain listener: %+v", got)
	}
	if got := (doctor.CertificateCheck{CalDAV: remote, TLSDir: tlsDir, Now: at, Live: plain}).Run(ctx); got.Status != doctor.StatusSkip {
		t.Errorf("plain certificate: %+v", got)
	}
}

func TestCertificateCheckSelfSigned(t *testing.T) {
	ctx := context.Background()
	remote := config.CalDAV{Listen: "0.0.0.0:8443", Hostnames: []string{"pcb.lan"}}
	at := func(d time.Time) func() time.Time { return func() time.Time { return d } }

	t.Run("plain HTTP needs none", func(t *testing.T) {
		got := doctor.CertificateCheck{CalDAV: config.CalDAV{Listen: "127.0.0.1:1080"}, TLSDir: t.TempDir(), Now: at(day0)}.Run(ctx)
		if got.Status != doctor.StatusSkip {
			t.Errorf("%+v", got)
		}
	})
	t.Run("not generated yet", func(t *testing.T) {
		got := doctor.CertificateCheck{CalDAV: remote, TLSDir: filepath.Join(t.TempDir(), "tls"), Now: at(day0)}.Run(ctx)
		if got.Status != doctor.StatusSkip || !strings.Contains(got.Message, "pcb serve") {
			t.Errorf("%+v", got)
		}
	})
	t.Run("valid shows the fingerprint", func(t *testing.T) {
		dir := t.TempDir()
		res := selfSigned(t, dir, "pcb.lan")
		got := doctor.CertificateCheck{CalDAV: remote, TLSDir: dir, Now: at(day0)}.Run(ctx)
		if got.Status != doctor.StatusOK || !strings.Contains(got.Message, res.Fingerprint) {
			t.Errorf("%+v, want ok with %s", got, res.Fingerprint)
		}
	})
	t.Run("under 30 days warns", func(t *testing.T) {
		dir := t.TempDir()
		res := selfSigned(t, dir, "pcb.lan")
		check := doctor.CertificateCheck{CalDAV: remote, TLSDir: dir, Now: at(res.Leaf.NotAfter.Add(-31 * 24 * time.Hour))}
		if got := check.Run(ctx); got.Status != doctor.StatusOK {
			t.Errorf("31 days left: %+v", got)
		}
		check.Now = at(res.Leaf.NotAfter.Add(-29 * 24 * time.Hour))
		got := check.Run(ctx)
		if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, res.Fingerprint) || !strings.Contains(got.Message, "expires") {
			t.Errorf("29 days left: %+v", got)
		}
	})
	t.Run("an uncovered hostname warns", func(t *testing.T) {
		dir := t.TempDir()
		selfSigned(t, dir, "pcb.lan")
		cc := remote
		cc.Hostnames = []string{"pcb.lan", "calendar.example"}
		got := doctor.CertificateCheck{CalDAV: cc, TLSDir: dir, Now: at(day0)}.Run(ctx)
		if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, "calendar.example") {
			t.Errorf("%+v", got)
		}
	})
	t.Run("a readable key is an error", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no Unix modes")
		}
		dir := t.TempDir()
		selfSigned(t, dir, "pcb.lan")
		if err := os.Chmod(tlsutil.KeyPath(dir), 0o644); err != nil {
			t.Fatal(err)
		}
		got := doctor.CertificateCheck{CalDAV: remote, TLSDir: dir, Now: at(day0)}.Run(ctx)
		if got.Status != doctor.StatusError || !strings.Contains(got.Fix, "chmod 600") {
			t.Errorf("%+v", got)
		}
	})
}

func TestCertificateCheckUserCertificate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	res := selfSigned(t, dir, "pcb.example")
	cc := config.CalDAV{Listen: "127.0.0.1:1080", Hostnames: []string{"pcb.example"}, TLSCert: tlsutil.CertPath(dir), TLSKey: tlsutil.KeyPath(dir)}
	stateTLS := filepath.Join(t.TempDir(), "tls")
	got := doctor.CertificateCheck{CalDAV: cc, TLSDir: stateTLS, Now: func() time.Time { return day0 }}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, res.Fingerprint) || !strings.Contains(got.Message, "tls_cert") {
		t.Errorf("valid: %+v", got)
	}
	got = doctor.CertificateCheck{CalDAV: cc, TLSDir: stateTLS, Now: func() time.Time { return res.Leaf.NotAfter.Add(-time.Hour) }}.Run(ctx)
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, res.Fingerprint) {
		t.Errorf("expiring: %+v", got)
	}
	cc.TLSKey = filepath.Join(dir, "missing.pem")
	if got := (doctor.CertificateCheck{CalDAV: cc, TLSDir: stateTLS, Now: func() time.Time { return day0 }}).Run(ctx); got.Status != doctor.StatusError {
		t.Errorf("missing key: %+v", got)
	}
}

func TestClientsCheck(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	// Not logged in: the login creates the client default (decision 0013).
	got := doctor.ClientsCheck{StateDir: state}.Run(ctx)
	if got.Status != doctor.StatusWarn || got.Fix != "pcb login (it creates the client default), then pcb client show" {
		t.Errorf("no client, logged out: %+v", got)
	}
	// Logged in with no client (all removed): only client add makes one.
	if err := store.SaveState(state, store.State{Phase: store.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	got = doctor.ClientsCheck{StateDir: state}.Run(ctx)
	if got.Status != doctor.StatusWarn || got.Fix != "pcb client add NAME" {
		t.Errorf("no client, logged in: %+v", got)
	}
	if _, _, err := access.NewStore(state, nil).Add(ctx, "Thunderbird"); err != nil {
		t.Fatal(err)
	}
	got = doctor.ClientsCheck{StateDir: state}.Run(ctx)
	if got.Status != doctor.StatusOK || !strings.Contains(got.Message, "1 client") {
		t.Errorf("one client: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(state, "clients.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (doctor.ClientsCheck{StateDir: state}).Run(ctx); got.Status != doctor.StatusError {
		t.Errorf("unreadable list: %+v", got)
	}
}

func TestBindResolvesOnceWithATimeoutAndListensFamily(t *testing.T) {
	calls := 0
	var budget time.Duration
	b := &doctor.Bind{Listen: "pcb.lan:8443", Lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
		calls++
		if d, ok := ctx.Deadline(); ok {
			budget = time.Until(d)
		}
		if host != "pcb.lan" {
			t.Errorf("looked up %q", host)
		}
		// net.Listen("tcp", …) binds the first IPv4 address.
		return []net.IPAddr{{IP: net.ParseIP("fd00::7")}, {IP: net.ParseIP("192.168.1.5")}}, nil
	}}
	cc := config.CalDAV{Listen: b.Listen}
	ctx := context.Background()
	lr := doctor.ListenerCheck{CalDAV: cc, Bind: b}.Run(ctx)
	cr := doctor.CertificateCheck{CalDAV: cc, TLSDir: filepath.Join(t.TempDir(), "tls"), Bind: b}.Run(ctx)
	if calls != 1 {
		t.Errorf("%d lookups in one doctor run, want 1", calls)
	}
	if budget <= 2*time.Second || budget > 3*time.Second {
		t.Errorf("lookup budget %v, want 3 s", budget)
	}
	if ip, err := b.IP(ctx); err != nil || !ip.Equal(net.ParseIP("192.168.1.5")) {
		t.Errorf("IP = %v, %v; want the IPv4 address net.Listen binds", ip, err)
	}
	if lr.Status != doctor.StatusOK || cr.Status != doctor.StatusSkip {
		t.Errorf("listener %+v, certificate %+v", lr, cr)
	}
}

func TestBindLookupEndsWithTheContext(t *testing.T) {
	blocking := func(ctx context.Context, _ string) ([]net.IPAddr, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := doctor.ListenerCheck{CalDAV: config.CalDAV{Listen: "pcb.lan:8443"}, Bind: &doctor.Bind{Listen: "pcb.lan:8443", Lookup: blocking}}.Run(ctx)
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, "cannot resolve pcb.lan") {
		t.Errorf("cancelled run: %+v", got)
	}
	b := &doctor.Bind{Listen: "pcb.lan:8443", Lookup: blocking, Timeout: 10 * time.Millisecond}
	if _, err := b.IP(context.Background()); err == nil {
		t.Error("a lookup past its timeout succeeded")
	}
}

func TestCertificateCheckBindIPAndDirectoryMode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	selfSigned(t, dir) // covers 192.168.1.5
	moved := config.CalDAV{Listen: "10.0.0.7:8443"}
	got := doctor.CertificateCheck{CalDAV: moved, TLSDir: dir, Now: func() time.Time { return day0 }}.Run(ctx)
	if got.Status != doctor.StatusWarn || !strings.Contains(got.Message, "10.0.0.7") {
		t.Errorf("uncovered bind IP: %+v", got)
	}
	same := config.CalDAV{Listen: "192.168.1.5:8443"}
	if got := (doctor.CertificateCheck{CalDAV: same, TLSDir: dir, Now: func() time.Time { return day0 }}).Run(ctx); got.Status != doctor.StatusOK {
		t.Errorf("covered bind IP: %+v", got)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got = doctor.CertificateCheck{CalDAV: same, TLSDir: dir, Now: func() time.Time { return day0 }}.Run(ctx)
	if got.Status != doctor.StatusError || !strings.Contains(got.Fix, "chmod 700") {
		t.Errorf("directory 0755: %+v", got)
	}
}
