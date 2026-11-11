package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/go-srp"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// Scope gains and drops the "locked" scope of C's session (needed by the key
// salts, Proton code 9101) by proving the login password over SRP.
type Scope struct {
	M        *gpa.Manager
	C        *api.Client
	Username string
	Password []byte // the login password, also in two-password mode
}

const redactedScope = "auth.Scope{<redacted>}"

// String implements fmt.Stringer.
func (Scope) String() string { return redactedScope }

// GoString implements fmt.GoStringer (%#v).
func (Scope) GoString() string { return redactedScope }

// LogValue implements slog.LogValuer.
func (Scope) LogValue() slog.Value { return slog.StringValue(redactedScope) }

// Unlock sends an SRP proof to PUT /core/v4/users/unlock and checks the
// server's proof. A wrong password is ErrBadCredentials; a mismatching proof
// is ErrServerProof, after a best-effort Lock.
func (s Scope) Unlock(ctx context.Context) error {
	info, err := s.M.AuthInfo(ctx, gpa.AuthInfoReq{Username: s.Username})
	if err != nil {
		perr := protonError(http.MethodPost, "/auth/v4/info", err)
		if api.IsCode(perr, codePasswordWrong) {
			return fmt.Errorf("%w: %w", ErrBadCredentials, perr)
		}
		return fmt.Errorf("unlock: %w", perr)
	}
	sa, err := srp.NewAuth(info.Version, s.Username, s.Password, info.Salt, info.Modulus, info.ServerEphemeral)
	if err != nil {
		return errors.New("unlock: cannot prepare the SRP proof")
	}
	proofs, err := sa.GenerateProofs(2048)
	if err != nil {
		return errors.New("unlock: cannot compute the SRP proof")
	}
	req := struct{ ClientEphemeral, ClientProof, SRPSession string }{
		base64.StdEncoding.EncodeToString(proofs.ClientEphemeral),
		base64.StdEncoding.EncodeToString(proofs.ClientProof),
		info.SRPSession,
	}
	var out struct{ ServerProof string }
	if err := s.C.Put(ctx, "/core/v4/users/unlock", req, &out); err != nil {
		var e *api.Error
		if errors.As(err, &e) && (e.Code == codePasswordWrong || e.Status == http.StatusUnprocessableEntity) {
			return fmt.Errorf("%w: %w", ErrBadCredentials, err)
		}
		return fmt.Errorf("unlock: %w", err)
	}
	got, err := base64.StdEncoding.DecodeString(out.ServerProof)
	if err != nil || len(got) == 0 || subtle.ConstantTimeCompare(got, proofs.ExpectedServerProof) != 1 {
		_ = s.Lock(ctx) // the server may have granted the scope anyway
		return ErrServerProof
	}
	return nil
}

// Lock drops the locked scope (PUT /core/v4/users/lock).
func (s Scope) Lock(ctx context.Context) error {
	return s.C.Put(ctx, "/core/v4/users/lock", struct{}{}, nil)
}
