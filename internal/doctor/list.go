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
)

// Checks lists the checks pcb doctor and the desktop app run, with one
// failing check per directory when the user directories cannot be located:
// the process checks, then the account checks of each account directory,
// named by the account (design §4.6). env looks up an environment variable;
// host is the machine (the pcb-ui.desktop and service checks); keyring is
// the desktop keyring a state sealed with it uses; live is the running
// daemon's listener (nil: none).
func Checks(env func(string) string, host install.Host, keyring service.Keyring, live *Live) []Check {
	checks := []Check{PlatformCheck{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}}
	dirs, err := paths.Resolve(env)
	if err != nil {
		fix := func(override, xdg string) string {
			return "set HOME, " + override + " or " + xdg + " to an absolute path"
		}
		return append(checks,
			FailedCheck{Label: "config dir", Message: err.Error(), Fix: fix(paths.EnvConfigDir, "XDG_CONFIG_HOME")},
			FailedCheck{Label: "state dir", Message: err.Error(), Fix: fix(paths.EnvStateDir, "XDG_STATE_HOME")})
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
		checks = append(checks, SealerCheck{Sealer: layout.Sealer, Fresh: layout.Fresh, Backend: cfg.Secrets.Backend})
	}
	if layout.Sealer == service.SealerSecretService {
		checks = append(checks, KeyringCheck{Keyring: keyring})
	} else { // the key file, also while the layout cannot be read
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
	return append(checks, DesktopCheck{AppDirs: desktopDirs(env, host), ServiceInstalled: func() bool { return serviceInstalled(host) }})
}

// serviceInstalled reports whether a pcb systemd unit is installed, user or
// system scope (an ambiguous install counts: both are units).
func serviceInstalled(host install.Host) bool {
	_, _, err := install.Detect(host)
	return err == nil || errors.Is(err, install.ErrAmbiguous)
}

// desktopDirs are the applications directories pcb-ui.desktop is looked
// for in: $XDG_DATA_HOME/applications (default ~/.local/share/applications),
// then the two system ones. A custom PREFIX install is not found.
func desktopDirs(env func(string) string, host install.Host) []string {
	var dirs []string
	data := env("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		if home := env("HOME"); filepath.IsAbs(home) {
			data = filepath.Join(home, ".local", "share")
		} else {
			data = ""
		}
	}
	if data != "" {
		dirs = append(dirs, filepath.Join(data, "applications"))
	}
	return append(dirs, host.Path("/usr/local/share/applications"), host.Path("/usr/share/applications"))
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
