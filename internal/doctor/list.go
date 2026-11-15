package doctor

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// Checks lists the checks pcb doctor and the desktop app run, with one
// failing check per directory when the user directories cannot be located:
// the process checks, then the account checks of each account directory,
// named by the account (design §4.6), then the checks of the OS
// (tailChecks: on Linux pcb-ui beside the service, on Windows elevation,
// the time zone and Start at login). env looks up an environment variable;
// host is the machine (the pcb-ui.desktop and service checks); keyring is
// the desktop keyring a state sealed with it uses; live is the running
// daemon's listener (nil: none).
func Checks(env func(string) string, host install.Host, keyring service.Keyring, live *Live) []Check {
	checks := []Check{PlatformCheck{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}}
	dirs, err := paths.Resolve(env)
	if err != nil {
		return append(checks,
			FailedCheck{Label: "config dir", Message: err.Error(), Fix: dirFix(paths.EnvConfigDir, configBase)},
			FailedCheck{Label: "state dir", Message: err.Error(), Fix: dirFix(paths.EnvStateDir, stateBase)})
	}
	checks = append(checks,
		DirModeCheck{Label: "config dir", Path: dirs.Config, Want: 0o700},
		DirModeCheck{Label: "state dir", Path: dirs.State, Want: 0o700},
		ConfigCheck{Path: config.Path(dirs), Dirs: dirs})
	cfg, err := config.Load(config.Path(dirs), dirs)
	if err != nil { // ConfigCheck already reports it
		return checks
	}
	layout, lerr := Accounts(dirs.State)
	if layout.Sealer != "" { // the layout is readable
		sc := SealerCheck{Sealer: layout.Sealer, Fresh: layout.Fresh, Backend: cfg.Secrets.Backend}
		if c, ok := store.ReadSealerChoice(dirs.State); ok && c.Sealer == layout.Sealer {
			sc.Reason = c.Reason
		}
		checks = append(checks, sc)
	}
	switch {
	case layout.Sealer == service.SealerSecretService:
		checks = append(checks, KeyringCheck{Keyring: keyring})
	case layout.Sealer == service.SealerDPAPI, layout.Fresh && runtime.GOOS == "windows" && cfg.Secrets.Backend != service.SealerKeyFile:
		// A fresh state on Windows gets DPAPI's key unless the key file is
		// asked for (service's chooseSealerOn).
		checks = append(checks, DPAPIKeyCheck{Path: filepath.Join(dirs.State, seal.DPAPIKeyName), Sealed: anySealed(layout.Accounts), StateDir: dirs.State})
	default: // the key file, also while the layout cannot be read
		checks = append(checks, KeyFileCheck{Path: cfg.Secrets.KeyFile})
	}
	bind := &Bind{Listen: cfg.CalDAV.Listen} // resolved once for both CalDAV checks
	checks = append(checks,
		ListenerCheck{CalDAV: cfg.CalDAV, Bind: bind, Live: live},
		CertificateCheck{CalDAV: cfg.CalDAV, TLSDir: filepath.Join(dirs.State, "tls"), Bind: bind, Live: live},
		MCPTokensCheck{StateRoot: dirs.State, Remote: cfg.MCP.Remote, Listen: cfg.CalDAV.Listen, Bind: bind, Live: live})
	var layoutErr *LayoutError
	switch {
	case errors.As(lerr, &layoutErr):
		checks = append(checks, FailedCheck{Label: "accounts", Message: layoutErr.Error(), Fix: layoutErr.Fix})
	case layout.Interrupted != "":
		checks = append(checks, warnCheck{label: "accounts", message: "the move to the per-account layout stopped midway (" + layout.Interrupted +
			"): the next pcb command that opens the state finishes it", fix: "pcb status"})
	case len(layout.Accounts) == 0:
		checks = append(checks, warnCheck{label: "accounts", message: "no account: nothing can sign in to pcb serve",
			fix: "pcb login (it creates the client default), then pcb client show"})
	}
	sealer, err := service.NewSealer(dirs.State, layout.Sealer, cfg.Secrets.KeyFile, keyring)
	if err != nil {
		return append(checks, FailedCheck{Label: "sealer", Message: err.Error(), Fix: exitcode.HintOf(err)})
	}
	for _, acct := range layout.Accounts {
		prefix := ""
		if acct.Label != "" {
			prefix = acct.Label + ": "
		}
		for _, c := range []Check{
			FileModeCheck{Label: "sealed secrets", Path: filepath.Join(acct.Dir, "secrets.sealed"), Want: 0o600},
			SecretsCheck{StateDir: acct.Dir, KeyFile: cfg.Secrets.KeyFile, Sealer: sealer},
			CacheCheck{StateDir: acct.Dir},
			ClientsCheck{StateDir: acct.Dir},
			SealedClientsCheck{StateDir: acct.Dir, KeyFile: cfg.Secrets.KeyFile, Sealer: sealer},
		} {
			checks = append(checks, named{prefix: prefix, Check: c})
		}
	}
	return append(checks, tailChecks(env, host, cfg.MCP.Timezone)...)
}

// anySealed reports whether an account directory holds secrets.sealed.
func anySealed(accounts []Account) bool {
	for _, a := range accounts {
		if exists(filepath.Join(a.Dir, "secrets.sealed")) {
			return true
		}
	}
	return false
}

// warnCheck is a warning found without a check of its own to run.
type warnCheck struct{ label, message, fix string }

func (c warnCheck) Name() string { return c.label }

func (c warnCheck) Run(context.Context) Result {
	return Result{Name: c.label, Status: StatusWarn, Message: c.message, Fix: c.fix}
}

// named is a check whose results are named with prefix first (the
// account's).
type named struct {
	prefix string
	Check
}

func (n named) Name() string { return n.prefix + n.Check.Name() }

func (n named) Run(ctx context.Context) Result {
	r := n.Check.Run(ctx)
	r.Name = n.prefix + r.Name
	return r
}

func (n named) RunAll(ctx context.Context) []Result {
	var rs []Result
	if m, ok := n.Check.(Multi); ok {
		rs = m.RunAll(ctx)
	} else {
		rs = []Result{n.Check.Run(ctx)}
	}
	for i := range rs {
		rs[i].Name = n.prefix + rs[i].Name
	}
	return rs
}
