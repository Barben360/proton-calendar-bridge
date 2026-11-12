package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"weak"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/go-resty/resty/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

const refreshPath = "/auth/v4/refresh"

// Refresh exchanges stale for new tokens on the same UID; it fits
// api.Options.Refresh. m must come from NewManager. The request carries
// stale's access token: Proton refuses a refresh token issued by a refresh
// without it (400, code 10013; measured live 2026-11-12, E01). Proton
// refusing the refresh token (HTTP 401 or 422, or code 10013) wraps
// api.ErrSessionEnded; any other failure is a plain error that leaves the
// session alone (ruling R6m).
func Refresh(ctx context.Context, m *gpa.Manager, stale api.Tokens) (api.Tokens, error) {
	if !built(m) {
		return api.Tokens{}, errors.New("proton refresh: the manager does not come from auth.NewManager, nothing sent")
	}
	ctx = context.WithValue(ctx, refreshAccess{}, stale.AccessToken)
	c, a, err := m.NewClientWithRefresh(ctx, stale.UID, stale.RefreshToken)
	if err != nil {
		perr := protonError(ctx, http.MethodPost, refreshPath, err)
		var ge *gpa.APIError
		if errors.As(err, &ge) && (ge.Status == http.StatusUnauthorized || ge.Status == http.StatusUnprocessableEntity || int(ge.Code) == api.CodeRefreshInvalid) {
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

// refreshAccess keys, in Refresh's context, the access token refreshHook puts
// in the request body.
type refreshAccess struct{}

// refreshHook is the request hook NewManager installs. go-proton-api's
// exported refresh (Manager.NewClientWithRefresh) sends no access token; its
// client's own refresh after a 401 does (Client.authRefresh, MIT), through the
// same request body, AuthRefreshReq. The hook sets that field on Refresh's
// request only; every other request passes untouched. resty runs it before
// encoding the body. A refresh body of another shape is not sent: without the
// access token Proton would refuse a refreshed session's token.
func refreshHook(_ *resty.Client, r *resty.Request) error {
	acc, ok := r.Context().Value(refreshAccess{}).(string)
	if !ok || r.Method != http.MethodPost || r.URL != refreshPath {
		return nil
	}
	body, ok := r.Body.(gpa.AuthRefreshReq)
	if !ok {
		return errors.New("proton refresh: go-proton-api's refresh body changed shape, nothing sent")
	}
	body.AccessToken = acc
	r.Body = body
	return nil
}

// managers holds the managers NewManager built, which carry refreshHook,
// without keeping them alive.
var managers sync.Map // weak.Pointer[gpa.Manager] → struct{}

func hook(m *gpa.Manager) {
	m.AddPreRequestHook(refreshHook)
	p := weak.Make(m)
	managers.Store(p, struct{}{})
	runtime.AddCleanup(m, func(p weak.Pointer[gpa.Manager]) { managers.Delete(p) }, p)
}

func built(m *gpa.Manager) bool {
	_, ok := managers.Load(weak.Make(m))
	return ok
}
