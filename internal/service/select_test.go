package service_test

import (
	"errors"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// SelectAccount is the account selector of Hub.Account and of the CLI over
// a daemon's list: by local ID first, else by address (case-insensitive),
// pending accounts included; without a selector the only ready account.
func TestSelectAccount(t *testing.T) {
	t.Parallel()
	a := service.AccountInfo{ID: "aaaaaaaa", Address: "a@x.test"}
	b := service.AccountInfo{ID: "bbbbbbbb", Address: "b@x.test"}
	p := service.AccountInfo{ID: "pppppppp", Address: "p@x.test", Pending: true}
	// An address that is another account's local ID: the ID wins.
	tricky := service.AccountInfo{ID: "cccccccc", Address: "aaaaaaaa"}
	tests := []struct {
		name     string
		list     []service.AccountInfo
		selector string
		want     string // the chosen ID; "" when refused
		msg      string
		hint     string
		several  []string
		code     exitcode.Code // the refusal's; Usage when 0
	}{
		{name: "by ID", list: []service.AccountInfo{a, b}, selector: "bbbbbbbb", want: "bbbbbbbb"},
		{name: "by address, any case", list: []service.AccountInfo{a, b}, selector: "B@X.Test", want: "bbbbbbbb"},
		{name: "ID before address", list: []service.AccountInfo{tricky, a}, selector: "aaaaaaaa", want: "aaaaaaaa"},
		{name: "a pending one named", list: []service.AccountInfo{a, p}, selector: "p@x.test", want: "pppppppp"},
		{name: "the only ready one", list: []service.AccountInfo{p, a}, want: "aaaaaaaa"},
		{name: "unknown", list: []service.AccountInfo{a}, selector: "z@x.test", msg: "no account z@x.test", hint: "see pcb account list"},
		// Ruling R38: with no account to choose a person must act (10).
		{name: "none", msg: "no account", hint: "add one with pcb login", code: exitcode.MustAct},
		{name: "only pending", list: []service.AccountInfo{p}, msg: "no account",
			hint: "finish the pending login (pcb login resume), or add an account with pcb login", code: exitcode.MustAct},
		{name: "several", list: []service.AccountInfo{a, p, b}, msg: "several accounts: name one with --account",
			hint: "name one with --account or PCB_ACCOUNT (pcb account list)", several: []string{"aaaaaaaa", "bbbbbbbb"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := service.SelectAccount(tt.list, tt.selector)
			if tt.want != "" {
				if err != nil || got.ID != tt.want {
					t.Fatalf("SelectAccount = %+v, %v; want %s", got, err, tt.want)
				}
				return
			}
			code := tt.code
			if code == 0 {
				code = exitcode.Usage
			}
			ee := wantExit(t, err, code)
			if ee.Error() != tt.msg || ee.Hint != tt.hint {
				t.Errorf("refusal %q, hint %q; want %q, %q", ee.Error(), ee.Hint, tt.msg, tt.hint)
			}
			var several *service.SeveralAccountsError
			switch {
			case tt.several == nil:
				if !errors.Is(err, service.ErrNoAccount) || errors.As(err, &several) {
					t.Errorf("cause %v, want ErrNoAccount", err)
				}
			case !errors.As(err, &several):
				t.Errorf("cause %v, want a *SeveralAccountsError", err)
			default:
				var ids []string
				for _, info := range several.Accounts {
					ids = append(ids, info.ID)
				}
				if len(ids) != len(tt.several) || ids[0] != tt.several[0] || ids[1] != tt.several[1] {
					t.Errorf("listed %v, want %v", ids, tt.several)
				}
			}
		})
	}
}
