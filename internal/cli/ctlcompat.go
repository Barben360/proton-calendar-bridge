package cli

import (
	"context"
	"errors"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// A stopgap until the CLI is account-aware: the single-account commands
// still talk to the control socket as they did, through these adapters.

// liveStatus is what pcb status shows: the login's fields flat at the top
// level as M1–M4 printed them (ruling M5-P4), then the daemon's live state.
// Daemon is nil in the offline view, with Health and Listener left out.
type liveStatus struct {
	service.Status
	Daemon   *ctl.Daemon     `json:"daemon"`
	Health   *service.Health `json:"health,omitempty"`
	Listener *ctl.Listener   `json:"listener,omitempty"`
	Alerts   []ctl.Alert     `json:"alerts"`
}

// flatStatus is the status of the daemon's first account as liveStatus
// (an empty login without one), with the process's alerts after its own.
func flatStatus(st ctl.Status) liveStatus {
	out := liveStatus{Daemon: st.Daemon, Listener: st.Listener, Alerts: []ctl.Alert{}}
	if len(st.Accounts) > 0 {
		a := st.Accounts[0]
		out.Status, out.Health = a.Login, a.Health
		for _, al := range a.Alerts {
			al.Account = ""
			out.Alerts = append(out.Alerts, al)
		}
	}
	out.Alerts = append(out.Alerts, st.Alerts...)
	ctl.SortAlerts(out.Alerts)
	return out
}

// soleAccountID is the local ID of the daemon's account: the only one, or
// else the only one that is pending.
func soleAccountID(ctx context.Context, c *ctl.Client) (string, error) {
	list, err := c.Accounts(ctx)
	if err != nil {
		return "", err
	}
	if len(list) == 1 {
		return list[0].ID, nil
	}
	var pending []string
	for _, a := range list {
		if a.Pending {
			pending = append(pending, a.ID)
		}
	}
	switch {
	case len(list) == 0:
		return "", &exitcode.Error{Code: exitcode.Usage, Err: errors.New("no account"), Hint: "run pcb login"}
	case len(pending) == 1:
		return pending[0], nil
	}
	return "", &exitcode.Error{Code: exitcode.Usage, Err: errors.New("several accounts: name one with --account"), Hint: "name one with --account"}
}

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
