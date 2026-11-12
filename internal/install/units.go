package install

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// UnitParams fill a unit: the binary and the directories it runs with
// (M5-P18: set explicitly, as the service manager does not see the shell's
// XDG variables).
type UnitParams struct {
	Exec, ConfigDir, StateDir string
}

const unitHead = `[Unit]
Description=pcb, the Proton Calendar CalDAV bridge
Documentation=https://github.com/Barben360/proton-calendar-bridge
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
`

// unitHardening needs no user namespace, so both units carry it (M5-P17).
// No ~@resources: Go's runtime calls setrlimit. RestartPreventExitStatus=10
// (M5-R8): exit 10 is a fault only the person can repair (a damaged cache,
// a bad config), so systemd must not restart into it again.
const unitHardening = `Restart=on-failure
RestartSec=10s
RestartPreventExitStatus=10
TimeoutStopSec=30s
UMask=0077
NoNewPrivileges=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM
`

// systemHardening needs the system manager (ReadWritePaths is added
// before PrivateTmp).
const systemHardeningHead = `ProtectSystem=strict
ProtectHome=yes
`

const systemHardeningTail = `PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
CapabilityBoundingSet=
AmbientCapabilities=
`

// UserUnit is the unit of a --user install: the common hardening only.
func UserUnit(p UnitParams) []byte { return unit(p, false) }

// SystemUnit is the unit of a --system install: it runs as pcb with the
// full hardening.
func SystemUnit(p UnitParams) []byte { return unit(p, true) }

func unit(p UnitParams, system bool) []byte {
	var b strings.Builder
	b.WriteString(unitHead)
	if system {
		b.WriteString("User=" + SystemUser + "\nGroup=" + SystemUser + "\n")
	}
	fmt.Fprintf(&b, "Environment=PCB_CONFIG_DIR=%s\nEnvironment=PCB_STATE_DIR=%s\nExecStart=%s serve --wait-for-login\n",
		p.ConfigDir, p.StateDir, p.Exec)
	b.WriteString(unitHardening)
	target := "default.target"
	if system {
		b.WriteString(systemHardeningHead)
		fmt.Fprintf(&b, "ReadWritePaths=%s %s\n", p.ConfigDir, p.StateDir)
		b.WriteString(systemHardeningTail)
		target = "multi-user.target"
	}
	b.WriteString("\n[Install]\nWantedBy=" + target + "\n")
	return []byte(b.String())
}

// checkUnitParams refuses a value a unit cannot hold verbatim: systemd
// splits on spaces and expands %, $, quotes and backslashes.
func checkUnitParams(p UnitParams) error {
	for _, v := range []struct{ name, value string }{{"the pcb binary", p.Exec}, {"the config directory", p.ConfigDir}, {"the state directory", p.StateDir}} {
		if i := strings.IndexFunc(v.value, unsafeInUnit); i >= 0 {
			r, _ := utf8.DecodeRuneInString(v.value[i:])
			return fmt.Errorf("%s, %s, holds %q (U+%04X): pcb writes into a systemd unit only paths made of ASCII letters, digits and /._-+@,:=~",
				v.name, v.value, r, r)
		}
	}
	return nil
}

func unsafeInUnit(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("/._-+@,:=~", r)
}

// DefaultConfig is the config.toml an install creates: comments only, so
// it loads as config.Defaults.
func DefaultConfig() []byte { return []byte(defaultConfig) }

const defaultConfig = `# pcb configuration (SPEC §14). Unknown keys are refused; run pcb doctor after editing.
[caldav]
# listen = "127.0.0.1:1080"   # HOST:PORT; any non-loopback address serves TLS
# hostnames = ["pcb.example"] # names clients use: certificate SANs, first one in the URL
[log]
# level = "info"              # debug, info, warn or error
`
