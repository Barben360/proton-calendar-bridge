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
// server's proof; each of its two calls has callTimeout. A wrong password
// (422, code 8002) is ErrBadCredentials; a mismatching, empty or absent proof
// is ErrServerProof, after a Lock whose failure is joined to it.
func (s Scope) Unlock(ctx context.Context) error {
	ictx, cancel := callContext(ctx)
	info, err := s.M.AuthInfo(ictx, gpa.AuthInfoReq{Username: s.Username})
	cancel()
	if err != nil {
		perr := protonError(ictx, http.MethodPost, "/auth/v4/info", err)
		if wrongPassword(perr) {
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
	pctx, cancel := callContext(ctx)
	err = s.C.Put(pctx, "/core/v4/users/unlock", req, &out)
	cancel()
	if err != nil {
		if wrongPassword(err) {
			return fmt.Errorf("%w: %w", ErrBadCredentials, err)
		}
		return fmt.Errorf("unlock: %w", err)
	}
	got, err := base64.StdEncoding.DecodeString(out.ServerProof)
	if err != nil || len(got) == 0 || subtle.ConstantTimeCompare(got, proofs.ExpectedServerProof) != 1 {
		// The server may have granted the scope anyway: drop it, even when
		// ctx is cancelled.
		lctx, cancel := cleanupContext(ctx)
		defer cancel()
		if lerr := s.Lock(lctx); lerr != nil {
			return errors.Join(ErrServerProof, fmt.Errorf("re-locking: %w", lerr))
		}
		return ErrServerProof
	}
	return nil
}

// wrongPassword reports Proton's answer to a wrong SRP proof: HTTP 422 with
// code 8002 (ruling R6m: nothing looser).
func wrongPassword(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Status == http.StatusUnprocessableEntity && e.Code == codePasswordWrong
}

// Lock drops the locked scope (PUT /core/v4/users/lock).
func (s Scope) Lock(ctx context.Context) error {
	return s.C.Put(ctx, "/core/v4/users/lock", struct{}{}, nil)
}
