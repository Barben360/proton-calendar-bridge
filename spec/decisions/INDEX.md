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
| [0016](0016-mcp-adapter.md) | An MCP adapter for agents, over stdio and remote; amends 0007 (accepted, planned as M8) |
