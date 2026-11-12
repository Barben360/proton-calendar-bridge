package cli

import (
	"context"
	"errors"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// apiHub lets the serve tests run a stub service as the one account of a
// Hub (see APIHub); pcb serve itself opens a real Hub.

// singleAccountID is the local ID apiHub gives its account.
const singleAccountID = "00000000"

// apiHub is a ctl.Hub of one account: a service.API.
type apiHub struct{ svc service.API }

var _ ctl.Hub = apiHub{}

var errOneAccount = &exitcode.Error{Code: exitcode.Usage, Err: errors.New("this pcb serves one account"), Hint: "use the pcb of the same version as the daemon"}

func (h apiHub) selected(selector string) error {
	if selector != "" && selector != singleAccountID {
		return &exitcode.Error{Code: exitcode.Usage, Err: errors.New("no account " + selector), Hint: "see pcb account list"}
	}
	return nil
}

func (h apiHub) info(ctx context.Context) (service.AccountInfo, error) {
	st, err := h.svc.Status(ctx)
	return service.AccountInfo{ID: singleAccountID, Address: st.Address, Phase: st.Phase, CalDAV: true}, err
}

func (h apiHub) Accounts() []service.AccountInfo {
	info, _ := h.info(context.Background())
	return []service.AccountInfo{info}
}

func (h apiHub) Account(selector string) (service.API, service.AccountInfo, error) {
	if err := h.selected(selector); err != nil {
		return nil, service.AccountInfo{}, err
	}
	info, err := h.info(context.Background())
	return h.svc, info, err
}

func (h apiHub) Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error) {
	res, err := h.svc.Login(ctx, req)
	res.Account = singleAccountID
	return res, err
}

func (h apiHub) SubmitTOTP(ctx context.Context, selector, code string) (service.LoginResult, error) {
	if err := h.selected(selector); err != nil {
		return service.LoginResult{}, err
	}
	res, err := h.svc.SubmitTOTP(ctx, code)
	res.Account = singleAccountID
	return res, err
}

func (h apiHub) SignOut(ctx context.Context, selector string, req service.LogoutRequest) (service.LogoutResult, error) {
	if err := h.selected(selector); err != nil {
		return service.LogoutResult{}, err
	}
	return h.svc.Logout(ctx, req)
}

func (h apiHub) Health(ctx context.Context, selector string) (service.Health, error) {
	if err := h.selected(selector); err != nil {
		return service.Health{}, err
	}
	return h.svc.Health(ctx)
}

func (apiHub) Remove(context.Context, string, service.LogoutRequest) (service.AccountInfo, error) {
	return service.AccountInfo{}, errOneAccount
}

func (apiHub) SetCalDAV(context.Context, string, bool) (service.AccountInfo, error) {
	return service.AccountInfo{}, errOneAccount
}

func (apiHub) Restart(context.Context, string) (service.AccountInfo, error) {
	return service.AccountInfo{}, errOneAccount
}
