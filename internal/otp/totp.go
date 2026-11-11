// Package otp computes RFC 6238 time-based one-time passwords for
// non-interactive Proton logins. Errors never contain the input, which may
// hold the TOTP secret.
package otp

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; HMAC-SHA1 is sound for TOTP
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// minValidity is the least remaining validity Wait accepts for a code.
	minValidity = 5 * time.Second
	// maxPeriod bounds period (1..maxPeriod): it prevents period*time.Second
	// overflowing and a zero period dividing by zero in Code and Wait.
	maxPeriod = 366 * 24 * 3600
)

// TOTP yields the 2FA code for a login: a static code or a computed one.
type TOTP struct {
	static string // ready code; empty when secret is set

	secret []byte
	newMAC func() hash.Hash
	digits int
	period int64 // seconds
}

// Parse accepts a 6-8 digit code or an otpauth://totp/ URI with a base32
// secret, optional algorithm (SHA1, SHA256, SHA512), digits (6-8) and period
// (> 0 seconds).
func Parse(s string) (*TOTP, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), "otpauth://") {
		if len(s) < 6 || len(s) > 8 || strings.Trim(s, "0123456789") != "" {
			return nil, errors.New("otp: expected a 6-8 digit code or an otpauth://totp/ URI")
		}
		return &TOTP{static: s}, nil
	}
	u, err := url.Parse(s)
	if err != nil { // url errors quote the input: do not wrap
		return nil, errors.New("otp: malformed otpauth URI")
	}
	if !strings.EqualFold(u.Host, "totp") {
		return nil, errors.New("otp: only otpauth://totp/ URIs are supported")
	}
	q := u.Query()
	raw := strings.ToUpper(strings.TrimRight(strings.ReplaceAll(q.Get("secret"), " ", ""), "="))
	if raw == "" {
		return nil, errors.New("otp: otpauth URI has no secret")
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(raw)
	if err != nil || len(secret) == 0 {
		return nil, errors.New("otp: secret is not valid base32")
	}
	t := &TOTP{secret: secret, newMAC: sha1.New, digits: 6, period: 30}
	switch strings.ToUpper(q.Get("algorithm")) {
	case "", "SHA1":
	case "SHA256":
		t.newMAC = sha256.New
	case "SHA512":
		t.newMAC = sha512.New
	default:
		return nil, errors.New("otp: unsupported algorithm (SHA1, SHA256 or SHA512)")
	}
	if d := q.Get("digits"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 6 || n > 8 {
			return nil, errors.New("otp: digits must be 6, 7 or 8")
		}
		t.digits = n
	}
	if p := q.Get("period"); p != "" {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n <= 0 || n > maxPeriod {
			return nil, fmt.Errorf("otp: period must be between 1 and %d seconds", maxPeriod)
		}
		t.period = n
	}
	return t, nil
}

// Code returns the code valid at time now.
func (t *TOTP) Code(now time.Time) string {
	if t.secret == nil {
		return t.static
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(now.Unix()/t.period)) //nolint:gosec // non-negative after 1970
	mac := hmac.New(t.newMAC, t.secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range t.digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", t.digits, bin%mod)
}

// Wait returns how long to wait from now until Code yields a code with at
// least 5 s of validity left; 0 for a static code or enough validity now.
func (t *TOTP) Wait(now time.Time) time.Duration {
	if t.secret == nil {
		return 0
	}
	period := time.Duration(t.period) * time.Second
	left := period - time.Duration(now.UnixNano()%int64(period))
	if left < minValidity {
		return left
	}
	return 0
}

const redacted = "otp.TOTP(redacted)"

// String hides the seed and static code from fmt.
func (t *TOTP) String() string { return redacted }

// GoString hides the seed and static code from %#v.
func (t *TOTP) GoString() string { return redacted }

// LogValue hides the seed and static code from slog.
func (t *TOTP) LogValue() slog.Value { return slog.StringValue(redacted) }
