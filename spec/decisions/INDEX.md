# Decisions

| # | Title |
|---|---|
| [0001](0001-headless-single-binary.md) | Headless, single Go binary |
| [0002](0002-fresh-project-inspiration-not-copies.md) | Fresh project; inspiration, not copies |
| [0003](0003-mit-license.md) | MIT licence |
| [0004](0004-thunderbird-reference-agents-target.md) | Thunderbird is the reference client; AI agents are the main consumer |
| [0005](0005-test-account-only.md) | Live tests only on a dedicated test account |
| [0006](0006-jscalendar-internal-model.md) | JSCalendar (RFC 8984) as the internal model (superseded for v1 by 0012) |
| [0007](0007-scope-caldav-only.md) | Scope: a connectable CalDAV interface, nothing more |
| [0008](0008-official-client-identity-accepted.md) | An official client identity is acceptable if needed |
| [0009](0009-deployment-agnostic.md) | Runs anywhere; exposure is a network matter |
| [0010](0010-login-modes.md) | Login modes: password, password + TOTP |
| [0011](0011-client-identity.md) | Client identity: Other by default (provisional) |
| [0012](0012-icalendar-native-model.md) | iCalendar-native model for v1 (accepted) |
| [0013](0013-viewable-caldav-credentials.md) | CalDAV credentials can be shown again, Bridge-style (accepted) |
| [0014](0014-several-accounts-per-process.md) | Several Proton accounts per process; per-account CalDAV URLs (accepted) |
| [0015](0015-pcb-ui.md) | pcb-ui: a separate cgo binary running the daemon in process; the keyring sealer (accepted) |
| [0016](0016-mcp-adapter.md) | An MCP adapter for agents, over stdio and remote; amends 0007 (accepted, built in M8, live session run 2026-11-13/14) |
| [0017](0017-release-process.md) | Tag-driven releases, attestations instead of minisign, a public image pushed on release; amends SPEC §3 and §13 (accepted, built, released as v0.1.0) |
| [0018](0018-windows-port.md) | Windows port: `pcb-ui` started at login (no service), a private named pipe, DPAPI, a native `pcb-ui` build; amends SPEC §3, §13 and 0015 (accepted, built, live check passed; its "Next" 1 by 0019) |
| [0019](0019-update-check.md) | Tell that a newer stable release exists: pcb-ui, `pcb doctor`, `pcb version --check`, from GitHub's `releases/latest` redirect; amends SPEC §3, §13, §19 and 0015 (accepted, built) |
| [0019](0019-windows-msi.md) | Windows installer: a per-user WiX MSI (`pcb-ui` required, `pcb` optional on the user's `PATH`, a Start-menu shortcut, Start at login); amends 0018 and SPEC §13 (accepted, built, live check pending) |
