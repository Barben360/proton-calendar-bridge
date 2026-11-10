package otp_test

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/otp"
)

func b32(s string) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(s))
}

func at(sec int64) time.Time { return time.Unix(sec, 0) }

func TestRFC6238Vectors(t *testing.T) {
	secrets := map[string]string{
		"SHA1":   "12345678901234567890",
		"SHA256": "12345678901234567890123456789012",
		"SHA512": "1234567890123456789012345678901234567890123456789012345678901234",
	}
	cases := []struct {
		unix int64
		want map[string]string
	}{
		{59, map[string]string{"SHA1": "94287082", "SHA256": "46119246", "SHA512": "90693936"}},
		{1111111109, map[string]string{"SHA1": "07081804", "SHA256": "68084774", "SHA512": "25091201"}},
		{20000000000, map[string]string{"SHA1": "65353130", "SHA256": "77737706", "SHA512": "47863826"}},
	}
	for _, c := range cases {
		for algo, secret := range secrets {
			uri := "otpauth://totp/x?digits=8&algorithm=" + algo + "&secret=" + b32(secret)
			o, err := otp.Parse(uri)
			if err != nil {
				t.Fatalf("%s: %v", algo, err)
			}
			if got := o.Code(at(c.unix)); got != c.want[algo] {
				t.Errorf("%s T=%d: got %s want %s", algo, c.unix, got, c.want[algo])
			}
		}
	}
}

func TestDefaultsSHA1SixDigits(t *testing.T) {
	o, err := otp.Parse("otpauth://totp/Proton:me?issuer=Proton&secret=" + b32("12345678901234567890"))
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Code(at(59)); got != "287082" {
		t.Fatalf("got %s want 287082", got)
	}
}

func TestSecretCaseInsensitiveSpacedPadded(t *testing.T) {
	s := b32("12345678901234567890")
	uri := "otpauth://totp/x?secret=" + strings.ToLower(s[:8]) + "%20" + s[8:] + "======"
	o, err := otp.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Code(at(59)); got != "287082" {
		t.Fatalf("got %s", got)
	}
}

func TestCustomPeriod(t *testing.T) {
	s := b32("12345678901234567890")
	p60, _ := otp.Parse("otpauth://totp/x?period=60&secret=" + s)
	// counter 0 for both T=0 and T=59 with a 60 s period, so codes match
	if p60.Code(at(0)) != p60.Code(at(59)) {
		t.Fatal("period=60 must keep one code across 0..59")
	}
	p30, _ := otp.Parse("otpauth://totp/x?secret=" + s)
	if p30.Code(at(0)) == p30.Code(at(59)) {
		t.Fatal("period=30 must change code between T=0 and T=59")
	}
}

func TestStaticCode(t *testing.T) {
	o, err := otp.Parse("  123456\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Code(at(12345)); got != "123456" {
		t.Fatalf("got %q", got)
	}
	if w := o.Wait(at(29)); w != 0 {
		t.Fatalf("static Wait = %v", w)
	}
}

func TestParseRejects(t *testing.T) {
	s := b32("12345678901234567890")
	bad := map[string]string{
		"empty":        "",
		"letters":      "12ab56",
		"too short":    "12345",
		"too long":     "123456789",
		"no secret":    "otpauth://totp/x?issuer=a",
		"non-base32":   "otpauth://totp/x?secret=!!!1",
		"md5":          "otpauth://totp/x?algorithm=MD5&secret=" + s,
		"digits 4":     "otpauth://totp/x?digits=4&secret=" + s,
		"digits 9":     "otpauth://totp/x?digits=9&secret=" + s,
		"period 0":     "otpauth://totp/x?period=0&secret=" + s,
		"period -1":    "otpauth://totp/x?period=-1&secret=" + s,
		"period huge":  "otpauth://totp/x?period=9223372036854775807&secret=" + s,
		"period nan":   "otpauth://totp/x?period=abc&secret=" + s,
		"hotp":         "otpauth://hotp/x?secret=" + s,
		"only padding": "otpauth://totp/x?secret=====",
	}
	for name, in := range bad {
		if o, err := otp.Parse(in); err == nil || o != nil {
			t.Errorf("%s: expected error, got %v", name, o)
		}
	}
}

func TestErrorsNeverContainInput(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXPSECRETMARK"
	inputs := []string{
		"otpauth://totp/x?algorithm=MD5&secret=" + secret,
		"otpauth://totp/x?digits=4&secret=" + secret,
		"otpauth://totp/x?period=0&secret=" + secret,
		"otpauth://totp/x?period=abc&secret=" + secret,
		"otpauth://hotp/x?secret=" + secret,
		"otpauth://totp/x?secret=" + secret + "!!",
		"otpauth://totp/%zz?secret=" + secret,
		"otpauth://totp/x?secret=" + secret + "&digits=9",
		"12ab56" + secret,
	}
	for _, in := range inputs {
		_, err := otp.Parse(in)
		if err == nil {
			t.Fatalf("expected error for %q", in)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), in) {
			t.Errorf("error leaks input: %v", err)
		}
	}
}

func TestWait(t *testing.T) {
	o, _ := otp.Parse("otpauth://totp/x?secret=" + b32("12345678901234567890"))
	cases := []struct {
		unix int64
		want time.Duration
	}{{10, 0}, {25, 0}, {26, 4 * time.Second}, {28, 2 * time.Second}, {30, 0}, {59, time.Second}}
	for _, c := range cases {
		if got := o.Wait(at(c.unix)); got != c.want {
			t.Errorf("T=%d: got %v want %v", c.unix, got, c.want)
		}
	}
	// sub-second precision
	if got := o.Wait(time.Unix(28, 500_000_000)); got != 1500*time.Millisecond {
		t.Errorf("got %v", got)
	}
}
