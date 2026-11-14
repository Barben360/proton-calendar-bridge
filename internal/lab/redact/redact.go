// Package redact strips secrets from JSON before it is recorded into files
// that are committed to the repository.
//
// It works on the parsed JSON tree, never line by line: a string value that
// spans several lines (PGP armor) is one value. Anything that is not exactly
// one valid JSON document is refused rather than guessed at.
package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// secretKeys are matched case-insensitively, at any depth. The whole value
// (string, number, object or array) is replaced.
var secretKeys = map[string]bool{}

// secretFragments: any key whose lowercased name contains one of these is
// secret (fail closed: RefreshTokenExpiry, SrpSalt, ...).
var secretFragments = []string{
	"token", "secret", "pass", "proof", "salt", "cookie", "signature",
	"armored", "keypacket", "sessionkey", "privatekey",
}

func isSecretKey(key string) bool {
	l := strings.ToLower(key)
	if secretKeys[l] {
		return true
	}
	if l == "code" || l == "error" || keepsByKey(key) {
		return false
	}
	for _, f := range secretFragments {
		if strings.Contains(l, f) {
			return true
		}
	}
	return false
}

func init() {
	for _, k := range []string{
		"AccessToken", "RefreshToken", "UID", "ServerProof", "ClientProof",
		"ClientEphemeral", "SRPSession", "Salt", "KeySalt", "PrivateKey",
		"Passphrase", "Token", "HumanVerificationToken", "SessionKey",
		"KeyPacket", "SharedKeyPacket", "CalendarKeyPacket", "Data",
		"Signature", "Armored", "TOTP", "Password", "Email", "Address",
		"Username", "TwoFactorCode", "Secret", "Cookie", "Author",
	} {
		secretKeys[strings.ToLower(k)] = true
	}
}

var (
	armorRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----`)
	jwtRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*$`)
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9+/_=-]{20,}$`)
)

// JSON returns raw with secrets replaced by "<redacted:len=N>", re-encoded
// indented with sorted keys. It errors (and returns nil) unless raw is exactly
// one valid JSON value.
func JSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("redact: not valid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("redact: trailing data after the JSON value")
	}
	tree = walk(tree, "", false)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(tree); err != nil {
		return nil, fmt.Errorf("redact: encode: %w", err)
	}
	return buf.Bytes(), nil
}

func walk(v any, key string, hasKey bool) any {
	if hasKey && isSecretKey(key) {
		return redactWhole(v)
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = walk(child, k, true)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			// Array elements carry no key of their own; an array under a
			// secret key was already replaced whole above.
			out[i] = walk(child, "", false)
		}
		return out
	case string:
		if hasKey && keepsByKey(key) && !armorRe.MatchString(t) && !jwtRe.MatchString(t) {
			return t // R10: the key rule wins over the base64 heuristic
		}
		if looksSecret(t) {
			return placeholder(utf8.RuneCountInString(t))
		}
	}
	return v
}

// keepsByKey: keys ending in "ID" (calendar, event, member IDs...) other than
// UID, which is a session secret.
func keepsByKey(key string) bool {
	if strings.EqualFold(key, "id") {
		return true
	}
	if strings.HasSuffix(strings.ToLower(key), "uid") {
		return false
	}
	return strings.HasSuffix(key, "ID")
}

func looksSecret(s string) bool {
	if armorRe.MatchString(s) || jwtRe.MatchString(s) {
		return true
	}
	// Line-wrapped base64 is still one value: test it without whitespace.
	compact := strings.Join(strings.Fields(s), "")
	return tokenRe.MatchString(compact)
}

func redactWhole(v any) any {
	switch t := v.(type) {
	case nil:
		return nil // absence leaks nothing and is useful evidence
	case string:
		return placeholder(utf8.RuneCountInString(t))
	default:
		b, _ := json.Marshal(t)
		return placeholder(len(b))
	}
}

func placeholder(n int) string { return fmt.Sprintf("<redacted:len=%d>", n) }
