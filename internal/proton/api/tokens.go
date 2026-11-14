package api

import "log/slog"

// Tokens is one Proton session. Every formatting path redacts the secrets.
type Tokens struct {
	UID, AccessToken, RefreshToken string
}

const redacted = "api.Tokens{<redacted>}"

// String implements fmt.Stringer.
func (Tokens) String() string { return redacted }

// GoString implements fmt.GoStringer (%#v).
func (Tokens) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Tokens) LogValue() slog.Value { return slog.StringValue(redacted) }
