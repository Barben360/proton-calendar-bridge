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

Logic reused from another project is audited, rewritten with our own tests and credited here (decision 0002). Nothing below was copied.

- **cheeseandcereal/proton-cal** (Unlicense) and our fork **Barben360/proton-cal**: the reverse-engineered API and crypto documentation. `internal/otp` (TOTP) is inspired by the fork's `pkg/auth/totp.go`. `internal/lab/session` (SRP login, TOTP, human-verification stop and resume) and `internal/proton/auth` (login, CAPTCHA stop, TOTP, locked-scope unlock with server-proof check, lock) are inspired by `pkg/auth` in the fork (`auth.go`, `unlock.go`, `loginopts.go`) and its `docs/crypto.md` and `docs/overview.md`. The fork's CLI is also run as an external tool by the data-set seeder (`internal/lab/seed`).
- **go-proton-api** (MIT): the in-memory test server behind our shared fake Proton (`internal/proton/protontest`, an httptest front over its server, which the lab's `internal/lab/labtest` builds on).
- **develonrails/carbonate**, **WentTheFox/caloxide**, **ProtonMail/WebClients** (GPL-3.0): read only for the client identity strings tried in experiment E03 (`spec/experiments/E03-client-identity.md`). No code from any of them; WebClients is never copied.
- **Thunderbird's CalDAV provider** (mozilla/releases-comm-central, MPL-2.0): read for experiment E08 (`spec/experiments/E08-thunderbird-caldav.md`), source only, no code copied.
- jmdlab/cal-gateway and the other prior art listed in spec §20: design references only; nothing from them is used in this repository so far.
