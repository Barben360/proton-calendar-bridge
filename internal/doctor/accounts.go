package doctor

import (
	"errors"
	"path/filepath"

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

// Layout is what pcb doctor found in a state directory.
type Layout struct {
	Accounts []Account
	// Interrupted is the directory of a move to the per-account layout that
	// stopped midway (accounts/<id>.migrating, or the account directory
	// without layout.json): the next command that opens the state finishes
	// it. No account is listed meanwhile.
	Interrupted string
	// Sealer is the sealer layout.json records ("keyfile" when it names
	// none, as an older pcb's state does); "" when the layout cannot be
	// read.
	Sealer string
	// Fresh marks a state with no layout.json, no account and nothing of
	// an older pcb: no sealer is recorded yet (Sealer says "keyfile"), the
	// first login chooses it.
	Fresh bool
}

// LayoutError is why the layout of a state directory cannot be read, with
// the fix.
type LayoutError struct {
	Err error
	Fix string
}

func (e *LayoutError) Error() string { return e.Err.Error() }
func (e *LayoutError) Unwrap() error { return e.Err }

// Accounts lists the account directories of stateDir and writes nothing
// (design §4.2: doctor reads both layouts): accounts/<id>/ in the
// per-account layout, stateDir itself while it holds the files of an
// older pcb's single account (the next command that opens the state moves
// them). A state that does not exist has none. A *LayoutError says why the
// layout cannot be read; the accounts it could list come with it.
func Accounts(stateDir string) (Layout, error) {
	info, err := store.ReadLayout(stateDir)
	var lerr error
	switch {
	case errors.Is(err, store.ErrLegacyBesideLayout):
		lerr = &LayoutError{Err: err, Fix: "remove them or move the state directory aside: this pcb version cannot be downgraded"}
	case errors.Is(err, store.ErrNewerLayout), errors.Is(err, store.ErrUnknownSealer):
		return Layout{}, &LayoutError{Err: err, Fix: "upgrade pcb"}
	case err != nil:
		return Layout{}, &LayoutError{Err: err, Fix: "check " + filepath.Join(stateDir, "layout.json") + " and the accounts directory beside it"}
	}
	if info.Version != store.LayoutVersion {
		sealer := info.Sealer
		switch {
		case info.Migrating != "":
			return Layout{Interrupted: store.AccountDir(stateDir, info.Migrating) + ".migrating", Sealer: sealer}, nil
		case len(info.Accounts) > 0:
			return Layout{Interrupted: store.AccountDir(stateDir, info.Accounts[0]), Sealer: sealer}, nil
		case len(info.Legacy) > 0:
			return Layout{Accounts: []Account{{Dir: stateDir, Label: address(stateDir)}}, Sealer: sealer}, nil
		}
		return Layout{Sealer: sealer, Fresh: true}, nil
	}
	out := Layout{Accounts: make([]Account, 0, len(info.Accounts)), Sealer: info.Sealer}
	for _, id := range info.Accounts {
		dir := store.AccountDir(stateDir, id)
		label := address(dir)
		if label == "" {
			label = id
		}
		out.Accounts = append(out.Accounts, Account{Dir: dir, Label: label})
	}
	return out, lerr
}

// address is the Proton address dir's state.json names, "" if none.
func address(dir string) string {
	st, err := store.LoadState(dir)
	if err != nil {
		return ""
	}
	return st.Address
}
