# 0016 — An MCP adapter for agents, over stdio and remote

- **Date**: 2026-11-13
- **Decided by**: author (an MCP server in `pcb`, stdio and remote; read-only or more by configuration; login inside MCP; multi-account; header-capable clients with one bearer token per agent, no OAuth; stdio relays to a running daemon or serves the session itself; never a login through remote MCP; a per-account MCP switch), the rest the agent's calls (design `spec/designs/2026-11-13-mcp-design.md`, plan `spec/plans/2026-11-13-m8-mcp.md`)
- **Status**: accepted, planned as M8 (not built); amends 0007 (MCP was not planned), 0014 (one adapter switch per account becomes two) and SPEC §12's note "agents never on the control socket"

`pcb` gains A4, an MCP adapter over `service` like CalDAV, built on the official Go SDK (`github.com/modelcontextprotocol/go-sdk`, pure Go, `pcb` stays `CGO_ENABLED=0`).

- **Three entries, one server.** Remote: `/mcp` on the daemon's listener (`[mcp] remote = true`; TLS whenever the bind is not loopback), one `pcbm_` bearer token per agent, hash-only, shown once. stdio: `pcb mcp` relays the session to a running daemon's control socket (`/mcp` route), or, when none runs, runs the daemon machinery itself for the session (kind `mcp`, control socket, no network listener). Writes keep running only during Serve (M4).
- **Grants.** Capabilities `freebusy` < `read` < `write`, plus `delete`, `login`, `manage`, scoped by account and optionally calendar. `login` and `manage` exist only over stdio. stdio profiles: `read-only` (read + login, the default) and `full`.
- **Login without secrets in the agent.** `login(username)` (stdio only) raises `pcb-ui`'s wizard, or opens a single-use page on 127.0.0.1 in the process that owns the accounts; the person types password, 2FA code and CAPTCHA there. Remote agents never get a login tool.
- **Per-account switch.** `pcb account mcp on|off` beside `caldav`; an account's loop runs while either is on; `mcp off` hides the account from every agent.
- **Control socket.** Agents reach it only through its `/mcp` route, under the socket's same-UID trust, never through `/v2` routes. This replaces SPEC §12's note "agents never on the control socket". Over stdio the profile is a guard rail, not a boundary; over remote the token is a boundary.

Why:
- Decision 0004: agents are the long-term main consumer, and they need the full event model that CalDAV flattens.
- No daemon as a prerequisite for a local agent, without reopening M4's single-writer invariants (a per-call direct mode was reviewed and dropped).
- Secrets typed by the person, never passed through a model or its transcript (AGENTS.md "no secrets anywhere").

Consequences and limits:
- While a `pcb mcp` session serves a state, `pcb serve` and `pcb-ui` refuse to start (they name kind `mcp`); a hand-over is a later improvement. With a system-scope unit installed, `pcb mcp` does not serve and points to remote `/mcp`.
- Headless (remote) accounts are logged in with `pcb login` or `pcb-ui` only.
- New core work: event view, recurrence expansion, patch edits (no exception dropped silently).
- Cost if wrong: the `internal/mcp` package and the switch; the event layer stays useful to any later adapter.
