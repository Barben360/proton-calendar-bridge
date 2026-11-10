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
