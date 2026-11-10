# proton-calendar-bridge

Private, design phase. The calendar counterpart of Proton Mail Bridge: a local
daemon that logs into Proton, decrypts Proton Calendar (end-to-end encrypted,
undocumented API) and serves it through standard and agent-friendly adapters.

Planned shape:

- **Core** (Go): accounts and login (as unattended as Proton allows), sync
  engine on Proton's change feed, encrypted local cache, access policy, and an
  internal event model based on JSCalendar (RFC 8984).
- **Adapters**: CalDAV (Thunderbird first), read-only `.ics` feeds, a REST API
  documented with OpenAPI 3.1 (management + automation), an MCP server for AI
  agents, a CLI on top of the REST API; GUIs later.
- **Runs** headless (systemd / Docker) or on a desktop, same daemon.

Reference material, not copied wholesale: `cheeseandcereal/proton-cal` and the
`Barben360/proton-cal` fork (reverse-engineered API docs, login and policy
work, audit findings), plus the community bridges carbonate, cal-gateway and
caloxide.

## Status

Design phase, milestone M0 (foundations): the CLI skeleton exists
(`pcb version`, the `--json` envelope, exit codes); there is no bridge yet.
The design is in `spec/SPEC.md`.

## Build

```sh
make build        # bin/pcb
make lint test    # gofmt, go vet, golangci-lint, go test -race
bin/pcb version
```

## Credits

Nothing is reused yet. When logic from another project is reused it is
audited, rewritten with our own tests and credited here. Reference projects
(spec §20): cheeseandcereal/proton-cal (Unlicense) and our fork
Barben360/proton-cal, develonrails/carbonate, jmdlab/cal-gateway,
WentTheFox/caloxide. ProtonMail/WebClients (GPL-3.0) is read for
understanding only, never copied.
