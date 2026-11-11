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
	got := doctor.ClientsCheck{StateDir: state}.Run(ctx)
	if got.Status != doctor.StatusWarn || got.Fix != "pcb client add NAME" {
		t.Errorf("no client: %+v", got)
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
