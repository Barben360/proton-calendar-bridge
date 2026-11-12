package cli_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// stubAccountID is the local ID of a stubHub's first account; the next
// ones count up (00000001, ...).
const stubAccountID = "00000000"

// stubAccount is one account of a stubHub: a stub service.
type stubAccount struct {
	id, address string // address "": the service's Status says it
	api         service.API
	pending     bool
	caldavOff   bool
}

// stubHub is a cli.Hub over stub services, one per account, choosing among
// them as service.Hub does; it records the account-changing calls (by
// local ID) and its closes. pcb serve serves its first account.
type stubHub struct {
	mu       sync.Mutex
	accounts []*stubAccount
	changes  []string // "signout ID", "remove ID", "caldav ID on|off", "restart ID", "totp ID", "login USERNAME"
	closed   int
}

var _ cli.Hub = (*stubHub)(nil)

// newStubHub is a stubHub holding one account per api.
func newStubHub(apis ...service.API) *stubHub {
	h := &stubHub{}
	for i, api := range apis {
		h.accounts = append(h.accounts, &stubAccount{id: "0000000" + string(rune('0'+i)), api: api})
	}
	return h
}

func (h *stubHub) record(change string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, change)
}

// calls is what the account-changing calls were, in order.
func (h *stubHub) calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.changes...)
}

func (h *stubHub) closes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *stubHub) info(a *stubAccount) service.AccountInfo {
	st, _ := a.api.Status(context.Background())
	address := a.address
	if address == "" {
		address = st.Address
	}
	return service.AccountInfo{ID: a.id, Address: address, Phase: st.Phase, Pending: a.pending, CalDAV: !a.caldavOff}
}

func (h *stubHub) list() []*stubAccount {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*stubAccount(nil), h.accounts...)
}

func (h *stubHub) Accounts() []service.AccountInfo {
	var out []service.AccountInfo
	for _, a := range h.list() {
		out = append(out, h.info(a))
	}
	return out
}

func noAccount(selector string) error {
	if selector == "" {
		return &exitcode.Error{Code: exitcode.Usage, Err: errors.New("no account"), Hint: "add one with pcb login"}
	}
	return &exitcode.Error{Code: exitcode.Usage, Err: errors.New("no account " + selector), Hint: "see pcb account list"}
}

// pick chooses as service.Hub.Account does.
func (h *stubHub) pick(selector string) (*stubAccount, error) {
	list := h.list()
	if selector != "" {
		for _, a := range list {
			if a.id == selector || strings.EqualFold(h.info(a).Address, selector) {
				return a, nil
			}
		}
		return nil, noAccount(selector)
	}
	var ready []*stubAccount
	for _, a := range list {
		if !a.pending {
			ready = append(ready, a)
		}
	}
	switch len(ready) {
	case 0:
		return nil, noAccount("")
	case 1:
		return ready[0], nil
	}
	infos := make([]service.AccountInfo, 0, len(ready))
	for _, a := range ready {
		infos = append(infos, h.info(a))
	}
	return nil, &exitcode.Error{Code: exitcode.Usage, Err: &service.SeveralAccountsError{Accounts: infos}, Hint: "name one with --account"}
}

func (h *stubHub) Account(selector string) (service.API, service.AccountInfo, error) {
	a, err := h.pick(selector)
	if err != nil {
		return nil, service.AccountInfo{}, err
	}
	return a.api, h.info(a), nil
}

// Login logs in the account whose address is the username, else the first.
func (h *stubHub) Login(ctx context.Context, r service.LoginRequest) (service.LoginResult, error) {
	h.record("login " + r.Username)
	list := h.list()
	if len(list) == 0 {
		return service.LoginResult{}, errors.New("a stubHub without an account")
	}
	a := list[0]
	for _, b := range list {
		if strings.EqualFold(h.info(b).Address, r.Username) {
			a = b
		}
	}
	res, err := a.api.Login(ctx, r)
	res.Account = a.id
	return res, err
}

func (h *stubHub) SubmitTOTP(ctx context.Context, sel, code string) (service.LoginResult, error) {
	a, err := h.pick(sel)
	if err != nil {
		return service.LoginResult{}, err
	}
	h.record("totp " + a.id)
	res, err := a.api.SubmitTOTP(ctx, code)
	res.Account = a.id
	return res, err
}

func (h *stubHub) SignOut(ctx context.Context, sel string, r service.LogoutRequest) (service.LogoutResult, error) {
	a, err := h.pick(sel)
	if err != nil {
		return service.LogoutResult{}, err
	}
	h.record("signout " + a.id)
	return a.api.Logout(ctx, r)
}

func (h *stubHub) Remove(ctx context.Context, sel string, r service.LogoutRequest) (service.AccountInfo, error) {
	a, err := h.pick(sel)
	if err != nil {
		return service.AccountInfo{}, err
	}
	h.record("remove " + a.id)
	if _, err := a.api.Logout(ctx, r); err != nil {
		return service.AccountInfo{}, err
	}
	info := h.info(a)
	h.mu.Lock()
	for i, b := range h.accounts {
		if b == a {
			h.accounts = append(h.accounts[:i], h.accounts[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
	return info, nil
}

func (h *stubHub) SetCalDAV(_ context.Context, sel string, on bool) (service.AccountInfo, error) {
	a, err := h.pick(sel)
	if err != nil {
		return service.AccountInfo{}, err
	}
	h.record("caldav " + a.id + " " + map[bool]string{true: "on", false: "off"}[on])
	h.mu.Lock()
	a.caldavOff = !on
	h.mu.Unlock()
	return h.info(a), nil
}

func (h *stubHub) Restart(_ context.Context, sel string) (service.AccountInfo, error) {
	a, err := h.pick(sel)
	if err != nil {
		return service.AccountInfo{}, err
	}
	h.record("restart " + a.id)
	return h.info(a), nil
}

func (h *stubHub) Health(ctx context.Context, sel string) (service.Health, error) {
	a, err := h.pick(sel)
	if err != nil {
		return service.Health{}, err
	}
	return a.api.Health(ctx)
}

func (*stubHub) OnChange(func()) {}

// Close closes every account's service; the first failure is returned.
func (h *stubHub) Close() error {
	h.mu.Lock()
	h.closed++
	h.mu.Unlock()
	var first error
	for _, a := range h.list() {
		if err := a.api.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Route answers the compatibility URLs with the first account's clients,
// and each account's own URLs with its.
func (h *stubHub) Route(ctx context.Context, accountID, password, remote string) (service.API, string, time.Duration, error) {
	list := h.list()
	if len(list) == 0 {
		return nil, "", 0, nil
	}
	a := list[0]
	if accountID != "" {
		a = nil
		for _, b := range list {
			if b.id == accountID {
				a = b
			}
		}
		if a == nil {
			return nil, "", 0, nil
		}
	}
	if ok, wait := a.api.CheckClient(ctx, password, remote); !ok {
		return nil, "", wait, nil
	}
	return a.api, a.id, 0, nil
}

// Serve serves the first account: its service's Serve, its states and
// passes reported under its ID.
func (h *stubHub) Serve(ctx context.Context, o service.HubServeOptions) error {
	list := h.list()
	if len(list) == 0 {
		return errors.New("a stubHub without an account")
	}
	a := list[0]
	so := o.ServeOptions
	so.OnState = func(s service.ServeState) {
		if o.OnState != nil {
			o.OnState(a.id, s)
		}
	}
	so.OnPass = func(p service.WatchPass) {
		if o.OnPass != nil {
			o.OnPass(a.id, p)
		}
	}
	return a.api.Serve(ctx, so)
}
