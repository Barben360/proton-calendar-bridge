package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// Refresh exchanges stale for new tokens on the same UID; it fits
// api.Options.Refresh. Proton refusing the refresh token (a 4xx other than
// 429) wraps api.ErrSessionEnded; any other failure is transient.
func Refresh(ctx context.Context, m *gpa.Manager, stale api.Tokens) (api.Tokens, error) {
	c, a, err := m.NewClientWithRefresh(ctx, stale.UID, stale.RefreshToken)
	if err != nil {
		perr := protonError(http.MethodPost, "/auth/v4/refresh", err)
		var ge *gpa.APIError
		if errors.As(err, &ge) && ge.Status >= 400 && ge.Status < 500 && ge.Status != http.StatusTooManyRequests {
			return api.Tokens{}, fmt.Errorf("%w: %w", api.ErrSessionEnded, perr)
		}
		return api.Tokens{}, perr
	}
	c.Close()
	if a.AccessToken == "" || a.RefreshToken == "" {
		return api.Tokens{}, errors.New("proton refresh: the answer has no tokens")
	}
	return api.Tokens{UID: stale.UID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken}, nil
}
