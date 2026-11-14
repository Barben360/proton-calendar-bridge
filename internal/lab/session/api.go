package session

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
)

// API returns pcb's own transport (internal/proton/api) on the saved
// session's tokens, identified as pcb-lab. A rejected access token is
// refreshed once with the saved pair (its access token in the body) and the
// rotated pair saved, as Resume's client does; a refused refresh ends the
// session (api.ErrSessionEnded). It never logs in. hc is the HTTP client and
// lim the request pacing (nil: the transport's defaults, one request per
// 250 ms after a burst of 8).
func (s *Session) API(hc *http.Client, lim *rate.Limiter) (*api.Client, error) {
	st, err := load(s.opts.StateDir)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(s.opts.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	app := s.opts.AppVersion
	if app == "" {
		app = DefaultAppVersion
	}
	o := s.opts
	return api.New(api.Tokens{UID: st.UID, AccessToken: st.AccessToken, RefreshToken: st.RefreshToken}, api.Options{
		BaseURL: base, AppVersion: app, UserAgent: "pcb-lab/" + buildinfo.Version, HTTP: hc, Limiter: lim,
		Refresh: func(ctx context.Context, stale api.Tokens) (api.Tokens, error) {
			r, nb, err := refreshOnce(ctx, o, stale.UID, stale.RefreshToken, stale.AccessToken)
			if err != nil {
				return api.Tokens{}, err
			}
			if r.Status != http.StatusOK || nb.AccessToken == "" {
				return api.Tokens{}, fmt.Errorf("%w: refresh refused: HTTP %d, Proton code %d", api.ErrSessionEnded, r.Status, r.Code)
			}
			forbid(o, nb)
			if err := saveRotation(o.StateDir, &st, authOf(nb)); err != nil {
				o.warn(err)
			}
			return api.Tokens{UID: stale.UID, AccessToken: st.AccessToken, RefreshToken: st.RefreshToken}, nil
		},
	}), nil
}

// Elevator gains and drops the locked scope of c's session (for the key
// salts) by an SRP proof of the password on PUT /core/v4/users/unlock: no new
// session, no login.
func (s *Session) Elevator(c *api.Client, username string, password []byte) keys.Elevator {
	return auth.Scope{M: s.manager, C: c, Username: username, Password: password}
}
