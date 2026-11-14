// Package store keeps pcb's secrets (sealed) and login state on disk.
package store

import "log/slog"

// Account is one Proton account's session and key material, keyed by Proton
// user ID. It is only ever stored sealed and never printed: every formatting
// verb and slog render a redacted form.
type Account struct {
	ID           string   `json:"id"`
	Username     string   `json:"username"`
	UID          string   `json:"uid"`
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	KeyPass      []byte   `json:"key_pass"` // the salted key passphrase
	Relogin      *Relogin `json:"relogin,omitempty"`
}

// Relogin is the credential kept for unattended relogin, when enabled.
type Relogin struct {
	Password string `json:"password"`
	TOTPURI  string `json:"totp_uri,omitempty"`
}

func (a Account) redacted() string {
	id := []rune(a.ID)
	if len(id) > 4 {
		id = id[:4]
	}
	return "store.Account(id=" + string(id) + "…, redacted)"
}

// String implements fmt.Stringer.
func (a Account) String() string { return a.redacted() }

// GoString implements fmt.GoStringer.
func (a Account) GoString() string { return a.redacted() }

// LogValue implements slog.LogValuer.
func (a Account) LogValue() slog.Value { return slog.StringValue(a.redacted()) }

const relogin = "store.Relogin(redacted)"

// String implements fmt.Stringer.
func (Relogin) String() string { return relogin }

// GoString implements fmt.GoStringer.
func (Relogin) GoString() string { return relogin }

// LogValue implements slog.LogValuer.
func (Relogin) LogValue() slog.Value { return slog.StringValue(relogin) }
