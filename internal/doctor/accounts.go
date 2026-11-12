package doctor

import (
	"errors"
	"fmt"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// Account is one account directory pcb doctor checks.
type Account struct {
	Dir string
	// Label names the account in the results: its address, else its local
	// ID; "" for the account of a state not moved to the per-account
	// layout yet (it holds one account).
	Label string
}

// Accounts lists the account directories of stateDir and writes nothing
// (design §4.2: doctor reads both layouts): accounts/<id>/ in the
// per-account layout, stateDir itself while it holds the files of an
// older pcb's single account (the next command that opens the state moves
// them). A state that does not exist has none. err says why the layout
// cannot be read; the accounts it could list come with it.
func Accounts(stateDir string) ([]Account, error) {
	info, err := store.ReadLayout(stateDir)
	switch {
	case errors.Is(err, store.ErrLegacyBesideLayout):
		err = fmt.Errorf("%w in %s (%v): remove them or move the state directory aside", err, stateDir, info.Legacy)
	case err != nil:
		return nil, fmt.Errorf("cannot read the state layout of %s: %w", stateDir, err)
	}
	if info.Version != store.LayoutVersion {
		if len(info.Legacy) > 0 {
			return []Account{{Dir: stateDir, Label: address(stateDir)}}, err
		}
		return nil, err
	}
	out := make([]Account, 0, len(info.Accounts))
	for _, id := range info.Accounts {
		dir := store.AccountDir(stateDir, id)
		label := address(dir)
		if label == "" {
			label = id
		}
		out = append(out, Account{Dir: dir, Label: label})
	}
	return out, err
}

// address is the Proton address dir's state.json names, "" if none.
func address(dir string) string {
	st, err := store.LoadState(dir)
	if err != nil {
		return ""
	}
	return st.Address
}
